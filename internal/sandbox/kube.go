package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// The marks the Kubernetes service puts on every pod it creates. It lists,
// watches and deletes only pods that carry ManagedByLabel=ManagedByValue.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "sandbox-manager"
	// PodTokenEnv is the environment variable the pod's entrypoint reads its
	// pod token from.
	PodTokenEnv = "SANDBOX_POD_TOKEN"
	// PodHostname is the hostname of a pod whose template sets none. Without
	// it the hostname would be the pod's name, which agents print into what
	// reaches Multica.
	PodHostname = "sandbox"
	// DaemonContainer is the name of the container the daemon runs in, when
	// the template has more than one.
	DaemonContainer = "daemon"
)

// Defaults of a KubeService.
const (
	DefaultMaxPods      = 10
	DefaultStartTimeout = 3 * time.Minute
	// createBackoffMin and createBackoffMax bound the wait after a pod that
	// could not be created or did not start, doubling from one to the other.
	createBackoffMin = 10 * time.Second
	createBackoffMax = 5 * time.Minute
	// resyncInterval is how often the service lists its pods again, in case
	// the watch missed an event.
	resyncInterval = 30 * time.Second
	// apiTimeout bounds one Kubernetes API call.
	apiTimeout = 30 * time.Second
)

// KubeConfig configures a KubeService.
type KubeConfig struct {
	// Client reaches the Kubernetes API.
	Client kubernetes.Interface
	// Namespace is where the pods are created.
	Namespace string
	// Template is the pod every pod is created from (see LoadPodTemplate).
	Template *corev1.Pod
	// MaxPods is the most pods the service has at once, starting, free,
	// held or being deleted. Zero means DefaultMaxPods.
	MaxPods int
	// WarmPods is how many ready pods the service keeps that no one holds.
	// Zero keeps none: a pod is created only when Create finds none.
	WarmPods int
	// StartTimeout is how long a new pod may take until its daemon first
	// calls the manager. Past it the pod is deleted. Zero means
	// DefaultStartTimeout.
	StartTimeout time.Duration
	// LostRetention is how long a lost pod stays listed. Zero means
	// DefaultLostRetention.
	LostRetention time.Duration
	// Notify is told when a held pod is lost. Nil tells no one.
	Notify Notifier
	// Audit takes the deletion records. Nil records nothing.
	Audit AuditWriter
	// Log is the service's logger. Nil means slog.Default().
	Log *slog.Logger
	// Now is the time source. Nil means time.Now.
	Now func() time.Time
	// TickInterval is how often the service checks start timeouts and the
	// warm pods. Zero means one second.
	TickInterval time.Duration
}

func (c *KubeConfig) normalize() error {
	switch {
	case c.Client == nil:
		return errors.New("a Kubernetes client is required")
	case c.Namespace == "":
		return errors.New("a namespace is required")
	case c.Template == nil:
		return errors.New("a pod template is required")
	case c.MaxPods < 0 || c.WarmPods < 0 || c.StartTimeout < 0 || c.LostRetention < 0:
		return errors.New("pod counts and timeouts must not be negative")
	}
	if _, err := daemonContainer(c.Template); err != nil {
		return err
	}
	if c.MaxPods == 0 {
		c.MaxPods = DefaultMaxPods
	}
	if c.WarmPods > c.MaxPods {
		return fmt.Errorf("warm pods (%d) exceed the most pods (%d)", c.WarmPods, c.MaxPods)
	}
	if c.StartTimeout == 0 {
		c.StartTimeout = DefaultStartTimeout
	}
	if c.LostRetention == 0 {
		c.LostRetention = DefaultLostRetention
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.TickInterval <= 0 {
		c.TickInterval = time.Second
	}
	return nil
}

// LoadPodTemplate reads the operator's pod YAML. It must be one Pod with the
// daemon's container named DaemonContainer, or with a single container.
// Unknown fields are refused, so a typo fails at start.
func LoadPodTemplate(path string) (*corev1.Pod, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the pod template: %w", err)
	}
	var pd corev1.Pod
	if err := yaml.UnmarshalStrict(raw, &pd); err != nil {
		return nil, fmt.Errorf("parse the pod template %s: %w", path, err)
	}
	if pd.Kind != "" && pd.Kind != "Pod" {
		return nil, fmt.Errorf("the pod template %s is a %s, not a Pod", path, pd.Kind)
	}
	if _, err := daemonContainer(&pd); err != nil {
		return nil, fmt.Errorf("the pod template %s: %w", path, err)
	}
	return &pd, nil
}

// daemonContainer is the index of the container the pod token goes to.
func daemonContainer(pd *corev1.Pod) (int, error) {
	cs := pd.Spec.Containers
	for i, c := range cs {
		if c.Name == DaemonContainer {
			return i, nil
		}
	}
	if len(cs) == 1 {
		return 0, nil
	}
	if len(cs) == 0 {
		return 0, errors.New("the pod has no container")
	}
	return 0, fmt.Errorf("the pod has %d containers and none is named %q", len(cs), DaemonContainer)
}

// KubeService is the sandbox service for pods it creates through the
// Kubernetes API from a pod template. A pod is ready once its daemon first
// calls the manager with the pod's token. Create takes a ready pod no one
// holds, and Delete deletes the pod: nothing is ever reset.
type KubeService struct {
	cfg KubeConfig
	log *slog.Logger

	mu     sync.Mutex
	pods   map[PodID]*kubePod
	tokens map[string]PodID
	seq    uint64
	// wantUntil keeps one more pod coming than the warm count, after Create
	// found none ready.
	wantUntil time.Time
	// backoffUntil and backoff delay the next creation after a failure.
	backoffUntil time.Time
	backoff      time.Duration
	// problem is the last reason pods could not be created or started, shown
	// in the inventory until a pod gets ready.
	problem     string
	prunedTotal int
	notes       []func()
	// run is the context of Run, used for the API calls it starts; nil
	// before Run.
	run context.Context
	// restored is set once Restore took over the pods a previous run left.
	restored bool
	// dirty are the held pods whose record changed and is not written yet;
	// kick wakes the loop that writes them.
	dirty map[PodID]bool
	kick  chan struct{}
	// recordRefused is why Kubernetes refused the last record, empty once
	// one is written. While it is set no pod is given out (see Halted).
	recordRefused string
}

type kubePod struct {
	id        PodID
	token     string
	seq       uint64
	state     PodState
	createdAt time.Time
	// startBy is when the start timeout began: the creation, or the
	// restore of a pod a previous run left.
	startBy  time.Time
	readyAt  time.Time
	lastSeen time.Time
	lostAt   time.Time
	// waiting is why the pod is not running yet, from its status.
	waiting string
	// record is what the pod serves, as last recorded (see Record).
	record PodRecord
	// lostWhy is why a lost pod was lost.
	lostWhy string
}

// NewKube returns a sandbox service with no pods. Run starts it.
func NewKube(cfg KubeConfig) (*KubeService, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &KubeService{cfg: cfg, log: cfg.Log, pods: map[PodID]*kubePod{}, tokens: map[string]PodID{},
		dirty: map[PodID]bool{}, kick: make(chan struct{}, 1)}, nil
}

func (k *KubeService) now() time.Time { return k.cfg.Now() }

// MaxPods is the most pods the service has at once: the budget.
func (k *KubeService) MaxPods() int { return k.cfg.MaxPods }

// StartTimeout is how long a new pod may take to get ready.
func (k *KubeService) StartTimeout() time.Duration { return k.cfg.StartTimeout }

func (k *KubeService) unlock() {
	notes := k.notes
	k.notes = nil
	k.mu.Unlock()
	for _, n := range notes {
		n()
	}
}

func (k *KubeService) selector() string { return ManagedByLabel + "=" + ManagedByValue }

// Run watches the pods, times out the ones that do not start, keeps the warm
// pods and writes the pods' records, until ctx ends. Without a Restore
// before it, it restores the pods a previous run left itself and deletes
// those that served anyone, which no caller holds.
func (k *KubeService) Run(ctx context.Context) {
	k.mu.Lock()
	k.run = ctx
	restored := k.restored
	k.mu.Unlock()
	if !restored {
		served, _ := k.Restore(ctx)
		for _, r := range served {
			_ = k.Delete(ctx, r.ID, AuditLabel{Reason: "left by a previous run"})
		}
	}
	go k.watchLoop(ctx)
	go k.recordLoop(ctx)
	t := time.NewTicker(k.cfg.TickInterval)
	defer t.Stop()
	resync := time.NewTicker(resyncInterval)
	defer resync.Stop()
	k.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			k.tick()
		case <-resync.C:
			k.resync(ctx)
		}
	}
}

func (k *KubeService) tick() {
	k.mu.Lock()
	defer k.unlock()
	k.sweepLocked()
	k.topUpLocked()
}

// sweepLocked deletes pods that did not start in time and prunes lost pods
// past their retention.
func (k *KubeService) sweepLocked() {
	now := k.now()
	for _, pd := range k.pods {
		switch {
		case pd.state == PodStarting && now.Sub(pd.startBy) > k.cfg.StartTimeout:
			why := fmt.Sprintf("pod %s did not start within %s", pd.id, k.cfg.StartTimeout)
			if pd.waiting != "" {
				why += ": " + pd.waiting
			}
			k.log.Warn("pod did not start", "pod", pd.id, "reason", pd.waiting)
			k.setProblemLocked(why)
			k.backOffLocked()
			k.loseLocked(pd, why)
			k.deleteLocked(pd.id)
		case pd.state == PodLost && now.Sub(pd.lostAt) > k.cfg.LostRetention:
			delete(k.pods, pd.id)
			delete(k.tokens, pd.token)
			k.prunedTotal++
		}
	}
}

// topUpLocked creates pods until the ready and starting ones no one holds
// reach the warm count, plus one while Create recently found none, within
// the most pods and outside a backoff.
func (k *KubeService) topUpLocked() {
	now := k.now()
	if k.run == nil || now.Before(k.backoffUntil) {
		return
	}
	spare, total := 0, 0
	for _, pd := range k.pods {
		if pd.state == PodLost {
			continue
		}
		total++
		if pd.state == PodFree || pd.state == PodStarting {
			spare++
		}
	}
	want := k.cfg.WarmPods
	if now.Before(k.wantUntil) {
		want++
	}
	for ; spare < want && total < k.cfg.MaxPods; spare, total = spare+1, total+1 {
		k.createLocked()
	}
}

// createLocked adds a starting pod and creates it in Kubernetes.
func (k *KubeService) createLocked() {
	now := k.now()
	pd := &kubePod{token: podTokenPrefix + randomHex(32), state: PodStarting, createdAt: now, startBy: now}
	for {
		pd.id = PodID("sandbox-" + randomHex(5))
		if k.pods[pd.id] == nil {
			break
		}
	}
	k.seq++
	pd.seq = k.seq
	k.pods[pd.id], k.tokens[pd.token] = pd, pd.id
	obj := k.podObject(pd)
	ctx := k.run
	go func() {
		c, cancel := context.WithTimeout(ctx, apiTimeout)
		defer cancel()
		_, err := k.cfg.Client.CoreV1().Pods(k.cfg.Namespace).Create(c, obj, metav1.CreateOptions{})
		if err == nil {
			k.log.Info("pod created", "pod", pd.id)
			return
		}
		k.failed("create pod "+string(pd.id), err)
		k.mu.Lock()
		defer k.unlock()
		k.backOffLocked()
		if cur := k.pods[pd.id]; cur == pd {
			delete(k.pods, pd.id)
			delete(k.tokens, pd.token)
		}
	}()
}

// podObject is the template with the service's own fields set.
func (k *KubeService) podObject(pd *kubePod) *corev1.Pod {
	obj := k.cfg.Template.DeepCopy()
	obj.TypeMeta = metav1.TypeMeta{}
	obj.ObjectMeta = metav1.ObjectMeta{
		Name:        string(pd.id),
		Namespace:   k.cfg.Namespace,
		Labels:      map[string]string{},
		Annotations: map[string]string{},
	}
	for key, v := range k.cfg.Template.Labels {
		obj.Labels[key] = v
	}
	for key, v := range k.cfg.Template.Annotations {
		obj.Annotations[key] = v
	}
	obj.Labels[ManagedByLabel] = ManagedByValue
	obj.Spec.RestartPolicy = corev1.RestartPolicyNever
	if obj.Spec.Hostname == "" {
		obj.Spec.Hostname = PodHostname
	}
	i, _ := daemonContainer(obj)
	c := &obj.Spec.Containers[i]
	env := c.Env[:0:0]
	for _, e := range c.Env {
		if e.Name != PodTokenEnv {
			env = append(env, e)
		}
	}
	c.Env = append(env, corev1.EnvVar{Name: PodTokenEnv, Value: pd.token})
	return obj
}

// failed logs a failed API call and keeps its reason for the inventory, and
// returns it.
func (k *KubeService) failed(what string, err error) string {
	why := what + ": " + err.Error()
	if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
		why = "Kubernetes refused the manager's credentials: " + why
	}
	k.log.Error("kubernetes", "error", why)
	k.mu.Lock()
	k.setProblemLocked(why)
	k.unlock()
	return why
}

func isNotFound(err error) bool { return apierrors.IsNotFound(err) }

func (k *KubeService) setProblemLocked(why string) { k.problem = why }

func (k *KubeService) backOffLocked() {
	switch {
	case k.backoff == 0:
		k.backoff = createBackoffMin
	case k.backoff < createBackoffMax:
		k.backoff = min(2*k.backoff, createBackoffMax)
	}
	k.backoffUntil = k.now().Add(k.backoff)
}

// loseLocked marks the pod lost and tells the caller when it held it.
func (k *KubeService) loseLocked(pd *kubePod, why string) {
	if pd.state == PodLost {
		return
	}
	held := pd.state == PodHeld
	pd.state, pd.lostAt, pd.lostWhy = PodLost, k.now(), why
	if held && k.cfg.Notify != nil {
		n, id := k.cfg.Notify, pd.id
		k.notes = append(k.notes, func() { n.Lost(id, why) })
	}
}

// deleteLocked deletes the pod in Kubernetes, in the background.
func (k *KubeService) deleteLocked(id PodID) {
	if k.run == nil {
		return
	}
	ctx := k.run
	go k.deleteAPI(ctx, string(id))
}

func (k *KubeService) deleteAPI(ctx context.Context, name string) {
	c, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	err := k.cfg.Client.CoreV1().Pods(k.cfg.Namespace).Delete(c, name, metav1.DeleteOptions{})
	if err != nil && !isNotFound(err) {
		k.failed("delete pod "+name, err)
	}
}

// Authenticate returns the pod that holds the pod token. The first call
// makes a starting pod ready: its daemon is up and reaches the manager.
func (k *KubeService) Authenticate(token string) (PodID, bool) {
	k.mu.Lock()
	defer k.unlock()
	id, ok := k.tokens[token]
	if !ok {
		return "", false
	}
	pd := k.pods[id]
	if pd == nil || pd.state == PodLost {
		return "", false
	}
	now := k.now()
	pd.lastSeen = now
	if pd.state == PodStarting {
		pd.state, pd.readyAt = PodFree, now
		k.backoff, k.problem = 0, ""
		k.log.Info("pod ready", "pod", pd.id, "after", now.Sub(pd.createdAt).Round(time.Second).String())
	}
	return id, true
}

// Create acquires a ready pod no one holds, the one ready longest first. With
// none it returns ErrNoCapacity, with the reason pods are not starting when
// one is known, and has one more pod created.
func (k *KubeService) Create(ctx context.Context) (PodID, error) {
	k.mu.Lock()
	defer k.unlock()
	k.sweepLocked()
	if k.recordRefused != "" {
		return "", fmt.Errorf("%w: %s", ErrNoCapacity, k.recordRefused)
	}
	var pick *kubePod
	for _, pd := range k.pods {
		if pd.state == PodFree && (pick == nil || pd.readyAt.Before(pick.readyAt) || pd.readyAt.Equal(pick.readyAt) && pd.seq < pick.seq) {
			pick = pd
		}
	}
	if pick == nil {
		k.wantUntil = k.now().Add(k.cfg.StartTimeout)
		k.topUpLocked()
		if k.problem != "" {
			return "", fmt.Errorf("%w: %s", ErrNoCapacity, k.problem)
		}
		return "", ErrNoCapacity
	}
	pick.state = PodHeld
	k.topUpLocked()
	return pick.id, nil
}

// Delete deletes a pod. A lost or unknown pod is left alone.
func (k *KubeService) Delete(ctx context.Context, id PodID, label AuditLabel) error {
	k.mu.Lock()
	defer k.unlock()
	pd := k.pods[id]
	if pd == nil || pd.state == PodLost {
		return nil
	}
	// A pod deleted on purpose leaves the inventory at once; only a lost pod
	// stays listed, with its reason.
	delete(k.pods, id)
	delete(k.tokens, pd.token)
	delete(k.dirty, id)
	k.log.Info("pod deleted", "pod", id, "reason", label.Reason)
	if k.cfg.Audit != nil {
		k.cfg.Audit.Record(AuditRecord{
			Time: k.now().UTC(), Action: AuditDelete, Decision: AuditAllow, Reason: "pod deleted: " + label.Reason,
			TaskID: label.TaskID, WorkspaceID: label.WorkspaceID, Initiator: label.Initiator,
			PodID: string(id), PodName: string(id),
		})
	}
	k.deleteLocked(id)
	k.topUpLocked()
	return nil
}

// watchLoop follows the pods' status until ctx ends, watching again whenever
// a watch ends.
func (k *KubeService) watchLoop(ctx context.Context) {
	for ctx.Err() == nil {
		w, err := k.cfg.Client.CoreV1().Pods(k.cfg.Namespace).Watch(ctx, metav1.ListOptions{LabelSelector: k.selector()})
		if err != nil {
			k.failed("watch the pods", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for ev := range w.ResultChan() {
			pd, ok := ev.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			k.observe(pd, ev.Type == watch.Deleted)
		}
		w.Stop()
	}
}

// resync lists the pods again and treats every pod of ours that Kubernetes
// no longer has as deleted.
func (k *KubeService) resync(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	list, err := k.cfg.Client.CoreV1().Pods(k.cfg.Namespace).List(c, metav1.ListOptions{LabelSelector: k.selector()})
	if err != nil {
		k.failed("list the pods", err)
		return
	}
	seen := map[PodID]bool{}
	for i := range list.Items {
		seen[PodID(list.Items[i].Name)] = true
		k.observe(&list.Items[i], false)
	}
	k.mu.Lock()
	defer k.unlock()
	cutoff := k.now().Add(-apiTimeout)
	for _, pd := range k.pods {
		// A pod created moments ago may not be listed yet.
		if pd.state != PodLost && !seen[pd.id] && pd.createdAt.Before(cutoff) {
			k.loseLocked(pd, "pod "+string(pd.id)+" no longer exists")
		}
	}
}

// observe takes one pod's status from Kubernetes.
func (k *KubeService) observe(obj *corev1.Pod, deleted bool) {
	k.mu.Lock()
	defer k.unlock()
	pd := k.pods[PodID(obj.Name)]
	if pd == nil || pd.state == PodLost {
		return
	}
	if why, gone := podGone(obj, deleted); gone {
		k.log.Warn("pod ended", "pod", pd.id, "state", pd.state, "reason", why)
		if pd.state == PodStarting {
			k.setProblemLocked(why)
			k.backOffLocked()
		}
		k.loseLocked(pd, why)
		k.deleteLocked(pd.id)
		return
	}
	pd.waiting = podWaiting(obj)
}

// podGone reports whether the pod can no longer run anything, and why, in
// words a member can read.
func podGone(pd *corev1.Pod, deleted bool) (string, bool) {
	for _, cs := range pd.Status.ContainerStatuses {
		t := cs.State.Terminated
		if t == nil {
			continue
		}
		if t.Reason == "OOMKilled" {
			return "the pod ran out of memory" + memoryLimit(pd, cs.Name), true
		}
		if t.ExitCode == 137 && pd.DeletionTimestamp == nil && hasLivenessProbe(pd, cs.Name) {
			// With restartPolicy Never a failed liveness probe kills the
			// container and leaves only this behind: killed, not out of
			// memory, and the pod not being deleted.
			who := cs.Name + " container"
			if i, err := daemonContainer(pd); err == nil && pd.Spec.Containers[i].Name == cs.Name {
				who = "daemon"
			}
			return "the pod's " + who + " stopped answering (its liveness probe failed)", true
		}
		why := fmt.Sprintf("the pod's %s container exited with code %d", cs.Name, t.ExitCode)
		if t.Reason != "" {
			why += " (" + t.Reason + ")"
		}
		return why, true
	}
	switch {
	case pd.Status.Phase == corev1.PodFailed || pd.Status.Phase == corev1.PodSucceeded:
		why := "the pod stopped"
		if pd.Status.Reason == "Evicted" {
			why = "the node evicted the pod"
		}
		if pd.Status.Message != "" {
			why += ": " + pd.Status.Message
		}
		return why, true
	case deleted || pd.DeletionTimestamp != nil:
		return "the pod was deleted", true
	}
	return "", false
}

// hasLivenessProbe reports whether the container has a liveness probe.
func hasLivenessProbe(pd *corev1.Pod, container string) bool {
	for _, c := range pd.Spec.Containers {
		if c.Name == container {
			return c.LivenessProbe != nil
		}
	}
	return false
}

// memoryLimit names the container's memory limit, for an out-of-memory
// reason.
func memoryLimit(pd *corev1.Pod, container string) string {
	for _, c := range pd.Spec.Containers {
		if c.Name != container {
			continue
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			return " (limit " + q.String() + ")"
		}
	}
	return ""
}

// podWaiting is why a pod that has not started is still waiting, when its
// status says.
func podWaiting(pd *corev1.Pod) string {
	for _, c := range pd.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
			return "not scheduled: " + c.Message
		}
	}
	for _, cs := range pd.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
			msg := w.Reason
			if w.Message != "" {
				msg += ": " + w.Message
			}
			return strings.TrimSpace(msg)
		}
	}
	return ""
}

// Pod reports one pod; false when the service does not know it.
func (k *KubeService) Pod(id PodID) (PodInfo, bool) {
	k.mu.Lock()
	defer k.unlock()
	pd := k.pods[id]
	if pd == nil {
		return PodInfo{}, false
	}
	return pd.info(), true
}

// Inventory lists the pods in creation order, with the last reason pods
// could not be created or started.
func (k *KubeService) Inventory() Inventory {
	k.mu.Lock()
	defer k.unlock()
	list := make([]*kubePod, 0, len(k.pods))
	for _, pd := range k.pods {
		list = append(list, pd)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].seq < list[j].seq })
	problem := k.problem
	if k.recordRefused != "" {
		problem = k.recordRefused
	}
	inv := Inventory{Pods: make([]PodInfo, 0, len(list)), PrunedTotal: k.prunedTotal, Problem: problem}
	for _, pd := range list {
		inv.Pods = append(inv.Pods, pd.info())
	}
	return inv
}

func (pd *kubePod) info() PodInfo {
	reason := pd.lostWhy
	if pd.state == PodStarting {
		reason = pd.waiting
	}
	return PodInfo{ID: pd.id, Label: string(pd.id), State: pd.state, CreatedAt: pd.createdAt, LastSeen: pd.lastSeen, Reason: reason}
}

// PutFiles is not supported.
func (k *KubeService) PutFiles(context.Context, PodID, string, io.Reader) error {
	return ErrUnsupported
}

// GetFiles is not supported.
func (k *KubeService) GetFiles(context.Context, PodID, string) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}

// Exec is not supported.
func (k *KubeService) Exec(context.Context, PodID, Command) (Result, error) {
	return Result{}, ErrUnsupported
}

var _ Service = (*KubeService)(nil)
