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
