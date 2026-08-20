# SPLAT-2887: CPMS rollout logging and condition messages

## Context

SPLAT-2887 (Story, epic SPLAT-2644) improves logging and condition reporting for the
Control Plane Machine Set (CPMS) rollout phase of the vcf-migration-operator. Today the
`WorkloadMigrated` condition shows the rollout as
`reason=Progressing message=CPMS updated, control plane rolling out (4/4 ready)` and the
ticket requires:

1. Structured log entries with machine-level detail during the CPMS rollout (individual
   machine replacement status, phase transitions).
2. Condition messages that clearly distinguish: waiting for rollout to start, rollout in
   progress, rollout completing.
3. Timestamp/duration information to diagnose slow rollouts.
4. Enough detail to diagnose a stalled rollout without extra debugging.

End state: the two rollout-phase condition messages above are replaced with distinct
phase messages (exact strings below), machine-level detail is logged each rollout
reconcile, and `ensureWorkloadMigrated` routes to the rollout path from **cluster state**
instead of the current fragile condition-message string matching
(`vmwarecloudfoundationmigration_controller.go:501-507`, which would silently break
progression if the messages were merely reworded). No API, CRD, RBAC, or reason-enum
changes: machines `get;list;watch` RBAC already exists (controller line 86).

## Approach

Step 1 adds two MachineManager helpers (pure additions). Step 2 changes the controller
phase reporting and gate (includes one signature change; all callsites updated in the
same step). Step 3 adds controller tests. The tree compiles and existing tests pass after
each step.

### Step 1 — New MachineManager helpers

File: `internal/openshift/machines.go` (place next to `IsCPMSGenerationObserved`).

1. Add:

```go
// IsCPMSUpdatedForFailureDomains reports whether the ControlPlaneMachineSet's spec
// already targets the given failure domain names with state Active. It detects that
// the CPMS update step of workload migration has completed.
func (m *MachineManager) IsCPMSUpdatedForFailureDomains(ctx context.Context, failureDomainNames []string) (bool, error)
```

Body: `cpms, err := m.GetControlPlaneMachineSet(ctx)`; on err `return false, err`.
If `cpms.Spec.State != machinev1.ControlPlaneMachineSetStateActive` → `return false, nil`.
`tmpl := cpms.Spec.Template.OpenShiftMachineV1Beta1Machine`; if `tmpl == nil ||
tmpl.FailureDomains == nil` → `return false, nil`. Collect
`tmpl.FailureDomains.VSphere[i].Name` into `current []string`. Return true iff `current`
and `failureDomainNames` are equal ignoring order (copy both, `sort.Strings`, compare
with `reflect.DeepEqual`). Imports needed in machines.go: `reflect`, `sort` (check
existing imports first; machinev1 already imported as `machinev1`).

2. Add:

```go
// ListControlPlaneMachines lists Machines in openshift-machine-api carrying a
// control-plane role label (master or control-plane).
func (m *MachineManager) ListControlPlaneMachines(ctx context.Context) ([]*machinev1beta1.Machine, error)
```

Body: `m.machineClient.MachineV1beta1().Machines(MachineAPINamespace).List(ctx,
metav1.ListOptions{LabelSelector: "machine.openshift.io/cluster-api-machine-role in
(master,control-plane)"})`; wrap error `fmt.Errorf("listing control plane machines: %w",
err)`; return `&machines.Items[i]` pointers (same pattern as `CheckMachinesReady`).

3. Unit tests in `internal/openshift/machines_test.go`, table-driven like
`TestCheckControlPlaneRolloutStatus` (fake clientset via `fakemachineclient.NewClientset(...)`,
manager via `NewMachineManager(fakekube.NewClientset(), machineClient, nil)`):

- `TestIsCPMSUpdatedForFailureDomains` — build CPMS with the existing `newTestCPMS(state,
  fds)` helper; build `fds` as `&machinev1.FailureDomains{Platform: configv1.VSpherePlatformType,
  VSphere: []machinev1.VSphereFailureDomain{{Name: ...}}}`. Cases:
  - Active, FDs [a,b], want [a,b] → true
  - Active, FDs [a,b], want [b,a] → true (order-independent)
  - Active, FDs [a], want [b] → false
  - Inactive, FDs [a], want [a] → false
  - Active, nil FailureDomains, want [a] → false
  - no CPMS in clientset → error containing `getting ControlPlaneMachineSet`
- `TestListControlPlaneMachines` — seed four Machines in the fake clientset: `cp-1` with
  label `machine.openshift.io/cluster-api-machine-role: master`, `cp-2` with `control-plane`,
  `worker-1` with `worker`, `unlabeled` with no labels. Assert returned set is exactly
  {cp-1, cp-2}.

After this step: `go test ./internal/openshift/ -run 'TestIsCPMSUpdatedForFailureDomains|TestListControlPlaneMachines' -v`
passes and full `go test ./internal/openshift/` still passes.

### Step 2 — Controller phase reporting and cluster-state gate

File: `internal/controller/vmwarecloudfoundationmigration_controller.go` unless noted.
All edits below are in this one step (they share the signature change); update every
callsite before moving on so the tree compiles at the end.

1. **Signature change** — `internal/openshift/machines.go`, `IsCPMSGenerationObserved`
   (multi-value returns match the existing `CheckControlPlaneRolloutStatus` style):

```go
// IsCPMSGenerationObserved checks whether the ControlPlaneMachineSet's observed
// generation matches its metadata generation, indicating the controller has processed
// the latest spec change. It also returns both generation values for reporting.
func (m *MachineManager) IsCPMSGenerationObserved(ctx context.Context) (observed bool, generation, observedGeneration int64, err error)
```

Body: after the Get, set `generation = cpms.Generation`,
`observedGeneration = cpms.Status.ObservedGeneration`, return
`generation == observedGeneration, generation, observedGeneration, nil`.

Callsites (exactly two, grep `IsCPMSGenerationObserved`):
- `internal/openshift/machines_test.go:238` (`TestIsCPMSGenerationObserved`): update the
  call to `got, gen, obsGen, err := ...`; assert `got == tt.want` and that `gen`/`obsGen`
  equal the fixture values.
- controller line 619: rewritten in substep 3 below.

2. **Replace the message-string gate** in `ensureWorkloadMigrated` (lines 501–507).
   Delete the whole block:

```go
	// If we are past Step 3 (CPMS updated), run Steps 4–6 (rollout and scale-down) from cluster state.
	if c := apimeta.FindStatusCondition(migration.Status.Conditions, condType); c != nil {
		pastCPMSUpdate := strings.HasPrefix(c.Message, "CPMS updated") || strings.Contains(c.Message, "Control plane rollout") || strings.Contains(c.Message, "Old workers")
		if pastCPMSUpdate {
			return r.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration)
		}
	}
```

   Then, after `machineMgr := openshift.NewMachineManager(...)` (line 520), hoist
   `targetFDNames := failureDomainNames(migration.Spec.FailureDomains)` (it is currently
   computed at line 593 inside Step 3 — remove that local; keep the variable name), and
   insert the new gate directly after the Step-1 existence loop (after the loop that
   sets `allTargetMSExist`, before `if !allTargetMSExist {`):

```go
	// Once the target worker MachineSets exist and the CPMS already targets the target
	// failure domains, the CPMS update step is done: continue from the rollout and
	// scale-down path, derived entirely from cluster state.
	cpmsUpdated, err := machineMgr.IsCPMSUpdatedForFailureDomains(ctx, targetFDNames)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking CPMS update state: %w", err)
	}
	if allTargetMSExist && cpmsUpdated {
		return r.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration)
	}
```

   `err` is already in scope (line 511); `:=` is valid because `cpmsUpdated` is new.
   The `strings` and `apimeta` imports remain (used elsewhere in the file).

3. **Step 3 message** (line ~589-599). After `UpdateCPMSFailureDomain` succeeds, fetch
   the generations and set the new waiting message (keep the existing `CPMSUpdated`
   event unchanged):

```go
	_, generation, observedGeneration, err := machineMgr.IsCPMSGenerationObserved(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking CPMS generation: %w", err)
	}
	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
		fmt.Sprintf("Waiting for control plane rollout to start (CPMS generation %d/%d observed)", generation, observedGeneration))
```

4. **Phase A branch** in `ensureWorkloadMigratedRolloutAndScaleDown` (lines 618–627),
   the `if !observed` case:

```go
	observed, generation, observedGeneration, err := machineMgr.IsCPMSGenerationObserved(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking CPMS generation: %w", err)
	}
	if !observed {
		log.V(1).Info("CPMS generation not yet observed", "generation", generation, "observedGeneration", observedGeneration)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			fmt.Sprintf("Waiting for control plane rollout to start (CPMS generation %d/%d observed)", generation, observedGeneration))
		r.Recorder.Eventf(migration, "Normal", "ControlPlaneRollout", "waiting for rollout to start (CPMS generation %d/%d observed)", generation, observedGeneration)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
```

5. **Phase B branch** (lines 628–637), the `if !complete` case — updated counts in the
   message, a progress event, and machine-level detail:

```go
	if !complete {
		log.V(1).Info("control plane rollout in progress", "replicas", replicas, "updated", updated, "ready", ready)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			fmt.Sprintf("Control plane rolling out (%d/%d updated, %d/%d ready)", updated, replicas, ready, replicas))
		r.Recorder.Eventf(migration, "Normal", "ControlPlaneRollout", "control plane rolling out (%d/%d updated, %d/%d ready)", updated, replicas, ready, replicas)
		if machines, merr := machineMgr.ListControlPlaneMachines(ctx); merr != nil {
			log.V(2).Info("listing control plane machines failed", "err", merr)
		} else {
			for _, machine := range machines {
				logControlPlaneMachine(log, machine)
			}
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
```

   A machine-list failure is logged and skipped (not returned), per the codebase
   non-critical-error convention — the rollout counts are already captured from the
   CPMS status.

6. **Machine-level log helper** — add near `checkWorkerReadiness` at the end of the
   controller file:

```go
// logControlPlaneMachine logs the status of a single control plane Machine so that
// rollout progress and stalled machines can be diagnosed from operator logs.
func logControlPlaneMachine(log klog.Logger, machine *machinev1beta1.Machine) {
	phase := ""
	if machine.Status.Phase != nil {
		phase = *machine.Status.Phase
	}
	kv := []interface{}{
		"machine", machine.Name,
		"phase", phase,
		"age", time.Since(machine.CreationTimestamp.Time).Round(time.Second),
	}
	if machine.Status.LastUpdated != nil {
		kv = append(kv, "lastUpdated", machine.Status.LastUpdated.Time)
	}
	if machine.Status.ErrorReason != nil {
		kv = append(kv, "errorReason", string(*machine.Status.ErrorReason))
	}
	if machine.Status.ErrorMessage != nil {
		kv = append(kv, "errorMessage", *machine.Status.ErrorMessage)
	}
	log.V(1).Info("control plane machine status", kv...)
}
```

   Add `machinev1beta1 "github.com/openshift/api/machine/v1beta1"` to the controller's
   third-party import group (alphabetical, between `configclient` and `machineclient`
   lines — place after `configclient`... actual order: `configv1`,
   `configclient`, `machinev1beta1`, `machineclient` by package path).
   Duration support (ticket item 3): `age` is the machine's age since creation and
   `lastUpdated` is the machine-controller's last observation time; klog line
   timestamps plus these two fields cover slow/stalled diagnosis without stored state.

7. **Doc comment** for `ensureWorkloadMigratedRolloutAndScaleDown` (lines 602–607):
   replace the sentence "Call when condition message indicates we are past \"CPMS
   updated\" (e.g. \"Control plane rollout\" or \"Old workers\" or we have observed
   generation and rollout complete)." with "Called from ensureWorkloadMigrated when the
   target worker MachineSets exist and the CPMS already targets the target failure
   domains."

8. **Existing test update** — `internal/controller/workload_migration_rollout_test.go`,
   case "requeues while CPMS generation is not observed" (lines 50–52): change the
   assertion from `strings.Contains(cond.Message, "generation observed")` to an exact
   match `cond.Message != "Waiting for control plane rollout to start (CPMS generation
   2/1 observed)"` (fixture `newCPMSForRollout(false, true)` has Generation=2,
   ObservedGeneration=1).

Do not change the other condition messages ("Creating new worker MachineSets",
"Workers created, waiting for machines ready", "Old workers scaled down, waiting for
deletion", "Deleting source MachineSets", "Workload migrated to target vCenter") — the
ticket scope is the CPMS rollout phase.

After this step: `go build ./...` and `go test ./internal/...` pass (the one existing
rollout test case is updated in substep 8).

### Step 3 — New controller tests

File: `internal/controller/workload_migration_rollout_test.go` (add; reuse existing
fixtures and harness pattern).

1. Extend fixture `newInfrastructureForRollout` to also set
   `Status: configv1.InfrastructureStatus{InfrastructureName: "test-infra"}` (required by
   `GetInfrastructureID`; harmless to existing cases, which never read it).

2. Add fixture `newCPMSUpdatedForRollout(fdNames []string, observed bool) *machinev1.ControlPlaneMachineSet`:
   Name "cluster", Namespace `openshift.MachineAPINamespace`, Generation 2,
   `Spec.State: machinev1.ControlPlaneMachineSetStateActive`,
   `Spec.Replicas: &three`,
   `Spec.Template.OpenShiftMachineV1Beta1Machine.FailureDomains: &machinev1.FailureDomains{Platform: configv1.VSpherePlatformType, VSphere: <names>}`,
   Status Replicas/UpdatedReplicas/ReadyReplicas all 3,
   `Status.ObservedGeneration` = 2 if observed else 1.

3. New case in `TestEnsureWorkloadMigratedRolloutAndScaleDown`:

```
name:        "reports progress while control plane is rolling out"
objects:     [newInfrastructureForRollout("source.example.com"), newCPMSForRollout(true, false)]
wantRequeue: 30 * time.Second
```

   `newCPMSForRollout(true, false)` gives replicas=3, updated=1, ready=1, observed.
   Assertions: `cond.Status == metav1.ConditionFalse`,
   `cond.Message == "Control plane rolling out (1/3 updated, 1/3 ready)"`, and the last
   entry of `resultReconciler.Recorder.(*record.FakeRecorder).Events` contains
   `control plane rolling out (1/3 updated, 1/3 ready)`.

4. New test `TestEnsureWorkloadMigratedRolloutGate` — table-driven, calls
   `reconciler.ensureWorkloadMigrated(ctx, migration)`. Harness mirrors the existing
   loop (fakekube with empty `&corev1.NodeList{}`, configfake with the infra object,
   fakemachineclient with the rest, `record.NewFakeRecorder(20)`). Migration fixture
   (built inline per case):

```go
migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
	ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 1},
	Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
		FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
			Name:   "target-fd-1",
			Server: "target.example.com",
			Topology: configv1.VSpherePlatformFailureDomainTopologySpec{
				Template:       "/dc1/vm/target-template",
				Datacenter:     "dc1",
				Datastore:      "ds1",
				ResourcePool:   "rp1",
				Cluster:        "cl1",
				ComputeCluster: "cl1",
			},
		}},
	},
}
```

   (The target worker MachineSet name is `workerMachineSetName("test-infra",
   "target-fd-1")` = `test-infra-worker-target-fd-1`.)

   Cases:
   - "routes to rollout path when CPMS targets failure domains and workers exist":
     objects = infra, `newSourceMachineSetForRollout("test-infra-worker-target-fd-1",
     "target.example.com", 1)`, `newCPMSUpdatedForRollout([]string{"target-fd-1"}, false)`,
     `newSourceMachineSetForRollout("source-worker-a", "source.example.com", 1)`.
     Expect requeue 15s and
     `cond.Message == "Waiting for control plane rollout to start (CPMS generation 2/1 observed)"`.
     (Under the old code this scenario would produce "Workers created, waiting for
     machines ready" because the fake NodeList is empty — so the exact message proves
     routing and that Step 3 is not re-run.)
   - "stays in worker phase when CPMS not updated": same objects but
     `newCPMSUpdatedForRollout([]string{"source-fd-1"}, false)`. Expect requeue 30s and
     `cond.Message == "Workers created, waiting for machines ready"`.
   - "stays in worker phase when target machinesets missing": objects = infra,
     `newCPMSUpdatedForRollout([]string{"target-fd-1"}, false)`,
     `newSourceMachineSetForRollout("source-worker-a", "source.example.com", 2)` (no
     target MS). Expect requeue 30s,
     `cond.Message == "Workers created, waiting for machines ready"`, and
     `GetMachineSet("test-infra-worker-target-fd-1")` to succeed (the create path ran).

5. New test `TestRolloutLogsMachineLevelDetail` — captures klog output to prove
   machine-level structured entries (ticket AC 1):

```go
	var buf bytes.Buffer
	klog.SetOutput(&buf)
	defer klog.SetOutput(nil)
```

   Objects: infra, `newCPMSUpdatedForRollout([]string{"target-fd-1"}, true)` with
   `Status.UpdatedReplicas = 1; Status.ReadyReplicas = 1` (observed, not complete →
   Phase B branch), and two Machines in the fake clientset:
   - `cp-1`: label role `master`, `Status.Phase = pointer to "Running"`, NodeRef set.
   - `cp-2`: label role `master`, `Status.Phase = pointer to "Provisioning"`,
     `Status.ErrorReason = pointer to machinev1beta1.CreateMachineError`,
     `Status.ErrorMessage = pointer to "vm creation timed out"`,
     `CreationTimestamp` = now minus 2 minutes, `Status.LastUpdated` = now minus 1 minute.

   Call `ensureWorkloadMigratedRolloutAndScaleDown`, `klog.Flush()`, then assert
   `buf.String()` contains each of: `control plane machine status`, `cp-1`, `Running`,
   `cp-2`, `Provisioning`, `CreateError`, `vm creation timed out`. Also assert requeue
   30s. Tests in this package do not run in parallel (no `t.Parallel`), so the global
   klog output swap is safe. If the vendored klog rejects `SetOutput(nil)`, use
   `io.Discard` in the defer instead.

After this step: `go test ./internal/controller/ -run 'TestEnsureWorkloadMigrated' -v`
and `go test ./internal/...` pass.

## Critical files & anchors

- `internal/controller/vmwarecloudfoundationmigration_controller.go` — the load-bearing
  file: gate at lines 501–507 (delete), Step 3 at 589–599, rollout function at
  608–714 (Phase A 618–627, Phase B 628–637), helper near `checkWorkerReadiness` (976).
- `internal/openshift/machines.go` — new methods next to `IsCPMSGenerationObserved`
  (286); signature change there.
- `internal/controller/workload_migration_rollout_test.go` — fixture
  `newCPMSForRollout` (209) shows the CPMS shape; harness loop (127–179) to mirror for
  the gate test.
- `internal/openshift/machines_test.go` — `newTestCPMS` (22) and
  `TestCheckControlPlaneRolloutStatus` (142) are the style/fixtures to copy.
- `vendor/github.com/openshift/api/machine/v1beta1/types_machine.go` — `MachineStatus`
  (333): `Phase *string` (Failed/Provisioning/Provisioned/Running/Deleting),
  `ErrorReason *MachineStatusError`, `ErrorMessage *string`, `LastUpdated *metav1.Time`.

## Verification

Working directory: repo root. Run after each step as noted; final gate:

1. `go build ./...`
2. `go test ./internal/openshift/ -run 'TestIsCPMSUpdatedForFailureDomains|TestListControlPlaneMachines|TestIsCPMSGenerationObserved' -v`
3. `go test ./internal/controller/ -run 'TestEnsureWorkloadMigrated' -v` — covers the
   updated message case, new Phase-B progress case, both gate-routing cases, and the
   log-capture test.
4. `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" make test` — full unit +
   envtest suite.
5. `make lint` (golangci-lint v2; formatters gofmt/goimports included).

Concrete new-behavior checks (each encoded as a test above):
- CPMS observed with replicas=3, updated=1, ready=1 → condition message exactly
  `Control plane rolling out (1/3 updated, 1/3 ready)`, requeue 30s, and a
  `Normal/ControlPlaneRollout` event with the same counts (previously the message was
  `CPMS updated, control plane rolling out (1/3 ready)` with no updated count).
- CPMS generation 2 / observed 1 → condition message exactly `Waiting for control plane
  rollout to start (CPMS generation 2/1 observed)`, requeue 15s.
- CPMS Active targeting `target-fd-1` + target worker MachineSet present →
  `ensureWorkloadMigrated` routes to the rollout path (message above) instead of
  re-running worker creation; CPMS targeting a different name or missing target MS →
  stays in the worker phase.
- Operator log at V(1) contains one `control plane machine status` entry per control
  plane Machine with `machine`, `phase`, `age`, and (when set) `lastUpdated`,
  `errorReason`, `errorMessage` — asserted via klog output capture.

## Assumptions & contingencies

- Worker-creation and scale-down phase messages are left unchanged; the ticket targets
  the CPMS rollout phase. If the reviewer wants them reworded too, that is a follow-up.
- Control plane machines carry role label `master` or `control-plane`; the selector
  matches both (this codebase's CPMS template uses `master`).
- The gate requires `allTargetMSExist && cpmsUpdated`. If a future migration reuses
  source failure-domain names verbatim (so CPMS FDs already match before Step 3), the
  rollout path runs early — same degenerate behavior the old message gate had after the
  first "CPMS updated" message; acceptable, noted here so it is a conscious choice.
- If `newInfrastructureForRollout`'s added `Status.InfrastructureName` ever breaks an
  existing case (it shouldn't — the rollout path never reads it), revert that fixture
  and build the Infrastructure inline in `TestEnsureWorkloadMigratedRolloutGate` instead.
- Save this plan document to `docs/plans/cpms-rollout-logging-plan.md` (user-requested
  location) as part of execution.
