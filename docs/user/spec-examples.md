# Spec Examples

The `VmwareCloudFoundationMigration` spec requires:

- `targetVCenterCredentialsSecret` — references a secret with keys `<vcenter-fqdn>.username` and `<vcenter-fqdn>.password`.
- `failureDomains`: one or more target failure domains (uses the OpenShift `configv1` `VSpherePlatformFailureDomainSpec` type). Each must specify an existing `topology.template` inventory path. The planned Hot/Cold VMotion path retains this requirement for the temporary network probe, not to recreate cluster nodes; see its design-only example below.

`state` is optional (defaults to `Pending`; set to `Running` to start the migration).
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
```

Multiple failure domains can share the same `region` while using different `zone` values. Region/zone are mirrored as OpenShift topology tags on the destination vCenter.

Tag reuse on previously-used hardware: if the target datacenter already has any tag in the `openshift-region` category (or the cluster in `openshift-zone`), the operator reuses it and skips tag creation. The spec values still drive the target-side OpenShift failure-domain config and node topology labels, so set them to match the existing tags (check with `govc tags.attached.ls -r /<datacenter>` and `... -r /<datacenter>/host/<cluster>`).

## Cross-vCenter VMotion Migration (not yet implemented)

> **Status: Design only.** The VMotion engine is not yet implemented in the
> controller. Submitting this CR today will not invoke cross-vCenter Relocate.
> See the [Hot and Cold relocation design](../plans/upi-cross-vcenter-vmotion-design.md)
> for the narrow first delivery and the [broader node migration design](../plans/upi-node-migration-design.md)
> for background and future proposals. The example below targets the first delivery.

The proposed path requires verified evidence that every planned node VM is
unmanaged by the Machine API. Absent/zero-replica MachineSets or no Running
Machines do not establish that by themselves. Positive-replica sets with no
Running Machines and incomplete/conflicting ownership evidence block dispatch.
Once eligibility is verified, explicitly select `type: VMotion` on both roles
as illustrated below. The current controller does not implement this path.

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
      type: VMotion
      vmotion:
        mode: Hot
      maxUnavailable: 1
    controlPlane:
      type: VMotion
      vmotion:
        mode: Hot
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
        template: /TargetDC/vm/rhcos-template # Existing network-probe template
        folder: /TargetDC/vm/my-cluster-infra-id
```

- Both `workers` and `controlPlane` are required when `nodeMigration` is set.
- For the proposed first delivery, both roles explicitly select `type: VMotion` with a `vmotion` block. API admission/default details are a prerequisite to implementation; the current branch does not expose these fields.
- The first implementation accepts explicit `vmotion.mode: Hot` or `Cold`, with the same mode for both roles. This example selects Hot; change both modes to Cold for powered-off relocation. Auto and automatic mode fallback are unsupported. Cold explicitly authorizes the shutdown/relocation/power-on sequence; a Hot failure never authorizes power-off.
- `maxUnavailable` is an integer node count, never a percentage. The first implementation accepts only `1` for both roles and processes one node at a time across the cluster.
- Neither Hot nor Cold relocation rebuilds cluster-node VMs from a template. The first implementation retains the existing network-probe VM check, so `topology.template` must reference an existing target template for that temporary probe. Cluster nodes are relocated, not recreated.

## IPI Cluster (Recreate)

IPI clusters have MachineSets and a ControlPlaneMachineSet. The current
migration path creates new VMs on the target vCenter from MachineSets/CPMS
and scales down the source. The example below uses the current API and
omits `nodeMigration`, which is not exposed by this branch's API. Explicit
per-role `type: Recreate` selection is proposed design only.

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

`topology.template` is required for the current MachineSet/CPMS replacement path because new VMs are cloned from this template. No `nodeMigration` field is needed to select this existing behavior.
