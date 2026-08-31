# vcf-migration-operator

Kubernetes operator for orchestrating migration of OpenShift clusters between VMware vCenters (e.g. VMware Cloud Foundation / VCF). Use the operator to drive the migration lifecycle.

## Description

vcf-migration-operator automates moving an OpenShift cluster from a source vCenter to a target vCenter. It is a Kubebuilder-based controller that reconciles the `VmwareCloudFoundationMigration` custom resource. It prepares infrastructure (credentials, failure domains), initializes the destination, configures multi-site, migrates workload (machines/nodes), and cleans up the source. The operator uses the cluster's Machine API and OpenShift-specific resources; vSphere operations are performed via govmomi against the target vCenter.

## Destination Topology Tags

During the `DestinationInitialized` phase, after preflight confirms the target vCenter user has the required tagging privileges, the operator creates the shared OpenShift topology tag model on the destination vCenter:

- The `openshift-region` and `openshift-zone` categories are created with `SINGLE` cardinality.
- New categories are created with associable types `Datacenter`, `ClusterComputeResource`, `Datastore`, and `Folder`.
- The region tag from `failureDomain.region` is attached to the target datacenter.
- The zone tag from `failureDomain.zone` is attached to the target compute cluster.
- Multiple failure domains can share the same region while using different zones.
- This mirrors the topology tag model used by the OpenShift vSphere cloud provider and CSI driver to discover failure domains.

The category ownership model is intentionally shared per vCenter rather than cluster-specific. If `openshift-region` or `openshift-zone` already exists, the operator reuses it and never deletes or rewrites it. Brownfield reuse is validated before proceeding: the category must keep `SINGLE` cardinality and must allow at least `Datacenter` and `ClusterComputeResource`. Extra associable types and different descriptions are tolerated. If an existing category is incompatible, the operator fails with an error that tells the administrator to update the category in the vSphere UI or delete it and let the operator recreate it.

## Destination Cluster Ownership Tags

Separate from topology tags, the operator also mirrors the OpenShift installer’s **cluster ownership** tagging on each destination vCenter (used for inventory ownership and destroy/cleanup):

- Category `openshift-<infraID>` and tag `<infraID>` are created with description `Added by openshift-install do not remove` and `SINGLE` cardinality.
- Associable types match the installer: `VirtualMachine`, `ResourcePool`, `Folder`, `Datastore`, and `StoragePod` (URN-prefixed).
- The ownership tag is attached to the destination infraID VM folder during `DestinationInitialized`.
- The machine-api-operator vSphere reconciler (`reconcileTags`) attaches the cluster ID tag to VMs by name from the machine cluster-ID label; this operator does **not** set `providerSpec.tagIDs`.

This is distinct from `openshift-region` / `openshift-zone`. Topology tags describe failure domains; ownership tags mark cluster-owned inventory.

## Getting Started

### Prerequisites
- OpenShift Container Platform 4.18+ or 5.0+ (or Kubernetes v1.30.0+)
- `cluster-admin` privileges
- OpenShift CLI (`oc`) or `kubectl`
- Container tool (`podman` or `docker`)
- Go v1.25.0+ (for local development)

### Installation via OLM v1 (Recommended)

On OpenShift 4.18+ and 5.0+, the operator is installed via Operator Lifecycle Manager v1 (OLM v1) using `ClusterCatalog` and `ClusterExtension`:

1. **Deploy the File-Based Catalog:**
   ```bash
   oc apply -f config/samples/olmv1/cluster_catalog.yaml
   ```

2. **Deploy the Operator Extension:**
   ```bash
   oc apply -f config/samples/olmv1/cluster_extension.yaml
   ```

3. **Verify Installation:**
   ```bash
   oc get clusterextension vcf-migration-operator
   oc get pods -n openshift-vcf-migration
   ```

For detailed instructions, step-by-step walkthrough, and configuration examples, see [Installing VCF Migration Operator with OLM v1](docs/install.md).

---

## Local Development

### Deploying Directly on Cluster (Non-OLM Dev Mode)

**Build and push the operator image:**

```sh
make operator-image operator-push IMG=<some-registry>/vcf-migration-operator:tag
```

**Install CRDs and Deploy Manager:**

```sh
make install
make deploy IMG=<some-registry>/vcf-migration-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**

Apply the sample migration CRs from `config/samples/`:

```sh
kubectl apply -k config/samples/
```

### To Uninstall Local Dev Deployment

**Delete instances and undeploy:**

```sh
kubectl delete -k config/samples/
make uninstall
make undeploy
```

## Container Images

The operator image is built with podman by default. The container tool can be overridden via `CONTAINER_TOOL`.

| Target | Description |
|--------|-------------|
| `make operator-image IMG=...` | Build the operator image |
| `make operator-push IMG=...` | Push the operator image |

`make deploy` uses kustomize to set the image in the manifests before applying, so the deployed image always matches the variable you pass.

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/vcf-migration-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/vcf-migration-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
operator-sdk edit --plugins=helm/v1-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing

Contributions are welcome. Please open an issue or PR and follow the code style and conventions described in this repo (see also `AGENTS.md` for build, lint, and test commands).

**NOTE:** Run `make help` for more information on all potential `make` targets.

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
