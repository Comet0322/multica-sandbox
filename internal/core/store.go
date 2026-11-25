package core

import (
	"context"
	"time"

	"sandbox-manager/internal/policy"
)

// Store keeps the core's dispatch record, so a core that starts takes over
// what a previous run left: the pods it held and the tasks it claimed. The
// core writes a row whenever what it holds changes; each row replaces the
// earlier row of the same ID.
type Store interface {
	// Load reads the record a previous run left: the pods it held and did
	// not let go, in the order it acquired them, and the tasks that did not
	// end or whose failure report the source has not taken, in the order
	// they were claimed.
	Load(ctx context.Context) (Record, error)
	// Write saves the rows, each replacing the row of the same ID: all of
	// them, or none when it returns an error.
	Write(ctx context.Context, pods []PodRow, tasks []TaskRow) error
}

// Record is the dispatch record as a Store loads it.
type Record struct {
	Pods  []PodRow
	Tasks []TaskRow
}

// PodRow is a pod the core holds, or held: acquired from the Pool and not
// handed back.
type PodRow struct {
	ID string
	// Phase, Tenant, Kind, Key, Since and Until are what the policy knows of
	// the pod (policy.PodInfo).
	Phase        policy.Phase
	Tenant       string
	Kind         policy.Kind
	Key          string
	Since, Until time.Time
	// CreatedAt is when the pod was created, zero when the Pool did not
	// say; ReadyAt is when the core acquired it, ready.
	CreatedAt, ReadyAt time.Time
	// GoneAt is when the core let the pod go, handed back or lost, and
	// GoneWhy why; zero while the core holds it.
	GoneAt  time.Time
	GoneWhy string
}

// TaskState is where a claimed task stands.
type TaskState string

// The states of a claimed task, in the order it goes through them.
const (
	// TaskPlaced: claimed, and placed on a pod or waiting for one; no pod
	// has been handed it.
	TaskPlaced TaskState = "placed"
	// TaskHanded: handed to its pod's daemon, which has not reported it
	// started. The pod is handed it again until it does.
	TaskHanded TaskState = "handed"
	// TaskRunning: its pod reported it started.
	TaskRunning TaskState = "running"
	// TaskEnded: over, whatever the outcome.
	TaskEnded TaskState = "ended"
)

// TaskRow is a task the core claimed. A Store keeps its Payload, encrypted,
// only while the task is placed or handed, and never keeps its Email.
type TaskRow struct {
	Task
	// PodID is the pod the task is placed on, handed to or runs on; empty
	// while it waits for a pod.
	PodID string
	State TaskState
	// ClaimedAt is when the task was claimed, StartedAt when its pod was
	// first handed it, EndedAt when it ended and EndReason why.
	ClaimedAt, StartedAt, EndedAt time.Time
	EndReason                     string
	// FailReason is the failure the core reports to the source;
	// FailPending is set until the source took the report.
	FailReason  string
	FailPending bool
}
