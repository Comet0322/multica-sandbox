package dispatcher

import (
	"fmt"
	"time"
)

// Defaults for a binding's maximum lifetime and drain grace, which the lending
// policy applies (see lending.Config).
const (
	DefaultPodMaxLifetime = 2 * time.Hour
	DefaultPodDrainGrace  = 10 * time.Minute
)

func (c *PoolConfig) normalizeLifetime() error {
	if c.HoldTimeout < 0 || c.PodMaxLifetime < 0 || c.PodDrainGrace < 0 {
		return fmt.Errorf("hold timeout, pod maximum lifetime and drain grace must not be negative")
	}
	if c.PodMaxLifetime == 0 {
		c.PodMaxLifetime = DefaultPodMaxLifetime
	}
	if c.PodDrainGrace == 0 {
		c.PodDrainGrace = DefaultPodDrainGrace
	}
	return nil
}
