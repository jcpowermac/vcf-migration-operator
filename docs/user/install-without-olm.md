# Installing the VCF Migration Operator without OLM

## Prerequisites

- OpenShift cluster running on VMware vSphere
- Access to both source and target vCenter instances
- Cluster admin privileges
- `oc` CLI authenticated to the cluster
- vSphere CSI storage removed from the cluster (required by preflight; see [Preflight: vSphere CSI storage](#preflight-vsphere-csi-storage) below)

## Install the Operator

### Option A: Deploy from Source

```bash
make install
make deploy IMG=<registry>/vcf-migration-operator:latest
```

### Option B: Single Manifest

Generate and apply a consolidated installer YAML:

```bash
make build-installer IMG=<registry>/vcf-migration-operator:latest
oc apply -f dist/install.yaml
```

This creates the `openshift-vcf-migration` namespace, CRDs, RBAC, and the operator deployment.

## Create the Target vCenter Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: target-vcenter-creds
  namespace: openshift-vcf-migration
type: Opaque
data:
  vcenter-target.example.com.username: <base64-encoded-username>
  vcenter-target.example.com.password: <base64-encoded-password>
```

The secret keys must follow the format `<vcenter-fqdn>.username` and `<vcenter-fqdn>.password`.

## Preflight: vSphere CSI storage

vSphere storage is not supported in the current release. Preflight **fails** unless the vSphere CSI driver is removed from the cluster:

```bash
oc patch clustercsidrivers.operator.openshift.io csi.vsphere.vmware.com \
  --type merge -p '{"spec":{"managementState":"Removed"}}'
```

Setting `storages.operator.openshift.io/cluster` to `Unmanaged` or `Removed` is recommended; if it is still `Managed`, preflight passes with a warning.

## Create a Migration

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

Set `spec.state` to `Running` to begin the migration:

```bash
oc patch vcfm cluster -n openshift-vcf-migration --type merge -p '{"spec":{"state":"Running"}}'
```

The operator progresses through these phases:

1. **InfrastructurePrepared** -- preflight validation
2. **DestinationInitialized** -- target vCenter folders and topology tags created
3. **DestinationImageImported** -- RHCOS OVA imported as a VM template (skipped when `spec.image` is unset)
4. **MultiSiteConfigured** -- cluster recognizes both vCenters
5. **WorkloadMigrated** -- workers created on target (ready counts reported in the condition message), control plane rolled out, source MachineSets scaled to 0 and deleted
6. **SourceCleaned** -- source vCenter detached
7. **Ready** -- migration complete; requires all operators and MachineConfigPools to be stable and sustained for ~3 minutes

For YAML examples of the migration spec, see [Spec Examples](spec-examples.md).

Monitor progress:

```bash
oc get vcfm -n openshift-vcf-migration
oc describe vcfm cluster -n openshift-vcf-migration
oc get events -n openshift-vcf-migration --field-selector involvedObject.name=cluster
```

Useful events: `OldWorkersStalled` (Warning, old worker deletion blocked, e.g. by a PodDisruptionBudget; repeated at most every 5 minutes) and `SourceWorkersDeleted` (Normal, empty source MachineSets removed after cutover).

## Pausing and Resuming

```bash
# Pause
oc patch vcfm cluster -n openshift-vcf-migration --type merge -p '{"spec":{"state":"Paused"}}'
# Resume
oc patch vcfm cluster -n openshift-vcf-migration --type merge -p '{"spec":{"state":"Running"}}'
```

While paused, the `Ready` condition shows `False` with reason `Paused` and a message explaining how to resume. The workflow resumes where it left off.

## After Migration: Destroying Source Infrastructure

Once the migration reaches `Ready`, the operator has written a metadata secret (`{name}-metadata`, labeled `migration.openshift.io/metadata: true`) that is compatible with the OpenShift installer's vSphere schema:

```bash
oc get secret cluster-metadata -n openshift-vcf-migration \
  -o jsonpath='{.data.metadata\.json}' | base64 -d > metadata.json
```

Use this `metadata.json` with `openshift-install destroy cluster` to tear down the source infrastructure.

## Uninstall

```bash
make undeploy
make uninstall
```

Or if installed via the single manifest:

```bash
oc delete -f dist/install.yaml
```
