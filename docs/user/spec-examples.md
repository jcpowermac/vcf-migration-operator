# Spec Examples

The `VmwareCloudFoundationMigration` spec requires:

- `targetVCenterCredentialsSecret` — references a secret with keys `<vcenter-fqdn>.username` and `<vcenter-fqdn>.password`.
- `failureDomains` — one or more target failure domains (uses the OpenShift `configv1` `VSpherePlatformFailureDomainSpec` type).

`state` is optional (defaults to `Pending`; set to `Running` to start the migration).
`image` is optional; see the full field reference in the [API reference](../dev/api.md).

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

## Auto-Resolved RHCOS Image

Set `image` without `ovaUrl` (an empty `image` object) and the operator resolves the RHCOS OVA from the CVO-delivered `coreos-bootimages` ConfigMap, imports it as a VM template into each failure domain, and populates `topology.template` automatically. `topology.template` is not required in this mode. When `image` is omitted entirely, no image import happens and `topology.template` must be set manually in each failure domain.

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
  image: {}
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

## Custom OVA URL with Disk Provisioning

For air-gapped environments or when you want to control the OVA source and disk provisioning mode, set `image`:

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
  image:
    ovaUrl: "https://internal-mirror.example.com/rhcos-4.17.0-x86_64-ova.ova"
    diskProvisioning: thin
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

- `image.ovaUrl` must be an `https://` URL ending in `.ova` (a query string is allowed for proxy tokens or integrity digests). When set, it overrides auto-resolution.
- `image.diskProvisioning` is one of `thin`, `thick`, or `eagerZeroedThick`; when omitted, vSphere defaults to the provisioning type specified in the OVF descriptor.

When `image` is set, the operator imports the OVA during the `DestinationImageImported` phase and sets `topology.template` for each failure domain. Progress is reported in `status.image`. If `image.ovaUrl` changes after import, the operator deletes and re-imports only the templates it imported itself; user-pre-configured templates are never touched.

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

- `type` is per-role: `VMotion` (default) or `Recreate`.
- `vmotion.mode` controls relocation: `Auto` (try Hot, fall back to Cold), `Hot`, or `Cold`.
- `maxUnavailable` controls the rolling window (count or percent). Control plane is validated to 1.
- When `type` is `Recreate`, the `vmotion` field is forbidden (rejected by CEL validation).
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
