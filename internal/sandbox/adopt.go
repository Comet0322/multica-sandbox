package sandbox

import (
	"context"
	"errors"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Adopt takes over the pods a previous run left: a pod named in held, with
// a pod token of its own, is held by the caller from now on, its token taken
// from its environment; every other pod is deleted, warm ones too, and the
// warm pods are made again. A held pod that ended meanwhile is reported lost
// once Run watches it; one that is gone is not adopted, which the caller
// finds by asking for it (Pod). It first reads the owner deployment, when
// one is set, and fails when it cannot. Call it once, before Run.
func (k *KubeService) Adopt(ctx context.Context, held []PodID) error {
	if err := k.resolveOwner(ctx); err != nil {
		return err
	}
	c, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	list, err := k.cfg.Client.CoreV1().Pods(k.cfg.Namespace).List(c, metav1.ListOptions{LabelSelector: k.selector()})
	if err != nil {
		return errors.New(k.failed("list the pods", err))
	}
	items := list.Items
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].CreationTimestamp, items[j].CreationTimestamp
		if !a.Equal(&b) {
			return a.Before(&b)
		}
		return items[i].Name < items[j].Name
	})
	keep := map[PodID]bool{}
	for _, id := range held {
		keep[id] = true
	}
	var drop []string
	unlock := k.lock()
	k.adopted = true
	now := k.now()
	for i := range items {
		obj := &items[i]
		id := PodID(obj.Name)
		ticket := podToken(obj)
		switch {
		case !keep[id]:
			drop = append(drop, obj.Name)
			continue
		case ticket == "" || k.tickets[ticket] != "" || k.pods[id] != nil:
			k.log.Warn("deleting a pod with no pod token of its own", "pod", id)
			drop = append(drop, obj.Name)
			continue
		}
		k.seq++
		k.pods[id] = &kubePod{id: id, ticket: ticket, seq: k.seq, state: PodHeld,
			createdAt: obj.CreationTimestamp.Time, startBy: now, readyAt: now}
		k.tickets[ticket] = id
	}
	unlock()
	for _, name := range drop {
		k.deleteAPI(ctx, name)
	}
	k.log.Info("pods adopted", "held", len(items)-len(drop), "deleted", len(drop))
	return nil
}

// podToken is the pod token in the pod's daemon container.
func podToken(obj *corev1.Pod) string {
	i, err := daemonContainer(obj)
	if err != nil {
		return ""
	}
	for _, e := range obj.Spec.Containers[i].Env {
		if e.Name == PodTicketEnv && strings.HasPrefix(e.Value, podTicketPrefix) {
			return e.Value
		}
	}
	return ""
}
