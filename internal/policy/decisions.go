package policy

import "fmt"

// Decision is one thing the policy tells the caller to do. The caller carries
// decisions out in the order they are returned.
type Decision interface {
	decision()
}

// ReservationID names a reservation for the claim made against it.
type ReservationID uint64

// Claim tells the caller to claim one task upstream from Tenant's runtimes of
// Kind, against Reservation on Pod, a ready pod any such task can run on. Its
// outcome goes back through ClaimReturned.
type Claim struct {
	Tenant      string
	Kind        Kind
	Reservation ReservationID
	Pod         string
}

// Release says a reservation is no longer held: the claim returned nothing,
// failed, or its task went to another pod. The pod keeps its phase.
type Release struct {
	Reservation ReservationID
	Pod         string
}

// Place puts a claimed task on a pod. Its lease starts when the pod picks it
// up (TaskStarted).
type Place struct {
	Task   string
	Tenant string
	Pod    string
}

// Delete tells the caller to delete Pod. Task is the task whose end caused
// it, empty when none did. The policy has forgotten the pod.
type Delete struct {
	Pod    string
	Reason string
	Task   string
}

// Acquire tells the caller to acquire a ready pod and report it with
// PodAcquired: a claim, or a task waiting for a pod, needs one.
type Acquire struct{}

// Fail tells the caller to fail Task upstream with Reason.
type Fail struct {
	Task   string
	Tenant string
	Reason string
}

// EndLease tells the caller to close the lease of Task.
type EndLease struct {
	Task   string
	Reason string
}

func (Claim) decision()    {}
func (Release) decision()  {}
func (Place) decision()    {}
func (Delete) decision()   {}
func (Fail) decision()     {}
func (EndLease) decision() {}
func (Acquire) decision()  {}

func (Acquire) String() string { return "acquire" }

func (d Claim) String() string {
	return fmt.Sprintf("claim %s/%s res=%d pod=%s", d.Tenant, d.Kind, d.Reservation, d.Pod)
}

func (d Release) String() string {
	return fmt.Sprintf("release res=%d pod=%s", d.Reservation, d.Pod)
}

func (d Place) String() string { return fmt.Sprintf("place %s pod=%s", d.Task, d.Pod) }

func (d Delete) String() string {
	if d.Task != "" {
		return fmt.Sprintf("delete %s after %s (%s)", d.Pod, d.Task, d.Reason)
	}
	return fmt.Sprintf("delete %s (%s)", d.Pod, d.Reason)
}

func (d Fail) String() string { return fmt.Sprintf("fail %s (%s)", d.Task, d.Reason) }

func (d EndLease) String() string { return fmt.Sprintf("end-lease %s (%s)", d.Task, d.Reason) }
