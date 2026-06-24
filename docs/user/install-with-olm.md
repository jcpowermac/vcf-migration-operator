# Installing the VCF Migration Operator with OLM

## Prerequisites

- OpenShift cluster running on VMware vSphere
- Access to both source and target vCenter instances
- Cluster admin privileges

## Install the Operator

1. Build and push the operator and bundle images:

```bash
make operator-image operator-push IMG=<registry>/vcf-migration-operator:latest
make bundle-build bundle-push BUNDLE_IMG=<registry>/vcf-migration-operator-bundle:v0.0.1
```

2. Build and push the catalog image:

```bash
make catalog-build catalog-push CATALOG_IMG=<registry>/vcf-migration-operator-catalog:v0.0.1
```

3. Create the `CatalogSource` on the cluster:

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: vcf-migration-operator
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: <registry>/vcf-migration-operator-catalog:v0.0.1
  displayName: VCF Migration Operator
```

4. Create the target namespace and an `OperatorGroup`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-vcf-migration
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: vcf-migration-operator
  namespace: openshift-vcf-migration
spec:
  targetNamespaces:
    - openshift-vcf-migration
```

5. Install from OperatorHub in the OpenShift console, or create a `Subscription`:

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: vcf-migration-operator
  namespace: openshift-vcf-migration
spec:
  channel: dev-preview
  name: vcf-migration-operator
  source: vcf-migration-operator
  sourceNamespace: openshift-marketplace
```

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

## Create a Migration

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

Set `spec.state` to `Running` to begin the migration. The operator progresses through these phases:

1. **InfrastructurePrepared** -- preflight validation
2. **DestinationInitialized** -- target vCenter folders and topology tags created
3. **MultiSiteConfigured** -- cluster recognizes both vCenters
4. **WorkloadMigrated** -- workers created on target, control plane rolled out, source MachineSets scaled to 0
5. **SourceCleaned** -- source vCenter detached
6. **Ready** -- migration complete

Monitor progress:

```bash
oc get vcfm -n openshift-vcf-migration
oc describe vcfm vcf-migration -n openshift-vcf-migration
```

## Console Plugin (Optional)

Deploy the web UI for managing migrations:

```bash
make console-plugin-image console-plugin-push CONSOLE_PLUGIN_IMG=<registry>/vcf-migration-console-plugin:latest
make deploy-console-plugin CONSOLE_PLUGIN_IMG=<registry>/vcf-migration-console-plugin:latest
```

## Uninstall

Remove the operator via the OpenShift console or delete the `Subscription` and `ClusterServiceVersion`:

```bash
oc delete subscription vcf-migration-operator -n openshift-vcf-migration
oc delete csv -n openshift-vcf-migration -l operators.coreos.com/vcf-migration-operator.openshift-vcf-migration=
```
