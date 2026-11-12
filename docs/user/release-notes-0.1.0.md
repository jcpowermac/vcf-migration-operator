# Release notes for VCF Migration Operator 0.1.0

<!-- Add the errata advisory link here once 0.1.0 is published. -->
Issued: 12 November 2026

The VCF Migration Operator moves an OpenShift cluster running on VMware vSphere from one
vCenter to another, for example from an existing vCenter into a VMware Cloud Foundation
workload domain. You describe the destination in a `VmwareCloudFoundationMigration` custom
resource; the Operator validates the plan, moves the workers and control plane, and detaches
the source vCenter.

## Technology Preview

Version 0.1.0 is a Technology Preview release. Technology Preview features are not supported
for production use, do not come with a support commitment, and might change without notice.
Test this release on non-production clusters, and report issues before planning a production
migration.

## Prerequisites

Prerequisites are enforced, not assumed. Preflight reports a clear error when a prerequisite is
missing, and the migration does not start until all of them are satisfied.

- The cluster must run an OpenShift version with the `VSphereMultiVCenterDay2` feature gate
  enabled. The gate is enabled by default in OpenShift 5.0; a backport for OpenShift 4.20 through
  4.22 is in progress, and migration of those versions becomes possible once the backport is
  available.
- The cluster must not be upgrading, and all ClusterOperators must be healthy.

## New features and enhancements

- **Migration from a single custom resource.** The VCF Migration Operator migrates an
  OpenShift cluster from one vCenter to another by using a single
  `VmwareCloudFoundationMigration` custom resource. The migration can be paused by setting
  `spec.state: Paused` and resumed by setting `spec.state: Running`.

- **Validation before anything changes.** Before making any changes, the Operator runs preflight
  checks that validate the destination vCenter topology and credentials, confirm cluster health
  and upgrade status, identify unsupported storage configurations, and compare network settings
  by starting a test VM on each destination failure domain. The migration does not proceed
  unless all checks pass.

- **Explicit source-to-destination failure domain mapping.** The `spec.failureDomainMappings`
  field specifies which source failure domain maps to which destination failure domain. The
  Operator records and enforces this mapping for the duration of the migration.

- **Destination preparation.** The Operator creates the target VM folder, sets the
  `openshift-region` and `openshift-zone` topology tags, and marks the migrated inventory with
  the destination cluster ownership tag.

- **Dual-vCenter operation and source vCenter removal.** The cluster is configured to recognize
  both the source and destination vCenters during migration. At completion, the source vCenter
  credentials and cloud provider configuration are removed. The Operator writes an
  installer-compatible platform metadata secret, which `openshift-install destroy cluster` can
  consume directly.

- **Workers and control plane moved to the destination.** The Operator creates new machines on
  the destination vCenter, waits for them to join the cluster, rolls out the control plane, and
  removes the source machines. If worker removal is delayed, the Operator reports progress as
  Kubernetes events.

- **Completion stability check.** The `VmwareCloudFoundationMigration` resource does not
  transition to `Ready` until all ClusterOperators and machine config pools have been stable for
  approximately three minutes.

- **Migration status reporting.** Migration status is reported through ordered status conditions,
  phase messages with counts such as ready worker nodes, and Kubernetes events that indicate the
  current phase and any blocking conditions.

## Known issues and limitations

- **Technology Preview only.** Do not use this release for production migrations.

- **vSphere storage is not supported.** The vSphere CSI driver must be removed and no vSphere
  CSI persistent volumes may remain, or preflight fails. Move workload data to another
  supported storage backend before migrating.

- **The network check is a comparison, not a connectivity test.** Preflight starts a test VM on
  the first network of each destination failure domain and compares its subnet and default
  gateway with the source node VMs. It does not prove connectivity to the destination and does
  not check any other destination network.

- **Test VMs can be left behind.** The Operator deletes its preflight test VMs, but if deletion
  fails, a VM remains in the destination inventory and must be removed manually.

- **The migration is one-way.** There is no rollback and no cancel operation. Pausing, or
  deleting the custom resource, stops new actions only: work already in progress continues, and
  VMs, machines, and tags already created on the destination are not removed. Recovering from a
  failed migration is a manual task.

- **One migration per cluster.** The Operator reconciles a single object,
  `openshift-vcf-migration/cluster`. The completed record is retained until it holds completion
  evidence for the current spec, or until an administrator sets the
  `migration.openshift.io/allow-deletion: "true"` annotation.

- **The recorded mapping is enforced.** After preflight, `spec.failureDomainMappings` is
  compared with the mapping the Operator recorded. Reordering or clearing the list is accepted,
  but changing an association, adding an entry, or removing one sets `Accepted=False` with
  reason `MappingRejected`. Editing `spec.failureDomains` does not change the recorded mapping
  or the destination already in use.

- **Topology tags on the destination are reused, not checked.** The Operator creates
  `openshift-region` and `openshift-zone` tags only where the destination datacenter or compute
  cluster has no tag in that category, and it does not compare existing tag values with
  `spec.failureDomains`. Review the topology tags on the target vCenter before migrating.

- **No proxy or FIPS support.** The Operator is not proxy-aware and does not run in FIPS mode.
