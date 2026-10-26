# Spec Examples

The `VmwareCloudFoundationMigration` spec requires:

- `targetVCenterCredentialsSecret` — references a secret with keys `<vcenter-fqdn>.username` and `<vcenter-fqdn>.password`.
- `failureDomains`: one or more target failure domains (uses the OpenShift `configv1` `VSpherePlatformFailureDomainSpec` type). Every failure domain requires `topology.template` for this release, including VMotion selections.

`state` is optional (defaults to `Pending`; set to `Running` to start the migration).

The API also accepts `Planning` and `Paused`. `Planning` runs only the non-disruptive preflight and storage planning checks and never starts migration work; set `state` to `Running` once the plan has no blockers.
See the full field reference in the [API reference](../dev/api.md).

## Single Target Failure Domain

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: VmwareCloudFoundationMigration
metadata:
  name: cluster
  namespace: openshift-vcf-migration
spec:
  state: Pending
  targetVCenterCredentialsSecret:
    name: target-vcenter-creds
    namespace: openshift-vcf-migration
  failureDomains:
    - name: target-fd-1
      region: target-region
      zone: target-zone-1
      server: vcenter-target.example.com
      topology:
        datacenter: TargetDC
        computeCluster: /TargetDC/host/TargetCluster
        datastore: /TargetDC/datastore/TargetDatastore
        networks:
          - "VM Network"
        resourcePool: /TargetDC/host/TargetCluster/Resources
        template: /TargetDC/vm/rhcos-template
        folder: /TargetDC/vm/my-cluster-infra-id
```

## Multiple Target Failure Domains

With more than one source or destination failure domain,
`failureDomainMappings` is required and must cover every source and
destination failure domain exactly once, and each destination failure domain must be
hosted on a vCenter other than the source. Source names come from the cluster's
`Infrastructure` resource.

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: VmwareCloudFoundationMigration
metadata:
  name: cluster
  namespace: openshift-vcf-migration
spec:
  state: Pending
  targetVCenterCredentialsSecret:
    name: target-vcenter-creds
    namespace: openshift-vcf-migration
  failureDomains:
    - name: target-fd-1
      region: target-region
      zone: target-zone-1
      server: vcenter-target.example.com
      topology:
        datacenter: TargetDC
        computeCluster: /TargetDC/host/TargetCluster1
        datastore: /TargetDC/datastore/TargetDatastore
        networks:
          - "VM Network"
        resourcePool: /TargetDC/host/TargetCluster1/Resources
        template: /TargetDC/vm/rhcos-template-1
        folder: /TargetDC/vm/my-cluster-infra-id
    - name: target-fd-2
      region: target-region
      zone: target-zone-2
      server: vcenter-target.example.com
      topology:
        datacenter: TargetDC
        computeCluster: /TargetDC/host/TargetCluster2
        datastore: /TargetDC/datastore/TargetDatastore
        networks:
          - "VM Network"
        resourcePool: /TargetDC/host/TargetCluster2/Resources
        template: /TargetDC/vm/rhcos-template-2
        folder: /TargetDC/vm/my-cluster-infra-id
  failureDomainMappings:
    - source: source-fd-1
      destination: target-fd-1
    - source: source-fd-2
      destination: target-fd-2
```

Multiple failure domains can share the same `region` while using different `zone` values. Region/zone are mirrored as OpenShift topology tags on the destination vCenter.

Tag reuse on previously-used hardware: if the target datacenter already has any tag in the `openshift-region` category (or the cluster in `openshift-zone`), the operator reuses it and skips tag creation. The spec values still drive the target-side OpenShift failure-domain config and node topology labels, so set them to match the existing tags (check with `govc tags.attached.ls -r /<datacenter>` and `... -r /<datacenter>/host/<cluster>`).

## Cross-vCenter VMotion API shape (execution not implemented)

> **API foundation only.** The CRD accepts the explicit VMotion shape below,
> but the migration controller does not implement the VMotion execution
> contract. Schema support is not runtime safety or an enablement signal. Do not
> use this selection until a separately reviewed and qualified backend exists.
> See the [first-release design](../plans/upi-cross-vcenter-vmotion-design.md).

Both roles must use the same explicit mode. `Auto`, omitted mode, mixed modes,
and mixed VMotion/Recreate roles are rejected. VMotion uses the effective
failure-domain mapping; `topology.template` is still required by the current
API release, although the relocation operation does not consume it:

```yaml
nodeMigration:
  workers:
    type: VMotion
    vmotion:
      mode: Hot # Use Cold on both roles for a Cold API selection
    maxUnavailable: 1
  controlPlane:
    type: VMotion
    vmotion:
      mode: Hot
    maxUnavailable: 1
```

For a Cold API shape, set both `vmotion.mode` fields to `Cold`. The API shape
does not itself authorize shutdown, relocation, power-on, recovery, or replay.
`maxUnavailable` is an integer; VMotion worker availability is limited to one.
`topology.template` remains required for VMotion in this release.

## IPI Cluster (Recreate)

IPI clusters have MachineSets and a ControlPlaneMachineSet. The legacy path
creates new VMs on the target vCenter from MachineSets/CPMS and scales down the
source. Omit `nodeMigration` to retain that path, or explicitly select
`type: Recreate` for both roles:

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: VmwareCloudFoundationMigration
metadata:
  name: cluster
  namespace: openshift-vcf-migration
spec:
  state: Pending
  targetVCenterCredentialsSecret:
    name: target-vcenter-creds
    namespace: openshift-vcf-migration
  nodeMigration:
    workers:
      type: Recreate
      maxUnavailable: 3
    controlPlane:
      type: Recreate
      maxUnavailable: 1
  failureDomains:
    - name: target-fd-1
      region: target-region
      zone: target-zone-1
      server: vcenter-target.example.com
      topology:
        datacenter: TargetDC
        computeCluster: /TargetDC/host/TargetCluster
        datastore: /TargetDC/datastore/TargetDatastore
        networks:
          - "VM Network"
        resourcePool: /TargetDC/host/TargetCluster/Resources
        template: /TargetDC/vm/rhcos-template
        folder: /TargetDC/vm/my-cluster-infra-id
```

`topology.template` is required for every engine in this release. Legacy and
Recreate paths clone new VMs from this template. Worker `maxUnavailable` values
above one remain valid for Recreate.
