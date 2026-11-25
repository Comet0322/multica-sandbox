# 03: Docs, tracker and agent instructions

**What to build:** Our domain docs, the tracker and the agent instructions move into `sandbox-manager/`. The repo root keeps only pointers, and ADR 0005 records the decision. See the spec's Docs and tracker decisions.

- **Move** with `git mv`:
  - `GLOSSARY.md` → `sandbox-manager/GLOSSARY.md`
  - `docs/adr/*` → `sandbox-manager/docs/adr/`
  - `docs/agents/*` → `sandbox-manager/docs/agents/`
  - `.scratch/README.md` and every `.scratch/<feature>/` → `sandbox-manager/.scratch/`, including this spec
- **Agent instructions.**
  - The current root `AGENTS.md` becomes `sandbox-manager/AGENTS.md`, with paths relative to `sandbox-manager/`. It gains a short section on running the module's checks: `go test -race ./...` in `sandbox-manager/` and the scenario drivers in `deploy/pool/compose/smoke/`.
  - `sandbox-manager/CLAUDE.md` contains `@AGENTS.md`.
  - The root `AGENTS.md` is a few lines: `sandbox-manager/` is our sandbox manager, kept apart from Multica's code, so read `sandbox-manager/AGENTS.md` before working on it and do not add our code under `server/`, `packages/` or `apps/`.
  - Follow the `writing-for-agents` skill for both files.
- **`.gitignore`.** It un-ignores `/sandbox-manager/.scratch/` and `/sandbox-manager/docs/agents/` instead of the old paths.
- **`sandbox-manager/README.md`** gives in a few lines:
  - what the tree is;
  - where the code, deploy files and docs are;
  - how to test and build.
- **Path references.** The moved docs point at the new code and deploy paths: POOL.md, SANDBOX.md, GITOPS.md, the READMEs, ADRs 0001–0004 and the glossary, plus the package comments. Earlier specs' and tickets' Comments are history and stay as they are.
- **ADR 0005,** "The sandbox manager lives in its own module". Use the `domain-modeling` skill. It records:
  - the context;
  - the decision;
  - the separate-repository option, and why it was deferred;
  - the wire-type copy and its contract tests as the price.

  Add the module to the glossary only if a term is needed.

**Blocked by:** 01, 02

**Status:** resolved

- [x] The repo root has no `GLOSSARY.md`, `docs/adr/`, `docs/agents/` or `.scratch/`; they exist under `sandbox-manager/`
- [x] The root `AGENTS.md` is a pointer, and `sandbox-manager/AGENTS.md` and `CLAUDE.md` exist and name the tracker and domain docs at their new paths
- [x] `git status` shows the moved tracker and agent docs as tracked, not ignored
- [x] ADR 0005 exists
- [x] No file outside earlier specs' and tickets' history still names `server/internal/sandboxmanager`, `server/internal/podsupervisor`, `server/cmd/sandbox-manager`, `server/cmd/pod-supervisor` or `deploy/sandbox` (checked with `git grep`)
- [x] Every relative link in the moved docs resolves
- [x] `git diff --check` is clean; no code test needs to run beyond `go vet ./...` in `sandbox-manager/` (package comments)

## Comments

**Done:**

- **Moved with `git mv`:**
  - `GLOSSARY.md`
  - `docs/adr/0001`–`0004`
  - `docs/agents/*`
  - `.scratch/`, every feature and this spec included
  - the root `AGENTS.md`, which became `sandbox-manager/AGENTS.md`
- **`sandbox-manager/AGENTS.md`** now opens with a boundary section and a checks section. Its tracker and domain paths are relative to `sandbox-manager/`.
- **`docs/agents/issue-tracker.md`** names `sandbox-manager/.scratch/`. `docs/agents/domain.md` notes that the glossary and ADRs are in `sandbox-manager/`.
- **The root `AGENTS.md`** is a three-line pointer.
- **The root `.gitignore`** un-ignores `/sandbox-manager/.scratch/` and `/sandbox-manager/docs/agents/`. `git check-ignore` confirms that new files in both are not ignored.
- **New docs:** `sandbox-manager/README.md` and ADR 0005.
- **Path references.** `git grep` finds no old code or deploy path outside earlier specs' and tickets' history. The remaining `deploy/sandbox-manager` hits are `kubectl port-forward deploy/<name>` commands. Every relative markdown link in the moved docs resolves.

**Deviation:**

- **`CLAUDE.md` is tracked by force.** The repository's `.git/info/exclude` excludes `CLAUDE.md`. On the operator's word, `sandbox-manager/CLAUDE.md` (`@AGENTS.md`) was added with `git add -f`. The root `CLAUDE.md` stays local.
- **The glossary is unchanged.** The move needs no new term.

**Checks:**

- `git diff --check` is clean.
- `go vet ./...` passes in `sandbox-manager/`; the docs change touched only package comments.

**Follow-up (operator's word):**

- **The excludes are gone.** `CLAUDE.md`, `docs/agents/` and `.scratch/` were removed from the repository's `.git/info/exclude`, which is local and not versioned.
- **Both `CLAUDE.md` files are committed** normally: the root one and `sandbox-manager/CLAUDE.md`.
- **The root `.gitignore` is deleted.** Its only job was to undo those excludes.
