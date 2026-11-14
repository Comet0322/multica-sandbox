package dispatcher

import "time"

// A model list request is the picker in the Multica UI asking which models the
// runtime's agent CLI can run. Multica hands it to the runtime in the answer to
// a heartbeat. The runtime is the manager's, but the CLIs are in the pods, whose
// image is the same everywhere, so the manager passes the request to one pod's
// daemon, which asks its CLI and reports to the manager, which relays the
// report upstream with the workspace's account.

const (
	// modelRequestRetry is how long a pod has to report before the request is
	// handed to a pod again.
	modelRequestRetry = 45 * time.Second
	// modelRequestExpiry is when an unanswered request is forgotten; Multica
	// has timed it out by then.
	modelRequestExpiry = 3 * time.Minute
)

// modelRequest is a model list request Multica made for a runtime.
type modelRequest struct {
	id     string
	seen   time.Time
	handed time.Time
	handTo string // ID of the pod it was handed to
}

// noteModelRequest remembers the request the upstream heartbeat brought.
func (a *podAPI) noteModelRequest(runtimeID, id string) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	if a.modelReqs == nil {
		a.modelReqs = map[string]*modelRequest{}
	}
	if cur := a.modelReqs[runtimeID]; cur != nil && cur.id == id {
		return
	}
	a.modelReqs[runtimeID] = &modelRequest{id: id, seen: a.clock.now()}
}

// takeModelRequest returns the request id to put into the heartbeat answer for
// the pod, or "" when there is none for it.
func (a *podAPI) takeModelRequest(runtimeID, podID string) string {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	r := a.modelReqs[runtimeID]
	if r == nil {
		return ""
	}
	now := a.clock.now()
	if now.Sub(r.seen) > modelRequestExpiry {
		delete(a.modelReqs, runtimeID)
		return ""
	}
	if r.handTo != "" && now.Sub(r.handed) < modelRequestRetry {
		return ""
	}
	r.handTo, r.handed = podID, now
	return r.id
}

// modelRequestHeldBy reports whether the request was handed to the pod.
func (a *podAPI) modelRequestHeldBy(runtimeID, id, podID string) bool {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	r := a.modelReqs[runtimeID]
	return r != nil && r.id == id && r.handTo == podID
}

// finishModelRequest forgets the request once Multica has taken the report.
func (a *podAPI) finishModelRequest(runtimeID, id string) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	if r := a.modelReqs[runtimeID]; r != nil && r.id == id {
		delete(a.modelReqs, runtimeID)
	}
}
