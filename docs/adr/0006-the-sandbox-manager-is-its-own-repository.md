# The sandbox manager is its own repository

[ADR 0005](0005-the-sandbox-manager-lives-in-its-own-module.md) moved the manager into its own Go module under `sandbox-manager/` inside the Multica repository and deferred a separate repository. Keeping both in one repository still made it easy to confuse our files and branches with Multica's.

Instead, the manager is a repository of its own. It sits next to a Multica checkout in one folder:

```
multica-sandbox/
  sandbox-manager/   this repository
  multica/           Multica at the daemon's version (slim/v0.6.4)
```

The pod image takes Multica's sources from a named build context (`--build-context multica=../multica`) and builds the stock daemon from its `server/`. Nothing else uses the Multica checkout. The repository started from the content of `sandbox-manager/` at Multica commit `4b9a871ab`. The earlier history stays on that repository's `feat/sandbox-manager-own-module` branch.

The daemon now comes from `slim/v0.6.4`, the version the company server runs, instead of the v0.7.0 snapshot it was built from before.

## Considered options

- **Keep the module inside the Multica repository** (ADR 0005). It needs no second checkout, but our work stays mixed with Multica's branches and root files.
- **Carry the history over with `git filter-repo`.** It would keep `git log` reaching the first commit, but the operator chose a fresh start.

## Consequences

- Building the pod image needs the Multica checkout beside this repository, or any path given to `--build-context multica=`.
- Upgrading the daemon means moving that checkout to another Multica version and running the scenarios again.
