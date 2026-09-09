# Spec Examples

The `VmwareCloudFoundationMigration` spec requires:

- `targetVCenterCredentialsSecret` — references a secret with keys `<vcenter-fqdn>.username` and `<vcenter-fqdn>.password`.
- `failureDomains` — one or more target failure domains (uses the OpenShift `configv1` `VSpherePlatformFailureDomainSpec` type). Each must specify an existing `topology.template` inventory path.

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

## UPI Cluster Cross-vCenter VMotion Migration

UPI clusters have no MachineSets or ControlPlaneMachineSet, so nodes must be
moved via cross-vCenter vMotion. Set `type: VMotion` on each role and choose
the relocation mode.

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
        mode: Cold
      maxUnavailable: 25%
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
        folder: /TargetDC/vm/my-cluster-infra-id
```

- Both `workers` and `controlPlane` are required when `nodeMigration` is set.
- `type` is per-role: `Recreate` (default) or `VMotion`.
- `vmotion` is required when `type` is `VMotion` and forbidden when `type` is `Recreate`.
- `vmotion.mode` controls relocation: `Auto` (try Hot, fall back to Cold), `Hot`, or `Cold`.
- `maxUnavailable` controls the rolling window (count or percent). Defaults to `1` (sequential). Control plane is validated to 1.
- `topology.template` is not needed when using VMotion — existing VMs are relocated, not rebuilt from a template.

## IPI Cluster (Recreate)

IPI clusters have MachineSets and a ControlPlaneMachineSet. The default
migration path (`nodeMigration` omitted or `type: Recreate`) creates new
VMs on the target vCenter from MachineSets/CPMS and scales down the source.

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
    controlPlane:
      type: Recreate
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

`topology.template` is required when using Recreate — new VMs are cloned from this template. Omitting `nodeMigration` entirely produces the same Recreate behavior.
