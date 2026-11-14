package sandbox

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RegisterPath is where a pod's entrypoint registers the pod (ADR 0003). It is
// the only pod call without a pod token.
const RegisterPath = "/pod/register"

// ControlPath is the control channel pod supervisors poll (GET) and report to
// (POST). It identifies the caller by its pod token, so a pod only ever sees
// and answers its own command.
const ControlPath = "/pod/control"

// Supervisor events (POST ControlPath with an "event" field). A daemon the
// supervisor restarted presents the same pod token, so it would otherwise
// pass for the old one still running its tasks.
const (
	// eventDaemonExited: the daemon exited without being told to; the
	// supervisor starts it again.
	eventDaemonExited = "daemon-exited"
	// eventStarted: the supervisor itself started, so the container did.
	eventStarted = "started"
)

// maxPodLabel bounds the display label a pod registers with.
const maxPodLabel = 253

// maxReportedFailures bounds how many paths of one reset report are logged.
const maxReportedFailures = 50

// controlReport is a supervisor's report on a command, or a supervisor event.
type controlReport struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Failed lists the paths the reset could not delete.
	Failed []string `json:"failed"`
	// Event is a supervisor event instead of a command outcome.
	Event string `json:"event"`
}

// Handler serves RegisterPath and ControlPath to pods. The caller mounts it on
// the listener the pods reach; any other path is not found.
func (p *GitOpsService) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case RegisterPath:
			p.serveRegister(w, r)
		case ControlPath:
			id, ok := p.Authenticate(bearer(r))
			if !ok {
				p.log.Warn("refused pod request: unknown pod token", "method", r.Method, "path", r.URL.Path)
				http.Error(w, errUnknownPod.Error(), http.StatusUnauthorized)
				return
			}
			p.serveControl(w, r, id)
		default:
			http.NotFound(w, r)
		}
	})
}

// serveRegister registers a new pod and answers its ID and pod token. The
// body may carry a display label, such as the Kubernetes pod name; the
// service assigns the identity itself.
func (p *GitOpsService) serveRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, token := p.registerPod(boundText(req.Label, maxPodLabel))
	writeJSON(w, http.StatusOK, map[string]string{"pod_id": string(id), "token": token})
}

// serveControl answers a supervisor's poll with its pending reset command, or
// takes its report on one, or a supervisor event.
func (p *GitOpsService) serveControl(w http.ResponseWriter, r *http.Request, id PodID) {
	switch r.Method {
	case http.MethodGet:
		resetID, err := p.controlPoll(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if resetID == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": resetID, "command": "reset"})
	case http.MethodPost:
		var rep controlReport
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rep); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := p.controlReport(id, rep); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// seenLocked records control-channel traffic from the pod, the only sign of
// its liveness. A lost pod that speaks again is the same container, which may
// have run anything meanwhile: it is reset before it is acquired again.
// Callers hold p.mu.
func (p *GitOpsService) seenLocked(pd *pod) {
	pd.lastSeen = p.now()
	if pd.state == PodLost {
		p.startResetLocked(pd, "lost pod came back; it is reset before it is lent again", AuditLabel{})
	}
}

// controlPoll returns the pod's pending reset command, "" when there is none.
func (p *GitOpsService) controlPoll(id PodID) (string, error) {
	p.mu.Lock()
	defer p.unlock()
	p.sweepLocked()
	pd := p.pods[id]
	if pd == nil {
		return "", errUnknownPod
	}
	p.seenLocked(pd)
	if pd.state != PodResetting {
		return "", nil
	}
	return pd.resetID, nil
}

// controlReport takes a supervisor's report on its reset command, or a
// supervisor event.
func (p *GitOpsService) controlReport(id PodID, rep controlReport) error {
	p.mu.Lock()
	defer p.unlock()
	p.sweepLocked()
	pd := p.pods[id]
	if pd == nil {
		return errUnknownPod
	}
	p.seenLocked(pd)
	if rep.Event != "" {
		// Unknown events are acknowledged and ignored, so the supervisor does
		// not resend them.
		p.log.Info("pod supervisor event", "pod_id", id, "event", boundText(rep.Event, 40), "state", pd.state)
		switch rep.Event {
		case eventDaemonExited:
			p.interruptLocked(pd, "pod daemon restarted")
		case eventStarted:
			p.interruptLocked(pd, "pod container restarted")
		}
		return nil
	}
	// A report for any other command than the pending one is stale (the reset
	// timed out, or the pod came back since); it is acknowledged so the
	// supervisor stops resending, and changes nothing.
	if pd.state != PodResetting || rep.ID != pd.resetID || rep.Status != "done" {
		return nil
	}
	// A reset is a file operation the daemon keeps running through: the
	// report completes it. Paths left behind are logged and the pod is free
	// again.
	failed := rep.Failed
	if len(failed) > maxReportedFailures {
		failed = failed[:maxReportedFailures]
	}
	for _, path := range failed {
		p.log.Warn("pod reset left a path behind", "pod_id", id, "reset_id", rep.ID, "path", boundText(path, 512))
	}
	reason := "reset completed"
	if n := len(rep.Failed); n > 0 {
		p.log.Warn("pod reset could not delete every path; lending the pod again", "pod_id", id, "reset_id", rep.ID, "failed", n)
		reason = fmt.Sprintf("reset completed; %d paths could not be deleted", n)
	}
	p.log.Info("pod reset completed", "pod_id", id, "reset_id", pd.resetID)
	p.auditReset(pd, AuditAllow, reason)
	pd.state, pd.resetID, pd.resetLabel = PodFree, "", AuditLabel{}
	return nil
}

// interruptLocked handles a held pod whose daemon or container restarted by
// itself: what ran on it is gone, so its holder is told and the pod is reset.
// A pod nobody holds is left alone. Callers hold p.mu.
func (p *GitOpsService) interruptLocked(pd *pod, why string) {
	if pd.state != PodHeld {
		return
	}
	p.log.Warn("held pod interrupted; it is reset", "pod_id", pd.id, "name", pd.label, "reason", why)
	p.startResetLocked(pd, why, AuditLabel{})
	p.notifyLocked(func(n Notifier) { n.Interrupted(pd.id, why) })
}

// bearer is the request's bearer token, "" when it has none.
func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(token)
}

// boundText flattens whitespace and cuts s to at most n bytes, for text that
// came from outside and goes into logs.
func boundText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
