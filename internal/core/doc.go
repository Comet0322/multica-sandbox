// Package core is the manager's core: it decides which pod runs which task
// and keeps the record of every task it dispatched. It knows tenants, opaque
// tasks and pod IDs, nothing else: tasks come from a TaskSource, pods from a
// Pool, and every placement, claim-selection and lifetime decision from the
// lending policy (package lending). It makes no HTTP call and reads no
// Multica or daemon JSON; the adapters in internal/multica do.
//
// The core talks to each side through the interfaces in ports.go. The Pool
// asks it about the pods it serves (what a pod runs next, whether a task is a
// pod's, whether a pod may act for a tenant) and tells it what happened (a
// task ended, a pod was lost); the TaskSource tells it which tenants exist.
// Everything that does not touch lending passes between the two sides
// directly, without the core.
package core
