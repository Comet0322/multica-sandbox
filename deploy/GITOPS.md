# Deploying the sandbox with Azure DevOps and GitOps

A short guide to running the sandbox manager and the pod pool
through an Azure DevOps pipeline and a GitOps controller. It assumes Kubernetes
(for example AKS), a container registry (for example ACR) and Argo CD or Flux
in the cluster. Replace those with whatever the company uses; the flow is the
same.

## What gets deployed

| Piece | Image | Built from | Manifests |
|---|---|---|---|
| Sandbox manager | `multica-sandbox-manager` | `sandbox-manager/deploy/Dockerfile.manager` | `sandbox-manager/deploy/pool/k8s/manager.yaml` |
| Sandbox pods | `multica-sandbox-pool` | `sandbox-manager/deploy/Dockerfile.pool` | `sandbox-manager/deploy/pool/k8s/sandbox/pool.yaml` |

`namespace.yaml` and `networkpolicy.yaml` sit next to the manager's manifests.
Build both images from the same commit.

## Two repositories

- **App repository** (this one): code, Dockerfiles, base manifests. The CI
  pipeline lives here.
- **GitOps repository**: one overlay per environment (dev, test, prod) that
  points at the base manifests and holds the image tags, the pool size, hosts
  and network ranges. The GitOps controller watches this repository only. Nobody
  runs `kubectl apply` by hand.

Overlay (in the GitOps repository), one per environment:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../base/pool/k8s            # the manager, namespace, policies
  - ../../base/pool/k8s/sandbox    # the pods
images:
  - name: multica-sandbox-manager
    newName: <registry>/multica/sandbox-manager
    newTag: <git sha>
  - name: multica-sandbox-pool
    newName: <registry>/multica/sandbox-pool
    newTag: <git sha>
replicas:
  - name: sandbox-pool
    count: 4
patches: []   # every PLACEHOLDER in the manifests: Multica URL, CIDRs
```

Every `PLACEHOLDER` in the base manifests is replaced here (use `patches`). The
base stays free of company values. The image names in `images:` must match the
names in the base manifests (`multica-sandbox-manager` and so on), not the short
names used in the Deployments.

Pool defaults (affinity rule, hold timeout, maximum lifetime, quota) are the
manager's environment variables, patched here like any other value.
Per-workspace overrides, keyed by workspace slug, go into the
`sandbox-manager-overrides` ConfigMap (patch its `overrides.yaml`); the manager
reads the file again on every sync, so a change applies without a restart (see
`POOL.md`, per-workspace overrides).

## Secrets

No secret goes into either repository. Create this one in the cluster by way of
Key Vault (CSI driver or External Secrets) or Sealed Secrets:

| Secret | Keys | Content |
|---|---|---|
| `sandbox-manager-config` | `pool-token` | the bot account's Multica PAT |

The GitOps controller applies only the reference to it (the Secret name). The
manager handles no git or LLM credentials: agents bring them in their custom
environment in Multica.

## The pipeline (CI)

One pipeline in the app repository, triggered on merge to the main branch:

1. Build the two images with the commit SHA as the tag and push them to the
   registry.
2. Open a pull request in the GitOps repository that changes the two `newTag`
   values in the environment's overlay (`kustomize edit set image ...`). Dev can
   merge automatically; test and prod need approval, which is the release gate.
3. The GitOps controller sees the merge and syncs the cluster.

```yaml
# azure-pipelines.yml (outline)
trigger: { branches: { include: [main] } }
pool: { name: <build agents> }
variables:
  tag: $(Build.SourceVersion)
steps:
  - script: |
      docker build -f sandbox-manager/deploy/Dockerfile.manager -t $(registry)/multica/sandbox-manager:$(tag) .
      docker build -f sandbox-manager/deploy/Dockerfile.pool \
        --build-arg VERSION=$(tag) --build-arg COMMIT=$(tag) \
        --build-arg MANAGER_URL=http://sandbox-manager.multica-sandbox.svc.cluster.local:8080 \
        --build-arg PACKAGE_MIRROR_URL=$(mirrorUrl) \
        -t $(registry)/multica/sandbox-pool:$(tag) .
    displayName: Build
  - script: docker push ... # both, or use the registry task
    displayName: Push
  - script: |
      git clone <gitops repo> && cd <gitops repo>/overlays/dev
      kustomize edit set image multica-sandbox-manager=$(registry)/multica/sandbox-manager:$(tag) \
        multica-sandbox-pool=$(registry)/multica/sandbox-pool:$(tag)
      git commit -am "sandbox $(tag)" && git push origin HEAD:refs/heads/bump-$(tag)  # then open a PR
    displayName: Propose the new tags
```

The pod image bakes in some addresses at build time (`MANAGER_URL`,
`PACKAGE_MIRROR_URL`). If an environment uses
different addresses, build the pod image per environment, or keep the same
in-cluster names everywhere (the defaults).

## Rolling out

- The GitOps controller applies the namespace, the policies and the manager,
  then the pods. Pods that start before the manager are fine: they keep
  retrying until it answers.
- Every pod passes through one reset after the manager starts; this is normal.
- Replacing pods ends the tasks they run; the manager reports those tasks failed to
  Multica. Roll out when the pool is quiet, or accept the retries.

## When the image gains or loses an agent CLI

Change `SANDBOX_PROVIDERS` in the manager's environment (overlay patch) in the
same pull request, for example `claude,opencode`. The manager only creates
runtimes for the CLIs in that list.

## Checking a release

After the sync, the checklist in `pool/k8s/README.md` applies. The short version:

```bash
kubectl -n multica-sandbox get deploy,pod
kubectl -n multica-sandbox port-forward deploy/sandbox-manager 8791:8791
curl -s localhost:8791/v1/inventory     # idle + leased + resetting + lost = pool size
```

Then run one task from the Multica UI on the sandbox runtime.

## Rollback

Revert the bump commit in the GitOps repository. The controller returns the
cluster to the previous tags. Nothing is stored outside the manager's memory
(inventory, leases), so a rollback loses no data; tasks that were running on
pods being replaced are reported failed to Multica, which can retry them.

## Before the first deployment

1. The bot account exists in Multica and is a member of the workspaces to serve;
   its PAT is in Key Vault.
2. The agents that run on the sandbox runtime carry their LLM endpoint and key,
   and their git token, in their custom environment in Multica.
3. The cluster's CNI enforces NetworkPolicy (pod registration on the manager is
   open, so only the policies keep other callers away from it). A mesh that
   rewrites source addresses is fine: pods are identified by their pod token.
4. The namespace allows Pod Security `restricted`, and the pod's uid 1001
   (group 2000) is allowed by the cluster's policy. Set a kubelet
   `podPidsLimit` so a fork bomb in a task cannot fill the node.
5. Egress from the pods to the package mirror, the LLM endpoint and the git
   host is open in `networkpolicy.yaml` (replace the CIDR placeholders in the
   overlay).
