# SPLAT-2655 Worker Node Rolling Replacement (IPI) — Gap Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the remaining gaps for SPLAT-2655: report worker rollout progress counts in the `WorkloadMigrated` condition and surface stalled old-worker deletion (PDB-blocked drains) with per-machine detail and a debounced Warning event.

**Architecture:** The existing ordered `ensureWorkloadMigrated` / `ensureWorkloadMigratedRolloutAndScaleDown` flow is kept unchanged in structure. We (1) add a metadata-preserving machine list method to `MachineManager`, (2) make `checkWorkerReadiness` return aggregate counts that feed a progress message, (3) build per-MachineSet stall detail for the old-worker deletion wait, and (4) emit a debounced `Warning OldWorkersStalled` event. No CRD, RBAC, config, or workflow-ordering changes.

**Tech Stack:** Go, controller-runtime, OpenShift machine-api client, klog/v2, standard `testing.T` with fake clientsets, Ginkgo/Gomega envtest (existing suites).

**Spec:** Approved design from the brainstorming session for SPLAT-2655 (Jira: https://redhat.atlassian.net/browse/SPLAT-2655); scope = "close the gaps" + "details + warning events" stall surfacing. Acceptance criteria reference: target MS from destination failure domains, target Ready before source scale-down, drain respects PDBs, source removed after destination healthy, status reflects worker rollout progress.

## Global Constraints

- Work on branch `jcpowermac/SPLAT-2655` off latest `main` (repo convention; see `git log`).
- Imports: three groups (stdlib / third-party / project-internal), alphabetical. Standard aliases: `apierrors`, `metav1`, `corev1`, `ctrl`, `machinev1beta1`.
- Errors: `fmt.Errorf` with `%w`, lowercase gerund start, contextual identifiers. No `errors.New()`.
- Logging: `klog.FromContext(ctx)`; V(1) for operational, V(2) for detail. Structured key-value with camelCase keys.
- Requeue intervals: 30s for machine readiness/deletion waits, 15s for generation/CPMS waits (do not change existing values).
- Non-critical/observational work must not change reconcile error behavior — this plan adds messages and events only, no new failure paths.
- Linters: `make lint` must pass (golangci-lint v2; `dupl`, `lll`, `goconst` are enabled — keep new code below 120 cols and avoid duplicated blocks).
- Commit style: `SPLAT-2655: <imperative summary>`, one commit per task's final step.

---

### Task 1: `ListMachinesForMachineSet` in `MachineManager`

**Files:**
- Modify: `internal/openshift/machines.go` (append after `CheckNodesDeletedForMachines`, which ends around line 575)
- Test: `internal/openshift/machines_test.go`

**Interfaces:**
- Consumes: existing `machinesetSelectorLabel(ctx, machineSetName) (string, error)` and `MachineAPINamespace` (both already in `machines.go`).
- Produces: `(m *MachineManager) ListMachinesForMachineSet(ctx context.Context, machineSetName string) ([]*machinev1beta1.Machine, error)` — used by Tasks 3 and 4.

- [ ] **Step 1: Write the failing test**

Append to `internal/openshift/machines_test.go`:

```go
func TestListMachinesForMachineSet(t *testing.T) {
	tests := []struct {
		name        string
		machineSets []*machinev1beta1.MachineSet
		machines    []*machinev1beta1.Machine
		lookupName  string
		wantNames   []string
	}{
		{
			name:       "returns machines selected by the machineset selector",
			lookupName: "workers-a",
			machineSets: []*machinev1beta1.MachineSet{
				testSelectorMachineSet("workers-a"),
			},
			machines: []*machinev1beta1.Machine{
				testMachineWithMSLabel("worker-a-0", "workers-a"),
				testMachineWithMSLabel("worker-b-0", "workers-b"),
			},
			wantNames: []string{"worker-a-0"},
		},
		{
			name:       "falls back to machineset name when selector label is absent",
			lookupName: "legacy-workers",
			machineSets: []*machinev1beta1.MachineSet{
				newTestMachineSet("legacy-workers", 1),
			},
			machines: []*machinev1beta1.Machine{
				testMachineWithMSLabel("legacy-worker-0", "legacy-workers"),
			},
			wantNames: []string{"legacy-worker-0"},
		},
		{
			name:       "returns an empty list when no machines match",
			lookupName: "workers-empty",
			machineSets: []*machinev1beta1.MachineSet{
				newTestMachineSet("workers-empty", 0),
			},
			wantNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.machineSets)+len(tt.machines))
			for _, ms := range tt.machineSets {
				objects = append(objects, ms)
			}
			for _, m := range tt.machines {
				objects = append(objects, m)
			}
			machineClient := fakemachineclient.NewClientset(objects...)
			mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			got, err := mgr.ListMachinesForMachineSet(context.Background(), tt.lookupName)
			if err != nil {
				t.Fatalf("ListMachinesForMachineSet: %v", err)
			}

			names := make([]string, 0, len(got))
			for _, m := range got {
				names = append(names, m.Name)
			}
			if len(names) != len(tt.wantNames) {
				t.Fatalf("machine names = %v, want %v", names, tt.wantNames)
			}
			for i := range names {
				if names[i] != tt.wantNames[i] {
					t.Fatalf("machine names = %v, want %v", names, tt.wantNames)
				}
			}
		})
	}
}

// testSelectorMachineSet creates a MachineSet whose selector carries the
// cluster-api-machineset label, matching how real worker MachineSets are labeled.
func testSelectorMachineSet(name string) *machinev1beta1.MachineSet {
	replicas := int32(1)
	return &machinev1beta1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: MachineAPINamespace},
		Spec: machinev1beta1.MachineSetSpec{
			Replicas: &replicas,
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"machine.openshift.io/cluster-api-machineset": name,
				},
			},
		},
	}
}

// testMachineWithMSLabel creates a Machine carrying the cluster-api-machineset
// label with the given value.
func testMachineWithMSLabel(name, labelValue string) *machinev1beta1.Machine {
	return &machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: MachineAPINamespace,
			Labels: map[string]string{
				"machine.openshift.io/cluster-api-machineset": labelValue,
			},
		},
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/openshift/ -run TestListMachinesForMachineSet -v`
Expected: build failure, `mgr.ListMachinesForMachineSet undefined`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/openshift/machines.go` after `CheckNodesDeletedForMachines`:

```go
// ListMachinesForMachineSet lists the full Machine objects (metadata and status)
// selected by the given MachineSet's selector. Callers that need per-machine
// detail (names, ages, error reasons) use this instead of CheckMachinesDeleted.
func (m *MachineManager) ListMachinesForMachineSet(ctx context.Context, machineSetName string) ([]*machinev1beta1.Machine, error) {
	labelValue, err := m.machinesetSelectorLabel(ctx, machineSetName)
	if err != nil {
		return nil, err
	}
	machines, err := m.machineClient.MachineV1beta1().Machines(MachineAPINamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("machine.openshift.io/cluster-api-machineset=%s", labelValue),
	})
	if err != nil {
		return nil, fmt.Errorf("listing machines for machineset %q: %w", machineSetName, err)
	}

	result := make([]*machinev1beta1.Machine, len(machines.Items))
	for i := range machines.Items {
		result[i] = &machines.Items[i]
	}
	return result, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/openshift/ -run TestListMachinesForMachineSet -v`
Expected: PASS (3 subtests). Then run the full package: `go test ./internal/openshift/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/openshift/machines.go internal/openshift/machines_test.go
git commit -m "SPLAT-2655: List machines for a MachineSet with full metadata"
```

---

### Task 2: Worker readiness progress counts in `WorkloadMigrated` status

**Files:**
- Modify: `internal/controller/vmwarecloudfoundationmigration_controller.go` — `checkWorkerReadiness` (ends near line 1130) and its call site in `ensureWorkloadMigrated` (Step 2 block, ~line 560).
- Test: `internal/controller/workload_migration_rollout_test.go` — `TestEnsureWorkloadMigratedRolloutGate`.

**Interfaces:**
- Consumes: `MachineManager.CheckMachinesReady` / `CheckNodesReady` (existing, unchanged).
- Produces: `checkWorkerReadiness(ctx, machineMgr, fds, infraID) (ready bool, machinesReady, machinesTotal, nodesReady, nodesTotal int32, err error)` — internal to the controller package.

- [ ] **Step 1: Update the existing exact-message test case and add a new counts case (failing)**

In `TestEnsureWorkloadMigratedRolloutGate` (`internal/controller/workload_migration_rollout_test.go`):

1. Add near the top of the function, after `targetMSName := ...`:

```go
	provisioning := "Provisioning"
```

2. Change the `wantMessage` of the case named `"stays in worker phase when CPMS not updated"` from `"Workers created, waiting for machines ready"` to:

```go
			wantMessage: "Waiting for target workers (machines 0/0 ready, nodes 0/0 ready)",
```

(The other case, `"stays in worker phase when target machinesets missing"`, hits the creation path in Step 1 and keeps its existing message — do not change it.)

3. Insert a new case after that one:

```go
		{
			name: "reports worker readiness counts while target workers are pending",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(targetMSName, "target.example.com", 2),
				newReadyWorkerMachineForRollout(targetMSName, "worker-node-1"),
				newReadyNodeForRollout("worker-node-1"),
				&machinev1beta1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      targetMSName + "-pending",
						Namespace: openshift.MachineAPINamespace,
						Labels: map[string]string{
							"machine.openshift.io/cluster-api-machineset": targetMSName,
						},
					},
					Status: machinev1beta1.MachineStatus{Phase: &provisioning},
				},
				newCPMSUpdatedForRollout([]string{"target-fd-1"}, false),
				newSourceMachineSetForRollout(1),
			},
			wantRequeue: 30 * time.Second,
			wantMessage: "Waiting for target workers (machines 1/2 ready, nodes 1/2 ready)",
		},
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestEnsureWorkloadMigratedRolloutGate -v`
Expected: FAIL — the two target cases still see the old static message `Workers created, waiting for machines ready`.

- [ ] **Step 3: Implement aggregate counts**

In `internal/controller/vmwarecloudfoundationmigration_controller.go`, replace the whole `checkWorkerReadiness` function with:

```go
// checkWorkerReadiness verifies that all machines and nodes for the target worker
// MachineSets are in a ready state. It returns whether everything is ready plus
// the aggregate machine and node readiness counts across all target MachineSets,
// which callers use to report rollout progress.
func checkWorkerReadiness(ctx context.Context, machineMgr *openshift.MachineManager, fds []configv1.VSpherePlatformFailureDomainSpec, infraID string) (ready bool, machinesReady, machinesTotal, nodesReady, nodesTotal int32, err error) {
	log := klog.FromContext(ctx)
	allReady := true
	for i := range fds {
		msName := workerMachineSetName(infraID, fds[i].Name)
		machinesOK, machineReadyCount, machineTotalCount, merr := machineMgr.CheckMachinesReady(ctx, msName)
		if merr != nil {
			return false, 0, 0, 0, 0, fmt.Errorf("checking machines for %q: %w", msName, merr)
		}
		machinesReady += machineReadyCount
		machinesTotal += machineTotalCount
		if !machinesOK {
			log.V(1).Info("machines not ready", "machineSet", msName, "ready", machineReadyCount, "total", machineTotalCount)
			allReady = false
		}
		nodesOK, nodeReadyCount, nodeTotalCount, nerr := machineMgr.CheckNodesReady(ctx, msName)
		if nerr != nil {
			return false, 0, 0, 0, 0, fmt.Errorf("checking nodes for %q: %w", msName, nerr)
		}
		nodesReady += nodeReadyCount
		nodesTotal += nodeTotalCount
		if !nodesOK {
			log.V(1).Info("nodes not ready", "machineSet", msName, "ready", nodeReadyCount, "total", nodeTotalCount)
			allReady = false
		}
	}
	return allReady, machinesReady, machinesTotal, nodesReady, nodesTotal, nil
}
```

In `ensureWorkloadMigrated`, replace the Step 2 call site:

```go
	// Step 2: Wait for target worker machines and nodes to be ready (cluster state).
	allReady, err := checkWorkerReadiness(ctx, machineMgr, migration.Spec.FailureDomains, infraID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !allReady {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Workers created, waiting for machines ready")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
```

with:

```go
	// Step 2: Wait for target worker machines and nodes to be ready (cluster state).
	allReady, machinesReady, machinesTotal, nodesReady, nodesTotal, err := checkWorkerReadiness(ctx, machineMgr, migration.Spec.FailureDomains, infraID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !allReady {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			fmt.Sprintf("Waiting for target workers (machines %d/%d ready, nodes %d/%d ready)", machinesReady, machinesTotal, nodesReady, nodesTotal))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run TestEnsureWorkloadMigratedRolloutGate -v`
Expected: PASS (4 cases). Then: `go test ./internal/controller/ ./internal/openshift/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/vmwarecloudfoundationmigration_controller.go internal/controller/workload_migration_rollout_test.go
git commit -m "SPLAT-2655: Report worker readiness counts in WorkloadMigrated status"
```

---

### Task 3: Old-worker deletion stall detail in the condition message

**Files:**
- Modify: `internal/controller/vmwarecloudfoundationmigration_controller.go` — Step 7 block in `ensureWorkloadMigratedRolloutAndScaleDown` (~line 745), `logControlPlaneMachine` (rename), and a new helper `oldWorkerStallDetail`. Add `"sort"` to the stdlib import group.
- Test: `internal/controller/workload_migration_rollout_test.go` — `TestEnsureWorkloadMigratedRolloutAndScaleDown` (new table case), `TestRolloutLogsMachineLevelDetail` (log-line assertion).

**Interfaces:**
- Consumes: `MachineManager.ListMachinesForMachineSet` (Task 1), `MachineManager.CheckNodesDeletedForMachines` (existing).
- Produces: `oldWorkerStallDetail(ctx context.Context, log klog.Logger, machineMgr *openshift.MachineManager, machineSets []*machinev1beta1.MachineSet) (detail string, key string, err error)` — Task 4 consumes `key` for event debouncing.

Message format (exact — tests assert on it):

- Condition message: `Waiting for old worker deletion:` + parts joined by `"; "`
- Per MachineSet part: `<msName> has <N> machine(s) [<names>] and <M> node(s) remaining`
- Per machine entry: `<machineName> (<age> old)` or `<machineName> (<age> old, <ErrorReason>)`

- [ ] **Step 1: Write the failing test**

In `TestEnsureWorkloadMigratedRolloutAndScaleDown` (`internal/controller/workload_migration_rollout_test.go`):

1. Add after `ctx := context.Background()`:

```go
	now := time.Now()
	running := "Running"
	// NOTE: pinned openshift/api has no DrainError constant; the machine controller
	// emits "DrainError" as errorReason for drain failures, so synthesize the value.
	drainErr := machinev1beta1.MachineStatusError("DrainError")
	drainMsg := "unable to drain node: pod disruption budget exceeded"
```

2. Insert a new table case before the closing `}` of the `tests` slice (after the `"completes workload migration and deletes zero-replica source machinesets"` case):

```go
		{
			name: "reports old worker deletion stall detail with machine names and error reasons",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newCPMSForRollout(true, true),
				newSourceMachineSetForRollout(0),
				&machinev1beta1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "source-worker-a-0",
						Namespace:         openshift.MachineAPINamespace,
						CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
						Labels: map[string]string{
							"machine.openshift.io/cluster-api-machineset": "source-worker-a",
						},
					},
					Status: machinev1beta1.MachineStatus{
						Phase:        &running,
						NodeRef:      &corev1.ObjectReference{Name: "source-node-a"},
						ErrorReason:  &drainErr,
						ErrorMessage: &drainMsg,
					},
				},
				newReadyNodeForRollout("source-node-a"),
			},
			wantRequeue: 30 * time.Second,
			assertions: func(t *testing.T, _ *VmwareCloudFoundationMigrationReconciler, resultMigration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				cond := findWorkloadCondition(t, resultMigration)
				if cond.Status != metav1.ConditionFalse {
					t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
				}
				// Assert on stable substrings; the machine age is wall-clock derived
				// and would make an exact match flaky.
				for _, want := range []string{
					"Waiting for old worker deletion: source-worker-a has 1 machine(s) [source-worker-a-0 (",
					" old, DrainError)] and 1 node(s) remaining",
				} {
					if !strings.Contains(cond.Message, want) {
						t.Fatalf("workload condition message %q missing substring %q", cond.Message, want)
					}
				}
			},
		},
```

3. In `TestRolloutLogsMachineLevelDetail`, change the first entry of the wanted substrings slice from `"control plane machine status"` to `"machine status"` (Task 3 renames the logger line; the assertion must move with it).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run 'TestEnsureWorkloadMigratedRolloutAndScaleDown|TestRolloutLogsMachineLevelDetail' -v`
Expected: FAIL — the new case sees `"Old workers scaled down, waiting for deletion"` and the log test still passes (rename not done yet; if it passes before the rename, that is fine — it pins the new name once Step 3 lands).

- [ ] **Step 3: Implement stall detail**

In `internal/controller/vmwarecloudfoundationmigration_controller.go`:

1. Add `"sort"` to the stdlib import group.

2. Rename `logControlPlaneMachine` to `logMachineDetail` and change its log line from `"control plane machine status"` to `"machine status"`. Update the existing call site in Step 5 of `ensureWorkloadMigratedRolloutAndScaleDown` (`logControlPlaneMachine(log, machine)` → `logMachineDetail(log, machine)`). Update the comment above the function to:

```go
// logMachineDetail logs the status of a single Machine so that rollout progress
// and stalled machines can be diagnosed from operator logs.
```

3. Replace the Step 7 wait block:

```go
	if !allDeleted {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Old workers scaled down, waiting for deletion")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
```

with:

```go
	if !allDeleted {
		detail, _, err := oldWorkerStallDetail(ctx, log, machineMgr, oldMachineSets)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("building old worker stall detail: %w", err)
		}
		log.V(1).Info("old worker deletion in progress", "detail", detail)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			"Waiting for old worker deletion: "+detail)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
```

4. Append the new helper (next to `checkWorkerReadiness` at the bottom of the file):

```go
// oldWorkerStallDetail formats per-MachineSet detail for old worker machines and
// nodes that are still being deleted (typically a PDB blocking node drain):
// machine names with age and error reason, plus the remaining node count. It also
// returns a stable key (machine set name plus sorted machine names) identifying
// the exact set of lingering machines, used to debounce Warning events.
func oldWorkerStallDetail(ctx context.Context, log klog.Logger, machineMgr *openshift.MachineManager, machineSets []*machinev1beta1.MachineSet) (string, string, error) {
	var parts, keyParts []string
	for _, ms := range machineSets {
		machines, err := machineMgr.ListMachinesForMachineSet(ctx, ms.Name)
		if err != nil {
			return "", "", err
		}
		if len(machines) == 0 {
			continue
		}
		names := make([]string, 0, len(machines))
		keyNames := make([]string, 0, len(machines))
		for _, machine := range machines {
			logMachineDetail(log, machine)
			age := time.Since(machine.CreationTimestamp.Time).Round(time.Second)
			entry := fmt.Sprintf("%s (%s old", machine.Name, age)
			if machine.Status.ErrorReason != nil {
				entry += fmt.Sprintf(", %s", *machine.Status.ErrorReason)
			}
			entry += ")"
			names = append(names, entry)
			keyNames = append(keyNames, machine.Name)
		}
		_, nodeRemaining, err := machineMgr.CheckNodesDeletedForMachines(ctx, ms.Name)
		if err != nil {
			return "", "", err
		}
		parts = append(parts, fmt.Sprintf("%s has %d machine(s) [%s] and %d node(s) remaining", ms.Name, len(machines), strings.Join(names, ", "), nodeRemaining))
		sort.Strings(keyNames)
		keyParts = append(keyParts, ms.Name+"="+strings.Join(keyNames, ","))
	}
	sort.Strings(keyParts)
	return strings.Join(parts, "; "), strings.Join(keyParts, "|"), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -v`
Expected: PASS, including the new stall-detail case and the renamed log assertion.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/vmwarecloudfoundationmigration_controller.go internal/controller/workload_migration_rollout_test.go
git commit -m "SPLAT-2655: Surface old worker deletion stall detail in status"
```

---

### Task 4: Debounced `OldWorkersStalled` Warning event

**Files:**
- Modify: `internal/controller/vmwarecloudfoundationmigration_controller.go` — `VmwareCloudFoundationMigrationReconciler` struct (~line 52), a new const near `reasonWaitingForVSpherePods`, and the Step 7 wait block (just landed in Task 3).
- Test: `internal/controller/workload_migration_rollout_test.go` — new `TestOldWorkersStallEventDebounce`.

**Interfaces:**
- Consumes: `oldWorkerStallDetail` (Task 3) returning `(detail, key string, err error)`.
- Produces: unexported reconciler fields `lastStallEventKey string` / `lastStallEventTime time.Time` and `const stallEventInterval = 5 * time.Minute`. No public API changes.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/workload_migration_rollout_test.go`:

```go
func TestOldWorkersStallEventDebounce(t *testing.T) {
	ctx := context.Background()
	running := "Running"

	newObjects := func(machineName string) []runtime.Object {
		return []runtime.Object{
			newInfrastructureForRollout(),
			newCPMSForRollout(true, true),
			newSourceMachineSetForRollout(0),
			&machinev1beta1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machineName,
					Namespace: openshift.MachineAPINamespace,
					Labels: map[string]string{
						"machine.openshift.io/cluster-api-machineset": "source-worker-a",
					},
				},
				Status: machinev1beta1.MachineStatus{
					Phase:   &running,
					NodeRef: &corev1.ObjectReference{Name: machineName + "-node"},
				},
			},
			newReadyNodeForRollout(machineName + "-node"),
		}
	}

	reconciler := newRolloutReconciler(t, newObjects("source-worker-a-0"))
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 1},
	}

	// stalledEvents drains the recorder channel, so accumulate across reconciles.
	var stalled []string

	// First stalled reconcile emits exactly one Warning event.
	if _, err := reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration); err != nil {
		t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown: %v", err)
	}
	stalled = append(stalled, stalledEvents(t, reconciler)...)
	if len(stalled) != 1 {
		t.Fatalf("stalled events after first reconcile = %v, want exactly 1", stalled)
	}

	// Second reconcile with the same lingering machines emits no new event.
	if _, err := reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration); err != nil {
		t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown: %v", err)
	}
	stalled = append(stalled, stalledEvents(t, reconciler)...)
	if len(stalled) != 1 {
		t.Fatalf("stalled events after repeat reconcile = %v, want still 1 (debounced)", stalled)
	}

	// A different lingering machine set emits a new event even within the window.
	kubeObjects, configObjects, machineObjects := splitRolloutTestObjects(t, newObjects("source-worker-a-1"))
	reconciler.KubeClient = fakekube.NewClientset(kubeObjects...)
	reconciler.ConfigClient = configfake.NewClientset(configObjects...)
	reconciler.MachineClient = fakemachineclient.NewClientset(machineObjects...)
	if _, err := reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration); err != nil {
		t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown: %v", err)
	}
	stalled = append(stalled, stalledEvents(t, reconciler)...)
	if len(stalled) != 2 {
		t.Fatalf("stalled events after changed machine set = %v, want 2", stalled)
	}
}

// stalledEvents drains the recorder and returns events carrying the
// OldWorkersStalled Warning reason.
func stalledEvents(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler) []string {
	t.Helper()
	recorder := reconciler.Recorder.(*record.FakeRecorder)
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "OldWorkersStalled") {
				events = append(events, event)
			}
		default:
			return events
		}
	}
}

// newRolloutReconciler builds a reconciler wired to fake clients populated with
// the given rollout objects.
func newRolloutReconciler(t *testing.T, objects []runtime.Object) *VmwareCloudFoundationMigrationReconciler {
	t.Helper()
	kubeObjects, configObjects, machineObjects := splitRolloutTestObjects(t, objects)
	return &VmwareCloudFoundationMigrationReconciler{
		KubeClient:    fakekube.NewClientset(kubeObjects...),
		ConfigClient:  configfake.NewClientset(configObjects...),
		MachineClient: fakemachineclient.NewClientset(machineObjects...),
		Recorder:      record.NewFakeRecorder(20),
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestOldWorkersStallEventDebounce -v`
Expected: FAIL — no event is recorded, `stalled events after first reconcile = [], want exactly 1`.

- [ ] **Step 3: Implement debounced event**

In `internal/controller/vmwarecloudfoundationmigration_controller.go`:

1. Add to the `VmwareCloudFoundationMigrationReconciler` struct, after `Recorder`:

```go
	// lastStallEventKey identifies the set of old worker machines described by the
	// most recent OldWorkersStalled Warning event; lastStallEventTime is when that
	// event was recorded. Together they debounce the event to at most one per
	// five minutes per distinct machine set. In-memory by design: a leader
	// restart may re-emit one event, which is harmless.
	lastStallEventKey  string
	lastStallEventTime time.Time
```

2. Add next to `reasonWaitingForVSpherePods`:

```go
const stallEventInterval = 5 * time.Minute
```

3. In the Step 7 wait block (from Task 3), capture the key and add the event. The condition message and `oldWorkerStallDetail` log keep the full stall detail; the Warning event note is capped at 1024 bytes via `boundOldWorkerStallEventNote`. When detail exceeds that limit, the event includes as many MachineSet parts as fit and appends a summary of omitted machine names.

```go
	if !allDeleted {
		detail, eventNote, key, err := oldWorkerStallDetail(ctx, log, machineMgr, oldMachineSets)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("building old worker stall detail: %w", err)
		}
		log.V(1).Info("old worker deletion in progress", "detail", detail)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			"Waiting for old worker deletion: "+detail)
		if key != r.lastStallEventKey || time.Since(r.lastStallEventTime) >= stallEventInterval {
			r.Recorder.Event(migration, "Warning", "OldWorkersStalled", eventNote)
			r.lastStallEventKey = key
			r.lastStallEventTime = time.Now()
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -v`
Expected: PASS, including `TestOldWorkersStallEventDebounce` and the Task 3 stall-detail case (its assertion is exact on the condition message, which is unchanged by this task).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/vmwarecloudfoundationmigration_controller.go internal/controller/workload_migration_rollout_test.go
git commit -m "SPLAT-2655: Emit debounced Warning event when old worker deletion stalls"
```

---

### Task 5: Full verification gate

**Files:** none (verification only; fix in place if anything fails).

- [ ] **Step 1: Format and vet**

Run: `make fmt && make vet`
Expected: no output, exit 0.

- [ ] **Step 2: Lint**

Run: `make lint`
Expected: PASS. Fix any findings (watch `lll` on the long `fmt.Sprintf` format strings and `dupl` on the machine-listing loops) and re-run.

- [ ] **Step 3: Full unit + integration suite**

Run: `make test`
Expected: PASS (envtest-backed controller suite and all unit packages).

- [ ] **Step 4: Build**

Run: `go build ./...`
Expected: exit 0.

- [ ] **Step 5: Final review of the diff**

Run: `git diff main --stat`
Expected: only `internal/controller/vmwarecloudfoundationmigration_controller.go`, `internal/controller/workload_migration_rollout_test.go`, `internal/openshift/machines.go`, `internal/openshift/machines_test.go` (plus this plan doc, committed separately). If the diff includes anything else, stop and investigate.

Commit the plan doc (from the working tree) if not already committed:

```bash
git add docs/plans/splat-2655-worker-rolling-replacement.md
git commit -m "SPLAT-2655: Add implementation plan"
```
