# What the manager knows of Multica lives in one package

The manager's dispatcher package held everything apart from the sandbox service: the Multica client, the daemon protocol served to the pods, runtime registration and its upkeep (orphan recovery, visibility, gone runtimes), the wake socket and model list relay, and the lending state they all touched, under one lock. The lending policy knew Multica's task model too: its `Task` carried agent, issue and chat session IDs, its tenants were called workspaces, and the `person-issue` family of affinity rules was built in. A reader could not tell which parts are about Multica and which are about lending pods, and replacing or upgrading Multica's side meant reading all of it.

Instead the program is split along that line:

- `internal/multica` holds everything the manager knows of Multica: `Client` and `Conn` (the server's API as the manager calls it), `DaemonAPI` (the daemon protocol as the manager serves it to the stock daemon in each pod, with the wake socket and model list requests), `Runtimes` (one workspace's registration and its upkeep), the claimed task as the manager reads it, the affinity rules that read what a task is about, and the wire types in `daemonwire`. It imports only the lending policy's types.
- `internal/lending` knows no Multica. A workspace is a tenant to it, a task carries opaque labels, and the only rule it knows by itself is `task`; the others come from the caller as a registry.
- `internal/manager` (formerly `internal/dispatcher`, whose `Pool` is now `Manager`) wires them to the sandbox service and keeps the lending state: held pods, claimed tasks, leases, reconciliation, drains and the admin API. It answers the `DaemonAPI` through a `Backend` interface and never parses Multica's JSON itself.

Import tests enforce the boundaries, as they already did for the sandbox service. The environment variables, the overrides file, the admin API's JSON and every HTTP endpoint the pods call are unchanged.

## Considered options

- **Ports for a task source and a worker gateway, with no Multica types in the manager.** The manager would see only tenants and opaque tasks. It is the cleaner cut, but workspaces, runtimes and their drain run through most of the manager's state, so it would be a rewrite of the lending glue rather than a move; this split is a step towards it.

## Consequences

- A change to Multica's API or daemon protocol is a change in `internal/multica`, plus the manager only when its meaning changes.
- The lending policy's tests use stand-in rules (`internal/lending/rules_test.go`); the Multica rules are tested in `internal/multica`.
- The removed-tenant reset reason is the caller's (`lending.Config.RemovedReason`), so the audit trail still says "workspace removed".
