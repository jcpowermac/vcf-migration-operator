# Dependency Updater Port Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the existing safe dependency updater to vcf-migration-operator without running an upgrade.

**Architecture:** Copy the existing launcher, CLI, engine, and tests. Adapt repository identity only; preserve policy and transaction behavior.

**Tech Stack:** Go 1.26.10, Bash, existing golang.org/x/mod, Go testing.

**Spec:** `docs/superpowers/specs/2026-11-09-dependency-updater-port-design.md`

## Global Constraints

- Target module: `github.com/openshift/vcf-migration-operator`.
- No new dependencies, dependency upgrades, CI, commits, or pushes.
- Preserve modes, exact pins, rollback, clean-worktree checks, reports, and exit codes.
- Exclude compiled `*.test` binaries and empty legacy helper; ignore root `_output/` reports.

## Review Focus

- Wrong repository identity: add explicit old-module rejection and target-module acceptance tests.
- Dirty worktree: retain `TestExecuteDirtyTreeReturnsPreconditionExitAndWritesReport`.
- Report publication failure: retain `TestExecuteReportPublicationControlsDependencyCommit` rollback assertions.
- Compiler selection with spaces: retain `TestLauncherUsesSelectedGoBinary`.
- Copied artifacts and stale source identity: assert file list excludes binaries/helper and search for old module/test environment names.

## Task 1: Port implementation, tests, and usage

**Files:**
- Create `hack/upgrade-automation/README.md`, `scripts/upgrade.sh`, `cmd/updater/{main.go,main_test.go}`, and all source `updater/*.go` files.
- Modify `.gitignore` to add `/_output/`.

**Interfaces:** Preserve `scripts/upgrade.sh` flags and `updater.Run` API; `validateRepository(root string) error` must accept only target module and a clean repository containing Makefile.

- [ ] Copy source README, launcher (preserving executable mode), and Go files; exclude binaries and empty legacy helper.
- [ ] Adapt test imports and module fixtures to target identity. Rename `MTO_GIT_STATUS` to `VCF_GIT_STATUS`. Add `TestValidateRepositoryModuleIdentity`: with fake clean Git and Makefile, accept target module; overwrite go.mod with source module and require rejection containing expected target identity.
- [ ] Adapt CLI import so tests compile; run `go test ./hack/upgrade-automation/cmd/updater -run TestValidateRepositoryModuleIdentity -count=1`. Expect failure until repository guard is adapted.
- [ ] Adapt repository guard and diagnostic to target module. Add `/_output/` ignore entry. No engine changes or dependency tidy.
- [ ] Run `go test ./hack/upgrade-automation/... -count=1`; expect all copied tests to pass.
- [ ] Run `go vet ./hack/upgrade-automation/...`, `go build -o /tmp/vcf-dependency-updater ./hack/upgrade-automation/cmd/updater`, `bash -n hack/upgrade-automation/scripts/upgrade.sh`, and `hack/upgrade-automation/scripts/upgrade.sh --help`; expect success.
- [ ] Inspect `git diff --check`, file list, and old module/environment-name search. Confirm go.mod/go.sum/vendor unchanged. Do not run actual updater or commit.

## Execution result

Task 1 complete, uncommitted on `update-go-and-k8s`. All steps verified with `GOFLAGS=-mod=readonly` because committed vendor metadata already mismatches go.mod. Identity test observed RED→GREEN. All 61 updater tests pass; vet, build, Bash syntax, documented launcher help, executable bit, and artifact checks pass. Engine files/tests byte-identical to source. go.mod/go.sum/vendor unchanged.

Independent review's launch-prerequisite finding addressed in README. Deferred minor: source backup-directory prefix `mto-upgrade-transaction-` remains; no runtime effect.

Broader non-e2e test run fails during setup for api/v1alpha1, cmd, internal/controller, internal/metadata, internal/metrics, and internal/openshift: existing client-go v0.36.4 imports k8s.io/api/scheduling/v1alpha2, absent from selected api v0.37.1. internal/execution and internal/vsphere pass. Dependency repair and actual upgrades remain outside scope.

## Self-review

Spec covered by one cohesive port task. Existing source tests cover policy and transaction behavior; new repository-identity test covers adaptation. No engine restructuring or new abstractions needed.
