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

// Pool is where pods come from. Acquire, HandBack and Pods may call out, so
// the core calls them without holding its lock; the others answer from the
// Pool's own state and may be called under it.
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
	// Record keeps what a held pod serves where a restarted core finds it
	// (see Restore). It must not block: the core calls it under its lock.
	Record(pod string, rec PodRecord)
	// Halted is why no task may be claimed now, empty when tasks may be;
	// the Pool shows the reason itself.
	Halted() string
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
