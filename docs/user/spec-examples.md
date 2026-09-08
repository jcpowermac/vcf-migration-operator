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
  name: vcf-migration
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
  name: vcf-migration
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

Omit `image` entirely and the operator resolves the RHCOS OVA from the CVO-delivered `coreos-bootimages` ConfigMap, imports it as a VM template into each failure domain, and populates `topology.template` automatically. `topology.template` is not required in this mode.

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: VmwareCloudFoundationMigration
metadata:
  name: vcf-migration
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
        folder: /TargetDC/vm/my-cluster-infra-id
```

## Custom OVA URL with Disk Provisioning

For air-gapped environments or when you want to control the OVA source and disk provisioning mode, set `image`:

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: VmwareCloudFoundationMigration
metadata:
  name: vcf-migration
  namespace: openshift-vcf-migration
spec:
  state: Pending
  targetVCenterCredentialsSecret:
    name: target-vcenter-creds
    namespace: openshift-vcf-migration
  image:
    ovaUrl: "https://internal-mirror.example.com/rhcos-4.17.0-x86_64-ova.ova?token=abc123"
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
- `image.diskProvisioning` is one of `thin` (default), `thick`, or `eagerZeroedThick`.

When `image` is set, the operator imports the OVA during the `DestinationImageImported` phase and sets `topology.template` for each failure domain. Progress is reported in `status.image`.
