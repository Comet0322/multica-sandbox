package lending

// Resources is an amount of each resource: what a pod offers (its capacity)
// or what a task needs (its demand). Only Slots is measured today; CPU and
// memory are added here once a usage source can fill them.
type Resources struct {
	// Slots is a number of tasks running at once.
	Slots int
}

// TaskDemand is what every task needs.
var TaskDemand = Resources{Slots: 1}

func (r Resources) add(o Resources) Resources { return Resources{Slots: r.Slots + o.Slots} }

func (r Resources) sub(o Resources) Resources { return Resources{Slots: r.Slots - o.Slots} }

// fits reports whether demand fits in r.
func (r Resources) fits(demand Resources) bool { return demand.Slots <= r.Slots }
