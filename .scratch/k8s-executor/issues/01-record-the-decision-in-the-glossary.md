# 01: Record the decision in the glossary

**What to build:** The domain docs use one vocabulary for the new design before any code changes. ADR 0009 already exists.

- **GLOSSARY.md:**
  - **Pool**: the pods the manager creates in one namespace, up to its budget. It is no longer a fixed set.
  - **Pod**: runs one task at a time.
  - **Pod token**: issued when the pod is created.
  - **Sandbox service**: creates and deletes pods through the Kubernetes API.
  - **Acquire** and **Hand back**: Create now provisions a pod, and Delete deletes it.
  - **Reset**: remove it, or mark it as no longer done.
  - **Binding**, **Capacity** and **Reservation**: remove them, or mark them historical.
  - **Affinity key**: becomes **Key**, the issue or chat session a pod is held for.
  - **Hold**: redefine it as best-effort, ending at the hold timeout or when the budget is full.
  - **Eviction**: deleting a held or idle pod because the budget is full.
  - **Quota** and **Occupancy**: count whole pods (running, held, idle).
- **New terms:** **Pool runtime**, **Clean runtime**, **Warm pod**, **Idle pod**, **Budget**, **Policy** (replaces "lending policy").
- **AGENTS.md:** moved to issue 04, which renames the package; naming `internal/policy` before it exists would mislead.

**Blocked by:** None (can start immediately)

**Status:** resolved

- [x] GLOSSARY.md defines every term above, each with its _Avoid_ list, and no term still describes GitOps pods, slots or resets as current
- [x] AGENTS.md: moved to issue 04
- [x] ADR 0009 is linked from the glossary entries it changes
- [x] `git diff --check` is clean; no code changes

## Comments

Done (docs only):
- GLOSSARY.md marks the terms ADR 0009 changes. It adds Policy, Pod template, Pool runtime, Clean runtime, Key, Warm pod, Idle pod and Budget.
- Reset, Binding, Capacity and Reservation move under "Until ADR 0009 lands".
- "Executor" joins the avoid lists, since the sandbox service is what creates pods.
- AGENTS.md is left for issue 04.
