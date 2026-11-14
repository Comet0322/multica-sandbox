package manager

import (
	"fmt"
	"time"
)

// Defaults for a binding's maximum lifetime and drain grace, which the lending
// policy applies (see lending.Config).
const (
	DefaultMaxLifetime = 2 * time.Hour
	DefaultDrainGrace  = 10 * time.Minute
)

func (c *Config) normalizeLifetime() error {
	if c.HoldTimeout < 0 || c.MaxLifetime < 0 || c.DrainGrace < 0 {
		return fmt.Errorf("hold timeout, pod maximum lifetime and drain grace must not be negative")
	}
	if c.MaxLifetime == 0 {
		c.MaxLifetime = DefaultMaxLifetime
	}
	if c.DrainGrace == 0 {
		c.DrainGrace = DefaultDrainGrace
	}
	return nil
}
