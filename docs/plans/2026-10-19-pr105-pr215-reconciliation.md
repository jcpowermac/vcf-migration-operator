# Reconciling PR #105 and PR #215 with failure-domain mapping

## Scope and evidence

Handoff for an implementation agent, based on branch `failure-domain-mapping`
at `ec0173e9c`. The current diffs of
[PR #105](https://github.com/openshift/vcf-migration-operator/pull/105) and
[PR #215](https://github.com/openshift/vcf-migration-operator/pull/215) were
retrieved with `gh pr diff` for this revision. PRs can change: refresh their
head SHAs and diffs before implementing. Review comments are leads, not
requirements; verify them against the source.

This document proposes integration work; it does not implement the VMotion
runtime or authorize merging either PR.

## Branch behavior to preserve

- `spec.failureDomainMappings` maps source FD names to destination FD names;
  it is a map list keyed by source, with MaxItems=64 and unique destinations.
  Do not confuse its markers with those on `spec.failureDomains`.
- Preflight resolves and records `status.failureDomainMappings`. Execution uses
  this recorded mapping, not a newly inferred mapping from mutated infrastructure.
- `ensureMappingImmutable` compares nonempty requested mappings against status,
  order-insensitively. Clearing the spec field is allowed; execution continues
  from status. This is effective-mapping stability, not unconditional spec-field
  immutability.
- `status.sourceVCenter` preserves cleanup identity across retries.
- Worker creation copies each source MachineSet separately, with deterministic
  base/base-2/base-3 names and exact replica counts, including zero.
- An FD with no matching source MachineSets is intentionally skipped
  (`f0bc8fdfd`); it differs from an FD containing a zero-replica MachineSet.
- `status.workerMachineSetNames` records generated destinations (`ec0173e9c`).
  Existence checks use recorded expectations after source cleanup; readiness
  retains those FDs even when source MachineSets disappear. The caller blocks
  an empty readiness FD list when names were recorded. The readiness helper
  itself does not universally reject empty lists: truly empty mappings remain
  intentional.

Inspect `api/v1alpha1/vmwarecloudfoundationmigration_types.go`,
`internal/controller/failuredomains.go`, `internal/controller/preflight.go`,
`internal/controller/vmwarecloudfoundationmigration_controller.go`, and
`internal/controller/review_regression_test.go` before editing.

Limit: recording currently happens in the creation loop. Do not assume it
retroactively reconstructs every expectation for in-flight migrations that
predate this status field. Missing destinations after source deletion cannot
be reconstructed from a deleted source template; do not silently mark success.

## PR #215: reconcile historical analysis with implemented behavior

Files:

- `docs/failure-domain-mapping-analysis.md`
- `docs/superpowers/plans/2026-10-15-failure-domain-mapping.md`

These documents already have counterparts on the implementation branch.
Compare content after rebasing; duplicate additions may disappear or conflict,
so do not assume both PRs must independently merge the same files.

Required documentation edits:

1. Label the original even-split/minimum-one replica behavior as the historical
   baseline. Preserve that analysis where it explains the motivation; do not
   mechanically replace historical statements with current behavior.
2. Update proposed/current behavior to exact per-source replica copying,
   heterogeneous source-template preservation, and deterministic suffixed names.
3. Replace the proposed error for no matching source MachineSets with the
   intentional empty-mapping skip. Contrast that with a zero-replica source
   MachineSet, which still produces a destination MachineSet.
4. Describe the recorded mapping's stability accurately, including allowed
   omission of the spec mapping after preflight.
5. Document recorded destination expectations and retry behavior after source
   cleanup, including missing suffixed copies and the in-flight migration limit.
6. Reconcile plan checkboxes with implementation evidence rather than marking
   unverified tasks complete. Include the cleanup retry regression tests and,
   if within scope, MCP convergence safety work (`7dd3f916c`).

## PR #105: actual API shape and integration changes

### Preserve the current design, not stale review suggestions

The current PR defines:

- Optional `spec.nodeMigration`; omission retains MachineSet/CPMS replacement.
- Required `workers` and `controlPlane` role objects when that block is present.
- Role-level `type` (`Recreate` or `VMotion`), currently optional/defaulted to
  Recreate. There is no `nodeMigration.type` field.
- `workers.vmotion.mode` and `controlPlane.vmotion.mode`: explicit Hot or Cold,
  not Auto. The `vmotion` member is required for VMotion and forbidden for
  Recreate. Both roles must select compatible engine/mode values.
- Positive integer `maxUnavailable`, default 1; control plane restricted to 1,
  with additional VMotion worker constraints in the PR's validation.
- Recovery/execution status and a proposed VMotion runtime design; API schema
  alone does not implement safe relocation or recovery.

Do not introduce Auto/fallback, flatten `vmotion.mode`, or add percentage
concurrency based on obsolete comments. Do not prescribe dropping VM identity
fields without checking their recovery consumers. `originalUnschedulable`
records pre-migration scheduling state; a generic phase/reason is not a safe
replacement. Any convention-driven redesign must retain unknown, originally
schedulable, and originally unschedulable semantics.

The optional/defaulted discriminator can be reviewed separately against API
conventions, but changing it is not an established requirement of this mapping
integration. Likewise, do not infer compatibility guarantees solely from the
v1alpha1 label.

### 1. Reconcile template validation by engine

This branch requires `topology.template` for every destination FD. PR #105
moves that requirement to spec-level, engine-aware CEL:

- Omitted `nodeMigration` or Recreate: template remains required.
- VMotion: exactly one destination FD, without requiring a template.

Keep the engine-aware rules and remove the unconditional field-level template
rule when combining the APIs. Retaining both would reject valid VMotion input.
Preserve mapping list bounds, uniqueness, and other unrelated validation.
Update admission tests, sample YAML, `docs/dev/api.md`,
`docs/dev/node-migration-api.md`, and `docs/user/spec-examples.md` consistently.

### 2. Integrate mappings with Recreate; decide VMotion support explicitly

Recreate uses MachineSets/CPMS. It must be allowed to use this branch's
`failureDomainMappings`; do not make `nodeMigration` and mappings mutually
exclusive, and do not classify all `nodeMigration` configurations as UPI.

VMotion mapping semantics remain an integration decision. The PR currently
limits VMotion to one destination FD; this does not by itself establish how
source FDs should be selected or recorded. Decide whether that engine accepts
explicit mappings, requires a single implicit mapping, or rejects explicit
mappings. Document the supported source topology before adding engine-specific
CEL/runtime checks. Do not add a blanket exclusion for Recreate.

### 3. Preserve runtime boundaries and retry identity

Review preflight and stage dispatch before wiring execution:

- Omitted selector and explicit Recreate must retain mapping-driven worker
  creation, readiness, rollout gates, and source cleanup behavior.
- VMotion must not enter MachineSet creation/scale-down merely because an API
  object is admitted. Its runtime requires its own inventory, frozen execution,
  action ownership, and recovery safeguards described in PR #105's design.
- Reconcile the VMotion design's frozen source identity with existing
  `status.sourceVCenter`; never rediscover cleanup identity from a mutated
  `VCenters[0]`. Do not reuse worker MachineSet expectations for relocation.
- Keep stage conditions coherent with the existing WorkloadMigrated →
  SourceCleaned → Ready sequence. A new stage is not automatically necessary.

Do not expand an API-only rebase into implementation of the VMotion runtime.
Track missing runtime work explicitly.

### 4. Resolve source conflicts, then regenerate

Review combined edits to the API types and documentation. Regenerate deepcopy
and CRDs rather than manually splicing generated blocks. Check the base CRD
and bundled CRD for equivalent combined schema. PR #105's inspected diff does
not change the CSV; do not claim that it necessarily conflicts. Bundle
regeneration may still update it and must be reviewed.

## Execution and validation checklist

1. Refresh PR diffs/head SHAs and compare them with the target branch. Choose
   merge/rebase ordering with the owner; if this branch lands first, rebase
   the remaining changes and drop duplicate doc additions as appropriate.
2. Apply the PR #215 documentation corrections above.
3. Combine PR #105 API sources with mapping support, reconcile engine-aware
   template validation, and resolve VMotion mapping policy before encoding it.
4. Add/retain admission tests for omitted selector, explicit Recreate with
   mappings, VMotion without a template, invalid engine/member combinations,
   role-mode constraints, and the chosen VMotion mapping policy.
5. Retain runtime regressions for originally empty mappings, zero replicas,
   missing base/suffixed destinations after cleanup, and readiness failures.
6. Run `make generate manifests`; run `make bundle` if refreshing release
   artifacts, using the repository's required tooling. Review generated diffs.
7. Run `go build ./...`, `go vet ./...`, applicable API/controller tests and
   `go test ./...` with valid envtest assets, then `make lint`.

Do not dismiss future lint failures as pre-existing from an earlier session's
45 typecheck errors. Reproduce failures on a clean baseline with the same
Go/tool versions and report what actually ran. This handoff revision is
 documentation-only; it does not establish passing integration tests.
