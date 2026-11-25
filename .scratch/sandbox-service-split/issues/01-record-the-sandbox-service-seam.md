# 01: Record the sandbox service seam

**What to build:** The domain docs name the new module and record why the seam exists, so the code tickets use one vocabulary.

- **ADR 0004** (`docs/adr/`): pods sit behind a sandbox service that the dispatcher reaches only through Create, Delete, PutFiles, GetFiles and Exec. Record the following:
  - For GitOps-started pods, Create means "take a free, clean pod" and Delete means "hand it back to be reset". The names are kept for future execution planes that really provision.
  - In the same process, Authenticate and the interrupted/lost notification hook are plumbing beside the interface. They turn into header forwarding and an event stream when the service becomes its own process.
  - The pods and their HTTP endpoints do not change.
  - This refines ADR 0003, which stays in force: the pod token is now issued and checked by the service.
- **GLOSSARY.md:**
  - Add **Sandbox service**: the module that owns pods' registration, tokens, control channel, reset, liveness and free set.
  - Add **Acquire** (a successful Create) and **Hand back** (Delete), with their _Avoid_ lists.
  - Keep "Sandbox" banned as a word for a pod.
  - Update **Pool** and **Manager** so that the manager no longer registers or resets pods itself.

**Blocked by:** None (can start immediately)

**Status:** resolved

- [x] ADR 0004 exists, states the decision, its alternatives (pod logic inside the dispatcher; a real provisioning API now) and consequences, and links ADR 0001 and 0003
- [x] GLOSSARY.md defines Sandbox service, Acquire and Hand back, and Pool and Manager no longer say the manager registers or resets pods
- [x] No other glossary term changes meaning
- [x] `git diff --check` is clean; no code changes

## Comments

**Done by the coordinator** (docs only, no code changed): `docs/adr/0004-pods-behind-a-sandbox-service.md` added; GLOSSARY.md gains Sandbox service, Acquire and Hand back, and Pool, Manager and Pod token now name the sandbox service as the owner of registration, tokens and resets. `git diff --check` clean; no code tests run (none apply).
