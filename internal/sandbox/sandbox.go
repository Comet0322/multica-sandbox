// Package sandbox is the sandbox service: it owns the pods themselves. It
// registers them and issues their pod tokens, runs the control channel their
// supervisors poll, resets them, judges their liveness, prunes the lost ones,
// and keeps the set of free pods. Callers reach pods only through Service
// (see docs/adr/0004-pods-behind-a-sandbox-service.md).
//
// Pods start on their own (GitOps) and register themselves, so Create
// acquires a free pod that has been reset and Delete hands it back to be
// reset; neither provisions anything. The pod supervisor cannot transfer files
// or run commands, so PutFiles, GetFiles and Exec return ErrUnsupported.
//
// The package imports only the standard library and generic helpers, so it
// can move to its own process unchanged.
package sandbox

import (
	"context"
	"errors"
	"io"
)

// PodID identifies a pod. The service assigns it when the pod registers.
type PodID string

// ErrNoCapacity is Create's answer when no pod is free.
var ErrNoCapacity = errors.New("sandbox: no free pod")

// ErrUnsupported is the answer of an operation the execution plane cannot
// perform.
var ErrUnsupported = errors.New("sandbox: not supported by this execution plane")

// Service is what callers use pods through.
type Service interface {
	// Create returns a pod that is registered, live, clean and held by no
	// one, which the caller then holds. With none it returns ErrNoCapacity.
	Create(ctx context.Context) (PodID, error)
	// Delete hands a held pod back. It starts the pod's reset and returns;
	// the pod is free again only once its supervisor reports the reset done.
	// label names the cause in the audit trail only. Delete of a pod that is
	// lost, resetting or unknown does nothing.
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

// Notifier tells the caller when a pod it holds stops being usable. The
// service calls it without holding its own lock, and only for held pods; the
// pod is no longer held when the call is made.
type Notifier interface {
	// Interrupted: the pod's daemon or container restarted by itself, so
	// whatever ran on it is gone. The service resets the pod.
	Interrupted(id PodID, why string)
	// Lost: the pod went silent, so nothing is known of what it did.
	Lost(id PodID, why string)
}

// PutFiles is not supported by the current supervisor.
func (p *GitOpsService) PutFiles(context.Context, PodID, string, io.Reader) error {
	return ErrUnsupported
}

// GetFiles is not supported by the current supervisor.
func (p *GitOpsService) GetFiles(context.Context, PodID, string) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}

// Exec is not supported by the current supervisor.
func (p *GitOpsService) Exec(context.Context, PodID, Command) (Result, error) {
	return Result{}, ErrUnsupported
}

var _ Service = (*GitOpsService)(nil)
