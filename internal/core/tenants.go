package core

import (
	"context"
	"slices"
	"time"
)

// tenant is the core's state for one tenant the source serves or served.
type tenant struct {
	id       string
	settings Tenant
	// removed is set when the source dropped the tenant; it is not claimed
	// from while set. A tenant that comes back before its drain ended is
	// served again with this same record, and drainGen tells the drain that
	// was started for an earlier removal to stop.
	removed  bool
	drainGen int
}

// TenantChanges lists what SetTenants changed.
type TenantChanges struct {
	Added, Removed, Updated []string
}

// SetTenants replaces the served tenants with list. A new tenant is claimed
// from at once. A removed one is no longer claimed from; its leased tasks run
// on, and once they ended, or DrainTimeout passed and their leases were
// ended, the source is told to close it (Close). The drain runs until ctx
// ends. A tenant that comes back before its drain ended carries on as it was.
func (c *Core) SetTenants(ctx context.Context, list []Tenant) TenantChanges {
	var res TenantChanges
	listed := map[string]bool{}
	c.mu.Lock()
	for _, t := range list {
		listed[t.ID] = true
		rec := c.tenants[t.ID]
		switch {
		case rec == nil:
			rec = &tenant{id: t.ID, settings: t}
			c.tenants[t.ID] = rec
			res.Added = append(res.Added, t.ID)
		case rec.removed:
			rec.removed = false
			rec.drainGen++
			rec.settings = t
			res.Added = append(res.Added, t.ID)
		default:
			if rec.settings.Settings != t.Settings {
				res.Updated = append(res.Updated, t.ID)
			}
			rec.settings = t
		}
		if err := c.policy.SetTenant(t.ID, t.Settings); err != nil {
			// The source refuses such settings before they get here.
			c.log.Error("lending policy refused workspace settings", "workspace_id", t.ID, "error", err)
		}
	}
	var dropped []*tenant
	for id, rec := range c.tenants {
		if listed[id] || rec.removed {
			continue
		}
		rec.removed = true
		rec.drainGen++
		dropped = append(dropped, rec)
		res.Removed = append(res.Removed, id)
		c.applyLocked(c.policy.RemoveTenant(id))
	}
	gens := make([]int, len(dropped))
	for i, rec := range dropped {
		gens[i] = rec.drainGen
	}
	c.mu.Unlock()
	c.settle(ctx)
	for i, rec := range dropped {
		go c.drainFrom(ctx, rec, gens[i])
	}
	slices.Sort(res.Added)
	slices.Sort(res.Removed)
	slices.Sort(res.Updated)
	return res
}

// TenantBusy reports whether a tenant holds anything here: a lease, a
// claimed task not yet picked up, or a claim in flight.
func (c *Core) TenantBusy(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tenantBusyLocked(id)
}

func (c *Core) tenantBusyLocked(id string) bool {
	return c.leasedLocked(id) > 0 || c.policy.Busy(id)
}

// drainFrom waits for the work of a tenant removed as removal number gen to
// end, then tells the source to close it. It stops without touching anything
// once the tenant is served again or removed anew (gen moved on).
func (c *Core) drainFrom(ctx context.Context, rec *tenant, gen int) {
	timeout := time.NewTimer(c.cfg.DrainTimeout)
	defer timeout.Stop()
	tick := time.NewTicker(c.cfg.DrainPoll)
	defer tick.Stop()
	for {
		c.mu.Lock()
		current := rec.removed && rec.drainGen == gen
		done := current && !c.tenantBusyLocked(rec.id)
		c.mu.Unlock()
		if !current {
			c.log.Info("drain stopped: the workspace is served again", "workspace_id", rec.id)
			return
		}
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-timeout.C:
			c.endDrainingLeases(ctx, rec)
		}
	}
	c.mu.Lock()
	if !rec.removed || rec.drainGen != gen {
		c.mu.Unlock()
		return
	}
	delete(c.tenants, rec.id)
	c.mu.Unlock()
	c.source.Close(rec.id)
	c.log.Info("removed workspace drained and disconnected", "workspace_id", rec.id)
}

// endDrainingLeases ends what is left of a removed tenant's leases when the
// drain timeout passed, and fails those tasks upstream. Claimed tasks still
// waiting for a pod lapse through the placement wait. A tenant that is served
// again keeps its leases.
func (c *Core) endDrainingLeases(ctx context.Context, rec *tenant) {
	var ended []*lease
	c.mu.Lock()
	if !rec.removed {
		c.mu.Unlock()
		return
	}
	for _, l := range c.leases {
		if l.Tenant == rec.id {
			ended = append(ended, l)
		}
	}
	for _, l := range ended {
		c.endTaskLocked(l.TaskID, "workspace removed from the configuration")
	}
	c.mu.Unlock()
	c.settle(ctx)
	for _, l := range ended {
		c.log.Warn("lease ended: workspace was removed and the drain timeout passed", "task_id", l.TaskID, "workspace_id", rec.id)
		c.fail(ctx, rec.id, l.TaskID, "the workspace was removed from the sandbox pool while this task was running")
	}
}
