# Kubernetes Release Target Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Update release-linked Kubernetes dependencies with one release flag, preserving module majors and reporting unavailable versions.

**Architecture:** Expand release input against baseline graph into existing exact-target map before ApplyTargets. Keep transaction/validation behavior; persist resolution decisions in report.

**Tech Stack:** Go, Bash, installed golang.org/x/mod, existing local Go proxy test helpers.

**Spec:** `docs/superpowers/specs/2026-11-09-kubernetes-release-target-design.md`

## Global Constraints

- `--k8s-release 1.36.6` maps client-go v0.36.4 to v0.36.6 and provider v1.36.0 to v1.36.6, not vice versa.
- Missing derived versions keep baseline and report unavailable; failed retrieval remains fatal.
- Explicit module targets override automatic targets; independent utilities remain pinned; no automatic downgrades.
- Preserve exact pins, compiler selection, rollback, report publication, and existing flag behavior.
- No new dependencies, actual upgrade, automatic commit, or push.

## Review Focus

- Missing provider version is not network failure: test both successful absence and failed version listing.
- Baseline graph includes indirect/graph-only modules: integration fixture must include both.
- Independent/pseudo/prerelease/newer-baseline versions must not be remapped or downgraded: table-driven resolution tests.
- Unavailable pin may conflict with another target: integration test expects failure and full rollback, retaining release results.
- Vendor bootstrap and selected compiler must work with unset or vendor GOFLAGS and paths containing spaces: launcher tests.

## Task 1: Release policy, resolution and reporting

**Files:** Modify `updater/policy.go`, `policy_test.go`, `engine.go`, `report.go`, `report_test.go`, `target_integration_test.go`, `engine_integration_test.go`, `cmd/updater/main.go`, `cmd/updater/main_test.go`; create `updater/release.go` and `release_test.go`.

**Interfaces:**
- Change `ParsePolicy(mode, goTarget, k8sRelease string, k8sTargetArgs []string) (Policy, error)` and update all callers with empty release for legacy cases.
- Add `Policy.KubernetesRelease string` with JSON `kubernetesRelease,omitempty`.
- Add exported `ReleaseTargetResult` (Path, CurrentVersion, TargetVersion, Disposition) and `Report.KubernetesReleaseTargets []ReleaseTargetResult`, JSON `kubernetesReleaseTargets,omitempty`.
- Implement `ResolveKubernetesRelease(ctx context.Context, runner GoRunner, baseline Snapshot, policy Policy) (Policy, []ReleaseTargetResult, error)`. Return cloned target map, sorted decisions, error for execution failures. No mutation of input policy or module files.

- [ ] Add failing parser tests: both/kubernetes-only accept release without explicit targets; reject malformed/prerelease release and release in pin-both/go-only. Existing target-only modes stay valid. Add CLI parsing/help tests for new flag.
- [ ] Run new tests RED, implement flag and parser changes, update all callers, run GREEN.
- [ ] Add resolver tests with existing local proxy fixtures: v0/v1 major preservation, missing provider v1.36.6, explicit override bypass, already-current, utilities/pseudo/prerelease skips, newer baseline preserved, unavailable vs proxy failure. Verify no policy-map mutation and deterministic sorted decisions.
- [ ] Run resolver tests RED, implement resolver with successful version-list membership checks and existing GoRunner. For skipped pseudo/prerelease baselines use independent-version disposition. Run GREEN.
- [ ] Add integration/report tests: api/client-go/streaming plus indirect selected modules update to v0.36.6, provider with only v1.36.0 stays pinned/reported unavailable; missing provider lists no newer substitute. Add unavailable-pin conflict fixture requiring rollback and persisted decisions, plus invalid-target and execution-error rollback assertions.
- [ ] Run integration tests RED; call resolver after baseline/result setup before emitRequestedTargets/ApplyTargets. Update cfg.Policy and report.Policy to resolved policy. Store decisions even on resolver failure and print each unavailable decision. Preserve normal ModuleResult outcomes, using structured release decisions to distinguish unavailability. Run GREEN and full updater suite.

## Task 2: Launcher and user documentation

**Files:** Modify `scripts/upgrade.sh`, `cmd/updater/main_test.go`, `README.md`.

**Interfaces:** Launcher retains flags/exit behavior and compiler selection; passes `-mod=readonly` to go run and treats --k8s-release as value-taking flag.

- [ ] Add launcher regression test using temporary repository tree with minimal local updater and incomplete vendor. Copy actual launcher, unset GOFLAGS/set vendor GOFLAGS, invoke --help with selected compiler path containing spaces and release argument before --go-bin. Assert exit 0/output, no go.mod/go.sum mutation. Run RED against old launcher.
- [ ] Add readonly go-run option and release flag scanner handling. Run launcher tests GREEN.
- [ ] Update help/README with single-release command, major-preservation examples, skip/unavailable/explicit-override behavior, conservative conflicts/new-module policy, and automatic module-mode bootstrap. Remove obsolete manual override prerequisite.
- [ ] Run `go test ./hack/upgrade-automation/... -count=1`, `go vet ./hack/upgrade-automation/...`, and build with `GOFLAGS=-mod=readonly`. Run Bash syntax and plain launcher --help with GOFLAGS unset/vendor. Verify diff whitespace and untouched go.mod/go.sum/vendor. Attempt broader non-e2e tests without code generation and report unrelated failures.
- [ ] Independent read-only final review, fix important findings with tests, leave changes uncommitted for user.

## Execution result

Both tasks implemented natively on existing feature branch, without commit or dependency changes. The release implementation and graph-only pin fix passed 66 updater tests with GOFLAGS=-mod=readonly, including local proxy release resolution/rollback/report tests, graph-only pin regression, and incomplete-vendor launcher regression. Vet, build, Bash syntax, plain help with unset GOFLAGS, help with vendor GOFLAGS, and diff checks pass. Independent final review found no issues. Broader non-e2e tests exceeded 120-second command timeout; completed output showed api/v1alpha1 and updater packages passing, but full suite is not verified. No live upgrade performed.

## Self-review

Both tasks share CLI/launcher flag contract: --k8s-release takes exact release string. Resolver returns effective Policy before ApplyTargets; report preserves requested release plus effective map and all decisions. Tests cover missing-version vs failed-list distinction and rollback interactions. No dependency or version edits are required for implementation.
