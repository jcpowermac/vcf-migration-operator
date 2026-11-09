# Kubernetes release-wide target

## Approved behavior

Provide `--k8s-release 1.36.6` with kubernetes-only/both modes. Expand the release to selected Kubernetes dependencies instead of requiring repeated manual targets. Preserve each baseline module's major version: client-go v0.36.4 targets v0.36.6; cloud-provider-vsphere v1.36.0 tries v1.36.6. If a derived version is unavailable, keep current version and report unavailable. Never silently use another minor release.

## Approaches

Selected: expand one release flag into the existing exact-target map before applying targets. Reuse current validation and rollback. Rejected: blanket v0.36.6 mapping (breaks provider version scheme); hand-maintained complete command (repeated omissions).

## Builder decisions and constraints

- Target selected baseline graph, including indirect/graph-only Kubernetes modules, not only direct requirements. Root requirements are included in that graph.
- Release input is exact stable 1.N.P. Derive v<current-major>.N.P using existing x/mod/semver.
- Independently versioned klog/v2, utils, kube-openapi and gengo/v2 remain pinned; do not infer Kubernetes release versions for these utilities. Skip pseudo-version/prerelease baselines rather than misinterpret them.
- Do not automatically downgrade a newer baseline; keep/report it. Existing explicit --k8s-target semantics remain unchanged and explicit entries override release-derived entries.
- Determine availability from successful `go list -m -versions` results. Absence means unavailable; retrieval/authentication/list failures remain execution failures, not unavailable.
- Unavailable derived targets stay out of target map, retaining exact baseline pins. If an available target requires changing such a pin, retain existing transaction failure/rollback rather than claim partial success or loosen constraints.
- Record release input and effective target map in policy/report. Add a structured release-target result list (path, current, target, disposition) for available, already-current, unavailable, independent-version, and downgrade-skipped cases. Print unavailable decisions clearly to stderr.
- Resolve release targets after baseline capture and before target application; preserve report information on rollback. Keep existing treatment of new Kubernetes graph modules (explicit policy required).
- Launcher uses `go run -mod=readonly` so vendor bootstrap needs no manual GOFLAGS. Recognize new flag while scanning --go-bin arguments.
- No new dependencies, actual upgrade, automatic commit/push, or broad policy relaxation.

## Verification

TDD for CLI parsing, major mapping, missing provider version, explicit overrides, utility skipping, downgrades and retrieval failures. Local Go proxy integration tests verify full selected-graph targets, preserved unavailable provider, report persistence, constraints and rollback. Launcher test runs with no GOFLAGS and incomplete vendor. Run updater suite/vet/build/help and attempt broader non-e2e tests; report pre-existing failures separately.
