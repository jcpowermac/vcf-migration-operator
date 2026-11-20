# 01: Planning Phase

Status: Implemented for planning. Data transfer, quiesce, cutover, and storage
execution remain later phases.

Parent design: [csi-volume-migration-plan.md](../plans/csi-volume-migration-plan.md)

## Goal

Make `spec.state: Planning` run every non-disruptive check, inventory vSphere CSI
volumes into per-namespace `StorageWorkloadMigration` (SWM) objects, and report
blockers, without changing the cluster or either vCenter's configuration.
`Running` reuses the same steps. With `spec.migrateStorage: true`, it then stops
with `Ready=False`, reason `StorageExecutionUnsupported`, before setting
`startTime` or entering a disruptive condition. This temporary guard remains at
the execution boundary until the complete storage workflow is implemented, even
when the inventory contains no volumes. A successful plan remains
`StoragePlanned=True`.

## Non-goals

- Creating `VolumeCopy` objects, transfer Pods, or target PVCs (target preparation plan).
- Site labels on Nodes and MachineSets, and recording Namespace selectors
  (placement plan). The customer does not touch the selector until
  `AwaitingPlacementUpdate`, so it can be recorded then.
- Revalidating a namespace's plan before each disruptive step (SWM controller plan).
- Cleaning up SWMs when the migration is deleted.

## 1. State semantics

VMO never writes `spec.state`. The customer creates the migration in `Pending`
(default) or `Planning`, reviews the result, and sets `Running`.

| `spec.state` | Behavior |
| :--- | :--- |
| `Pending` | No work. Unchanged from today. |
| `Planning` | Run the planning conditions, then stop. Never run a disruptive condition. |
| `Running` | Run planning first. With `migrateStorage: true`, stop at the execution boundary with `StorageExecutionUnsupported`; otherwise continue the compute workflow. |
| `Paused` | No work. Unchanged from today. |

Setting `Running` directly is allowed and runs the same planning checks without
the review pause. The storage execution guard still applies.

`Planning` or `Pending` after the migration has started (see section 2.3) is
treated like `Paused`, with `Ready=False`, reason `PlanningAfterStart`, message
"migration has started; set spec.state to Running or Paused". CEL cannot enforce
this because transition rules cannot read status, and `Running -> Paused ->
Planning` would bypass a simple transition rule.

## 2. Reconcile loop changes

### 2.1 Condition order

Add `ConditionStoragePlanned = "StoragePlanned"` after `InfrastructurePrepared`:

```go
var conditionOrder = []string{
	ConditionInfrastructurePrepared, // planning
	ConditionStoragePlanned,         // planning
	ConditionDestinationInitialized,
	ConditionMultiSiteConfigured,
	ConditionWorkloadMigrated,
	ConditionSourceCleaned,
	ConditionReady,
}

var planningConditions = sets.New(ConditionInfrastructurePrepared, ConditionStoragePlanned)
```

`InfrastructurePrepared` (preflight) is already non-disruptive. Its probe-VM
network check creates and deletes temporary VMs in the target vCenter but does
not change the cluster, so it is acceptable during `Planning`.

### 2.2 Loop

```go
switch state {
case Pending, Paused:
	// existing behavior
case Planning:
	if migrationStarted(m) { setPlanningAfterStart(m); return }
case Running:
	// existing resume handling
}

for _, condType := range conditionOrder {
	if state == Planning && !planningConditions.Has(condType) {
		setPlanComplete(m) // Ready=False, reason PlanComplete
		return ctrl.Result{RequeueAfter: planningResync}, nil
	}
	if conditionSatisfied(m, condType) {
		continue
	}
	if !planningConditions.Has(condType) && m.Status.StartTime == nil {
		m.Status.StartTime = now // first disruptive step
	}
	// existing handler dispatch
}
```

- `Ready` while in `Planning`: `False`, reason `Planning` while conditions run,
  `PlanComplete` ("plan has no blockers; set spec.state to Running to start") when
  both planning conditions are True, and `PlanBlocked` when `StoragePlanned` is False.
- `planningResync` is 5 minutes. PVC events also trigger reconciles (section 3.6).

### 2.3 Plan freshness

```go
func conditionSatisfied(m, condType) bool {
	c := FindStatusCondition(m.Status.Conditions, condType)
	if c == nil || c.Status != True {
		return false
	}
	if planningConditions.Has(condType) && !migrationStarted(m) {
		return c.ObservedGeneration == m.Generation
	}
	return true
}

func migrationStarted(m) bool { return m.Status.StartTime != nil }
```

Changing `spec.state` from `Planning` to `Running` bumps `metadata.generation`, so
both planning conditions re-run automatically. That implements "revalidate when
the migration changes to Running" without extra state, and also covers edits to
`failureDomains` or `storage` after planning. After the first disruptive step,
planning conditions stay satisfied across generation changes (pause/resume), and
per-namespace revalidation moves to the SWM controller.

`StartTime` moves from "first reconcile in `Running`" to "first disruptive
condition". It is set and persisted before that handler runs.

Storage inventory is also refreshed on every pre-start reconcile, including PVC
metadata events and the five-minute resync. A successful `StoragePlanned` condition
must not suppress these reads just because the migration generation is unchanged.
After successful storage validation the reconcile continues to the review boundary
or the first disruptive step.

### 2.4 `updateStatus` guard

`updateStatus` never downgrades a committed `True`. Re-planning must be able to
turn a planning condition from `True` to `False`, so relax the guard:

```go
if existing.Status == True && cond.Status != True &&
	cond.ObservedGeneration <= existing.ObservedGeneration {
	continue // stale update
}
```

A newer generation may downgrade. For same-generation storage refreshes before
`StartTime`, a downgrade is also permitted when the committed condition still
matches the snapshot this reconcile read. This allows new inventory blockers to
invalidate a successful plan while retaining protection against stale concurrent
writers. Other same-or-older-generation downgrades remain protected.

Before persisting the first `StartTime`, the status writer verifies that the
latest spec still has the same generation, is `Running`, and has both planning
conditions satisfied. A concurrent spec change stops disruption and forces
revalidation.

## 3. `StoragePlanned` handler

`ensureStoragePlanned` lives in `internal/controller/storage_planning.go`. Pure
inventory and blocker logic lives in a new package `internal/storageplan` so it
can be unit-tested without envtest.

### 3.1 Storage migration not enabled

If `spec.migrateStorage` is false, set `StoragePlanned=True`, reason
`NotConfigured`. The preflight check in section 4 still blocks clusters that have
vSphere CSI PVs. When `migrateStorage` is true, inventory runs even if the optional
`spec.storage` configuration is omitted. The API rejects `spec.storage` unless
`migrateStorage` is true.

### 3.2 Inventory

1. List PVs with `spec.csi.driver == csi.vsphere.vmware.com`. Include in-tree
   vSphere volumes and vSphere CSI-migrated annotations so source disks are
   inventoried rather than silently omitted.
2. For each PV:
   - `Bound`: read `spec.claimRef`, get the PVC, and require PVC UID ==
     `claimRef.uid` and `pvc.spec.volumeName == pv.name`. Otherwise record a
     cluster-level blocker `PVCBindingMismatch`.
   - Not bound (`Available`, `Released`): cluster-level blocker `UnboundVolume`.
     These would be left on the source vCenter when it is removed, so the
     customer must delete them (or bind them to a PVC) before the migration can
     proceed.
3. Group bound volumes by PVC namespace. Each namespace gets one SWM.

### 3.3 Per-volume checks

Each failed check adds a `{reason, message}` blocker to the volume
(`status.volumes[].blockers`, see section 5).

| Reason | Check |
| :--- | :--- |
| `RawBlockUnsupported` | `pv.spec.volumeMode == Block` |
| `StaticVolumeUnsupported` | PVC has no `storageClassName`, or the StorageClass does not exist |
| `ImmediateBindingUnsupported` | StorageClass `volumeBindingMode` is not `WaitForFirstConsumer` |
| `StorageClassPinnedToSource` | StorageClass has a `datastoreurl` parameter (case-insensitive) or `allowedTopologies` |
| `StoragePolicyMissing` | StorageClass has no `storagepolicyname` parameter, or (customer-created StorageClasses only) the policy does not exist in the target vCenter or matches no datastore in the target failure domain (section 3.5) |
| `TargetFailureDomainUnresolved` | section 3.4 |
| `TargetFailureDomainInvalid` | override annotation names a failure domain not in `spec.failureDomains` |
| `IncompatiblePodTopology` | a Pod mounting several migrating PVCs resolves them to different target failure domains |

In-tree vSphere PVs, CSI-migrated PVs, and vSAN file volumes (`file:` handles)
are supported by planning and do not receive type-specific blockers. They still
undergo the checks above. Data transfer is a later phase; target StorageClass
mapping and validation of file-capable target provisioning remain follow-ups.

### 3.4 Target failure domain resolution

Source failure domain of a PV:

1. Read `pv.spec.nodeAffinity` for `topology.csi.vmware.com/openshift-region` and
   `topology.csi.vmware.com/openshift-zone` (fall back to
   `topology.kubernetes.io/region|zone`). Match against Infrastructure
   `spec.platformSpec.vsphere.failureDomains[].region/zone`.
2. No topology affinity and the source has exactly one failure domain: that one.
3. Otherwise: unknown.

Target failure domain:

```
migration.openshift.io/target-failure-domain annotation on the PVC
  -> failureDomainMappings[sourceFD]
  -> the only entry in spec.failureDomains, if there is exactly one
  -> TargetFailureDomainUnresolved
```

When the target has a single failure domain, no mapping or annotation is
required: every volume resolves to it.

`IncompatiblePodTopology` is computed from current Pods (any phase except
`Succeeded`/`Failed`). Workloads scaled to zero are not checked; the SWM
controller rechecks before target preparation.

### 3.5 Storage policy checks

The CSI operator creates its own policy (`openshift-storage-policy-<infraID>`) in
the target vCenter only after `MultiSiteConfigured` adds the target vCenter to
Infrastructure. Planning therefore skips the policy lookup for StorageClasses
owned by the CSI operator (`thin-csi`, recognized by that policy name) and the
target preparation plan verifies them later.

For other StorageClasses, connect to each target vCenter used by a resolved
target failure domain and use a new `internal/vsphere/pbm.go`:

- `GetProfileIDByName(ctx, session, name)` (`pbm.Client.ProfileIDByName`)
- `IsDatastoreCompatible(ctx, session, profileID, datastore)` (`CheckRequirements`
  against the failure domain's `topology.datastore`)

Cache results per (vCenter, policy, datastore) within one reconcile.
The pinned govmomi PBM client, methods, types, and simulator are vendored with
`go mod vendor`. Policy lookup uses `QueryProfile` and `RetrieveContent` so a
missing policy can be distinguished from transport or permission errors;
`ProfileIDByName` represents both cases as errors. Datastore compatibility uses
`CheckRequirements` against the exact target datastore.

### 3.6 Cluster-level checks

Recorded on the migration's `StoragePlanned` condition, not on an SWM:

| Reason | Check |
| :--- | :--- |
| `UnboundVolume`, `PVCBindingMismatch` | section 3.2 |
| `TopologyCategoriesConflict` | `spec.failureDomains` has one entry and `ClusterCSIDriver` `spec.driverConfig.vSphere.topologyCategories` is set to anything other than `[openshift-zone, openshift-region]` (parent design 5.2) |
| `TargetTopologyOverlapsSource` | a target failure domain has the same region and zone as a source failure domain |
| `PlatformNamespaceUnsupported` | a vSphere CSI PVC exists in an `openshift-*` or `kube-*` namespace other than `openshift-monitoring` and `openshift-image-registry` (section 3.11) |

### 3.7 Consumers

For each Pod that mounts a migrating PVC, follow controller owner references:
Pod -> ReplicaSet -> Deployment, Pod -> StatefulSet, Pod -> DaemonSet, Pod -> Job
-> CronJob. Stop at the first owner that is not one of these kinds and record it
(for example an operator CR). A Pod with no controller is recorded itself.
Deduplicate by UID.

### 3.8 Writing SWMs

- Name `vcf-storage-<first 8 characters of migration UID>`, label
  `migration.openshift.io/migration-uid=<uid>`. No owner reference: the
  migration lives in the operator namespace and cannot own objects in other
  namespaces.
- Create or update `spec.migrationRef`, then update status:
  - `phase: Planning`
  - `volumes`, `consumers`
  - condition `Planned`: `True`, reason `NoBlockers`; or `False`, reason
    `Blocked`, message `"<n> of <m> volumes blocked"`.
- Delete an SWM with `phase: Planning` in a namespace that no longer has vSphere
  CSI PVCs. Never delete an SWM past `Planning`.

### 3.9 Migration condition

- `StoragePlanned=True`, reason `Completed`: "Planned <m> volumes in <k> namespaces".
- `StoragePlanned=False`, reason `PlanBlocked`: "<n> volumes blocked in <j>
  namespaces; <cluster blockers>; see StorageWorkloadMigration status". Bound the
  message with `boundConditionMessage`.

Blockers are not errors: the handler returns `RequeueAfter: planningResync` and
no error, so the existing error path does not overwrite the condition and the
rate limiter does not back off.

### 3.10 Watches

Add a metadata-only watch on PVCs (`builder.OnlyMetadata`) that enqueues the
singleton `cluster` request, so adding or fixing an override annotation is
reflected quickly. PVs, Pods, and StorageClasses are read with `KubeClient` on
each pass and rely on the 5-minute resync.

### 3.11 Platform namespaces

`openshift-monitoring` (Prometheus and Alertmanager) and
`openshift-image-registry` (image registry PVC) are planned like any other
namespace: they get an SWM, and their volumes go through the same checks.
Their consumers are operator-managed: the owner walk stops at the Prometheus or
Alertmanager CR (`monitoring.coreos.com`) or at the image registry Deployment.
Customers cannot quiesce them by scaling workloads down, because the operators
reconcile them back; the quiesce procedure for these namespaces is defined in
the quiesce plan.

Any other `openshift-*` or `kube-*` namespace with a vSphere CSI PVC (for
example a layered operator) is a `PlatformNamespaceUnsupported` blocker.

## 4. Preflight changes

In `runPreflightChecks`:

- With `spec.migrateStorage: false`, `checkNoVSphereCSIPersistentVolumes` blocks
  vSphere CSI PVs and suggests enabling `spec.migrateStorage`;
  `checkVSphereStorageManagement` requires `ClusterCSIDriver` `Removed`.
- With `spec.migrateStorage: true`, the PV check is skipped and PVs become
  inventory. `checkStorageManagementForPlan` requires `ClusterCSIDriver` `Managed`.

## 5. API changes

`api/v1alpha1/vmwarecloudfoundationmigration_types.go`:

- `ConditionStoragePlanned = "StoragePlanned"`.
- Reasons `ReasonPlanning`, `ReasonPlanComplete`, `ReasonPlanBlocked`,
  `ReasonPlanningAfterStart`, `ReasonNotConfigured`.
- Update the `Conditions` godoc and `State` godoc to describe `Planning`.

`api/v1alpha1/storageworkloadmigration_types.go`:

```go
// StorageWorkloadMigrationBlocker explains why a volume cannot be migrated.
type StorageWorkloadMigrationBlocker struct {
	// Reason is a CamelCase blocker identifier.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`

	// Message explains the blocker and how to resolve it.
	// +required
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message"`
}

// in StorageWorkloadMigrationVolume:
	// Blockers lists reasons this volume cannot be migrated; empty when it can.
	// +optional
	// +listType=map
	// +listMapKey=reason
	Blockers []StorageWorkloadMigrationBlocker `json:"blockers,omitempty"`
```

Plus the condition type constant `StorageWorkloadMigrationConditionPlanned = "Planned"`.

## 6. RBAC

Add `+kubebuilder:rbac` markers:

- `""`: `persistentvolumeclaims` get/list/watch, `namespaces` get/list
- `storage.k8s.io`: `storageclasses` get/list
- `apps`: `replicasets`, `statefulsets`, `daemonsets` get
- `batch`: `jobs`, `cronjobs` get
- `migration.openshift.io`: `storageworkloadmigrations` get/list/watch/create/update/delete,
  `storageworkloadmigrations/status` get/update/patch

`persistentvolumes`, `pods`, and `clustercsidrivers` are already readable.

## 7. Tests

Unit (`internal/storageplan`, table-driven):

- source failure domain from PV affinity, single-FD fallback, ambiguous
- target failure domain precedence, invalid override, single-target fallback
- each per-volume blocker in section 3.3
- multi-PVC Pod topology compatibility
- consumer owner walk, including an unknown owner kind and a bare Pod
- each cluster-level blocker in section 3.6

Unit (`internal/vsphere`, govmomi simulator): policy lookup and datastore compatibility.

Controller (envtest, Ginkgo):

- `Planning` runs `InfrastructurePrepared` and `StoragePlanned` only;
  `DestinationInitialized` never runs and `StartTime` stays nil.
- `Planning` -> `Running` re-runs both planning conditions (generation bump),
  then proceeds.
- `Running` with a blocked plan stops at `StoragePlanned`.
- Fixing a blocker (adding an override annotation) clears it.
- `Planning` after `StartTime` is set reports `PlanningAfterStart` and does nothing.
- `updateStatus` lets a newer generation downgrade a planning condition.
- Preflight: `spec.storage` set skips the PV check and requires CSI `Managed`.

## 8. Implementation order

1. API: condition, reasons, blocker type; `make manifests generate`.
2. Reconcile loop: planning prefix, freshness, `StartTime`, `updateStatus` guard
   (with `StoragePlanned` stubbed to `NotConfigured`). Controller tests.
3. Preflight changes.
4. `internal/storageplan` inventory and blockers, then the handler, SWM writes,
   watch, and RBAC.
5. PBM helpers and customer-StorageClass policy checks.

Each step is a separate PR.

## 9. Open decisions

Tracked in [data_migration_action.md](../learn/data_migration_action.md#open-questions-from-the-planning-phase-plan).
