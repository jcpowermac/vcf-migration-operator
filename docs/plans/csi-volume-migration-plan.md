# Customer-Coordinated File-Copy Migration of vSphere CSI Volumes

Status: Draft / Proposal

The authoritative end-to-end design is the [vSphere Storage Transfer with VMO enhancement](https://github.com/gnufied/enhancements-1/blob/add-vmo-storage-migration/enhancements/storage/vsphere-storage-transfer-vmo.md), proposed in [openshift/enhancements#2155](https://github.com/openshift/enhancements/pull/2155). This document supplements it with implementation details for this repository. API examples describe the intended design; they do not imply that the current controller implements it. Unresolved enhancement details are listed in section 13.

## 1. Scope

VMO copies CNS volumes to new datastores in new vCenter environment using `pvc-transfer` as a library.

The customer is responsible for quiescing and resuming workloads, managing workload placement in the target environment, and approving application validation.

Migration is performed per namespace with VMO doing precopy, final copy, rebinding, and placement verification. Automated deletion of retained source data is deferred to a later phase of the enhancement; it is not a prerequisite for completing this release's migration.

## Goals:

1. VMO copies target CNS volumes using github.com/migtools/pvc-transfer as a library to new vCenter.  
2. It should be possible to replace rsync with something like `CnsRelocateVolume` in future, when it becomes feasible to move CNS volumes between vCenters.   
3. Minimize manual work and downtime; supported configurations and downtime estimates remain open questions.
4. Target Tech Preview in OpenShift 5.1.

## Non-Goals

1. Automated quiescing of customer workloads.
2. Modifying customer workload to fit new topology.  
3. Moving raw block volumes.
4. Copying of Snapshot data.
5. Implementing cross-vCenter `CnsRelocateVolume` in this phase.
6. Reimplementing the infrastructure migration workflow or implementing automated source-data deletion in this phase.

## 2. API Changes

### 1. Changes to VmwareCloudFoundationMigration type:

Storage migration is opt-in through `VmwareCloudFoundationMigration.spec.migrateStorage`, defaulting to false. When disabled, VMO retains the existing compute-migration behavior and storage safety checks; disabling the opt-in must not bypass rejection of unsupported vSphere volumes.

When storage migration is enabled, the customer must start in `Planning`. VMO inventories volumes, resolves target failure domains, StorageClasses, and movers, and reports blockers through namespace-scoped `StorageWorkloadMigration` objects. A direct request for `Running` without a completed, valid plan reports `PlanningRequired` on VCFM and does not enter destination initialization or multi-site configuration. Planning must finish before adding the second vCenter or creating target workers: changing a previously single-failure-domain cluster to multi-site can affect existing PV affinity and Pod scheduling.

Planning creates inventory objects, not `VolumeCopy` instances, target PVCs, or transfer Pods. The customer reviews the plan and resolves blockers before setting `state: Running`.

The proposed shape of the `VmwareCloudFoundationMigration` storage configuration is shown below. The current API accepts `Auto` (the default) and `Rsync`; restricting it to `Rsync` and changing the default remain pending decisions in [Data Migration Action Items](../learn/data_migration_action.md#restrict-datamigrationstrategy-to-rsync-until-other-movers-exist).

```yaml
spec:
  migrateStorage: true
  state: Planning
  storage:
    # Volume mover used to copy data (see "Volume movers"). Defaults to Auto.
    dataMigrationStrategy: <Auto|Rsync>
    # Optional: use a different StorageClass for target provisioning.
    storageClassMapping:
      - source: thin-csi
        target: thin-csi-2
  failureDomainMappings:
    - source: source-a
      destination: target-a
    - source: source-b
      destination: target-b
    - source: source-c
      destination: target-c
```

`failureDomainMappings` are entries in the migration CR, not separate Kubernetes objects.

`storageClassMapping` optionally maps a source StorageClass to a different target StorageClass. Without an entry, the target PVC uses the source PVC's StorageClass. This supports source classes pinned by `datastoreurl` and volumes migrated from the in-tree vSphere plugin; the target must use a suitable vSphere CSI StorageClass. Planning records the resolved target class and validates its provisioning requirements before execution.

When VMO adds the target vCenter, the CSI driver operator creates the same-named default storage policy there and tags the target failure-domain datastores, so an appropriate class such as `thin-csi` can span both vCenters. For customer-created policies, the customer must prepare the policy referenced by the resolved target class in the target vCenter. Target placement comes from the receiver's topology (section 5). Planning blocks a PVC whose resolved target StorageClass:

- references a storage policy that is missing in the target vCenter or matches no datastore in the resolved target failure domain;
- Migration couldn't place a PV to a target datastore.

Source StorageClass restrictions do not by themselves block migration when an explicit mapping resolves to a valid target class. VMO must account for CSI-migrated in-tree vSphere volumes during inventory rather than assuming every source PV already exposes a native `spec.csi` source. Exact legacy identity resolution and supported combinations require implementation validation.

#### Planning completion and restart recovery

Planning completion and its inputs must be persisted, so a controller restart does not require customers to repeat Planning solely because an in-memory cache was lost. A proposed repository implementation is a VCFM `StoragePlanned=True` condition together with `status.storagePlan.inputFingerprint`, written after all required namespace plans are persisted. The fingerprint identifies relevant storage configuration, mappings, target failure domains, and inventoried source identities; changing `state` from `Planning` to `Running` must not invalidate it. The enhancement does not yet prescribe this status field or the exact fingerprint inputs.

On entry to `Running`, VMO verifies completion, compares current inputs, and checks that the namespace plans still exist and are complete. Missing planning reports `PlanningRequired`; changed inputs before execution require replanning with an explanatory condition. An execution wait, such as unavailable target capacity, reports its own blocker rather than requesting Planning again. Once execution starts, recovery uses persisted execution checkpoints; re-entering global Planning must not discard a partially executed cutover. Recovery after identity changes during execution remains to be specified.

#### Per-PVC target failure-domain override

A customer may override the mapped target failure domain for an individual source PVC by adding below annotation:

```yaml
metadata:
  annotations:
    migration.openshift.io/target-failure-domain: target-b
```

The annotation takes precedence over `failureDomainMappings`. VMO validates the value against `spec.failureDomains`. During Planning, VMO records the resolved failure domain in `StorageWorkloadMigration.status.volumes[].targetFailureDomain`. Before target preparation starts, VMO copies it into the immutable `VolumeCopy.spec.targetFailureDomain`. If source topology is missing or ambiguous and there is no valid override, the namespace remains blocked in Planning. Any blocked namespace prevents the cluster migration from starting infrastructure reconfiguration.

### 2. VolumeCopy object

Since data migration can take a long time, VMO tracks each PVC through a corresponding `VolumeCopy` object. These objects are created during `Running`, after target capacity is ready and before target preparation, with owner references to the namespace's `StorageWorkloadMigration`:

```go
type VolumeCopy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeCopySpec   `json:"spec"`
	Status VolumeCopyStatus `json:"status,omitempty"`
}
```

`VolumeCopy.spec.method` records the volume mover resolved during Planning and is immutable, like `targetFailureDomain`. Mover-specific progress lives in a per-method field of `VolumeCopy.status` (for example `status.rsync`).


### 3. Volume movers

The customer-facing lifecycle in this document is the same for every copy mechanism. Only the step that produces a target-vCenter volume with the source's data at the quiesce point is pluggable. VMO calls it through a `VolumeMover` interface:

```go
type VolumeMover interface {
	Method() migrationv1alpha1.VolumeCopyMethod
	Capabilities() MoverCapabilities

	// Validate runs during Planning and returns blockers. It never mutates anything.
	Validate(ctx context.Context, plan VolumePlan) []Blocker

	Prepare(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (StepResult, error)
	PreCopy(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (StepResult, error)
	FinalCopy(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (StepResult, error)
	// Release removes mover-owned Pods and attachments before cutover.
	Release(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (StepResult, error)
	// TargetVolume returns the target volume handle, capacity, and node affinity used by cutover.
	TargetVolume(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (TargetVolumeRef, error)
}

type MoverCapabilities struct {
	SupportsPreCopy bool // incremental, non-authoritative seed copies
	ConsumesSource  bool // the source FCD no longer exists after FinalCopy
	VolumeModes     []corev1.PersistentVolumeMode
	AccessModes     []corev1.PersistentVolumeAccessMode
}
```

Movers are level-triggered and idempotent, and persist all progress (including any vCenter task reference) in `VolumeCopy.status` so they can resume after an operator restart. Cutover, validation, placement, and cleanup consume only `TargetVolumeRef` and do not depend on the mover.

- **`Rsync`** (this release) uses `pvc-transfer` to copy files between a source reader Pod and a target receiver Pod. It supports precopy and does not consume the source. Sections 5 and 6 describe its Pod placement and target provisioning.
- **`CNSRelocate`** (future) would move the FCD to a target-vCenter datastore with a vSphere API such as `CnsRelocateVolume`, then statically create the target PV from the returned handle. It would have no transfer Pods or precopy, and it would consume the source.

Under the proposed `Auto` semantics, `dataMigrationStrategy: Auto` would resolve to `Rsync` for supported volumes and report blockers when no implemented mover is valid. Whether to retain `Auto` or expose only `Rsync` remains pending; mover execution is not implemented yet. Future mover selection may prefer movers that avoid a file-level copy. A mover can only be swapped before `Prepare` starts; after that, changing method requires an explicit recovery/reset procedure, whose API is not yet defined.

A future mover with `ConsumesSource` would remove the source during `FinalCopying`, rather than after validation and a rollback-retention period. Planning must disclose that source rollback will be unavailable. After setting VCFM to `Running`, the customer would still wait for `AwaitingQuiesce`, stop workloads, and set `finalCopyApproved: true` before relocation begins; no precopy runs. That approval would authorize the source-consuming operation. Eligibility and cleanup configuration for such a mover belong to the future relocation design; this release does not expose `cleanupSourceData` in the proposed VCFM configuration.

## 3. StorageWorkloadMigration

During Planning, VMO creates one `StorageWorkloadMigration` in every namespace that contains a vSphere-backed PVC. Keeping it in the workload namespace lets it own the namespace's `VolumeCopy` objects; each `VolumeCopy` can in turn own its transfer Pods, temporary target PVC etc.

Example:

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: StorageWorkloadMigration
metadata:
  name: vcf-storage-<short-migration-uid>
  namespace: payments
spec:
  migrationRef:
    name: cluster
    uid: <migration-uid>
  actions:
    finalCopyApproved: false
    validationApproved: false
    # Reserved for future source-data cleanup; not acted on in this phase.
    sourceCleanupApproved: false
status:
  phase: Planning
  volumes: []
  consumers: []
  conditions: []
```

Customers write only `spec.actions`. VMO writes only `status`. VMO must never advance progress by changing the object's spec.

`status.phase` uses the following ordered values (with optional precopy):

- `Planning`: VMO inventories the namespace, resolves targets, and reports blockers.
- `PreparingTargets`: the mover prepares target volumes and VMO verifies placement and ownership.
- `PreCopying`: the mover runs optional, non-authoritative seed copies; skipped when unsupported.
- `AwaitingQuiesce`: VMO waits for stopped consumers and final-copy approval.
- `FinalCopying`: VMO verifies quiescence and the mover performs authoritative copies.
- `CuttingOver`: the mover releases attachments and VMO rebinds original PVC names to target PVs.
- `AwaitingPlacementUpdate`: VMO waits for the customer to apply the published target-site Namespace selector.
- `ReadyForResume`: storage and placement are ready for the customer to resume workloads.
- `Validating`: VMO checks resumed workloads while the customer validates the application.
- `WorkloadMigrationComplete`: storage cutover and workload validation are complete; source compute retirement may proceed.
- `AwaitingPlacementCleanup`: source workers are gone and VMO waits for customer Namespace selector cleanup.
- `CleaningPlacement`: VMO verifies Namespace changes and removes migration labels from Nodes and MachineSets.
- `Succeeded`: workload migration and placement cleanup are complete. Retained source data does not block this phase.
- `Failed`: an unrecoverable error requires explicit recovery. Retriable waits stay in their current phase with explanatory conditions.

`status.conditions` reports readiness, blockers, retries, and customer actions within the current phase. A Planning blocker therefore leaves `phase: Planning` and sets an explanatory condition; it does not change the phase to `Blocked`.

Planning inventory records object identity, not names alone:

```yaml
status:
  volumes:
    - sourcePVC: {name: data-database-0, uid: <pvc-uid>}
      sourcePV:
        name: pvc-1234
        uid: <pv-uid>
        volumeHandle: <source-fcd-uuid>
      targetFailureDomain: target-a
  consumers:
    - apiVersion: apps/v1
      kind: StatefulSet
      name: database
      uid: <statefulset-uid>
```

VMO revalidates this inventory when the migration changes to `Running` and again before each disruptive transition. A new or replaced PVC or PV invalidates the affected plan. Before execution, this requires Planning again; during execution VMO must stop the affected operation and preserve checkpoints pending explicit recovery rather than silently adopting the replacement.

`status.consumers` is informational. It lists the workload controllers observed consuming the namespace's PVCs, so the customer knows what to stop and resume, and VMO knows which workloads to check for readiness during `Validating`. A changed or replaced consumer does not block progress; safety relies on PVC and PV identities and on the Pod and VolumeAttachment checks. Approval fields never suppress identity, placement, attachment, or writer checks.

## 4. Site placement fence

While the migration is `Running`, VMO labels Nodes only after positively mapping each Node through its Machine provider spec `workspace.server`:

```yaml
migration.openshift.io/site: <migration-uid>-source
migration.openshift.io/site: <migration-uid>-target
```

VMO also records and temporarily updates the relevant *MachineSet* templates so replacement Nodes receive the correct site label while migration is active. VMO continually verifies Node UID, Machine identity, vCenter, readiness, and topology before using a Node. A newly created, unlabeled Node remains ineligible until VMO classifies it.

The site label has one purpose: distinguish source-vCenter Nodes from target-vCenter Nodes. It does not select a zone. Region and zone constraints remain separate.

During Planning, VMO records whether `openshift.io/node-selector` exists and its original value in `StorageWorkloadMigration.status`. Customers manage their application Namespace selectors. The enhancement explicitly permits VMO to remove the temporary selector from OpenShift system namespaces during cleanup; the scope of that exception must be defined before implementation (section 13).

After all inventoried, supported vSphere-backed PVCs in a namespace are copied, VMO publishes the required target-site selector in status and emits a supplemental Event asking the customer to add it to the Namespace:

```yaml
metadata:
  annotations:
    openshift.io/node-selector: >-
      migration.openshift.io/site=<migration-uid>-target
```

If the Namespace already has a selector, the customer merges the target-site requirement with the existing value rather than replacing unrelated scheduling constraints. VMO observes the resulting annotation, records the applied value, and verifies that it includes the required target-site constraint and remains compatible with the migrated workloads.

VMO must not request this namespace change before the authoritative final copy. The final source reader runs in the workload namespace and must still schedule on a source-vCenter Node. VMO does not publish `ReadyForResume` until it observes and validates the customer-applied selector.

## 5. Topology rules

### 5.1 Non-zonal clusters

The source reader requires the source site label. The target receiver requires the target site label. The temporary namespace selector keeps resumed workloads on the target vCenter while source and target workers coexist.

While source and target coexist, Infrastructure has more than one failure domain, so the CSI driver runs with topology enabled and every target PV gets `topology.csi.vmware.com/openshift-{region,zone}` node affinity.

### 5.2 Keeping CSI topology after source removal

The CSI operator enables topology only when Infrastructure has more than one failure domain, or when `ClusterCSIDriver.spec.driverConfig.vSphere.topologyCategories` is set. If the target has a single failure domain, removing the source failure domains would disable topology. New Nodes would then lack the topology labels that migrated PVs require, and those PVs would become unschedulable as workers are replaced.

This only applies when the target has a single failure domain; with more, the Infrastructure-derived categories take precedence. In that case, before removing source failure domains from Infrastructure, VMO sets `topologyCategories: [openshift-zone, openshift-region]` on the `ClusterCSIDriver` if it is unset. This is a supported field (used by UPI zonal clusters), not a workaround.

- If `topologyCategories` is already set to a different value, Planning reports a blocker: those categories would take over after source removal and no longer match the PVs' node affinity.
- Because the value equals the Infrastructure-derived one, the effective categories never change. VMO verifies that the CSI controller Deployment's arguments are unchanged after source failure domains are removed, so no CSI Pod restarts and no Node loses topology.
- `openshift-region`/`openshift-zone` tags must be on the target datacenter and vSphere cluster (which new hosts inherit), not on individual hosts. VMO verifies this before the change.
- Clearing the field after migration silently strands migrated PVs as Nodes are replaced. The release notes must call this out; an alert for "PVs with `topology.csi.vmware.com` affinity exist while CSI topology is disabled" is a follow-up.

### 5.3 Zonal clusters

VMO resolves a target failure domain for every PVC using this precedence:

```
source PVC target-failure-domain annotation
    -> failureDomainMappings[source PV failure domain]
    -> the only target failure domain, if spec.failureDomains has exactly one
    -> unresolved Planning blocker
```

A mapping is therefore only required when the target has more than one failure domain.

if a PVC's failure-domain can not be resolved during `Planning` phase it blocks migration with following values in `StorageWorkloadMigration`.

```yaml
status:
  conditions:
    - type: Ready
      status: "False"
      reason: TargetFailureDomainUnresolved
      message: >-
        PVC payments/data has no target failure-domain override and source
        failure domain source-a has no mapping

```

The StorageClass does not select the receiver's failure domain.

VMO uses both topology and `migration.openshift.io/site` selector to place source(rsync server) and target (rsync target) pods during migration.

For example:

```yaml
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: migration.openshift.io/site
                operator: In
                values: [<migration-uid>-target]
              - key: topology.kubernetes.io/region
                operator: In
                values: [new-vc]
              - key: topology.kubernetes.io/zone
                operator: In
                values: [zone-a]
```

Every set of PVCs mounted by one Pod must resolve to a compatible target topology. StatefulSet ordinals may resolve independently when each ordinal has its own Pod and PVC. An incompatible multi-PVC Pod is a Planning blocker.

The target PV's CSI-generated node affinity is the permanent zonal placement constraint. The temporary site selector adds protection while source workers remain and is removed after source compute retirement.

## 6. Target preparation and precopy

After the customer completes Planning and changes the migration to `Running`, VMO revalidates the persisted plan before infrastructure changes and follows this pipeline:

```
InfrastructurePrepared
DestinationInitialized
MultiSiteConfigured
TargetCapacityReady
StorageMigrated
  PrepareTargets
  PreCopy
  AwaitQuiesce
  FinalCopy
  CutOverAndValidate
WorkloadMigrated
SourceCleaned
Ready
```

`TargetCapacityReady` means the required target MachineSets exist and Ready, schedulable target Nodes are available in every selected target failure domain.

For each PVC, VMO revalidates the planned target failure domain and mover, then persists them in `VolumeCopy.spec` for execution. With the `Rsync` mover, VMO then:

1. Creates a temporary target PVC using the resolved target `WaitForFirstConsumer` StorageClass from `storageClassMapping`, or the source class when no mapping applies.
2. Creates a receiver Pod with the target site constraint and, for zonal clusters, the resolved target region and zone.  
3. Lets the receiver act as the first consumer that drives target volume provisioning.  
4. Verifies the receiver Node through Node \-\> Machine \-\> vCenter, verifies the bound PV topology, confirms target ownership through `CNSVolumeInfo` or a target CNS query, and validates capacity and storage policy.  
5. Creates a read-only source reader using independently resolved source placement and runs the optional, non-authoritative precopy.

Online RWO precopy remains limited to the Node already mounting the volume. RWOP is not eligible while the application owns the volume. An unavailable or unsafe precopy does not prevent an offline final copy.

When every eligible PVC in a namespace has completed precopy, or precopy is skipped, VMO sets the `StorageWorkloadMigration.status.phase` to `AwaitingQuiesce` and emits a supplemental Event. Status, rather than the Event, is the durable customer interface.

## 7. Customer quiesce and authoritative final copy

At `AwaitingQuiesce`, the customer:

1. Stops every workload that can mount a source PVC in the namespace.  
2. Suspends Jobs, CronJobs, application operators, GitOps reconciliation, or other mechanisms that could recreate a writer.  
3. Sets `spec.actions.finalCopyApproved: true`.

VMO independently verifies that no Pod has a writable source mount, and required workload VolumeAttachments have disappeared. For this first implementation, VMO may require all known consumers to be stopped even when a read-only consumer could theoretically remain.

VMO then performs one namespace cutover:

1. Run the mover's authoritative final copy for every PVC while writers remain absent.  
2. Recheck for writers before, during, and after the final copies. A writer that reappears invalidates final-copy success and returns the namespace to a blocked quiesce state.  
3. Persist successful final-copy attempts in their `VolumeCopy` statuses.  
4. Release mover resources. For `Rsync`, delete the source readers and target receivers, then wait for their VolumeAttachments to disappear so the target filesystems are flushed and unmounted before changing any binding.  
5. Switch every PVC in the namespace to the mover's `TargetVolumeRef` using the selected cutover strategy (in this case rebinding the same PVC name to the new PV). A same-name rebind must preserve and verify retained source and target PV identities before deleting any PVC.  
6. Verify every original PVC name or consumer reference resolves to the expected target PV and target volume handle.  
7. Set `StorageWorkloadMigration.status.phase` to `AwaitingPlacementUpdate`, publish the required selector, and ask the customer to add it to the Namespace.  
8. Observe and validate the customer-applied Namespace selector.  
9. Set `StorageWorkloadMigration.status.phase` to `ReadyForResume`.

## 8. Resume and validation

At `ReadyForResume`, VMO emits a supplemental Event asking the customer to resume the namespace's applications. The customer resumes them through their authoritative configuration, not by editing generated Pods or controllers.

VMO sets the phase to `Validating` and verifies that:

- application Pods run only on Nodes positively mapped to the target vCenter;  
- zonal Pods run in a topology allowed by every mounted target PV;  
- mounted CSI volume handles equal the recorded target handles;  
- no application configuration references a temporary or retained source PVC;  
- the expected workloads become ready; and  
- no source volume is attached to an application Pod.

The customer performs application-level validation and then sets `spec.actions.validationApproved: true`. VMO revalidates the observable checks and sets the phase to `WorkloadMigrationComplete`.

The operator repeats this lifecycle for each affected namespace. With the proposed Rsync mover, a blocked namespace retains its source data and prevents source compute retirement until all namespace workload migrations complete. When all `StorageWorkloadMigration` objects are `WorkloadMigrationComplete`, the top-level migration may continue `WorkloadMigrated` and retire source workers.

## 9. Placement cleanup

Placement cleanup removes the temporary scheduling dependency on migration labels. Before requesting cleanup, VMO verifies that:

- source worker MachineSets cannot recreate source workers;  
- no schedulable source-vCenter worker Nodes remain;  
- all namespace migrations reached `WorkloadMigrationComplete`; and
- no active rollback requires source placement.

It then sets each affected `StorageWorkloadMigration.status.phase` to `AwaitingPlacementCleanup`, publishes the recorded original selector, and emits a supplemental Event asking the customer to remove the migration site constraint. The customer may restore the recorded original value or apply a different final selector that is valid for the target cluster.

The customer then:

1. Removes `migration.openshift.io/site=<migration-uid>-target` from each Namespace's `openshift.io/node-selector` annotation.  
2. Restores the previous selector or installs the intended final target selector.  
3. Deletes and recreates any Pending application Pods that still contain the injected migration selector.

VMO verifies that the migration selector is absent and that the resulting selector remains compatible with target placement. For OpenShift system namespaces, the enhancement permits VMO to remove the temporary site constraint itself, preserving unrelated constraints. For customer namespaces, VMO observes the customer's changes.

VMO sets the phase to `CleaningPlacement`. Only after all Namespace and Pod dependencies on the temporary constraint are gone does it remove migration site labels from MachineSet templates and Nodes, then set `Succeeded`. Future target Nodes no longer need migration labels. Retained source volumes do not keep application scheduling constrained.

## 10. Source-data retention and future cleanup

Automated source-data deletion is deferred to a later phase of the enhancement. This phase retains source data and completes each namespace at `Succeeded` after workload validation and placement cleanup. `AwaitingSourceCleanup` and `CleaningSource` are not phases in the current enhancement lifecycle. The example's `sourceCleanupApproved` action is reserved for future use and does not gate completion here.

`SourceCleaned` in the top-level infrastructure pipeline concerns removal of source vCenter configuration and references. It must not be interpreted as confirmation that retained source FCDs were deleted. Retained data alone does not prevent `Ready`; the existing infrastructure health and cleanup checks still apply.

For the later source-cleanup implementation, preserve these requirements from the original plan:

- Require separate customer approval after successful application validation and rollback retention, unless an explicit early-cleanup acknowledgement applies.
- Verify that no rollback or active attachment still depends on a source volume.
- Publish source PV names, UIDs, volume handles, and source-vCenter ownership before accepting approval.
- Delete retained source PV objects and their underlying source FCDs, then verify absence through a source CNS query. Deleting the PV object alone is insufficient.

The cleanup API, retention configuration, phase transitions, and treatment of future source-consuming movers must be specified when that work is introduced. Retention preserves recovery material; after target writes, restoring old source bindings alone is not a safe rollback.

## 11. Failure behavior and retries

- A Planning or target-preparation failure leaves applications running on source storage.  
- A precopy failure leaves the source authoritative and may be retried.  
- A final-copy or cutover failure leaves the namespace quiesced by default and preserves both recorded source and target data identities.  
- A pause preserves transfer checkpoints, quiescence, and placement fences.  
- Before target writes are enabled, rollback may restore source bindings and source placement before allowing applications to resume. This does not apply to movers with `ConsumesSource`.  
- VMO never falls back to a different mover after `Prepare` has started for a volume.  
- After target writes are enabled, metadata-only rollback is forbidden. The workload must be frozen again and data reverse-synchronized, or recovered by an application-specific manual procedure.  
- VMO never adopts a same-name replacement PVC or PV when its recorded UID differs.

The enhancement specifies resumable, idempotent operations and conditions for retriable waits, but does not yet define retry timing or a complete recovery API. Persist progress in `VolumeCopy.status`; after restart, reconcile recorded state and any existing external task before starting another operation. Retriable problems remain in the current phase; `Failed` is reserved for errors requiring explicit recovery. Backoff, retry limits, transfer-Pod recreation, approval renewal after writer reappearance, and reset semantics remain implementation decisions to resolve. Do not infer a customer retry action from the existing approval booleans.

## 12. Validation and observability

The enhancement owns the end-to-end test matrix and graduation criteria. Repository validation should exercise the implementation boundaries behind those scenarios:

- Planning opt-in, direct-`Running` rejection, persisted plan recovery after restart, and invalidation of changed inputs.
- StorageClass mapping, source-pinned classes, CSI-migrated in-tree volume inventory, target policy validation, and compatible multi-PVC placement.
- Target capacity gates, site classification, WFFC provisioning, and target CNS ownership checks.
- Quiescence checks, final-copy checkpoints, attachment release, resumable rebinding, and refusal to adopt replacement PVC/PV UIDs.
- Customer placement and validation approvals, system-namespace cleanup ownership, source compute retirement, and removal of temporary placement dependencies.
- Preservation of CSI topology when the final target has one failure domain, including replacement Nodes after migration.

The minimum end-to-end scenarios are single-zone migration, multi-datacenter zonal migration, RWX volumes, CSI-migrated in-tree PVs, and source classes using datastore URLs. Additional candidates are zonal-to-single-zone migration and zonal migration across multiple compute clusters. The degree of end-to-end automation remains open. Tech Preview readiness requires passing end-to-end CI, completed documentation, successful QE scenarios, and merged engineering stories.

`StorageWorkloadMigration.status` and `VolumeCopy.status` are the durable progress and troubleshooting interfaces; Events supplement them. The enhancement proposes telemetry for successful and failed migrations, the volume migration method, and migration duration. Exact metrics and telemetry fields remain to be defined.

## 13. Open details to reconcile with the enhancement

- The workflow explicitly permits automatic selector removal in OpenShift system namespaces, while the operational API section says VMO never writes Namespace selectors. This plan follows the specific cleanup exception; namespace classification and the permitted mutation must be defined.
- Source-data deletion is deferred, but the enhancement retains `sourceCleanupApproved` and future cleanup safeguards. They are documented here as future work, not as active completion gates.
- Supported versions and topology combinations, SNO and OpenShift Kubernetes Engine support, downtime estimates, retry/recovery semantics, and exact persisted plan inputs remain open. `CnsRelocateVolume` remains future work.
