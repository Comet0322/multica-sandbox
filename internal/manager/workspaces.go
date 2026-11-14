package manager

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// workspaceSet is the set of served workspaces: which workspaces the
// manager serves, which removed ones still drain, how listings are debounced
// and how the latest listing went. Its index is guarded by its own lock. The
// live state on each workspace record (registration, quota and claims, drain
// generation) belongs to the manager and is guarded by the manager's
// lock, which also serializes every change to the set (apply, finishDrain).
type workspaceSet struct {
	log *slog.Logger

	mu sync.Mutex
	// byID are the served workspaces by ID; order is the stable order the fair
	// rotation walks. apply replaces both (order as a fresh slice, never in
	// place), so a snapshot is safe to range over without the lock.
	byID  map[string]*workspace
	order []*workspace
	// draining holds removed workspaces that still have work.
	draining []*workspace
	// missing counts, by workspace ID, the listings in a row that left a served
	// workspace out; it is removed only on the syncRemoveAfter-th.
	missing map[string]int
	// lastSync is the outcome of the latest listing and lastSyncOK when one
	// last succeeded; readiness follows the latter.
	lastSync   *SyncStatus
	lastSyncOK time.Time
}

func newWorkspaceSet(log *slog.Logger) *workspaceSet {
	return &workspaceSet{log: log, byID: map[string]*workspace{}, missing: map[string]int{}}
}

// served is a snapshot of the served workspaces in rotation order.
func (s *workspaceSet) served() []*workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order
}

// get returns the served workspace with the ID, nil when none is.
func (s *workspaceSet) get(id string) *workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[strings.ToLower(id)]
}

// drainingList is a snapshot of the removed workspaces still draining.
func (s *workspaceSet) drainingList() []*workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.draining)
}

// apply replaces the served workspaces with next, which is normalized. A
// workspace that is new is made with connect, unless it is back before its
// drain ended: it is then served again as the same object, so its
// registration and running leases carry on and nothing treats them as
// orphans. It returns what changed and the workspaces dropped, which the
// caller drains. Callers hold the manager's lock, which guards the
// workspace records apply updates.
func (s *workspaceSet) apply(next []WorkspaceConfig, connect func(WorkspaceConfig) *workspace) (ReloadResult, []*workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res ReloadResult
	order := make([]*workspace, 0, len(next))
	byID := make(map[string]*workspace, len(next))
	for _, wc := range next {
		ws := s.byID[wc.WorkspaceID]
		if ws == nil {
			if i := slices.IndexFunc(s.draining, func(d *workspace) bool { return d.cfg.WorkspaceID == wc.WorkspaceID }); i >= 0 {
				// Its drain stops.
				ws = s.draining[i]
				s.draining = slices.Delete(s.draining, i, i+1)
				ws.removed = false
				ws.drainGen++
				ws.cfg.setSettings(wc)
			} else {
				ws = connect(wc)
			}
			res.Added = append(res.Added, wc.WorkspaceID)
		} else {
			if ws.cfg.RuntimeName != wc.RuntimeName {
				res.Unapplied = append(res.Unapplied, wc.WorkspaceID)
			}
			if ws.cfg.setSettings(wc) {
				res.Updated = append(res.Updated, wc.WorkspaceID)
			}
		}
		order = append(order, ws)
		byID[wc.WorkspaceID] = ws
	}
	var dropped []*workspace
	for _, ws := range s.order {
		if byID[ws.cfg.WorkspaceID] == nil {
			ws.removed = true
			ws.drainGen++
			dropped = append(dropped, ws)
			res.Removed = append(res.Removed, ws.cfg.WorkspaceID)
		}
	}
	s.byID, s.order = byID, order
	s.draining = append(s.draining, dropped...)
	return res, dropped
}

// finishDrain forgets a removed workspace whose drain ended.
func (s *workspaceSet) finishDrain(ws *workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = slices.DeleteFunc(s.draining, func(w *workspace) bool { return w == ws })
}

// debounce adds back to a listing the served workspaces it left out for fewer
// than syncRemoveAfter listings in a row.
func (s *workspaceSet) debounce(list []WorkspaceConfig) []WorkspaceConfig {
	listed := make(map[string]bool, len(list))
	for _, wc := range list {
		listed[strings.ToLower(wc.WorkspaceID)] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	served := make(map[string]bool, len(s.order))
	for _, ws := range s.order {
		id := ws.cfg.WorkspaceID
		served[id] = true
		if listed[id] {
			delete(s.missing, id)
			continue
		}
		s.missing[id]++
		if s.missing[id] >= syncRemoveAfter {
			delete(s.missing, id)
			continue
		}
		s.log.Info("workspace missing from the listing; kept until the next one confirms it", "workspace_id", id)
		list = append(list, ws.cfg)
	}
	for id := range s.missing {
		if !served[id] {
			delete(s.missing, id)
		}
	}
	return list
}

// recordSync keeps the outcome of a listing for the inventory and readiness.
func (s *workspaceSet) recordSync(err error, now time.Time) {
	st := &SyncStatus{OK: err == nil, At: now}
	if err != nil {
		st.Error = boundText(err.Error(), 300)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSync = st
	if err == nil {
		s.lastSyncOK = now
	}
}

// syncStatus copies the outcome of the latest listing; nil before the first.
func (s *workspaceSet) syncStatus() *SyncStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSync == nil {
		return nil
	}
	st := *s.lastSync
	return &st
}

// notReady explains why the served set is not trustworthy at now, or returns
// "" when it is: a listing must have succeeded within three sync intervals.
func (s *workspaceSet) notReady(now time.Time, interval time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSyncOK.IsZero() {
		return "no workspace listing has succeeded yet"
	}
	if now.Sub(s.lastSyncOK) > 3*interval {
		return "the last successful workspace listing is older than three sync intervals"
	}
	return ""
}
