# Source MachineSet Coverage Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fail closed before worker scale-down when a source-vCenter MachineSet with running replicas is not covered by the failure domain mapping (no verified replacement), while skipping and logging zero-replica MachineSets.

**Architecture:** A pure gate function classifies the fresh source-vCenter MachineSet list (the same list Step 6 already fetches) against the recorded effective mappings and recorded destination MachineSet names. Step 6 in `ensureWorkloadMigratedRolloutAndScaleDown` calls the gate before scaling anything; an error stops the migration via the existing condition-failure path.

**Tech Stack:** Go, controller-runtime, machine API v1beta1 fakes (existing test patterns in `internal/controller/`).

**Spec:** This conversation (Slack thread + review analysis), invariant confirmed against revision `fc073254`: every source MachineSet selected for scale-down must have a corresponding replacement whose readiness has been verified — or migration must stop before scale-down.

## Global Constraints

- User caveats, verbatim: "a source machineset that is scaled to 0 prior to migration should be skipped and logged" and "a machineset that doesn't map to a failuredomain and scaled >0 should stop migration".
- Preserve intentional empty-FD skipping: a mapped source failure domain with zero MachineSets stays valid (gate passes vacuously for it).
- Zero-replica mapped MachineSets keep their existing behavior: creation copies replicas exactly (zero stays zero), no minimum-1 floor.
- Reuse the FD linkage heuristic (provider-spec workspace datacenter + resource pool) — do not invent a new matching rule.
- Error style: `fmt.Errorf` with `%w`, lowercase, gerund prefix (repo convention, AGENTS.md).
- Logging: `klog.FromContext(ctx)`, `log.V(1)` for the skip log, structured camelCase keys.
- Do not commit this plan file (user removed plan documents from the branch history).

## Review Focus

- Unparseable provider spec on a >0 source MachineSet: must error, not skip (both `GetMachineSetsByVCenter` and `GetMachineSetsByFailureDomain` currently `log.V(3)`-skip these).
- Nil replicas: MAPI defaults nil to 1, so nil must be treated as "has capacity" and fail closed, not skipped.
- MachineSet matching two source FDs (one FD with empty resource pool overlapping another FD's pool): must error even though creation's `claimedSources` check only sees per-FD lists at creation time.
- MachineSet created mid-migration (after `createMappedWorkerMachineSets`, before Step 6) on the source vCenter: the gate runs on the fresh Step 6 listing, so it must catch this — pin with a test that adds the MachineSet after creation.
- Zero-replica unmapped MachineSet: must be skipped with a log line, not an error, and must NOT be required to have a replacement.

---

### Task 1: `validateSourceMachineSetCoverage` gate function

**Files:**
- Modify: `internal/controller/failuredomains.go`
- Test: `internal/controller/review_regression_test.go`

**Interfaces:**
- Consumes: `findFailureDomainByName`, `recordedWorkerMachineSetNames(recorded, base)`, `openshift.ExtractVSphereProviderSpec`, `workerMachineSetName(infraID, fdName)` — all existing.
- Produces: `func validateSourceMachineSetCoverage(ctx context.Context, msList []*machinev1beta1.MachineSet, infraID string, mappings []migrationv1alpha1.FailureDomainMapping, sourceFDs []configv1.VSpherePlatformFailureDomainSpec, recorded []string) error`

Behavior (msList is the fresh source-vCenter listing the caller already has):
- For each `ms` in msList:
  - Extract provider spec; if extraction fails or `Workspace == nil` → return error (fail closed, name the MachineSet).
  - `hasReplicas := ms.Spec.Replicas == nil || *ms.Spec.Replicas > 0` (nil = MAPI default 1).
  - Count how many source FDs match `ms` (same predicate as `GetMachineSetsByFailureDomain`: `Workspace.Datacenter == fd.Topology.Datacenter` and, when `fd.Topology.ResourcePool != ""`, `Workspace.ResourcePool == fd.Topology.ResourcePool`).
  - If `!hasReplicas` → `log.V(1).Info("skipping zero-replica source MachineSet for coverage gate", "machineSet", ms.Name, "matchedFailureDomains", count)` and continue — regardless of mapping.
  - If no FD matches → return error naming the MachineSet and its datacenter/resource pool (this is the user's "stop migration" case).
  - If more than one FD matches → return error (ambiguous mapping).
  - Exactly one FD matches: look up the mapping whose `Source` is that FD's name; if none (FD not in mappings) → return error. Then require a recorded destination name for the mapping: `recordedWorkerMachineSetNames(recorded, workerMachineSetName(infraID, mapping.Destination))` must be non-empty → else return error (no verified replacement).
- Return nil when every eligible MachineSet passes.

- [ ] **Step 1: Write the failing tests** (table-driven, in `review_regression_test.go`, reuse `newSourceMachineSetForRollout`, `newSourceMachineSetForRolloutNamed`, `newInfrastructureForRollout` helpers from `workload_migration_rollout_test.go`; for an out-of-topology MachineSet build one via `newTargetMachineSetForRollout(name, "source.example.com", 1)` then override the provider-spec workspace to a different datacenter, mirroring `newSourceMachineSetForRolloutNamed`):

```go
name    string
ms      *machinev1beta1.MachineSet  // one per case
rec     []string                    // recorded workerMachineSetNames
wantErr string                      // "" means pass
```
Cases:
1. "mapped >0 with recorded replacement passes" — source MS in `source-fd-1`, `rec = {base, base+"-2"}`, `wantErr = ""`.
2. "mapped >0 without recorded replacement errors" — same MS, `rec = nil`, `wantErr = "no recorded destination"`.
3. "unmapped >0 stops migration" — MS datacenter `dc-other`, wantErr containing `does not match any source failure domain`.
4. "unmapped zero-replica skipped" — MS datacenter `dc-other`, replicas 0, wantErr `""`.
5. "mapped zero-replica skipped" — MS in `source-fd-1`, replicas 0, `rec = nil`, wantErr `""`.
6. "unparseable provider spec errors" — MS with `ProviderSpec.Value = nil`, replicas 1, wantErr containing provider spec extraction failure.
7. "nil replicas treated as >0 and stops migration" — unmapped MS with `Replicas = nil`, wantErr containing `does not match any source failure domain`.
8. "matching two failure domains errors" — source FDs where one has `ResourcePool: ""`; MS in that datacenter, wantErr containing `matches multiple source failure domains`.

Assert with `t.Fatalf("validateSourceMachineSetCoverage: %v", err)` style per repo conventions; skip-log cases just assert nil error (log content not asserted).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run TestValidateSourceMachineSetCoverage -v`
Expected: FAIL, `undefined: validateSourceMachineSetCoverage`

- [ ] **Step 3: Implement `validateSourceMachineSetCoverage` in `failuredomains.go`**

Per the Interfaces block. Extract the shared FD-match predicate as an unexported helper `matchesSourceFailureDomain(workspace *machinev1beta1.VSphereMachineProviderSpecWorkspace, fd configv1.VSpherePlatformFailureDomainSpec) bool` — the workspace field type is whatever `ExtractVSphereProviderSpec` returns; check the actual type in `internal/openshift/machines.go` before writing it (do not import machinev1beta1 provider-spec types by guess).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run TestValidateSourceMachineSetCoverage -v`
Expected: PASS, all 8 cases

- [ ] **Step 5: Commit**

```bash
git add internal/controller/failuredomains.go internal/controller/review_regression_test.go
git commit -m "Add source MachineSet coverage gate before scale-down"
```

---

### Task 2: Wire the gate into Step 6

**Files:**
- Modify: `internal/controller/vmwarecloudfoundationmigration_controller.go` (Step 6 block, ~line 931)
- Test: `internal/controller/workload_migration_rollout_test.go`

**Interfaces:**
- Consumes: `validateSourceMachineSetCoverage(ctx, msList, infraID, mappings, sourceFDs, recorded)` from Task 1. Inside `ensureWorkloadMigratedRolloutAndScaleDown`, `oldMachineSets` is the fresh listing; the mapping list is `migration.Status.FailureDomainMappings` (the recorded effective mapping — same source `targetMachineSetsExist` uses); source FDs come from the infrastructure failure-domain list already available on that path (pass the same `infraFDs` value the rollout function already resolves); `recorded` is `migration.Status.WorkerMachineSetNames`.
- Produces: none (behavioral change only).

- [ ] **Step 1: Write the failing test** in `workload_migration_rollout_test.go`, modeled on `TestReconcileWorkloadMigratedFailedWorkerMachine`: full migration state with earlier phases complete, an extra out-of-topology source-vCenter MachineSet at replicas 1 present in the fakes, and target workers ready (reuse the rollout fixtures). Assert `Reconcile` returns an error containing `does not match any source failure domain` AND the out-of-topology MachineSet was **not** scaled (its fake replicas still 1) AND condition `WorkloadMigrated` is `False`.

- [ ] **Step 2: Run test to verify it fails**

Run: `KUBEBUILDER_ASSETS="$(pwd)/bin/k8s/1.35.0-darwin-arm64" go test ./internal/controller/ -run TestScaleDownStopsOnUnmappedSourceMachineSet -v`
Expected: FAIL — currently the reconcile scales it down and proceeds without error.

- [ ] **Step 3: Implement the gate call**

In Step 6, immediately after `oldMachineSets` is fetched and before the scale loop:

```go
if err := validateSourceMachineSetCoverage(ctx, oldMachineSets, infraID, migration.Status.FailureDomainMappings, infraFDs, migration.Status.WorkerMachineSetNames); err != nil {
    return ctrl.Result{}, fmt.Errorf("validating source MachineSet coverage before scale-down: %w", err)
}
```
(Adapt variable names to what the enclosing function actually holds; the error propagates to the existing condition-failure path which sets `WorkloadMigrated=False/ReasonFailed` and emits `ConditionFailed` — no new event plumbing needed.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `KUBEBUILDER_ASSETS="$(pwd)/bin/k8s/1.35.0-darwin-arm64" go test ./internal/controller/ -v`
Expected: PASS — new test and all existing tests (the zero-replica and empty-FD cases must not regress).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/vmwarecloudfoundationmigration_controller.go internal/controller/workload_migration_rollout_test.go
git commit -m "Stop migration before worker scale-down on unmapped source MachineSets"
```

---

## Self-Review Notes

- Spec coverage: zero-replica skip+log (cases 4–5), unmapped >0 stop (cases 3, 7), verified-replacement requirement (case 2), fresh-listing recheck (Task 2 uses the live Step 6 list), unparseable spec fail-closed (case 6), multi-FD ambiguity (case 8). Empty-FD skip is preserved because the gate iterates MachineSets, not FDs.
- Not included (YAGNI, flag to the human if wanted): fixing the `log.V(3)`-skip of unparseable specs in `GetMachineSetsByVCenter`/`GetMachineSetsByFailureDomain` themselves — the gate fail-closes the destructive path, which is what the invariant requires.
- Mid-flight-created MachineSet after Step 6 has already scaled some MachineSets: the gate runs each reconcile before scaling, so a MachineSet created between requeues is caught on the next pass before any *further* scaling, but ones already scaled are gone. That matches the invariant's "stop before scale-down" strength as discussed.
