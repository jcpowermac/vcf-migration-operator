# Control-plane migration story — coverage verification

## Context

User story: as a cluster administrator, migrate control plane nodes from source to
destination failure domains so cluster management runs on the destination vCenter
without losing etcd quorum. Five acceptance criteria (AC1–AC5, listed in the map
below).

The user states the ControlPlaneMachineSet (CPMS) machinery likely already covers
this story; the task is to **verify coverage, not to implement new code**. The
control plane rollout is implemented as sub-steps inside the existing
`ConditionWorkloadMigrated` handler of the ordered six-condition workflow
(`conditionOrder` in
`internal/controller/vmwarecloudfoundationmigration_controller.go`, lines 68–75):
worker MachineSets are created on the target FDs, then the CPMS is updated in place
to the target failure domains, then the controller waits for the CPMS-driven
rolling rollout to complete, then source workers are drained and deleted.

Deliverable: a per-AC coverage report — each AC mapped to concrete implementation
anchors (file + symbol) and passing tests, with AC3 (etcd quorum) explicitly
documented as covered by delegation to the upstream OpenShift CPMS operator.
No code changes are expected. A check failing means that AC is NOT covered; see
Assumptions & contingencies for the pre-decided fallback.

## Approach

All commands run from the repository root
(`/home/jcallen/Development/vcf-migration-operator`).

### Step 1 — Run the targeted behavior tests

These pin the CP rollout behavior. Expect every test to PASS:

```
go test ./internal/openshift/ -run 'TestUpdateCPMSFailureDomain|TestCheckControlPlaneRolloutStatus|TestIsCPMSGenerationObserved|TestIsCPMSUpdatedForFailureDomains|TestListControlPlaneMachines' -v

go test ./internal/controller/ -run 'TestEnsureWorkloadMigratedRolloutAndScaleDown|TestEnsureWorkloadMigratedRolloutGate|TestRolloutLogsMachineLevelDetail' -v
```

The second command matches only plain `testing.T` tests; the Ginkgo envtest suite
(`TestControllers` in `internal/controller/suite_test.go`) is excluded by the
`-run` filter and does not need envtest binaries.

Expected key assertions (these are the behavior under audit):
- `TestUpdateCPMSFailureDomain`: CPMS updated in place to `state: Active` with
  `failureDomains.platform: vsphere` and the exact target FD names (3 cases:
  nil→target, replace-old→target, multi-FD).
- `TestCheckControlPlaneRolloutStatus`: complete only when
  `replicas > 0 && updatedReplicas == replicas && readyReplicas == replicas`.
- `TestEnsureWorkloadMigratedRolloutAndScaleDown`: while CPMS generation unobserved
  → requeue 15s with message `"Waiting for control plane rollout to start (CPMS
  generation 2/1 observed)"`; mid-rollout → requeue 30s with message `"Control
  plane rolling out (1/3 updated, 1/3 ready)"` plus a `ControlPlaneRollout` event;
  rollout complete → source worker MachineSet scaled to 0; zero-replica source
  MachineSets deleted and `ConditionWorkloadMigrated` set True only after deletion.
- `TestEnsureWorkloadMigratedRolloutGate`: CP rollout path is gated on target
  workers being ready AND the CPMS already targeting the target FDs
  (`IsCPMSUpdatedForFailureDomains`).

### Step 2 — Verify AC1 anchors (control plane inputs updated to destination FDs)

Read and confirm each anchor exists and behaves as described:

1. `MachineManager.UpdateCPMSFailureDomain` (`internal/openshift/machines.go`,
   ~line 230): fetches CPMS named `cluster` in `openshift-machine-api`, sets
   `spec.template.openShiftMachineV1Beta1Machine.failureDomains` to
   `{platform: configv1.VSpherePlatformType, vsphere: [{Name: <each target FD>}…]}`
   and `spec.state = machinev1.ControlPlaneMachineSetStateActive`, then `Update`s
   in place (no delete/recreate). Error paths: missing CPMS, missing
   `machines_v1beta1_machine_openshift_io` template.
2. Caller: `ensureWorkloadMigrated`
   (`internal/controller/vmwarecloudfoundationmigration_controller.go`, ~line
   654–668, "Step 3"): only reached after target workers are ready; guards against
   re-updating with `IsCPMSUpdatedForFailureDomains` (idempotent); after the
   update it records event `CPMSUpdated` and requeues 15s.
3. Prerequisite — destination FDs resolvable by the CPMS operator:
   `ensureMultiSiteConfigured` (443–543; call at ~line 493) calls
   `InfrastructureManager.AddTargetVCenter` (`internal/openshift/infrastructure.go`,
   ~line 71), which appends the target vCenter entry (server, port 443,
   datacenters) and each missing failure domain to
   `Infrastructure.spec.platformSpec.vsphere` and persists it. This condition is
   ordered before `ConditionWorkloadMigrated` in `conditionOrder`.
4. RBAC: `config/rbac/role.yaml` includes `controlplanemachinesets` under
   machine.openshift.io (~line 91; generated from the marker at the controller
   file ~line 91 with `get;list;watch;create;update;patch;delete` verbs). Note:
   the marker grants `delete` on CPMS, but the operator never issues a CPMS
   delete (see Step 4) — the permission is broader than the code paths that use
   it, which does not weaken the AC3 invariant.

Verdict if all confirmed: **AC1 covered** — the operator's "control plane inputs"
are the CPMS spec, updated to the destination failure domains.

### Step 3 — Verify AC2 + AC4 anchors (replacement CP nodes reach Ready; controlled sequence)

Read and confirm:

1. `MachineManager.CheckControlPlaneRolloutStatus`
   (`internal/openshift/machines.go`, ~line 267): complete iff
   `replicas > 0 && updatedReplicas == replicas && readyReplicas == replicas` —
   i.e., all replacement control plane machines are up and Ready (AC2).
2. Wait loop: `ensureWorkloadMigratedRolloutAndScaleDown` (~line 677), "Step 5"
   (~line 687–716): waits for `IsCPMSGenerationObserved` first, then polls
   `CheckControlPlaneRolloutStatus` every 30s until complete, logging
   per-machine detail via `logControlPlaneMachine` (~line 1131).
3. Sequencing (AC4): only after CP rollout is complete does the handler proceed to
   scale source worker MachineSets to 0 (Step 6, ~line 718), wait for machine/node
   deletion (Step 7, ~line 740), then delete zero-replica source MachineSets
   (Step 8, ~line 766). `DeleteMachineSetsByVCenter`
   (`internal/openshift/machines.go`, ~line 174) refuses MachineSets with nil or
   positive replicas, and rejects an empty vCenter string.
4. The one-at-a-time, quorum-safe *order of control plane machine replacement
   itself* is executed by the upstream OpenShift CPMS operator (machine-api),
   which the operator triggers via the Step-3 CPMS update; this operator only
   writes the desired FDs and waits.

Verdict if confirmed: **AC2 covered** (operator gates the workflow on
`ReadyReplicas == Replicas`); **AC4 covered by delegation** (rolling replacement
sequence enforced upstream; this operator adds the ordering constraint that source
workers are drained only after CP rollout completes).

### Step 4 — Verify AC3 invariant (etcd quorum: no destructive CP path in this repo)

Confirm the operator can never break quorum by verifying it has **no code path
that deletes, scales, or mutates control plane machines or the CPMS spec beyond
the FD/state update**:

1. Grep `ControlPlaneMachineSets(` across `internal/`: expect exactly two
   non-test call sites — the `Get` (machines.go ~line 215) and the single
   `Update` (~line 257) — plus one test `Get` (`machines_test.go` ~line 111).
   Expect **zero** `Delete` calls on CPMS in code. (The RBAC role grants `delete`
   on `controlplanemachinesets` per Step 2.4, but no code path exercises it —
   RBAC breadth is not a code path.)
2. Grep for control-plane Machine mutation: the only CP Machine operations are
   `ListControlPlaneMachines` (`machines.go` ~line 316, list-only) and
   `logControlPlaneMachine` (logging). All `Delete*`/`Scale*` MachineManager
   methods operate on `MachineSet` resources (workers) and never on control-plane
   Machines.
3. Verify the etcd/quorum claim via behavior anchors rather than literal string
   matches: `ensureReady` must call
   `OperatorManager.CheckAllOperatorsStable`, and readiness tests should include
   `etcd` in unstable/stable operator scenarios (for example in
   `internal/controller/ready_test.go` and `internal/openshift/operators_test.go`).
   This confirms the operator gates completion on cluster-operator health while
   quorum-safe replacement ordering remains delegated to the upstream CPMS
   operator.
4. Backstop: `ensureReady` (~line 907) requires **all** ClusterOperators,
   including `etcd`, to be Available/not Progressing/not Degraded
   (`OperatorManager.CheckAllOperatorsStable`) and only target vCenters present in
   Infrastructure before setting `ConditionReady` True.

Verdict if confirmed: **AC3 covered by delegation** — quorum-safe ordering is the
contract of the upstream CPMS operator; this operator never intervenes in the
rolling replacement and gates completion on the etcd ClusterOperator being
healthy.

### Step 5 — Verify AC5 anchors (status reflects CP rollout progress in the ordered workflow)

Read and confirm:

1. `conditionOrder` (~line 68–75) places `ConditionWorkloadMigrated` 4th of 6
   (after `ConditionInfrastructurePrepared`, `ConditionDestinationInitialized`,
   `ConditionMultiSiteConfigured`; before `ConditionSourceCleaned`,
   `ConditionReady`); `Reconcile` processes the first non-True condition only.
2. `ConditionWorkloadMigrated` messages track CP rollout progress through the
   exact literals asserted by Step 1 tests:
   `"Waiting for control plane rollout to start (CPMS generation %d/%d observed)"`,
   `"Control plane rolling out (%d/%d updated, %d/%d ready)"`,
   `"Workload migrated to target vCenter"`.
3. Events recorded: `CPMSUpdated`, `ControlPlaneRollout` (per 30s poll),
   `ControlPlaneRolledOut`, `WorkloadMigrated` (all via `r.Recorder`, visible with
   `kubectl describe`).
4. `updateStatus` (~line 1055) persists condition changes with optimistic
   concurrency, so progress survives concurrent reconciles.

Verdict if confirmed: **AC5 covered** — rollout progress is a first-class part of
the ordered workflow's status and events.

### Step 6 — Run the full suite

```
make test
```

This runs `manifests generate fmt vet setup-envtest` then the whole unit+
integration suite (envtest binaries are fetched automatically; e2e excluded).
Expect PASS. If envtest binary download fails in this environment (no network),
fall back to a non-e2e package run:

```
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" go test ./api/... ./cmd/... ./internal/... -v
```

and, if that is also impossible, run the Step 1 commands plus
`go test ./internal/openshift/ ./internal/vsphere/ ./internal/metadata/ -v`
and note the limitation in the report.

### Step 7 — Write the coverage report

Produce the final report in this exact shape (prose reply, no file needed):

```
AC1 control plane inputs → destination FDs: COVERED
  impl: <file:symbol anchors from Step 2>
  tests: <names from Step 1>, all passing
AC2 replacement CP nodes Ready on destination: COVERED
  impl: <anchors from Step 3>
  tests: <names>, all passing
AC3 etcd quorum maintained: COVERED (by delegation to upstream CPMS operator)
  invariant: <grep evidence from Step 4: no CP delete/scale path; etcd CO health gate>
AC4 source CP nodes replaced in controlled sequence: COVERED (by delegation + operator sequencing)
  impl: <anchors from Step 3>
  tests: <names>, all passing
AC5 status reflects CP rollout progress in ordered workflow: COVERED
  impl: <anchors from Step 5>
  tests: <names>, all passing
Full suite: make test → PASS (or fallback note)
```

## Critical files & anchors

- `internal/controller/vmwarecloudfoundationmigration_controller.go` — `conditionOrder` (68–75), `ensureMultiSiteConfigured` (443–543), `ensureWorkloadMigrated` (549–669, CPMS update at 654–668), `ensureWorkloadMigratedRolloutAndScaleDown` (677–793), `ensureReady` (907–970), `logControlPlaneMachine` (1131–1154).
- `internal/openshift/machines.go` — `UpdateCPMSFailureDomain` (230–263), `CheckControlPlaneRolloutStatus` (267–283), `IsCPMSUpdatedForFailureDomains` (288–312), `ListControlPlaneMachines` (316–329), `IsCPMSGenerationObserved` (334–343), `DeleteMachineSetsByVCenter` (174–210).
- `internal/openshift/infrastructure.go` — `AddTargetVCenter` (71–134).
- `internal/controller/workload_migration_rollout_test.go` — behavior tests + fixtures `newCPMSForRollout` (443), `newCPMSUpdatedForRollout` (486).
- `internal/openshift/machines_test.go` — `TestUpdateCPMSFailureDomain` (55), `TestCheckControlPlaneRolloutStatus` (142), `TestIsCPMSGenerationObserved` (209), `TestIsCPMSUpdatedForFailureDomains` (451), `TestListControlPlaneMachines` (549).

## Verification

- Step 1 both commands: every listed test passes (`ok` + `--- PASS` per test).
- Step 6: `make test` → `ok` for every package.
- Step 4 greps return exactly the call sites enumerated (no `Delete` on
  `ControlPlaneMachineSets`, no CP Machine mutation).
- Report (Step 7) contains all five ACs with verdicts and anchors; any AC whose
  checks failed is reported as NOT COVERED with the failing evidence.

## Assumptions & contingencies

- **AC3 standard (user-overridable):** "etcd quorum is maintained" is verified as
  delegation to the upstream OpenShift CPMS operator (one-at-a-time rolling
  replacement, source machine removed only after the replacement joins etcd),
  plus this operator's no-destructive-CP-path invariant and the etcd
  ClusterOperator health gate in `ensureReady`. Explicit in-operator quorum
  monitoring (e.g., watching etcd member counts) would be a new feature and is
  out of scope unless the user requests it.
- **CP rollout lives inside `ConditionWorkloadMigrated`** rather than a dedicated
  `ConditionControlPlaneMigrated`; AC5's "as part of the ordered workflow" is
  satisfied by the condition message/event progress tracking. A dedicated
  condition would be a design change, out of scope for a coverage check.
- **If any Step 1–6 check fails** (test failure, missing anchor, unexpected grep
  hit): that AC is NOT covered. Then implement or fix the minimum code to match
  the behavior described in this plan's Steps 2–5 (the described behavior is the
  intended contract; e.g., a missing event literal is restored to the exact
  string the tests assert), add/repair the failing test to the existing table
  style in the cited test files, and re-run Step 1 + Step 6. Do not redesign
  beyond restoring the described behavior.
- Line numbers are hints from the current tree; re-read around the cited symbols
  before relying on them.
