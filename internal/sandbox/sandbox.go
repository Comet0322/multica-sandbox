// Package sandbox is the sandbox service: it owns the pods themselves. It
// creates them through the Kubernetes API from the operator's pod template,
// issues their pod tokens, watches them, records on each what it serves, and
// deletes them (ADR 0009). Callers reach pods only through Service (see
// docs/adr/0004-pods-behind-a-sandbox-service.md).
//
// Create returns a ready pod no one holds and Delete deletes it: a pod is
// never reset. PutFiles, GetFiles and Exec return ErrUnsupported.
//
// The package imports nothing else of this module, so it can move to its own
// process unchanged.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"time"
)

// PodID identifies a pod: its name in Kubernetes.
type PodID string

// ErrNoCapacity is Create's answer when no pod is ready.
var ErrNoCapacity = errors.New("sandbox: no ready pod")

// ErrUnsupported is the answer of an operation the execution plane cannot
// perform.
var ErrUnsupported = errors.New("sandbox: not supported by this execution plane")

// Service is what callers use pods through.
type Service interface {
	// Create returns a pod that is ready and held by no one, which the
	// caller then holds. With none it returns ErrNoCapacity.
	Create(ctx context.Context) (PodID, error)
	// Delete deletes a held pod. label names the cause in the audit trail
	// only. Delete of a pod that is lost or unknown does nothing.
	Delete(ctx context.Context, id PodID, label AuditLabel) error
	// PutFiles unpacks the tar stream under dst in the pod.
	PutFiles(ctx context.Context, id PodID, dst string, tar io.Reader) error
	// GetFiles returns src in the pod as a tar stream.
	GetFiles(ctx context.Context, id PodID, src string) (io.ReadCloser, error)
	// Exec runs cmd in the pod.
	Exec(ctx context.Context, id PodID, cmd Command) (Result, error)
}

// Command is a command to run in a pod.
type Command struct {
	Argv []string
	Env  []string
	Dir  string
}

// Result is how a command ended.
type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Notifier tells the caller when a pod it holds is lost. The service calls it
// without holding its own lock, and only for held pods; the pod is no longer
// held when the call is made.
type Notifier interface {
	// Lost: the pod can run nothing any more (it ended, was evicted or
	// deleted), so whatever ran on it is gone. why says so in words.
	Lost(id PodID, why string)
}

// PodState is where a pod stands for callers.
type PodState string

const (
	// PodStarting: created, its daemon has not called the manager yet.
	PodStarting PodState = "starting"
	// PodFree: ready and held by no one; Create may return it.
	PodFree PodState = "free"
	// PodHeld: a caller acquired it with Create and has not deleted it.
	PodHeld PodState = "held"
	// PodLost: it can run nothing any more, or was deleted.
	PodLost PodState = "lost"
)

// DefaultLostRetention is how long a lost pod stays listed before it is pruned.
const DefaultLostRetention = 10 * time.Minute

// PodInfo is one pod as the inventory lists it.
type PodInfo struct {
	ID    PodID
	Label string
	State PodState
	// CreatedAt is when the pod was created; LastSeen when its daemon last
	// called the manager.
	CreatedAt time.Time
	LastSeen  time.Time
	// Reason is why a lost pod was lost, or why a starting pod is still
	// waiting, when known.
	Reason string
}

// Inventory lists the pods in creation order.
type Inventory struct {
	Pods []PodInfo
	// PrunedTotal counts the lost pods pruned since the service started.
	PrunedTotal int
	// Problem is why pods could not be created or started lately, empty
	// when none is known.
	Problem string
}

// podTokenPrefix marks a pod token.
const podTokenPrefix = "mpt_"

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
