package dispatcher

import (
	"context"
	"time"
)

// DefaultDispatchInterval is the dispatch tick when WakeInterval is zero.
const DefaultDispatchInterval = time.Second

// Run runs the sandbox service's liveness checks and the dispatch tick until
// ctx ends; with ReconcileInterval set it also starts the lease
// reconciliation, which runs until ctx ends too.
func (p *Pool) Run(ctx context.Context) {
	if p.sandboxRun != nil {
		go p.sandboxRun(ctx)
	}
	if p.cfg.ReconcileInterval > 0 {
		go p.reconcileLoop(ctx)
	}
	every := p.cfg.WakeInterval
	if every <= 0 {
		every = DefaultDispatchInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.dispatchOnce(ctx)
		}
	}
}

// dispatchOnce is the dispatch tick, the only place tasks are claimed. It
// reads the pending lists, forgets held pods the sandbox service no longer
// lends, wakes pods that hold a placed task, and claims at most one task
// upstream against a reservation on a held pod, acquiring one when the policy
// asks; the pod the task is placed on is woken to pick it up.
func (p *Pool) dispatchOnce(ctx context.Context) {
	p.readPending(ctx)
	p.mu.Lock()
	p.healHeldLocked()
	p.tickLocked()
	var holding []string
	for _, pd := range p.held {
		if len(pd.queue) > 0 {
			holding = append(holding, pd.ID)
		}
	}
	p.mu.Unlock()
	p.settle(ctx)
	for _, podID := range holding {
		p.api.wakePod(podID)
	}
	p.claimRound(ctx)
}
