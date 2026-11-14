package manager

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"

	"sandbox-manager/internal/lending"
	"sandbox-manager/internal/sandbox"
)

// The manager reaches pods only through the sandbox service
// (docs/adr/0004-pods-behind-a-sandbox-service.md). It acquires a pod with
// Create when the lending policy asks for one, keeps it while it lends it or
// until the policy hands it back, and hands it back with Delete. The service
// tells it when a pod it holds is interrupted or lost.

// podService is the sandbox service as the manager uses it: the Service
// interface, plus the in-process plumbing beside it.
type podService interface {
	sandbox.Service
	// Authenticate returns the pod that holds a pod token.
	Authenticate(token string) (sandbox.PodID, bool)
	// Inventory lists every pod with its state.
	Inventory() sandbox.Inventory
	// Pod reports one pod; false when the service does not know it.
	Pod(id sandbox.PodID) (sandbox.PodInfo, bool)
}

// pod is a pod the manager holds: acquired from the sandbox service and
// not handed back.
type pod struct {
	ID string
	// Name is the label the pod registered with (such as its Kubernetes pod
	// name), shown only.
	Name string
	// seq orders the held pods by acquisition.
	seq uint64
	// leases are the tasks the pod runs, by task ID, and queue the claimed
	// tasks the lending policy placed on it that it has not picked up yet.
	leases map[string]*lease
	queue  []*claimedTask
}

// handBack is a pod the policy handed back, with the cause for the audit
// trail.
type handBack struct {
	id    string
	label sandbox.AuditLabel
}

// notifier takes the sandbox service's notifications about held pods. The
// service calls it without its own lock held.
type notifier struct{ p *Manager }

func (n notifier) Interrupted(id sandbox.PodID, why string) {
	n.p.podGone(string(id), why, func(pol *lending.Policy) []lending.Decision {
		return pol.PodInterrupted(string(id), "the pod's daemon restarted", why)
	})
}

func (n notifier) Lost(id sandbox.PodID, why string) {
	n.p.podGone(string(id), why, podLost(string(id)))
}

// podLost is the policy's decision for a held pod that went silent or that
// the sandbox service no longer lends.
func podLost(id string) func(*lending.Policy) []lending.Decision {
	return func(pol *lending.Policy) []lending.Decision {
		return pol.PodLost(id, "the pod running this task was lost", "pod lost")
	}
}

// podGone forgets a held pod the sandbox service no longer lends: the policy
// fails its started tasks and places the others elsewhere.
func (p *Manager) podGone(id, why string, tell func(*lending.Policy) []lending.Decision) {
	p.mu.Lock()
	p.podGoneLocked(id, why, tell)
	p.mu.Unlock()
	p.settle(context.Background())
}

// podGoneLocked is podGone with p.mu held.
func (p *Manager) podGoneLocked(id, why string, tell func(*lending.Policy) []lending.Decision) {
	pd := p.held[id]
	if pd == nil {
		return
	}
	p.log.Warn("held pod gone; its tasks are failed or placed elsewhere", "pod_id", id, "name", pd.Name, "reason", why, "tasks", len(pd.leases))
	delete(p.held, id)
	p.applyLocked(tell(p.policy))
}

// sandboxAudit writes the sandbox service's audit records to the manager's
// audit log, which shares their shape.
type sandboxAudit struct{ log *auditLog }

func (a sandboxAudit) Record(rec sandbox.AuditRecord) { a.log.write(AuditRecord(rec)) }

// acquire takes a free pod from the sandbox service and records it with the
// policy, with the workspaces its daemon registered for. It reports false
// when the service has none to give. A pod the service no longer lends by the
// time it is recorded (a notification for it arrived before the manager
// knew it) is not recorded; that counts as an acquisition, so the caller
// tries again.
func (p *Manager) acquire(ctx context.Context) bool {
	id, err := p.sandbox.Create(ctx)
	if err != nil {
		if !errors.Is(err, sandbox.ErrNoCapacity) {
			p.log.Warn("acquire a pod from the sandbox service", "error", err)
		}
		return false
	}
	// The check and the record are one step under p.mu: a notification that
	// comes after it finds the pod held, and one that came before is seen here.
	p.mu.Lock()
	info, ok := p.sandbox.Pod(id)
	if !ok || info.State != sandbox.PodHeld {
		p.mu.Unlock()
		p.log.Warn("acquired pod is gone already; not recorded", "pod_id", id)
		return true
	}
	name := info.Label
	p.heldSeq++
	p.held[string(id)] = &pod{ID: string(id), Name: name, seq: p.heldSeq}
	workspaces := make([]string, 0, len(p.daemonWorkspaces[string(id)]))
	for ws := range p.daemonWorkspaces[string(id)] {
		workspaces = append(workspaces, ws)
	}
	sort.Strings(workspaces)
	p.applyLocked(p.policy.PodAcquired(string(id), workspaces...))
	p.mu.Unlock()
	p.log.Info("pod acquired", "pod_id", id, "name", name, "workspaces", workspaces)
	p.settle(ctx)
	return true
}

// handBackLocked forgets a held pod the policy decided to reset and queues it
// for Delete, with the lease that caused the reset as its audit label.
// Callers hold p.mu, and settle once they release it.
func (p *Manager) handBackLocked(d lending.Reset) {
	pd := p.held[d.Pod]
	if pd == nil {
		return
	}
	delete(p.held, d.Pod)
	label := sandbox.AuditLabel{Reason: d.Reason}
	if l := p.leaseOfLocked(d.Task); l != nil {
		label.TaskID, label.WorkspaceID, label.Initiator = l.TaskID, l.WorkspaceID, l.Initiator
	}
	p.log.Info("pod handed back to be reset", "pod_id", pd.ID, "reason", d.Reason)
	p.handBacks = append(p.handBacks, handBack{id: pd.ID, label: label})
}

// settle carries out what decisions taken under p.mu left for the network:
// it hands the pods back to the sandbox service and fails upstream the tasks
// the policy decided to fail. Callers invoke it without holding p.mu.
func (p *Manager) settle(ctx context.Context) {
	p.mu.Lock()
	backs := p.handBacks
	p.handBacks = nil
	p.mu.Unlock()
	for _, b := range backs {
		if err := p.sandbox.Delete(context.WithoutCancel(ctx), sandbox.PodID(b.id), b.label); err != nil {
			p.log.Error("hand a pod back to the sandbox service", "pod_id", b.id, "error", err)
		}
	}
	p.reportForced(ctx)
}

// healHeldLocked forgets the held pods the sandbox service no longer lists
// as held (a notification that could not be delivered, such as one for a pod
// pruned meanwhile), and the daemon workspaces of pods it no longer lists at
// all. Callers hold p.mu.
func (p *Manager) healHeldLocked() {
	for id := range p.daemonWorkspaces {
		if _, ok := p.sandbox.Pod(sandbox.PodID(id)); !ok {
			delete(p.daemonWorkspaces, id)
		}
	}
	for _, id := range p.heldOrderLocked() {
		if info, ok := p.sandbox.Pod(sandbox.PodID(id)); !ok || info.State != sandbox.PodHeld {
			p.podGoneLocked(id, "the sandbox service no longer lends the pod", podLost(id))
		}
	}
}

// heldOrderLocked lists the held pods to reserve for a claim, best first:
// those with a wake socket, then the others, each in acquisition order.
// Callers hold p.mu.
func (p *Manager) heldOrderLocked() []string {
	pods := make([]*pod, 0, len(p.held))
	for _, pd := range p.held {
		pods = append(pods, pd)
	}
	slices.SortFunc(pods, func(a, b *pod) int {
		if wa, wb := p.api.Wakeable(a.ID), p.api.Wakeable(b.ID); wa != wb {
			if wa {
				return -1
			}
			return 1
		}
		return int(a.seq) - int(b.seq)
	})
	ids := make([]string, len(pods))
	for i, pd := range pods {
		ids[i] = pd.ID
	}
	return ids
}

// podByToken returns the ID of the pod holding the pod token.
func (p *Manager) podByToken(token string) (string, bool) {
	id, ok := p.sandbox.Authenticate(token)
	return string(id), ok
}

// sandboxPaths serves the sandbox service's pod paths, not found when the
// service brings none.
func (p *Manager) sandboxPaths() http.Handler {
	if p.sandboxHandler == nil {
		return http.NotFoundHandler()
	}
	return p.sandboxHandler
}

// podFree reports whether the sandbox service lists the pod as free.
func (p *Manager) podFree(id string) bool {
	info, ok := p.sandbox.Pod(sandbox.PodID(id))
	return ok && info.State == sandbox.PodFree
}

// boundText flattens whitespace and cuts s to at most n bytes, for text that
// came from outside and goes into logs and audit records.
func boundText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

// initiatorsOf lists the distinct initiators of the tasks the pod holds, sorted.
func initiatorsOf(pd *pod) []string {
	var out []string
	for _, l := range pd.leases {
		if l.Initiator != "" && !slices.Contains(out, l.Initiator) {
			out = append(out, l.Initiator)
		}
	}
	sort.Strings(out)
	return out
}
