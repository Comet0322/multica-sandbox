package manager

import (
	"fmt"
	"sandbox-manager/internal/multica"
	"time"
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

func (c *Config) normalizeLending() error {
	if err := multica.CheckAffinityRule(c.AffinityRule); err != nil {
		return err
	}
	if c.AffinityRule == "" {
		c.AffinityRule = multica.DefaultAffinityRule
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

// claimedTask is a task the manager claimed upstream and its pod has not picked
// up yet. It sits in its pod's queue once the policy placed it.
type claimedTask struct {
	task      multica.ClaimedTask
	ws        *workspace
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
