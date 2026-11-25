package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"sandbox-manager/internal/policy"
)

// Task is a claimed task as the core sees it: the policy's view of
// it, the person behind it for the audit trail, and the body the pod gets,
// which the core hands over unread.
type Task struct {
	policy.Task
	// Email is the email of the person the task runs for, for the audit
	// trail; empty when no person triggered it.
	Email string
	// Payload is the task as the source claimed it, handed to the pod as it
	// is.
	Payload json.RawMessage
}

// Tenant is one tenant the source serves, with its policy settings.
type Tenant struct {
	ID       string
	Settings policy.TenantSettings
}

// TaskSource is where tasks come from. Every method may call out over the
// network, so the core never calls one while holding its lock.
type TaskSource interface {
	// Claimable lists the tenants that may be claimed from now, in rotation
	// order.
	Claimable() []string
	// Pending reads the pending list of a tenant's runtimes of one kind.
	Pending(ctx context.Context, tenant string, kind policy.Kind) ([]policy.Pending, error)
	// Claim claims at most one task from a tenant's runtimes of one kind;
	// nil and no error when nothing is claimable. The task's Tenant is the
	// one it belongs to, which the core checks against the tenant asked;
	// its Kind is the kind claimed for.
	Claim(ctx context.Context, tenant string, kind policy.Kind) (*Task, error)
	// Fail reports a claimed task as failed. An error means the report did
	// not get through and is to be sent again; a report the upstream took,
	// or refused for good, returns none.
	Fail(ctx context.Context, tenant, taskID, reason string) error
	// TaskEnded reports whether a leased task is over upstream, and why. A
	// task whose state cannot be read now is not over.
	TaskEnded(ctx context.Context, tenant, taskID string) (reason string, ended bool)
	// Close lets the source forget a removed tenant, once its drain ended.
	Close(tenant string)
}

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

// ErrNoCapacity is the Pool's answer when it has no free pod to give.
var ErrNoCapacity = errors.New("no free pod")

// PodState is the state a Pool reports a pod in.
type PodState string

// Pod states.
const (
	PodStarting PodState = "starting"
	PodFree     PodState = "free"
	PodHeld     PodState = "held"
	PodLost     PodState = "lost"
)

// PodInfo is one pod as its Pool reports it.
type PodInfo struct {
	ID string
	// Name is the pod's name, shown only.
	Name      string
	State     PodState
	CreatedAt time.Time
	LastSeen  time.Time
	// Reason is why a lost pod was lost, or why a starting pod still waits.
	Reason string
}

// PodHealth is how pod creation goes, for the inventory: why pods could not
// be created or started lately, why they do not start at all, and the
// latest pod failures, newest first, with every failure counted by kind.
type PodHealth struct {
	Problem       string
	Broken        string
	Failures      []PodFailure
	FailureCounts map[string]int
}

// PodFailure is one pod failure as the sandbox service classified it.
type PodFailure struct {
	At  time.Time `json:"at"`
	Pod string    `json:"pod,omitempty"`
	// Phase is create, start or run; Kind what went wrong (oom, evicted,
	// image, refused...); Permanent whether retrying does not help.
	Phase     string `json:"phase"`
	Kind      string `json:"kind"`
	Permanent bool   `json:"permanent"`
	What      string `json:"what"`
	Advice    string `json:"advice,omitempty"`
}

// HandBack says why a pod is handed back, for the audit trail: the reason
// and the lease that caused it, when one did.
type HandBack struct {
	Reason    string
	TaskID    string
	Tenant    string
	Initiator string
}

// Pool is where pods come from. Acquire, HandBack, Pods and Adopt may call
// out, so the core calls them without holding its lock; the others answer
// from the Pool's own state and may be called under it.
type Pool interface {
	// Acquire takes a ready pod that served no one; ErrNoCapacity when the
	// pool has none.
	Acquire(ctx context.Context) (string, error)
	// HandBack returns an acquired pod, which the Pool deletes.
	HandBack(ctx context.Context, pod string, why HandBack) error
	// Pod reports one pod; false when the Pool does not know it.
	Pod(id string) (PodInfo, bool)
	// Pods lists every pod the Pool knows, in creation order.
	Pods() []PodInfo
	// Tenants lists the tenants a pod can serve, sorted.
	Tenants(pod string) []string
	// Wake tells a pod a task is placed on it, when the pod can be told.
	Wake(pod string)
	// Preferred reports whether a pod is preferred for new work: it can be
	// woken at once.
	Preferred(pod string) bool
	// Adopt takes over the pods a previous run held, each with the tenant it
	// serves (empty for a warm pod): those that still exist are held from
	// now on, and every other pod the Pool finds is deleted. The core calls
	// it once, from Restore, before it serves anything.
	Adopt(ctx context.Context, pods []HeldPod) error
}

// HeldPod is a pod a previous run held, as the core asks the Pool to adopt
// it.
type HeldPod struct {
	ID string
	// Tenant is the tenant the pod serves, empty while it is warm.
	Tenant string
}

// Verdict is the core's answer to whether a pod may act for a tenant.
type Verdict int

const (
	// NotHeld: the core does not hold the pod; the Pool decides from the
	// pod's own state.
	NotHeld Verdict = iota
	// Allowed: the pod holds a task of the tenant.
	Allowed
	// Denied: the pod works for another tenant, or served a task and has
	// none placed on it now.
	Denied
	// Clean: the core holds the pod, but it is still warm: it served no one,
	// so it is as clean as a free pod.
	Clean
)
