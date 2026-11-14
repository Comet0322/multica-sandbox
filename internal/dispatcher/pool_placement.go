package dispatcher

import (
	"fmt"
	"strings"
	"time"

	"sandbox-manager/internal/lending"
)

// DefaultHoldTimeout is the hold timeout the command line applies when none is
// configured.
const DefaultHoldTimeout = 15 * time.Minute

// DefaultCapacity is the pod capacity in slots when none is configured:
// the daemon's own concurrency limit.
const DefaultCapacity = 20

// DefaultPlacementWait stays well under the 90 seconds after which the server
// hands a claimed but unstarted task to the next claimer.
const DefaultPlacementWait = 30 * time.Second

// placements are the affinity rules a pool or workspace may use.
var placements = []lending.AffinityRule{lending.RuleTask, lending.RulePersonIssue, lending.RulePersonAgent, lending.RulePerson}

// CheckPlacement refuses a placement that names no affinity rule. Empty is the
// default.
func CheckPlacement(p lending.AffinityRule) error {
	if p == "" {
		return nil
	}
	for _, known := range placements {
		if p == known {
			return nil
		}
	}
	names := make([]string, len(placements))
	for i, known := range placements {
		names[i] = string(known)
	}
	return fmt.Errorf("unknown affinity rule %q (want one of %s)", p, strings.Join(names, ", "))
}

func (c *PoolConfig) normalizePlacement() error {
	if err := CheckPlacement(c.Placement); err != nil {
		return err
	}
	if c.Placement == "" {
		c.Placement = lending.RulePersonIssue
	}
	if c.Capacity < 0 {
		return fmt.Errorf("pod capacity must not be negative")
	}
	if c.Capacity == 0 {
		c.Capacity = DefaultCapacity
	}
	if c.PlacementWait < 0 {
		return fmt.Errorf("placement wait must not be negative")
	}
	if c.PlacementWait == 0 {
		c.PlacementWait = DefaultPlacementWait
	}
	return c.normalizeLifetime()
}

// parkedTask is a task the manager claimed upstream and its pod has not picked
// up yet. It sits in its pod's queue once the policy placed it.
type parkedTask struct {
	raw       []byte
	ws        *workspace
	ref       claimedTaskRef
	claimedAt time.Time
}

// firstLease is the pod's longest-held lease, nil when it holds none.
func firstLease(pd *pod) *lease {
	var first *lease
	for _, l := range pd.leases {
		if first == nil || l.StartedAt.Before(first.StartedAt) {
			first = l
		}
	}
	return first
}

// claimedTaskRef is the part of a claimed task the pool reads: which task it
// is, where it comes from and who started it.
type claimedTaskRef struct {
	ID             string `json:"id"`
	AgentID        string `json:"agent_id"`
	RuntimeID      string `json:"runtime_id"`
	WorkspaceID    string `json:"workspace_id"`
	IssueID        string `json:"issue_id"`
	ChatSessionID  string `json:"chat_session_id"`
	AutopilotRunID string `json:"autopilot_run_id"`
	InitiatorEmail string `json:"initiator_email"`
	Attribution    struct {
		Originator *struct {
			ID string `json:"id"`
		} `json:"originator"`
	} `json:"attribution"`
}

// person is the user ID of the human who authorized the task
// (attribution.originator.id), the initiator in affinity keys. Empty when no
// human did, as for autopilot runs.
func (r claimedTaskRef) person() string {
	if o := r.Attribution.Originator; o != nil {
		return strings.ToLower(strings.TrimSpace(o.ID))
	}
	return ""
}
