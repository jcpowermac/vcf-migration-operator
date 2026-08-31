# Installing VCF Migration Operator with OLM v1

This guide explains how to install and deploy the VCF Migration Operator using Operator Lifecycle Manager v1 (OLM v1 / `operator-controller`) on OpenShift Container Platform 4.18+ and 5.0+.

---

## Overview

The VCF Migration Operator is packaged as an OLM bundle and distributed via a File-Based Catalog (FBC). In OLM v1, extensions are declared cluster-wide via `ClusterCatalog` and `ClusterExtension` custom resources, managed by `operator-controller` and `catalogd`.

---

## Prerequisites

Before installing the operator:
1. **Cluster Administration Privileges**: You must have `cluster-admin` permissions on the target OpenShift cluster.
2. **OpenShift with OLM v1 Enabled**: Ensure OLM v1 (`operator-controller` and `catalogd`) is active on your cluster (standard on OpenShift 4.18+ and 5.0+).
3. **vSphere Environment**: Access to both source and destination VMware vCenters with required privileges (tagging, inventory, storage, network).
4. **OpenShift CLI (`oc`)**: Configured to access your cluster.

---

## Installation Steps

### Step 1: Create Operator Namespace

Create the dedicated namespace for running the VCF Migration Operator controller:

```bash
oc create namespace openshift-vcf-migration
```

### Step 2: Create the ClusterCatalog

Create a `ClusterCatalog` resource pointing to the operator's File-Based Catalog image:

```yaml
# cluster_catalog.yaml
apiVersion: olm.operatorframework.io/v1
kind: ClusterCatalog
metadata:
  name: vcf-migration-operator-catalog
spec:
  sourceType: Image
  image: registry.ci.openshift.org/origin/vcf-migration-operator-catalog:latest
  priority: 0
```

Apply the manifest:

```bash
oc apply -f config/samples/olmv1/cluster_catalog.yaml
```

Verify that the catalog is unpacked and available:

```bash
oc get clustercatalog vcf-migration-operator-catalog
```

Expected status:
```text
NAME                              SOURCE TYPE   STATUS      AGE
vcf-migration-operator-catalog    Image         Serving     30s
```

### Step 3: Create the ClusterExtension

Create a `ClusterExtension` resource to install the operator from the catalog:

```yaml
# cluster_extension.yaml
apiVersion: olm.operatorframework.io/v1
kind: ClusterExtension
metadata:
  name: vcf-migration-operator
spec:
  packageName: vcf-migration-operator
  channel: dev-preview
  installNamespace: openshift-vcf-migration
```

Apply the manifest:

```bash
oc apply -f config/samples/olmv1/cluster_extension.yaml
```

---

## Step 4: Verify Installation

Check that the `ClusterExtension` successfully resolves and installs the bundle:

```bash
oc get clusterextension vcf-migration-operator
```

Expected output:
```text
NAME                      PACKAGE                  VERSION   STATUS      AGE
vcf-migration-operator    vcf-migration-operator   0.0.1     Installed   1m
```

Verify the operator pod is running and ready:

```bash
oc get pods -n openshift-vcf-migration
```

Expected output:
```text
NAME                                                        READY   STATUS    RESTARTS   AGE
vcf-migration-operator-controller-manager-xxxxxxxxx-xxxxx   1/1     Running   0          45s
```

Verify that the `VmwareCloudFoundationMigration` CRD is registered:

```bash
oc get crd vmwarecloudfoundationmigrations.migration.openshift.io
```

---

## Step 5: Preparing Migration Resources

### 1. Create Target vCenter Credentials Secret

Create the Secret containing credentials for the destination vCenter in `openshift-vcf-migration`:

```bash
oc create secret generic target-vcenter-creds \
  --namespace openshift-vcf-migration \
  --from-literal=vcenter-target.example.com.username="administrator@vsphere.local" \
  --from-literal=vcenter-target.example.com.password="secretPassword"
```

### 2. Create Migration Custom Resource

Create and apply the `VmwareCloudFoundationMigration` custom resource:

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
        resourcePool: /TargetDC/host/TargetCluster/Resources
        datastore: /TargetDC/datastore/TargetDatastore
        folder: /TargetDC/vm/my-cluster-infra-id
        networks:
          - "VM Network"
        template: /TargetDC/vm/rhcos-template
```

Apply the migration resource:

```bash
oc apply -f config/samples/migration_v1alpha1_vmwarecloudfoundationmigration.yaml
```

To initiate migration, update `spec.state` from `Pending` to `Running`:

```bash
oc patch vmwarecloudfoundationmigration cluster -n openshift-vcf-migration \
  --type=merge -p '{"spec":{"state":"Running"}}'
```

---

## Troubleshooting

### Check ClusterExtension Status and Conditions

If the extension fails to install, inspect the detailed conditions on the `ClusterExtension`:

```bash
oc describe clusterextension vcf-migration-operator
```

Look for:
- `Installed`: Whether installation succeeded.
- `Progressing`: Whether resolution or rollout is currently underway.
- `Reason` / `Message`: Explanations if resolution failed (e.g. missing package or channel).

### Check Catalogd and Operator Controller Logs

Inspect the OLM v1 system component logs for catalog extraction or bundle installation issues:

```bash
oc logs -n openshift-operator-controller deployment/operator-controller -c manager
oc logs -n openshift-catalogd deployment/catalogd-controller-manager -c manager
```

### Check Migration Operator Logs

View logs from the running operator:

```bash
oc logs -n openshift-vcf-migration -l app.kubernetes.io/name=vcf-migration-operator -c manager -f
```
