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

## Getting Started

### Prerequisites
- go version v1.25.0+
- podman
- kubectl version v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make operator-image operator-push IMG=<some-registry>/vcf-migration-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don't work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/vcf-migration-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**

Apply the sample migration CRs from `config/samples/` (if present):

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
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
