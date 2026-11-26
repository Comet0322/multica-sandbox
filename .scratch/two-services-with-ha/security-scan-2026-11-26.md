Status: resolved

# Security scan before the handover (2026-11-26)

The `security-scan` skill (user-level) on `two-services-ha-step1` at 22f17d9, committed state, git history included. Its script runs gosec and govulncheck only under a `server/` directory, so both were run by hand on the scan's snapshot at the module root.

| Tool | Result |
| --- | --- |
| OSV-Scanner, Gitleaks (tree and history), YARA, ClamAV, Semgrep, credscan | clean |
| govulncheck | clean with Go 1.27.4 (13 standard-library vulnerabilities with 1.27.1/1.27.3, fixed by b3c1109) |
| gosec | 2 G304, accepted (below) |
| Trivy misconfig | 7 MEDIUM, 7 LOW, accepted (below) |

Fixed in 22f17d9: G115 (pod order by `cmp.Compare`), G407 (payload sealed with `cipher.NewGCM` and a `crypto/rand` nonce, same layout as before), a credential-named constant in a store test.

Accepted by the operator, left in the report, no suppression annotations:

- gosec G304 `internal/sandbox/kube.go` (`LoadPodTemplate`) and `cmd/sandbox-devpods/main.go` (`-overrides`): the path is the operator's own setting.
- Trivy KSV-0048 on `deploy/k8s/rbac.yaml` and `deploy/k8s/dev/dev-user.yaml`: managing pods is the manager's job.
- Trivy KSV-0125 on the manifests: point the images at the company registry when deploying.
- Trivy KSV-0110 (no namespace in the manifests: they are applied with `-n`) and KSV-0020/0021 (the kind dev Postgres).
- Semgrep partial parsing of two lines of `deploy/k8s/e2e/scenarios.sh`: a parser limit, no finding.

Rerun after any change to `go.mod`, the Dockerfiles or code that reads files, crypto or integers.
