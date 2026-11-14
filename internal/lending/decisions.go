package lending

import "fmt"

// Decision is one thing the policy tells the manager to do. The manager
// carries decisions out in the order they are returned.
type Decision interface {
	decision()
}

// ReservationID names a reservation for the claim made against it.
type ReservationID uint64

// BindingID names one binding: a pod's lending to one affinity key, from its
// first lease to its reset.
type BindingID uint64

// Claim tells the manager to claim one task upstream from Workspace, against
// Reservation on Pod. Its outcome goes back through ClaimReturned.
type Claim struct {
	Workspace   string
	Reservation ReservationID
	Pod         string
}

// Release says a reservation is no longer held: the claim returned nothing,
// failed, or its task was placed in an existing binding elsewhere.
type Release struct {
	Reservation ReservationID
	Pod         string
}

// Place puts a claimed task on a pod, in Binding. NewBinding is set when the
// task opens the binding. The task occupies its demand on the pod from now on;
// its lease starts when the pod picks it up (TaskStarted).
type Place struct {
	Task       string
	Workspace  string
	Pod        string
	Binding    BindingID
	NewBinding bool
}

// Reset tells the manager to hand Pod back to the sandbox service, which
// resets it. Task is the task whose end caused the reset, empty when none
// did. The policy has forgotten the pod.
type Reset struct {
	Pod    string
	Reason string
	Task   string
}

// Acquire tells the manager to acquire a pod from the sandbox service and
// report it with PodAcquired: a claim, or a task waiting for a pod, needs one
// no held pod can give.
type Acquire struct{}

// Fail tells the manager to fail Task upstream with Reason.
type Fail struct {
	Task      string
	Workspace string
	Reason    string
}

// EndLease tells the manager to close the lease of Task.
type EndLease struct {
	Task   string
	Reason string
}

func (Claim) decision()    {}
func (Release) decision()  {}
func (Place) decision()    {}
func (Reset) decision()    {}
func (Fail) decision()     {}
func (EndLease) decision() {}
func (Acquire) decision()  {}

func (Acquire) String() string { return "acquire" }

func (d Claim) String() string {
	return fmt.Sprintf("claim %s res=%d pod=%s", d.Workspace, d.Reservation, d.Pod)
}

func (d Release) String() string {
	return fmt.Sprintf("release res=%d pod=%s", d.Reservation, d.Pod)
}

func (d Place) String() string {
	s := fmt.Sprintf("place %s pod=%s binding=%d", d.Task, d.Pod, d.Binding)
	if d.NewBinding {
		s += " new"
	}
	return s
}

func (d Reset) String() string {
	if d.Task != "" {
		return fmt.Sprintf("reset %s after %s (%s)", d.Pod, d.Task, d.Reason)
	}
	return fmt.Sprintf("reset %s (%s)", d.Pod, d.Reason)
}

func (d Fail) String() string { return fmt.Sprintf("fail %s (%s)", d.Task, d.Reason) }

func (d EndLease) String() string { return fmt.Sprintf("end-lease %s (%s)", d.Task, d.Reason) }
