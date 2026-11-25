# Dispatch from the server's pending queue, reserve then claim

The manager decides which workspace to claim from by merging every workspace's `GET /api/daemon/runtimes/{id}/tasks/pending` list into one queue, ordered as the server orders it: task priority first, then oldest first. It skips any workspace with no quota left. Before it claims, it reserves capacity on a pod, so a claimed task never waits. A claimed task does not start immediately: the server hands a task that has not started within about 90 seconds to the next claimer.

It never reads Multica's database. Database access would let the manager read everything, tie it to one server schema (the company runs v0.6.4), and still could not claim a chosen task. The server's claim only returns the runtime's next claimable task, so the manager accepts any task from the workspace it picked.

## Consequences

- Each dispatch tick sends one pending request per served workspace.
- When no pod can take a task, nothing is claimed, and the task stays queued in Multica, where members can see it.
