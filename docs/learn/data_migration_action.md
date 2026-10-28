# Data Migration Action Items

Open decisions and follow-ups for CSI volume migration (see
[csi-volume-migration-plan.md](../plans/csi-volume-migration-plan.md)).
`StorageMigrationSpec` is the Go type for `VmwareCloudFoundationMigration.spec.storage`;
its `DataMigrationStrategy` field is exposed as `spec.storage.dataMigrationStrategy`.

## Restrict `dataMigrationStrategy` to `Rsync` until other movers exist

`Rsync` is the only mover we can realistically support today, but
`VolumeDataMigrationStrategy` currently also allows `Auto` (the default) and
defines a `CnsNative` constant. Decide on `Auto`, and until then:

- Change the enum to `Rsync` only and default `StorageMigrationSpec.dataMigrationStrategy`
  to `Rsync`. Widening the enum later is backward compatible; removing a value
  after release is not. Defaults are written when an object is saved, so objects
  created now keep `Rsync` even if a later release defaults to `Auto`.
- Remove the `Auto` and `CnsNative` constants, and add them back with the code
  that supports them.
- Drop the `StorageMigrationSpec` CEL rule on `cleanupSourceData`; with only
  `Rsync` it can never fail. The current rule is also inverted: when `CnsNative`
  arrives, the rule should be "`CnsNative` requires `cleanupSourceData: true`",
  because a mover that consumes the source cannot retain source data.
- Keep `VolumeCopy.spec.dataMigrationStrategy`'s `self != 'Auto'` rule, since
  `VolumeCopy` reuses the same type.
- Make `dataMigrationStrategy` immutable once the migration leaves `Planning`.
- Update the plan's API example from `<Auto|Rsync>` to `Rsync`, noting `Auto`
  and other movers as future additions.
- If a consuming mover is added, decide whether `finalCopyApproved` is enough
  consent to destroy the source, or whether a dedicated acknowledgement in
  `spec.actions` is needed.

## CSI driver operator issues to file

- **Tagging skips non-first vCenters.** `nextCheck` in
  `storageclasscontroller.go:153-169` is shared by all vCenters, so after the
  first pass only the first vCenter is re-checked. Failure domains added later
  to another vCenter are not tagged until the operator restarts. VMO should add
  the target vCenter and its failure domains in one Infrastructure update.
- **One failing vCenter blocks the rest.** An error on any vCenter returns early
  (`storageclasscontroller.go:114-118`), so a flaky source vCenter stops policy
  creation in the target.
- **Topology can be disabled while PVs depend on it.** Once enabled, topology
  should stay on while any PV has `topology.csi.vmware.com` affinity. Until
  then VMO sets `ClusterCSIDriver` `topologyCategories` (plan section 5.2).

Because of the first two, VMO must verify the storage policy and datastore tags
in the target vCenter before `PreparingTargets` rather than assume them.

## Planning blockers to define

- In-tree vSphere PVs migrated to CSI (volume handle is a VMDK path, and
  `migration-datastore-url` is only set with a single vCenter).
- Static or non-CNS-registered PVs.
- RWX vSAN file volumes (target StorageClass must be file-capable).
- PV capacity that differs from the PVC request (size the target from the PV).
- Source volumes with CNS snapshots (source FCD deletion fails while snapshots
  exist).
- Platform namespaces such as `openshift-monitoring`, whose operators recreate
  writers; define an exclusion list or a separate procedure.
- Target failure domains that reuse a source region and zone pair, which breaks
  topology-based placement.

## Design gaps

- **Downtime estimate (Goal 3).** Record bytes, files scanned, and duration for
  each precopy pass in `VolumeCopy.status`, run at least two passes to measure
  the steady-state delta, and publish an estimated final-copy duration before
  `AwaitingQuiesce`. The final rsync pass is dominated by the metadata walk on
  volumes with many files, not by changed bytes.
- **Rollback action.** Section 11 describes rollback, but `spec.actions` has no
  rollback field and the recovery action out of `Failed` is undefined.
- **Same-name rebind mechanics.** Set both PVs to `Retain`, persist the rebind
  intent in `VolumeCopy.status` before deleting the PVC, pre-bind the target
  PV's `claimRef`, recreate the PVC with original labels, annotations, and
  ownerReferences, strip binding annotations, and record the new PVC UID.
- **Data integrity check** after the final copy (at least file count and bytes;
  optionally a checksum mode).
- **Pause semantics** for in-flight rsync Pods.
- **Transfer Pod privileges.** rsync needs root or `CAP_DAC_READ_SEARCH` and
  must preserve ownership, xattrs, and ACLs in the workload namespace. Define
  the ServiceAccount, SCC, and PodSecurity handling.
- **Source cleanup ordering.** Deleting a source PV relies on the CSI driver
  still knowing the source vCenter. If the source vCenter is removed from the
  CSI configuration first, delete through CNS with govmomi instead.
- **Throttling.** Cap concurrent copies per namespace and cluster, and consider
  a bandwidth limit.
- **Approval authority.** Namespace admins who can edit
  `StorageWorkloadMigration` can set `sourceCleanupApproved`. Decide whether
  that is intended and which aggregated roles to ship.
- **Customer toil.** Each namespace needs two manual selector edits. Consider a
  cluster-level opt-in for VMO to manage the namespace selector.
- **Pipeline conditions.** `TargetCapacityReady` and `StorageMigrated` do not
  exist yet; target worker creation may currently live in `WorkloadMigrated`
  and need splitting.
- **Feature gate** for TP in 5.1.

## Validation and follow-ups

- Test the non-zonal to multi-failure-domain transition: topology is enabled
  for the first time, the provisioner restarts, and Nodes registered without
  topology may not report it until the CSI node plugin restarts.
- Release note and alert: clearing `ClusterCSIDriver` `topologyCategories`
  after migration strands migrated PVs as Nodes are replaced.
- Check `migtools/pvc-transfer` maintenance status and whether its Kubernetes
  and controller-runtime versions align with ours.

## Open questions from the Planning phase plan

See [01-planning-phase.md](../csi-migration-plan/01-planning-phase.md).

- **User workload monitoring.** Planning blocks vSphere CSI PVCs in any
  `openshift-*` namespace except `openshift-monitoring` and
  `openshift-image-registry`. Clusters with user workload monitoring and
  persistent storage have PVCs in `openshift-user-workload-monitoring` and would
  be blocked until those PVCs (and their metrics history) are removed. The same
  cluster-monitoring-operator manages it, so the `openshift-monitoring` quiesce
  procedure likely applies. Decide whether to add it to the allow-list.
- **Locking spec after planning.** Decide whether to make `spec.failureDomains`
  and `spec.storage` immutable with CEL once `spec.state` leaves
  `Pending`/`Planning` (for example
  `oldSelf.state in ['Pending', 'Planning'] || self.storage == oldSelf.storage`).
  This is coarse: it also locks them while `Paused`.

## Housekeeping
- `bin/golangci-lint` v2.1.0 panics with the Go 1.27 toolchain; update it.
