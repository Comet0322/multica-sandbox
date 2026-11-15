package lending

import (
	"slices"
	"strings"
	"time"
)

// Task is what the policy knows of a claimed task.
type Task struct {
	ID     string
	Tenant string
	// Initiator is the person the task runs for; empty when unknown. Tasks of
	// different initiators never share a binding.
	Initiator string
	// Labels are what the affinity rules read to find the unit a task may
	// share a pod with (an issue, a chat, an agent). The policy itself never
	// reads them; the caller's rules give them meaning.
	Labels map[string]string
}

// AffinityRule names the rule that decides which tasks share a pod.
type AffinityRule string

// UnitFunc maps a task to the unit it may share a pod with under one affinity
// rule, usually from its Labels. An empty unit means the task shares with
// nothing.
type UnitFunc func(Task) string

// Registry holds the affinity rules by name. Adding a sharing rule means adding
// one entry.
type Registry map[AffinityRule]UnitFunc

// RuleTask gives every task a binding of its own. Every Registry has it.
const RuleTask AffinityRule = "task"

// RuleTenant lets every task of a tenant share its bindings, whoever the
// task runs for, and keeps a binding of it once it opened: its pod stays in
// hold however long no task comes, and it has no maximum lifetime. Only an
// eviction for another tenant's task, the tenant's removal or the pod's loss
// ends it. It is the loosest rule; every Registry has it.
const RuleTenant AffinityRule = "tenant"

func ownBinding(Task) string { return "" }

// tenantUnit stands in the Registry for RuleTenant, whose key affinityKey
// builds itself.
func tenantUnit(Task) string { return "" }

// tenantKeyPrefix starts the keys of RuleTenant.
const tenantKeyPrefix = "tenant/in:"

// keptKey reports whether a binding of the key is kept: never reset for
// running nothing, nor for its age (RuleTenant).
func keptKey(key string) bool { return strings.HasPrefix(key, tenantKeyPrefix) }

// affinityKey is the key tasks must share to share a binding:
// "<rule>:<unit>/by:<initiator>/in:<tenant>". It names the initiator, so
// tasks of different people never share one, and the tenant, so one person's
// tasks in two tenants do not either. A task without a unit or without a
// known initiator gets a key of its own, "task:<id>". RuleTenant is the one
// exception: its key, "tenant/in:<tenant>", names the tenant only.
const uniqueKeyPrefix = "task:"

// uniqueKey reports whether no other task can have the key, so a binding of
// it has nothing to hold its pod for.
func uniqueKey(key string) bool { return strings.HasPrefix(key, uniqueKeyPrefix) }

func affinityKey(name AffinityRule, unit UnitFunc, t Task) string {
	if name == RuleTenant {
		return tenantKeyPrefix + t.Tenant
	}
	u := unit(t)
	if u == "" || t.Initiator == "" {
		return uniqueKeyPrefix + t.ID
	}
	return string(name) + ":" + u + "/by:" + t.Initiator + "/in:" + t.Tenant
}

// DefaultPlacementWait bounds how long a displaced task waits for a pod.
const DefaultPlacementWait = 30 * time.Second

// BindingInfo describes a pod's binding, for the inventory.
type BindingInfo struct {
	ID BindingID
	// Key is the affinity key the binding serves.
	Key string
	// Tenant is the tenant of the task that opened it.
	Tenant string
	// Used is the capacity its tasks occupy; Tasks is how many it holds.
	Used  Resources
	Tasks int
	// Hold is set while no task runs; the pod is reset at HoldUntil, unless
	// the binding is Kept (RuleTenant): then HoldUntil is zero and the hold
	// lasts until an eviction.
	Hold      bool
	HoldUntil time.Time
	Kept      bool
}

// Binding describes the binding of a pod, false when the pod has none.
func (p *Policy) Binding(pod string) (BindingInfo, bool) {
	pd := p.pods[pod]
	if pd == nil || pd.binding == nil {
		return BindingInfo{}, false
	}
	b := pd.binding
	info := BindingInfo{ID: b.id, Key: b.key, Tenant: b.tenant, Used: b.used, Tasks: len(b.tasks), Kept: keptKey(b.key)}
	if b.state == bindingHold {
		info.Hold, info.HoldUntil = true, b.holdUntil
	}
	return info, true
}

// ruleOf is the affinity rule of a tenant: its own, or the pool's.
func (p *Policy) ruleOf(tenant string) AffinityRule {
	if s, ok := p.tenants[tenant]; ok && s.Rule != "" {
		return s.Rule
	}
	return p.cfg.Rule
}

func (p *Policy) newTask(t Task) *taskState {
	name := p.ruleOf(t.Tenant)
	return &taskState{Task: t, key: affinityKey(name, p.cfg.Rules[name], t)}
}

// bindingFor is the most recently used binding of the task's key that has
// room for it, running or in hold; nil when none.
func (p *Policy) bindingFor(ts *taskState) *binding {
	var best *binding
	for _, pd := range p.pods {
		b := pd.binding
		if b == nil || b.key != ts.key || !pd.tenants[ts.Tenant] || p.pastLifetime(b, p.cfg.Now()) {
			continue
		}
		if !p.cfg.Capacity.sub(b.used).fits(TaskDemand) {
			continue
		}
		if best == nil || b.lastUsed.After(best.lastUsed) || (b.lastUsed.Equal(best.lastUsed) && b.id < best.id) {
			best = b
		}
	}
	return best
}

func (p *Policy) join(b *binding, ts *taskState) Decision {
	ts.binding = b
	ts.waitingSince = time.Time{}
	b.tasks[ts.ID] = ts
	b.used = b.used.add(TaskDemand)
	b.lastUsed = p.cfg.Now()
	// A task in hold renews the binding; its countdown restarts when the
	// binding is left without a task again.
	b.state, b.holdUntil = bindingRunning, time.Time{}
	p.tasks[ts.ID] = ts
	return Place{Task: ts.ID, Tenant: ts.Tenant, Pod: b.pod.id, Binding: b.id}
}

func (p *Policy) open(pd *podState, ts *taskState) Decision {
	p.nextBinding++
	now := p.cfg.Now()
	b := &binding{id: p.nextBinding, key: ts.key, tenant: ts.Tenant, pod: pd, tasks: map[string]*taskState{}, openedAt: now, lifetime: p.maxLifetime(ts.Tenant)}
	if keptKey(b.key) {
		b.lifetime = 0
	}
	pd.binding = b
	d := p.join(b, ts).(Place)
	d.NewBinding = true
	return d
}

// placeWaiting places the displaced tasks that fit somewhere now, oldest
// first: in a binding of their key with room, else in a new binding on a
// free held pod. With evict set (the sandbox service has no free pod either),
// a task no hold was ended for yet ends the hold idle longest: that pod is
// handed back, and the task takes the next pod acquired.
func (p *Policy) placeWaiting(evict bool) []Decision {
	if len(p.waiting) == 0 {
		return nil
	}
	slices.SortStableFunc(p.waiting, func(a, b *taskState) int { return a.waitingSince.Compare(b.waitingSince) })
	var ds []Decision
	still := p.waiting[:0]
	for _, ts := range p.waiting {
		if b := p.bindingFor(ts); b != nil {
			ds = append(ds, p.join(b, ts))
			continue
		}
		if pd := p.freePod(ts.Tenant); pd != nil {
			ds = append(ds, p.open(pd, ts))
			continue
		}
		if evict && !ts.evictedFor {
			if pd := p.evictable(ts.Tenant); pd != nil {
				ds = append(ds, p.evict(pd, ts))
			}
		}
		still = append(still, ts)
	}
	p.waiting = still
	return ds
}

// placeOrphan places a task whose reservation was dropped while it was being
// claimed; when no held pod can take it now it waits for the next one.
func (p *Policy) placeOrphan(t *Task) []Decision {
	ts := p.newTask(*t)
	if b := p.bindingFor(ts); b != nil {
		return []Decision{p.join(b, ts)}
	}
	if pd := p.freePod(ts.Tenant); pd != nil {
		return []Decision{p.open(pd, ts)}
	}
	p.wait(ts)
	return nil
}

// wait makes a task wait for a pod, from now.
func (p *Policy) wait(ts *taskState) {
	ts.binding = nil
	ts.waitingSince = p.cfg.Now()
	p.tasks[ts.ID] = ts
	p.waiting = append(p.waiting, ts)
}

// freePod is the first held pod, by ID, that serves the tenant and can
// take a new binding.
func (p *Policy) freePod(tenant string) *podState {
	ids := make([]string, 0, len(p.pods))
	for id, pd := range p.pods {
		if pd.tenants[tenant] && p.reservable(pd) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return p.pods[slices.Min(ids)]
}
