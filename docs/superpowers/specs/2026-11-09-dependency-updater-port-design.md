# Dependency updater port

Approved scope: copy the dependency updater from `~/openshift_working/multiarch-manager-operator/hack/upgrade-automation` into this repository.

## Intent and constraints

Provide the same manual Go/dependency update workflow for vcf-migration-operator. Preserve all four policy modes, exact pins, clean-worktree checks, rollback, source migration, reports, and exit codes. Do not run a real dependency upgrade, change dependency versions, add dependencies, commit, push, or add CI integration.

## Approaches

- **Selected: copy and adapt repository identity.** Keeps existing implementation and tests; smallest behavioral change.
- Rewrite as a smaller script: less copied code, but loses existing safety behavior and requires rebuilding tests. Rejected.

## Changes

Copy README, launcher, Go implementation and tests under `hack/upgrade-automation/`. Replace source module identity with `github.com/openshift/vcf-migration-operator` in CLI imports, repository guard, and test fixtures. Rename source-specific fake-Git test environment variable. Omit compiled `*.test` binaries and empty legacy `scripts/lib/file-updates.sh`. Ignore root `_output/` reports in `.gitignore`.

Existing `golang.org/x/mod` is sufficient. Preserve dependency classification and vendor contents during this port; no tidy required.

## Verification and limits

Run copied unit/integration tests, launcher syntax/help, build/vet for copied packages, and inspect diff for leftover source identity or artifacts. Tests exercise temporary repositories, not a real upgrade of this repository. Updater requires a clean worktree before actual use. No automatic commit is part of this work.
