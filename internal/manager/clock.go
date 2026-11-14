package manager

import (
	"sync"
	"time"
)

// clock is the manager's time source, shared by every module so a test can
// replace it in one place.
type clock struct {
	mu sync.Mutex
	fn func() time.Time
}

func newClock() *clock { return &clock{fn: time.Now} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	fn := c.fn
	c.mu.Unlock()
	return fn()
}

func (c *clock) set(fn func() time.Time) {
	c.mu.Lock()
	c.fn = fn
	c.mu.Unlock()
}
