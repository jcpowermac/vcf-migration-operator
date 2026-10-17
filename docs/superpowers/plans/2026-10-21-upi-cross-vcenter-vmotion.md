# UPI Cross-vCenter VMotion Implementation Plan

> **For agentic workers:** Use `superpowers:executing-plans` or `superpowers:subagent-driven-development` to implement this plan PR by PR. Keep each PR independently reviewable and mergeable; do not enable an incomplete migration path.

**Goal:** Implement the first-release UPI cross-vCenter VMotion workflow described in the design, preserving existing Node/VM identity and safely supporting explicit Hot and Cold modes.

**Architecture:** Keep the existing CR, reconciler, leader election, and migration phases. Dispatch explicit VMotion before legacy probe/template and MachineSet/CPMS assumptions; freeze a per-node plan before mutations; use a durable action-claim state machine and a separately testable vSphere adapter. Land shared preparation and operator availability before integrating worker and then control-plane workflows. Keep incomplete paths blocked until their safety dependencies and qualification evidence are present.

**Tech Stack:** Go, controller-runtime, Kubernetes/OpenShift clients, govmomi, envtest, Go unit tests, Ginkgo/Gomega, existing Kustomize/OLM packaging.

**Spec:** `docs/plans/upi-cross-vcenter-vmotion-design.md` (authoritative implementation contract).

## Global Constraints

- Preserve omitted `spec.nodeMigration` behavior and explicit `Recreate`; VMotion dispatch must not create replacement Machines or use the legacy probe/template path.
- Only `openshift-vcf-migration/cluster` is executable; reject unsupported scope before migration side effects.
- Support three dedicated control-plane nodes plus workers; no SNO, compact, dual-role, or unclassified-node expansion.
- Require explicit `VMotion` and explicit matching `Hot` or `Cold` modes; no `Auto`, mixed modes, or automatic fallback.
- Allow one node workflow and one mutating VM action in flight cluster-wide; workers precede control-plane nodes in stable order.
- Consume the effective source-to-destination failure-domain mapping produced by shared pre-processing; do not add VMotion-specific mapping cardinality. Freeze the selected per-node Node/VM/placement identities before configuration mutation; retries reuse the plan and reject unapproved drift.
- A persisted action claim with unknown submission outcome is observed or escalated, never blindly resubmitted; leader election is not external-operation fencing.
- Pause allows observation and result persistence but no new migration mutations. Hot never shuts down/powers off; Cold uses graceful shutdown only.
- Preserve PDB/quorum protections, pre-existing cordons, and migration-owned mutation boundaries. No automatic source power-on, VM deletion, network repair, or rollback.
- Do not store credentials, TLS private material, ServiceLocator secrets, or full task output in status/logs.
- Initial intent is OCP 5.0 and vSphere 8/9 families; support claims require qualification for exact OCP/vCenter/ESXi, address-family, DRS/pool, and boot combinations.

## Review Focus

- **Pre-processing mapping contract:** PR #219 proposes `status.failureDomainMappings` as the effective mapping. VMotion must consume that output without assuming one-to-one cardinality, while still requiring an unambiguous target for each planned node.
- **Observation after pause/spec change:** `internal/controller/execution.go` currently gates mutations on Running state and generation. Result observation/persistence for an already-claimed action must not reuse that mutation-only gate.
- **Parallel PR contract:** PRs 3 and 4 can proceed independently only after PR 2 records the shared action, identity, and backend contract; otherwise their state/result types can diverge.
- **Credential cache correctness:** `internal/vsphere/session.go` caches sessions by server/datacenter/username, excluding password and TLS policy. Verify VMotion calls cannot reuse an authenticated session after credential/trust changes.
- **Late/ambiguous external results:** task absence, wrong-owner task references, late shutdown completion, and a successful relocation followed by failed power-on must retain distinct outcomes and block further disruption/cleanup.

---

## Delivery map

```text
PR 1 API (current branch) + PR #219 mapping contract -> PR 2 runtime selection/frozen plan
                                  |          |          |
                                  PR 3       PR 4       PR 5
                                  durable    vSphere    preparation + availability
                                  execution  adapter          |
                                      \       |             /
                                       PR 6 Hot workers
                                             |
                                       PR 7 control plane
                                             |
                                       PR 8 Cold extension
                                             |
                                       PR 9 cleanup + final qualification/enablement
```

PRs 3, 4, and 5 may be developed in parallel after the shared interfaces and acceptance boundary are recorded in PR 2. PR 6 requires PRs 2–5. PR 7 and 8 extend the integrated workflow in order. PR 9 is final end-to-end qualification and guarded cleanup, not the first point at which each prerequisite is qualified.

If the operator availability and packaging changes make PR 5 too broad, split those changes into a separate parallel PR after PR 2; PR 6 still depends on both shared preparation and availability.

PR 1 and PR #219 both change API types and generated CRD/bundle artifacts. Coordinate/rebase those changes: use PR #219's effective mapping status as the shared contract and do not add a duplicate VMotion mapping field.

## PR 1 — API and bounded status foundation (current branch)

**Purpose:** Establish explicit engine/mode selection and bounded status/action fields without changing the legacy runtime path.

**Files:** `api/v1alpha1/vmwarecloudfoundationmigration_types.go`, `api/v1alpha1/nodemigration_api_test.go`, generated API artifacts under `api/v1alpha1/`, `config/crd/bases/`, `bundle/manifests/`, and `docs/dev/node-migration-api.md`.

- [x] Remove VMotion-only failure-domain-count validation and the single-target `FrozenPlan.TargetFailureDomain` assumption. Consume the effective mapping in `status.failureDomainMappings`; keep the mapping owned by shared pre-processing and preserve each selected target FD/host/pool/datastore/network on its per-node record.
- [ ] Check the rest of the API contract against design §2 and gate G1: defaults/immutability, bounded status, recovery acknowledgement, and status merge semantics. Do not describe API fields as shipped until the PR is merged.
- [x] Verify omitted selection and Recreate still require/use the legacy template behavior, while VMotion does not require a template.
- [x] Run API/envtest cases for legacy, Recreate, explicit Hot/Cold VMotion, invalid mode combinations, VMotion with the FD sets accepted by pre-processing, and bounded action/status round-trip; verify mapping fields are not duplicated and regenerate manifests/bundle artifacts.

**Merge gate:** The schema permits exactly the design-supported input space, including the agreed failure-domain policy; generated CRDs/bundle match Go types.

## PR 2 — Runtime selection, eligibility, and frozen plan

**Depends on:** PR 1 API contract and PR #219's effective mapping result (or an agreed equivalent pre-processing contract).

**Files:** `internal/controller/vmwarecloudfoundationmigration_controller.go`, new `internal/controller/vmotion_plan.go` and focused tests, plus `internal/controller/preflight.go` only where dispatch requires it.

- [ ] Dispatch explicit VMotion before template/probe requirements, MachineSet/CPMS creation/scaling/deletion, and Machine-derived progress checks. Leave omitted selection and Recreate on their existing path.
- [ ] Check canonical namespace/name before preparation or migration side effects. Unsupported explicit requests return actionable blocked status and do not fall through to replacement behavior.
- [ ] Consume the immutable effective `status.failureDomainMappings` produced by pre-processing; do not recompute it from FD counts/names or `workerMachineSetNames`, and do not impose a one-to-one mapping rule in VMotion. Block if the result cannot select one target FD for a planned node.
- [ ] Resolve every planned Node to one unique VM using the qualified providerID/BIOS-UUID identity contract; classify Managed, VerifiedUnmanaged, or Indeterminate, and block relevant conflicting provisioning. Do not require a globally empty Machine inventory.
- [ ] Freeze CR UID/execution identity, source/destination, mode, per-node role/Node UID/providerID/VM identity, selected target failure domain/host/pool/datastore/network, and original cordon state before configuration changes. Keep the effective mapping in its existing preprocessing status field rather than duplicating it in the VMotion API; preserve the frozen source identity for later cleanup.
- [ ] Validate plan stability on reconcile; do not silently adopt added/replaced nodes, changed identity, or changed mapping. Leave the path blocked until the frozen plan is valid.
- [ ] Define and document the small shared plan/action/backend contract consumed by PRs 3 and 4 (including task ownership, ambiguity, and read-only versus mutating calls) before parallel implementation proceeds.
- [ ] Test no side effects for missing/ambiguous preprocessing output, ambiguous VM identity, unsupported scope, Machine-managed nodes, relevant provisioning conflicts, and changed mapping; test the supplied mapping is preserved and the frozen per-node plan survives restart/reconcile.

**Merge gate:** Explicit dispatch and read-only plan creation are safe; unsupported/ambiguous requests stop before migration side effects; no vSphere mutation is enabled.

## PR 3 — Durable execution and recovery state machine

**Depends on:** PR 1 status contract and PR 2 shared action/backend contract.

**Files:** `internal/controller/execution.go`, new `internal/controller/vmotion_execution.go` and focused tests, and `api/v1alpha1/` only if an agreed status gap is found.

- [ ] Implement conflict-safe, identity-bound action claims and status merging using CR UID plus execution/plan/action IDs. A stale writer must not replace a newer action or terminal result.
- [ ] Separate permission to submit a new mutation from permission to observe and persist an existing action result; keep pause/generation changes from losing an in-flight result without authorizing new actions.
- [ ] Record shutdown, relocation, and power-on outcomes independently. Keep a claim without task reference uncertain; never resubmit it automatically. Scope every task reference to its owning vCenter.
- [ ] Implement pause A, bounded deadlines, late-completion observation, restart/leader-takeover handling, action-bound recovery acknowledgement, and deletion handoff without creating an unbounded journal or separate workflow service.
- [ ] Test claim/call/task-persistence crash windows, competing reconciles/resourceVersion conflicts, stale writers, pause before/after claim, restart with unknown submission, timeout plus late completion, stale recovery requests, and relocation success plus later power-on failure. Assert no blind duplicate submission and no recovery bypass of pause.

**Merge gate:** Fake-backend failures and conflict tests prove at-most-one acknowledged submission attempt; observation/recovery never implies replay permission.

## PR 4 — vSphere operation adapter

**Depends on:** PR 2 shared backend contract. Can be developed alongside PR 3.

**Files:** new `internal/vsphere/vmotion.go` and unit/simulator tests; `internal/vsphere/session.go` only for verified session/cache corrections.

- [ ] Implement selected-placement resolution and native `CheckRelocate_Task` using the exact frozen VM/specification, interpreting per-VM/host findings rather than request success alone.
- [ ] Implement trust/credential and entity-permission checks for the selected feasibility, relocation, graceful shutdown, and destination power-on APIs. Keep permission-only checks distinct from actual-operation rights.
- [ ] Implement relocation/task observation, `ShutdownGuest`, and qualified normal destination power-on behind the PR 2 interface. Preserve owning-vCenter task identity; return an explicit uncertain result when submission outcome cannot be established.
- [ ] Audit govmomi/transport mutation retry behavior and `GetOrCreate` session cache inputs/lifecycle so credential rotation or TLS-policy changes cannot silently reuse an incompatible session.
- [ ] Test fatal findings inside a successful task, warning-only findings, empty/uninterpretable results, wrong-owner task references, task failures, ambiguous submission, and cache behavior when password or TLS policy changes. Do not let adapter availability enable the migration path.

**Merge gate:** Adapter tests establish result semantics, trust/permissions, task ownership, and non-replay behavior; no controller workflow is enabled by this PR alone.

## PR 5 — Shared preparation and operator availability

**Depends on:** PR 1 and PR 2 frozen source/destination identity.

**Files:** `internal/controller/vmwarecloudfoundationmigration_controller.go`, existing `internal/openshift/` configuration/pod helpers and tests, `config/manager/manager.yaml`, `config/default/`, `config/rbac/`, OLM files under `config/manifests/` and `bundle/`.

- [ ] Adapt `ensureMultiSiteConfigured` and existing `AddTargetVCenter` helpers to add destination credentials/vCenter/FD while retaining source entries; verify same-name FD entries match intended topology.
- [ ] Checkpoint configuration revision and required-consumer rollout/readiness so retry does not continually restart consumers. Respect intentional component absence, including removed vSphere CSI; treat API errors as errors, not absence.
- [ ] Configure two operator replicas, required anti-affinity across distinct Nodes, matching PDB with `minAvailable: 1`, and a pre-disruption gate for a nonterminating Ready replica on another healthy Node.
- [ ] Update direct-install and OLM/bundle packaging and RBAC as required; do not silently add placement/toleration assumptions.
- [ ] Test safe retries, configuration change checkpointing, required/optional consumer discovery, off-node Ready-replica gate, PDB refusal, and leader eviction/takeover preserving the same execution. Validate generated packaging.

**Merge gate:** Preparation retries are idempotent, readiness reflects qualified reloads, and a planned node cannot be disrupted without an eligible off-node operator replica.

## PR 6 — Hot worker orchestration

**Depends on:** PRs 2–5.

**Files:** new `internal/controller/vmotion_workflow.go` and focused tests; reuse existing OpenShift drain/cordon helpers only after checking their mutation guards.

- [ ] Implement workers-only Hot flow in stable order: protected drain → fresh admission/recheck → durable claim → relocate → verify destination VM identity/placement → fresh Node identity/heartbeat recovery → restore only migration-owned scheduling changes.
- [ ] Enforce one workflow/action in flight cluster-wide and keep the CR execution/plan frozen across restart and pause.
- [ ] Keep control-plane nodes and Cold-only operations blocked in this PR. Do not enable the full VMotion selector before PR 7 and PR 8 satisfy the selected mode's entire worker/control-plane contract.
- [ ] Test PDB refusal, pause at each boundary, leader restart, identity drift, stale Ready/no fresh heartbeat, original cordon preservation, and assert Hot never calls shutdown or power-off.

**Merge gate:** Worker Hot orchestration is safe under tests but remains unavailable as a supported end-to-end mode until control-plane and release gates are met.

## PR 7 — Control-plane safety and orchestration

**Depends on:** PR 6 worker state machine.

**Files:** `internal/controller/vmotion_workflow.go` and tests; a focused health-check implementation in `internal/openshift/` with tests; `internal/controller` RBAC markers and generated RBAC/bundle if required.

- [ ] Add authenticated, bounded, member-specific etcd health reads and a qualified expected member-ID-to-frozen-Node binding. Resolve credential authority, delivery/rotation, least-privilege RBAC, freshness, and supported-version integration before enabling this path.
- [ ] Require fresh three-member health, API access, and no conflicting rollout/maintenance before disruption; recheck after drain and before action. During selected Cold absence, require both remaining members healthy; after return, require the same member and all three healthy before continuing.
- [ ] Run control-plane nodes only after workers, one at a time, without Machine membership replacement or private drain hooks. An additional member failure blocks new disruption and cleanup but preserves observation and explicitly authorized current-node recovery; pause still blocks new mutations.
- [ ] Test stale/ambiguous health, member identity mismatch, an additional member failure during absence, pause, and recovery while refusing new-node admission.

**Merge gate:** Control-plane VM mutation remains unavailable until the exact combination is qualified; when enabled, it requires fresh member-bound health and quorum-safe recovery evidence, and never replaces Machine/etcd membership.

## PR 8 — Cold orchestration

**Depends on:** PRs 6 and 7 plus qualified Cold/boot sequence for the intended combination.

**Files:** `internal/controller/vmotion_workflow.go` and tests; `internal/vsphere/vmotion.go` and simulator/unit tests.

- [ ] Extend the shared state machine: validate prerequisites and admission → protected drain → claim and call `ShutdownGuest` → observe actual powered-off state → run final native feasibility check in that state → claim/relocate → verify destination VM → claim normal power-on → verify Node/member recovery.
- [ ] Keep shutdown, relocation, and power-on evidence separate. Preserve relocation success if power-on/readiness fails; block subsequent nodes and cleanup.
- [ ] Enforce deadlines and pause boundaries. A paused VM after relocation may remain powered off; timeout/late completion does not reset a claim. No hard-off fallback or automatic source power-on.
- [ ] Test shutdown timeout and late completion, pause between relocation and power-on, feasibility after actual power-off, relocation success/power-on failure, unknown task submission, and no automatic source recovery.

**Merge gate:** Cold is enabled only for combinations with recorded sequence/boot qualification; unresolved action outcomes remain blocked for administrator intervention.

## PR 9 — Completion, guarded cleanup, and release enablement

**Depends on:** PRs 1–8 and evidence for all applicable design gates G1–G8.

**Files:** `internal/controller/vmwarecloudfoundationmigration_controller.go` source cleanup/readiness flow and tests, `internal/controller/retention.go` and tests if finalizer integration changes, and operator-facing recovery/qualification docs under `docs/`.

- [ ] Before cleanup, require every frozen entry complete; no active/unknown action or intervention; fresh identity/placement/Node heartbeat (and full etcd recovery for control plane); and no relevant live Node/ownership/provisioning drift.
- [ ] Use frozen source vCenter/FD identity and destination-exclusion guards. Remove only intended source references; preserve destination and unrelated entries; make partial cleanup retryable from actual state.
- [ ] Require final consumer rollout/readiness for current execution before `SourceCleaned`/`Ready`. Recovery acknowledgement or deletion handoff alone never authorizes cleanup; retain safe finalizer semantics.
- [ ] Add integrated failure tests for node additions/replacements before cleanup, partial source-reference removal, destination preservation, final readiness failure, pause/deletion/ownership change mid-cleanup, and no VM deletion.
- [ ] Record exact qualified OCP/vCenter/ESXi/address-family/Hot/Cold combinations and operational recovery instructions. Keep unsupported combinations blocked; do not treat unit/simulator success as cross-vCenter lab qualification.

**Merge gate:** Release enablement follows recorded qualification evidence; `SourceCleaned` describes intended operator-managed references only and does not claim external infrastructure retirement.

## Qualification timing

PR 9 owns final integrated drills and the operator-facing evidence record, but the following decisions/evidence must precede dependent implementation or enablement:

- **Before PR 1/PR #219 integration:** G1 API/status shape, compatibility, bounds, and the shared effective-mapping contract; coordinate generated API/CRD changes across both PRs.
- **Before PRs 3/4 parallelize:** agreed action/backend interface; before PR 4 completion, G2 native result, permission, power-on, and SDK retry semantics.
- **Before PR 5 merge:** G5 affected consumers, reload/readiness proxy, availability/placement, and packaging behavior.
- **Before PR 7 control-plane enablement:** G4 etcd security/identity/freshness and guard-respecting drain.
- **Before PR 8 Cold enablement:** G3 supported combination and G6 shutdown/boot/runtime evidence for that combination.
- **Before PR 9 cleanup:** G7 recovery/deletion handoff contract and G8 source-cleanup/idempotency evidence.
- **Before any full-path release:** all required exact-version lab qualification, including cross-vCenter and failure/restart drills; no universal support claim from API minimum versions.

## Repository verification per PR

Run focused package tests for changed code on each PR. Run `make generate manifests` when API markers, RBAC markers, or generated CRDs change; validate bundle/OLM output when packaging changes. Run repository-required `make test`, `make lint`, and build before release qualification. Cross-vCenter lab qualification remains a separate release gate.
