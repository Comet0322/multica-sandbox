package lending

import (
	"slices"
	"strings"
	"time"
)

// Task is what the policy knows of a claimed task.
type Task struct {
	ID        string
	Workspace string
	// Initiator is the person the task runs for; empty when unknown.
	Initiator     string
	AgentID       string
	IssueID       string
	ChatSessionID string
}

// AffinityRule names the rule that decides which tasks share a pod.
type AffinityRule string

// UnitFunc maps a task to the unit it may share a pod with under one affinity
// rule: an issue, a chat session, an agent. An empty unit means the task shares
// with nothing.
type UnitFunc func(Task) string

// Registry holds the affinity rules by name. Adding a sharing rule means adding
// one entry.
type Registry map[AffinityRule]UnitFunc

// The built-in affinity rules.
const (
	// RuleTask gives every task a binding of its own.
	RuleTask AffinityRule = "task"
	// RulePersonIssue shares a binding between one person's tasks on one
	// issue, or in one chat session for a task without an issue. It is the
	// default.
	RulePersonIssue AffinityRule = "person-issue"
	// RulePersonAgent shares a binding between one person's tasks of one agent.
	RulePersonAgent AffinityRule = "person-agent"
	// RulePerson shares a binding between all of one person's tasks.
	RulePerson AffinityRule = "person"
)

// Builtin returns the built-in affinity rules.
func Builtin() Registry {
	return Registry{
		RuleTask: func(Task) string { return "" },
		RulePersonIssue: func(t Task) string {
			switch {
			case t.IssueID != "":
				return "issue:" + t.IssueID
			case t.ChatSessionID != "":
				return "chat:" + t.ChatSessionID
			}
			return ""
		},
		RulePersonAgent: func(t Task) string { return t.AgentID },
		RulePerson:      func(Task) string { return "all" },
	}
}

// affinityKey is the key tasks must share to share a binding:
// "<rule>:<unit>/by:<initiator>/in:<workspace>". It always names the
// initiator, so tasks of different people never share one, and the workspace,
// so one person's tasks in two workspaces do not either. A task without a unit
// or without a known initiator gets a key of its own, "task:<id>".
const uniqueKeyPrefix = "task:"

// uniqueKey reports whether no other task can have the key, so a binding of
// it has nothing to hold its pod for.
func uniqueKey(key string) bool { return strings.HasPrefix(key, uniqueKeyPrefix) }

func affinityKey(name AffinityRule, unit UnitFunc, t Task) string {
	u := unit(t)
	if u == "" || t.Initiator == "" {
		return uniqueKeyPrefix + t.ID
	}
	return string(name) + ":" + u + "/by:" + t.Initiator + "/in:" + t.Workspace
}

// DefaultPlacementWait bounds how long a displaced task waits for a pod.
const DefaultPlacementWait = 30 * time.Second

// BindingInfo describes a pod's binding, for the inventory.
type BindingInfo struct {
	ID BindingID
	// Key is the affinity key the binding serves.
	Key string
	// Workspace is the workspace of the task that opened it.
	Workspace string
	// Used is the capacity its tasks occupy; Tasks is how many it holds.
	Used  Resources
	Tasks int
	// Hold is set while no task runs; the pod is reset at HoldUntil.
	Hold      bool
	HoldUntil time.Time
}

// Binding describes the binding of a pod, false when the pod has none.
func (p *Policy) Binding(pod string) (BindingInfo, bool) {
	pd := p.pods[pod]
	if pd == nil || pd.binding == nil {
		return BindingInfo{}, false
	}
	b := pd.binding
	info := BindingInfo{ID: b.id, Key: b.key, Workspace: b.workspace, Used: b.used, Tasks: len(b.tasks)}
	if b.state == bindingHold {
		info.Hold, info.HoldUntil = true, b.holdUntil
	}
	return info, true
}

func (p *Policy) newTask(t Task) *taskState {
	name := p.cfg.Rule
	if s, ok := p.workspaces[t.Workspace]; ok && s.Rule != "" {
		name = s.Rule
	}
	return &taskState{Task: t, key: affinityKey(name, p.cfg.Rules[name], t)}
}

// bindingFor is the most recently used binding of the task's key that has
// room for it, running or in hold; nil when none.
func (p *Policy) bindingFor(ts *taskState) *binding {
	var best *binding
	for _, pd := range p.pods {
		b := pd.binding
		if b == nil || b.key != ts.key || !pd.workspaces[ts.Workspace] || p.pastLifetime(b, p.cfg.Now()) {
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
	return Place{Task: ts.ID, Workspace: ts.Workspace, Pod: b.pod.id, Binding: b.id}
}

func (p *Policy) open(pd *podState, ts *taskState) Decision {
	p.nextBinding++
	now := p.cfg.Now()
	b := &binding{id: p.nextBinding, key: ts.key, workspace: ts.Workspace, pod: pd, tasks: map[string]*taskState{}, openedAt: now, lifetime: p.maxLifetime(ts.Workspace)}
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
		if pd := p.freePod(ts.Workspace); pd != nil {
			ds = append(ds, p.open(pd, ts))
			continue
		}
		if evict && !ts.evictedFor {
			if pd := p.evictable(ts.Workspace); pd != nil {
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
	if pd := p.freePod(ts.Workspace); pd != nil {
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

// freePod is the first held pod, by ID, that serves the workspace and can
// take a new binding.
func (p *Policy) freePod(workspace string) *podState {
	ids := make([]string, 0, len(p.pods))
	for id, pd := range p.pods {
		if pd.workspaces[workspace] && p.reservable(pd) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return p.pods[slices.Min(ids)]
}
