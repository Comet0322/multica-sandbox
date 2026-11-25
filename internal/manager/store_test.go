package manager_test

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"sandbox-manager/internal/core"
)

// memStore keeps the dispatch record in memory as the Postgres store does:
// the latest row of each ID, a payload only while its task is placed or
// handed, no email. A kubeWorld keeps one across the managers it starts.
type memStore struct {
	mu    sync.Mutex
	pods  map[string]core.PodRow
	tasks map[string]core.TaskRow
	// err is what Write returns; nil writes.
	err error
}

func newMemStore() *memStore {
	return &memStore{pods: map[string]core.PodRow{}, tasks: map[string]core.TaskRow{}}
}

func (s *memStore) Load(context.Context) (core.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rec core.Record
	for _, p := range s.pods {
		if p.GoneAt.IsZero() {
			rec.Pods = append(rec.Pods, p)
		}
	}
	slices.SortFunc(rec.Pods, func(a, b core.PodRow) int {
		return cmp.Or(a.ReadyAt.Compare(b.ReadyAt), cmp.Compare(a.ID, b.ID))
	})
	for _, t := range s.tasks {
		if t.State != core.TaskEnded || t.FailPending {
			rec.Tasks = append(rec.Tasks, t)
		}
	}
	slices.SortFunc(rec.Tasks, func(a, b core.TaskRow) int {
		return cmp.Or(a.ClaimedAt.Compare(b.ClaimedAt), cmp.Compare(a.ID, b.ID))
	})
	return rec, nil
}

func (s *memStore) Write(_ context.Context, pods []core.PodRow, tasks []core.TaskRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	for _, p := range pods {
		s.pods[p.ID] = p
	}
	for _, t := range tasks {
		t.Email = ""
		if t.State != core.TaskPlaced && t.State != core.TaskHanded {
			t.Payload = nil
		}
		s.tasks[t.ID] = t
	}
	return nil
}

// failWith makes Write fail with err, nil to write again.
func (s *memStore) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// task is the task's row, as a restarted manager would load it.
func (s *memStore) task(id string) core.TaskRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[id]
}
