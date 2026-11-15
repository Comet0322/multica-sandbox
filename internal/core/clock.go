package core

import (
	"sync"
	"time"
)

// Clock is the manager's time source, shared by every module so a test can
// replace it in one place.
type Clock struct {
	mu sync.Mutex
	fn func() time.Time
}

// NewClock returns a clock on the wall time.
func NewClock() *Clock { return &Clock{fn: time.Now} }

// Now is the current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	fn := c.fn
	c.mu.Unlock()
	return fn()
}

// Set replaces the time source.
func (c *Clock) Set(fn func() time.Time) {
	c.mu.Lock()
	c.fn = fn
	c.mu.Unlock()
}
