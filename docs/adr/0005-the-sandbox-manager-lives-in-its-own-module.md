# The sandbox manager lives in its own module

The manager's code sat inside Multica's tree and Go module: the binaries under `server/cmd/`, the packages under `server/internal/`, the deploy files under `deploy/sandbox/`, and the glossary, ADRs and tracker at the repo root. A reader could not tell our files from Multica's, importing a Multica release meant merging around our files, and nothing stopped our code from importing Multica packages; the dispatcher already used `server/pkg/protocol`.

Instead, everything of ours lives under `sandbox-manager/`, in a Go module of its own that requires no Multica module. The few daemon protocol types we exchange with Multica and with the pods' daemons are our own copy in `internal/daemonwire`, holding only the fields we use. Golden fixtures in the v0.6.4 wire format, the version the company server runs, pin them. The repo root keeps only a pointer in `AGENTS.md` and the `.gitignore` entries for the tracker. The pod image still builds Multica's stock daemon from `server/`, unchanged.

## Considered options

- **Stay in Multica's module and enforce a no-import rule with a test.** It is cheaper, but it keeps our files in Multica's directories, which is the confusion we are removing.
- **A separate repository now.** It is the cleanest cut, but the pod image build and the end-to-end scenarios need Multica's sources and backend image at a known tag, so CI and fetching would have to be designed first. With everything under one directory, `git subtree split --prefix=sandbox-manager` makes the move later.

## Consequences

- A change to Multica's daemon protocol no longer reaches us through the compiler. The fixtures and the end-to-end scenarios against a pinned backend version are the contract checks, and a backend upgrade means re-checking the fields `daemonwire` uses.
- `go list -m all` in the module must list no Multica module; a test enforces it.
