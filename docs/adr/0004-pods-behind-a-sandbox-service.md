# Pods sit behind a sandbox service with a create / delete / files / exec interface

The manager used to run the pods itself. The same structs held the pod registry, pod tokens, the control channel, resets and liveness alongside bindings and leases, and the lending policy tracked whether a pod was resetting or lost. Replacing the execution plane would have meant rewriting the dispatcher.

Instead, a sandbox service module owns everything about pods: registration and pod tokens, the control channel, resets, liveness, pruning and the set of free pods. The dispatcher reaches pods only through `Create`, `Delete`, `PutFiles`, `GetFiles` and `Exec`. The service imports nothing from the dispatcher, the lending policy or Multica.

For the GitOps-started pods we run today, `Create` acquires a free pod that has been reset, and `Delete` hands it back to be reset; neither provisions anything. The names are kept so a later execution plane that does provision can implement the same interface. The current supervisor cannot transfer files or run commands, so `PutFiles`, `GetFiles` and `Exec` return "not supported" until a plane that can arrives.

While the service runs in the manager's process, two pieces of plumbing sit beside the interface: `Authenticate`, which turns a pod token into a pod ID for the daemon protocol on the shared listener, and a hook that tells the dispatcher when a pod it holds is interrupted or lost. When the service becomes its own process, they become the service forwarding daemon traffic with the pod's ID in a header, and an event stream.

The pods, the image and every HTTP endpoint the pods call stay as they were. This refines [ADR 0003](0003-pod-identity-by-manager-issued-token.md), which still holds: the service, not the dispatcher, now issues and checks the pod token. Reserve-then-claim ([ADR 0001](0001-dispatch-from-pending-queue-reserve-then-claim.md)) is unchanged: a successful `Create`, or a pod the dispatcher already holds, is the reservation.

## Considered options

- **Keep pod handling inside the dispatcher.** No new seam, but the execution plane stays welded to the lending code.
- **Adopt a real provisioning API now** (create and destroy a pod per binding). It would also fix leftover processes after a reset, but it changes the pods, the image and the deployment at once; the operator chose to keep the pods as they are first.

## Consequences

- Liveness is judged from control-channel traffic only; daemon calls no longer count.
- The dispatcher keeps pods it acquired but never ran a task on, so an empty claim does not cause a reset.
- An evicted-for task no longer waits on the same pod: the pod is handed back and the task waits for the next acquired pod.
- A polling pod's daemon no longer triggers an upstream claim; only the dispatch tick claims.
