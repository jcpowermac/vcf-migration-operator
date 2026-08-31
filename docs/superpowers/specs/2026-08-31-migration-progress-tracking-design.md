# Migration Progress Tracking Design Spec

**Issue:** [SPLAT-2656](https://redhat.atlassian.net/browse/SPLAT-2656)  
**Status:** Approved  
**Date:** 2026-08-31  

## Overview
This document specifies the design for real-time progress tracking of the `VmwareCloudFoundationMigration` resource. It exposes the current migration phase, ordered condition statuses, detailed machine/node progress counts, granular timestamps, and custom Prometheus metrics to provide visibility to CLI, UI (OpenShift Console), and monitoring systems without relying on an ETA.

## Acceptance Criteria
1. The migration CR status exposes the current phase and ordered conditions.
2. Start (`startTime`), last-update (`lastUpdateTime`), and completion (`completionTime`) timestamps are recorded.
3. Counts of migrated and remaining machines or nodes are surfaced in a structured format in status where practical.
4. Metrics, events, and status messages provide actionable progress details.
5. UI and CLI consumers can render progress without depending on a guaranteed ETA (via `status.phase`, `status.progress`, and `+kubebuilder:printcolumn`).

---

## 1. API Changes (`api/v1alpha1/vmwarecloudfoundationmigration_types.go`)

### Phase Enum
The top-level `status.phase` indicates the high-level stage of the migration workflow:
```go
type MigrationPhase string

const (
    PhasePending                MigrationPhase = "Pending"
    PhaseInfrastructurePrepared MigrationPhase = "InfrastructurePrepared"
    PhaseDestinationInitialized MigrationPhase = "DestinationInitialized"
    PhaseMultiSiteConfigured    MigrationPhase = "MultiSiteConfigured"
    PhaseWorkloadMigrated       MigrationPhase = "WorkloadMigrated"
    PhaseSourceCleaned          MigrationPhase = "SourceCleaned"
    PhaseCompleted              MigrationPhase = "Completed"
    PhaseFailed                 MigrationPhase = "Failed"
    PhasePaused                 MigrationPhase = "Paused"
)
```

### Structured Progress Field
```go
// MigrationProgress contains real-time counters and details of resource migration.
type MigrationProgress struct {
    // Workers tracks worker machine and node migration progress.
    // +optional
    Workers *WorkerMigrationProgress `json:"workers,omitempty"`

    // ControlPlane tracks ControlPlaneMachineSet rollout progress.
    // +optional
    ControlPlane *ControlPlaneProgress `json:"controlPlane,omitempty"`
}

// WorkerMigrationProgress surfaces worker machine and node counts across source and target.
type WorkerMigrationProgress struct {
    // TargetMachinesTotal is the desired number of worker machines in target failure domains.
    TargetMachinesTotal int32 `json:"targetMachinesTotal"`
    // TargetMachinesReady is the number of target worker machines in Running phase with a NodeRef.
    TargetMachinesReady int32 `json:"targetMachinesReady"`
    // TargetNodesReady is the number of target worker nodes reporting NodeReady=True.
    TargetNodesReady int32 `json:"targetNodesReady"`
    // SourceMachinesRemaining is the number of source worker machines still existing.
    SourceMachinesRemaining int32 `json:"sourceMachinesRemaining"`
}

// ControlPlaneProgress surfaces CPMS rollout counts.
type ControlPlaneProgress struct {
    // Replicas is the total desired control plane replicas.
    Replicas int32 `json:"replicas"`
    // UpdatedReplicas is the number of control plane replicas matching the target spec.
    UpdatedReplicas int32 `json:"updatedReplicas"`
    // ReadyReplicas is the number of control plane replicas running and ready.
    ReadyReplicas int32 `json:"readyReplicas"`
}
```

### Status Struct
```go
type VmwareCloudFoundationMigrationStatus struct {
    // Phase indicates the current stage of the migration workflow.
    // +optional
    Phase MigrationPhase `json:"phase,omitempty"`

    // Progress surfaces real-time counts for worker and control-plane migration.
    // +optional
    Progress *MigrationProgress `json:"progress,omitempty"`

    // Conditions represent the current state of the migration.
    // +optional
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // StartTime is when the migration started.
    // +optional
    StartTime *metav1.Time `json:"startTime,omitempty"`

    // LastUpdateTime is when the migration status was last updated.
    // +optional
    LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

    // CompletionTime is when the migration completed.
    // +optional
    CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}
```

### CRD Printer Columns
```go
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
```

---

## 2. Prometheus Metrics (`internal/metrics/metrics.go`)

Export the following metrics registered in `sigs.k8s.io/controller-runtime/pkg/metrics.Registry`:

1. `vcf_migration_phase`: `prometheus.GaugeVec` with label `phase` (value 1 for active phase, 0 for inactive).
2. `vcf_migration_condition_status`: `prometheus.GaugeVec` with labels `condition` and `status` (`True`, `False`, `Unknown`).
3. `vcf_migration_duration_seconds`: `prometheus.Gauge` tracking total elapsed time in seconds.
4. `vcf_migration_workers_total`: `prometheus.Gauge` tracking total target worker machines.
5. `vcf_migration_workers_ready`: `prometheus.Gauge` tracking target worker machines ready.
6. `vcf_migration_nodes_ready`: `prometheus.Gauge` tracking target worker nodes ready.
7. `vcf_migration_source_workers_remaining`: `prometheus.Gauge` tracking source worker machines remaining.
8. `vcf_migration_control_plane_replicas`: `prometheus.Gauge` tracking CPMS desired replicas.
9. `vcf_migration_control_plane_updated_replicas`: `prometheus.Gauge` tracking CPMS updated replicas.
10. `vcf_migration_control_plane_ready_replicas`: `prometheus.Gauge` tracking CPMS ready replicas.

---

## 3. Controller Logic & Reconciler Updates (`internal/controller/`)

1. **Phase Tracking**:
   - `spec.state == Paused` -> `PhasePaused`
   - `spec.state == Pending` -> `PhasePending`
   - When running, phase is set to the name of the condition currently being reconciled:
     - `PhaseInfrastructurePrepared`
     - `PhaseDestinationInitialized`
     - `PhaseMultiSiteConfigured`
     - `PhaseWorkloadMigrated`
     - `PhaseSourceCleaned`
   - When all conditions including `Ready` are `True` -> `PhaseCompleted`
   - If a fatal condition error occurs -> `PhaseFailed`

2. **Progress Collection in `ensureWorkloadMigrated`**:
   - Query target worker machines/nodes ready and total.
   - Query source worker machines remaining.
   - Query CPMS replicas, updatedReplicas, readyReplicas.
   - Assign to `migration.Status.Progress`.

3. **Status Update & Timestamps**:
   - `updateStatus` detects differences in conditions, phase, progress, and timestamps compared to `baseStatus`.
   - On any status change, `Status.LastUpdateTime` is set to `metav1.Now()`.
   - On completion (`Ready=True`), `Status.CompletionTime` is set.
   - Metrics are synchronized on every reconcile/status update.

---

## 4. Testing & Validation

1. **Unit Tests**:
   - `internal/metrics`: verify metric registration, label consistency, and collector updates.
   - `internal/controller`: verify phase transitions, progress population, `LastUpdateTime` and `CompletionTime` recording, and metrics synchronization.
2. **Code Generation**:
   - `make generate` to update `zz_generated.deepcopy.go`.
   - `make manifests` to update CRD YAML in `config/crd/bases/`.
3. **Verification**:
   - `make lint` and `make test`.
