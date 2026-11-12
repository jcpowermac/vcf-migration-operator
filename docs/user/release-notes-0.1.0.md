# Release notes for VCF Migration Operator 0.1.0

Issued: 12 November 2026

The VCF Migration Operator moves an OpenShift cluster running on VMware vSphere from one
vCenter to another, for example from an existing vCenter to a VMware Cloud Foundation (VCF)
workload domain. You describe the destination once in a `VmwareCloudFoundationMigration`
custom resource, and the Operator validates the plan and walks the cluster through the
migration phases.

## Technology Preview

Version 0.1.0 is a Technology Preview release. Technology Preview features are not supported
for production use, do not come with a support commitment, and might change without notice.
The `migration.openshift.io/v1alpha1` API is not yet stable.

Test this release on non-production clusters and report issues before planning a production
migration.

## New features and enhancements

- **Single-resource migration workflow.** One `VmwareCloudFoundationMigration` object drives
  the whole migration, from preflight validation to a final readiness check. Progress is
  reported as ordered conditions: `Accepted`, `InfrastructurePrepared`,
  `DestinationInitialized`, `MultiSiteConfigured`, `WorkloadMigrated`, `SourceCleaned`, and
  `Ready`. Each condition carries a reason, a message with progress detail such as ready
  worker counts, and matching Kubernetes events.

- **Preflight validation before anything changes.** The Operator fails fast and mutates
  nothing unless all checks pass:
  - every source failure domain and every source MachineSet is covered by the mapping
  - the `VSphereMultiVCenterDay2` feature gate is enabled and no cluster upgrade is in progress
  - all ClusterOperators report healthy
  - the vSphere CSI driver is removed and no vSphere CSI persistent volumes remain
  - no rollout resources are in flight that would interfere with the migration
  - the destination datacenter, compute cluster, datastore, networks, resource pool, and
    template are reachable, and the credentials hold the required vCenter privileges
  - probe VMs can start on and reach the destination networks

- **Explicit source-to-destination failure domain mapping.** You map each source failure
  domain to a destination failure domain. The mapping is resolved once during preflight,
  recorded in `status.failureDomainMappings`, and reused for every later phase, so the plan
  cannot drift mid-migration.

- **Destination preparation.** The Operator creates the target VM folder and the
  `openshift-region` and `openshift-zone` topology tags, and tags the migrated inventory with
  the destination cluster ownership tag.

- **Multi-site configuration.** The Operator updates the vSphere credentials secret, the
  `Infrastructure` resource, and the cloud provider configuration so the cluster recognizes
  both vCenters, then restarts the vSphere components and waits for them to become ready.

- **Worker and control plane migration.** The Operator creates one destination MachineSet per
  source MachineSet, waits for the new nodes, updates the control plane machine set, waits for
  the control plane rollout, then scales down and deletes the source MachineSets. Stalled
  deletion of old workers is surfaced with debounced warning events instead of silent waiting.

- **Source cleanup and metadata handoff.** The source vCenter is removed from the cluster
  configuration and credentials, and the Operator writes an installer-compatible metadata
  secret so that later cluster operations have a consistent view of the destination platform.

- **Sustained readiness gate.** `Ready` is set only after every ClusterOperator is available
  and stable and every MachineConfigPool has converged at full machine count, observed
  continuously for about three minutes. A failure or a gap in reconciliation restarts the
  observation window.

- **Pause and resume.** Set `spec.state` to `Paused` to stop the workflow between phases and
  back to `Running` to continue.

## Limitations in this release

- **Technology Preview only.** Do not use this release for production migrations.

- **vSphere storage is not supported.** The vSphere CSI driver must be removed from the
  cluster and no vSphere CSI persistent volumes may exist; preflight fails otherwise. Move
  workload data to another supported storage backend before migrating.

- **The migration is one-way.** There is no rollback and no cancel operation. Setting
  `spec.state` to `Paused`, or deleting the custom resource, stops new actions only:
  in-flight work is left running, and VMs, MachineSets, and tags already created on the
  destination are not removed. If a migration fails partway, recovering or reversing it is a
  manual, administrator-driven task.

- **One migration per cluster.** The Operator reconciles a single object,
  `openshift-vcf-migration/cluster`. Other objects are ignored with `Accepted=False` and a
  warning event. The completed record is retained until it holds completion evidence for the
  current spec, or until an administrator sets the
  `migration.openshift.io/allow-deletion: "true"` annotation.

- **The failure domain mapping is immutable.** After preflight records the mapping, editing
  `spec.failureDomains` or `spec.failureDomainMappings` is rejected. Prepare a new migration
  if the destination topology changes.

- **Prerequisites are enforced, not assumed.** The cluster must run an OpenShift version where
  the `VSphereMultiVCenterDay2` feature gate is enabled, with no upgrade in progress and all
  ClusterOperators healthy. The gate is enabled by default in OpenShift 5.0; a backport for
  OpenShift 4.20 through 4.22 is in progress, and migration of those versions becomes possible
  once the backport is available and the gate is enabled on the cluster. Preflight reports a
  clear error when a prerequisite is missing.

- **No proxy or FIPS support.** The Operator is not proxy-aware and does not run in FIPS mode.
