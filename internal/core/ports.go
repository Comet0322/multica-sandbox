package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"sandbox-manager/internal/lending"
)

// Task is a claimed task as the core sees it: the lending policy's view of
// it, the person behind it for the audit trail, and the body the pod gets,
// which the core hands over unread.
type Task struct {
	lending.Task
	// Email is the email of the person the task runs for, for the audit
	// trail; empty when no person triggered it.
	Email string
	// Payload is the task as the source claimed it, handed to the pod as it
	// is.
	Payload json.RawMessage
}

// Tenant is one tenant the source serves, with its lending settings.
type Tenant struct {
	ID       string
	Settings lending.TenantSettings
}

// TaskSource is where tasks come from. Every method may call out over the
// network, so the core never calls one while holding its lock.
type TaskSource interface {
	// Claimable lists the tenants that may be claimed from now, in rotation
	// order.
	Claimable() []string
	// Pending reads a tenant's pending list.
	Pending(ctx context.Context, tenant string) ([]lending.Pending, error)
	// Claim claims at most one task from a tenant; nil and no error when
	// nothing is claimable. The task's Tenant is the one it belongs to, which
	// the core checks against the tenant asked.
	Claim(ctx context.Context, tenant string) (*Task, error)
	// Fail reports a claimed task as failed. A report that does not get
	// through is the source's to send again.
	Fail(ctx context.Context, tenant, taskID, reason string)
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
	PodFree      PodState = "free"
	PodHeld      PodState = "held"
	PodResetting PodState = "resetting"
	PodLost      PodState = "lost"
)

// PodInfo is one pod as its Pool reports it.
type PodInfo struct {
	ID string
	// Name is the label the pod registered with, shown only.
	Name         string
	State        PodState
	RegisteredAt time.Time
	LastSeen     time.Time
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
	// Acquire takes a free pod that has been reset; ErrNoCapacity when the
	// pool has none.
	Acquire(ctx context.Context) (string, error)
	// HandBack returns an acquired pod, which the Pool resets.
	HandBack(ctx context.Context, pod string, why HandBack) error
	// Pod reports one pod; false when the Pool does not know it.
	Pod(id string) (PodInfo, bool)
	// Pods lists every pod the Pool knows, in registration order.
	Pods() []PodInfo
	// Tenants lists the tenants a pod can serve, sorted.
	Tenants(pod string) []string
	// Wake tells a pod a task is placed on it, when the pod can be told.
	Wake(pod string)
	// Preferred reports whether a pod is preferred for new work: it can be
	// woken at once.
	Preferred(pod string) bool
}

// Verdict is the core's answer to whether a pod may act for a tenant.
type Verdict int

const (
	// NotHeld: the core does not hold the pod; the Pool decides from the
	// pod's own state.
	NotHeld Verdict = iota
	// Allowed: the pod holds a task of the tenant.
	Allowed
	// Denied: the pod works for another tenant, or between its work and
	// its reset.
	Denied
	// Clean: the core holds the pod, but it runs nothing and has no binding,
	// so it is as clean as a free pod.
	Clean
)
