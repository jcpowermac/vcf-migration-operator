# Migration Progress Tracking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement real-time migration progress tracking (phase, progress counts, timestamps, printer columns, and Prometheus metrics) for OpenShift vCenter migrations in `vcf-migration-operator`.

**Architecture:** Extend CRD status types in `api/v1alpha1` with `Phase`, `Progress`, and `LastUpdateTime`; implement Prometheus metrics collection in `internal/metrics`; update controller reconciliation in `internal/controller` to calculate phase/progress and update metrics; verify with unit & controller tests.

**Tech Stack:** Go 1.23+, Kubernetes/OpenShift controller-runtime, Prometheus client_golang, Ginkgo/Gomega, Kubebuilder v4.

**Spec:** `docs/superpowers/specs/2026-08-31-migration-progress-tracking-design.md`

## Global Constraints
- Follow Go code style specified in `AGENTS.md` (three import groups stdlib / 3rd-party / internal, klog/v2 logging with structured fields, gerund error wrapping).
- Backward compatible CRD changes: all new status fields are optional (`+optional`, `omitempty`).
- Do not introduce external dependencies beyond existing ones.
- Enforce full test coverage with passing `make test` and `make lint`.

---

### Task 1: CRD Status Types & Printer Columns

**Files:**
- Modify: `api/v1alpha1/vmwarecloudfoundationmigration_types.go`
- Modify: `config/crd/bases/migration.openshift.io_vmwarecloudfoundationmigrations.yaml`
- Modify: `api/v1alpha1/zz_generated.deepcopy.go`
- Test: `api/v1alpha1/types_test.go` (or run `make generate && make manifests`)

**Interfaces:**
- Produces: `MigrationPhase` constants, `MigrationProgress`, `WorkerMigrationProgress`, `ControlPlaneProgress`, `LastUpdateTime` field in `VmwareCloudFoundationMigrationStatus`.

- [ ] **Step 1: Write test for API types deepcopy and validation**

Create `api/v1alpha1/types_test.go`:
```go
package v1alpha1

import (
	"testing"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMigrationProgressDeepCopy(t *testing.T) {
	now := metav1.Now()
	orig := &VmwareCloudFoundationMigrationStatus{
		Phase: PhaseWorkloadMigrated,
		Progress: &MigrationProgress{
			Workers: &WorkerMigrationProgress{
				TargetMachinesTotal:     3,
				TargetMachinesReady:     3,
				TargetNodesReady:       3,
				SourceMachinesRemaining: 0,
			},
			ControlPlane: &ControlPlaneProgress{
				Replicas:        3,
				UpdatedReplicas: 3,
				ReadyReplicas:   3,
			},
		},
		StartTime:      &now,
		LastUpdateTime: &now,
		CompletionTime: &now,
	}

	copied := orig.DeepCopy()
	if copied == nil {
		t.Fatal("DeepCopy returned nil")
	}
	if copied.Phase != PhaseWorkloadMigrated {
		t.Errorf("expected phase %s, got %s", PhaseWorkloadMigrated, copied.Phase)
	}
	if copied.Progress == nil || copied.Progress.Workers == nil || copied.Progress.Workers.TargetMachinesReady != 3 {
		t.Errorf("progress workers not copied properly: %+v", copied.Progress)
	}
	if copied.Progress.ControlPlane == nil || copied.Progress.ControlPlane.UpdatedReplicas != 3 {
		t.Errorf("progress control plane not copied properly: %+v", copied.Progress)
	}
}
```

- [ ] **Step 2: Update `api/v1alpha1/vmwarecloudfoundationmigration_types.go`**

Add `MigrationPhase`, `MigrationProgress`, `WorkerMigrationProgress`, `ControlPlaneProgress`, `LastUpdateTime`, and update printer columns.

- [ ] **Step 3: Run code generators**

Run: `make generate && make manifests`
Expected: Updates `zz_generated.deepcopy.go` and `config/crd/bases/migration.openshift.io_vmwarecloudfoundationmigrations.yaml`.

- [ ] **Step 4: Run unit tests**

Run: `go test ./api/v1alpha1/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/ config/crd/bases/
git commit -m "feat(api): add migration phase, progress structs, and printer columns"
```

---

### Task 2: Prometheus Metrics Package

**Files:**
- Create: `internal/metrics/metrics.go`
- Create: `internal/metrics/metrics_test.go`

**Interfaces:**
- Produces: `InitMetrics()`, `UpdateMigrationMetrics(status *migrationv1alpha1.VmwareCloudFoundationMigrationStatus)`, `ResetMetrics()`

- [ ] **Step 1: Write tests for metrics recording and registration**

Create `internal/metrics/metrics_test.go`:
```go
package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func TestUpdateMigrationMetrics(t *testing.T) {
	InitMetrics()
	defer ResetMetrics()

	now := metav1.Now()
	startTime := metav1.NewTime(now.Add(-5 * time.Minute))

	status := &migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
		Phase:     migrationv1alpha1.PhaseWorkloadMigrated,
		StartTime: &startTime,
		Conditions: []metav1.Condition{
			{
				Type:   migrationv1alpha1.ConditionWorkloadMigrated,
				Status: metav1.ConditionTrue,
			},
			{
				Type:   migrationv1alpha1.ConditionReady,
				Status: metav1.ConditionFalse,
			},
		},
		Progress: &migrationv1alpha1.MigrationProgress{
			Workers: &migrationv1alpha1.WorkerMigrationProgress{
				TargetMachinesTotal:     3,
				TargetMachinesReady:     2,
				TargetNodesReady:       2,
				SourceMachinesRemaining: 1,
			},
			ControlPlane: &migrationv1alpha1.ControlPlaneProgress{
				Replicas:        3,
				UpdatedReplicas: 2,
				ReadyReplicas:   2,
			},
		},
	}

	UpdateMigrationMetrics(status)

	if val := testutil.ToFloat64(MigrationPhaseGauge.WithLabelValues(string(migrationv1alpha1.PhaseWorkloadMigrated))); val != 1 {
		t.Errorf("expected phase gauge to be 1, got %f", val)
	}
	if val := testutil.ToFloat64(WorkersTotalGauge); val != 3 {
		t.Errorf("expected workers total to be 3, got %f", val)
	}
	if val := testutil.ToFloat64(WorkersReadyGauge); val != 2 {
		t.Errorf("expected workers ready to be 2, got %f", val)
	}
	if val := testutil.ToFloat64(NodesReadyGauge); val != 2 {
		t.Errorf("expected nodes ready to be 2, got %f", val)
	}
	if val := testutil.ToFloat64(SourceWorkersRemainingGauge); val != 1 {
		t.Errorf("expected source workers remaining to be 1, got %f", val)
	}
	if val := testutil.ToFloat64(ControlPlaneReplicasGauge); val != 3 {
		t.Errorf("expected cp replicas to be 3, got %f", val)
	}
	if val := testutil.ToFloat64(ControlPlaneUpdatedReplicasGauge); val != 2 {
		t.Errorf("expected cp updated replicas to be 2, got %f", val)
	}
	if val := testutil.ToFloat64(ControlPlaneReadyReplicasGauge); val != 2 {
		t.Errorf("expected cp ready replicas to be 2, got %f", val)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/metrics -v`
Expected: FAIL (package does not exist)

- [ ] **Step 3: Implement `internal/metrics/metrics.go`**

Define metrics descriptors and register with `ctrlmetrics.Registry`. Provide update and reset helpers.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/metrics -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/
git commit -m "feat(metrics): add prometheus metrics for migration progress"
```

---

### Task 3: Controller Progress & Phase Calculation

**Files:**
- Modify: `internal/controller/vmwarecloudfoundationmigration_controller.go`
- Modify: `internal/openshift/machines.go` (if helper queries for total counts are needed)

**Interfaces:**
- Consumes: `internal/metrics`, `api/v1alpha1`
- Produces: Updated status with `Phase`, `Progress`, `LastUpdateTime`, and metrics synchronization on reconcile.

- [ ] **Step 1: Update controller logic for progress collection and phase resolution**

In `internal/controller/vmwarecloudfoundationmigration_controller.go`:
1. In `Reconcile`:
   - Set `status.Phase` based on `spec.State`, current active condition, or completion status.
   - On reconcile end or when updating status, invoke `metrics.UpdateMigrationMetrics(&latest.Status)`.
2. In `ensureWorkloadMigrated`:
   - Gather worker machine counts (`targetWorkersTotal`, `targetWorkersReady`, `targetNodesReady`, `sourceWorkersRemaining`).
   - Gather CPMS rollout counts (`replicas`, `updatedReplicas`, `readyReplicas`).
   - Populate `migration.Status.Progress`.
3. In `updateStatus`:
   - Compare `Phase` and `Progress` changes between `baseStatus` and `migration.Status`.
   - Update `latest.Status.Phase`, `latest.Status.Progress`.
   - If conditions, phase, progress, or timestamps changed, set `latest.Status.LastUpdateTime = metav1.Now()`.
   - Call `metrics.UpdateMigrationMetrics(&latest.Status)`.

- [ ] **Step 2: Run unit and controller tests**

Run: `go test ./internal/controller/... -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/controller/ internal/openshift/
git commit -m "feat(controller): track migration phase, progress counts, and timestamps"
```

---

### Task 4: Controller Tests for Progress & Metrics

**Files:**
- Modify: `internal/controller/vmwarecloudfoundationmigration_controller_test.go`

**Interfaces:**
- Consumes: Controller reconciler, envtest.

- [ ] **Step 1: Add tests verifying phase transitions, progress struct population, and timestamps**

Add Ginkgo test cases in `internal/controller/vmwarecloudfoundationmigration_controller_test.go`:
- Test that status starts with `PhasePending` when `spec.state=Pending`.
- Test that status moves to `PhaseInfrastructurePrepared`, etc., when running.
- Test that `LastUpdateTime` is updated on status changes.
- Test that `Progress` is populated during workload migration.
- Test that `CompletionTime` and `PhaseCompleted` are set when `Ready=True`.

- [ ] **Step 2: Run Ginkgo controller tests**

Run: `make test`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/controller/
git commit -m "test(controller): add tests for progress tracking and status updates"
```

---

### Task 5: End-to-End Verification, Formatting, and Linting

**Files:**
- All touched files.

- [ ] **Step 1: Run formatting and code generators**

Run:
```bash
make fmt
make vet
make generate
make manifests
```
Expected: Clean code generation and formatting.

- [ ] **Step 2: Run full lint suite**

Run: `make lint`
Expected: 0 lint errors.

- [ ] **Step 3: Run full test suite**

Run: `make test`
Expected: All tests pass.

- [ ] **Step 4: Commit any generated changes or adjustments**

```bash
git add .
git commit -m "chore: format and verify code generation and tests"
```
