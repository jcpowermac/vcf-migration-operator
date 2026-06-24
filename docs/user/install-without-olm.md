# Installing the VCF Migration Operator without OLM

## Prerequisites

- OpenShift cluster running on VMware vSphere
- Access to both source and target vCenter instances
- Cluster admin privileges
- `oc` CLI authenticated to the cluster

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

Set `spec.state` to `Running` to begin the migration:

```bash
oc patch vcfm vcf-migration -n openshift-vcf-migration --type merge -p '{"spec":{"state":"Running"}}'
```

The operator progresses through these phases:

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

```bash
make undeploy
make uninstall
```

Or if installed via the single manifest:

```bash
oc delete -f dist/install.yaml
```
