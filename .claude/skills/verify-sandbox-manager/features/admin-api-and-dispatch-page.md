# Admin API and dispatch page

An operator reads the manager's state on its admin listener: the pods by phase, the budget, failures and why pods do not start, each workspace's share and queue, and the leases. The dispatch page shows the same and reloads every 10 seconds. `/readyz` on the pod listener says whether the manager can serve. The fields are listed in docs/DEPLOYMENT.md, "Admin API and dispatch page".

## Sub-features

- `inventory` `GET /v1/inventory`: budget, pod counts and pods, `problem`, `broken`, failures, workspaces.
- `leases` `GET /v1/leases`: active and recently closed leases.
- `dispatch` `GET /v1/dispatch` and the page `GET /dispatch`.
- `probes` `GET /healthz` (always 200) and `GET /readyz` (503 while Multica is unreachable, the dispatch record cannot be written, or the pods are broken) on the pod listener.

## How to get to it (user POV)

- In production: `kubectl port-forward deploy/sandbox-manager 8791`, then `http://127.0.0.1:8791/dispatch`.
- In the kind namespace: a manager started by `deploy/k8s/dev/manager.sh` (admin `127.0.0.1:8793`, pods `:8792`), by the e2e scripts (admin `127.0.0.1:8797`, pods `:8796`) or by `deploy/devpods/run.sh` on simulated pods (admin `127.0.0.1:8791`).

## Driving it with e2e.sh

Preconditions:

- `doctor.sh` is ready; ports 8792 and 8793 free.
- `manager.sh` signs in `pool-bot@dev.test` and keeps its workspace `dev` in `.state`; it creates `WARM` warm pods (default 1) at once.

- **Start a manager, keeping its PID.** From the repo root: `ev=${TMPDIR:-/tmp}/verify-sandbox-manager/$(date -u +%Y%m%dT%H%M%SZ)-admin; mkdir -p "$ev"; deploy/k8s/dev/manager.sh >"$ev/manager.log" 2>&1 & echo $! >"$ev/manager.pid"`. `manager.sh` execs the manager, so that PID is the manager. Wait until `curl -fsS http://127.0.0.1:8793/v1/inventory` answers.
- **Read it as an operator.** `curl -fsS http://127.0.0.1:8793/v1/inventory | tee "$ev/inventory.json" | jq -c '{max_pods, warm, broken, problem, pods: [.pods[] | {name, phase}]}'` shows `max_pods` 4, warm pods and `broken` null. `curl -fsS http://127.0.0.1:8793/v1/dispatch >"$ev/dispatch.json"`, `curl -fsS http://127.0.0.1:8793/v1/leases >"$ev/leases.json"`, and `curl -fsS http://127.0.0.1:8793/dispatch -o "$ev/dispatch.html"` (title `Pod Dispatch`).
- **Probes.** `curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8792/readyz` prints 200 once the manager has listed its workspaces; `/healthz` prints 200.
- **Broken state, end to end.** `../scripts/e2e.sh scenarios` checks `/v1/inventory` shows `broken` with a permanent `image` failure and `/readyz` returns 503 (see [Pod failures](./pod-failures.md)).
- **Stop and clean up.** `kill "$(cat "$ev/manager.pid")"`, wait until `kill -0` fails, then `KUBECONFIG=deploy/k8s/dev/.state/dev-user.kubeconfig kubectl delete pods -l app.kubernetes.io/managed-by=sandbox-manager --wait=true`. `doctor.sh` then shows no managed pods and ports 8792 and 8793 free.

## Gotchas

- Stopping the manager keeps its pods: a restarted manager takes them over from the dispatch record. Delete them after a manual run, or the e2e scripts refuse to start (`namespace multica-sandbox already has managed pods`).
- The next `manager.sh` start finds the pods you deleted lost (docs/DEPLOYMENT.md, "Known limits"); warm pods carry no task, so nothing fails.
- A page screenshot needs a browser; the JSON behind it (`/v1/dispatch`) is the evidence the page renders.
