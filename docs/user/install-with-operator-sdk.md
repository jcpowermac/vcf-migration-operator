# Installing the VCF Migration Operator with Operator SDK

This installs the operator through OLM without starting a migration. A migration CR, target vCenter secret, and CSI removal are not required for installation.

## Prerequisites

- Cluster admin privileges and `oc` configured for the intended cluster
- Operator SDK v1.42.0 installed on `PATH`
- Git, Make, Go satisfying `go.mod`, and Podman
- Authorized registry repositories that the workstation and cluster can pull from, including access to SDK helper images

The Makefile defaults to Podman. The Dockerfile's Go builder must satisfy `go.mod`.

## Install the Operator

1. Build and push the operator image:

   ```bash
   export VERSION=0.1.0
   export IMG="registry.example.com/your-namespace/vcf-migration-operator:sdk-${VERSION}-unique-build"
   export BUNDLE_IMG="registry.example.com/your-namespace/vcf-migration-operator-bundle:sdk-${VERSION}-unique-build"

   make operator-image operator-push IMG="$IMG"
   ```

   Replace the registry/repositories and `unique-build` with your authorized destinations and a fresh tag suffix.

2. Generate, build, and push the bundle referencing the operator image:

   ```bash
   make bundle OPERATOR_SDK=operator-sdk IMG="$IMG" VERSION="$VERSION"
   make bundle-build bundle-push BUNDLE_IMG="$BUNDLE_IMG"
   ```

   `make bundle` regenerates and validates the bundle. Check the generated CSV's image and version in `bundle/manifests/vcf-migration-operator.clusterserviceversion.yaml` before publishing.

3. Create a fresh installation namespace, run the bundle, and check readiness:

   ```bash
   oc create namespace openshift-vcf-migration || exit 1
   operator-sdk run bundle "$BUNDLE_IMG" \
     --namespace openshift-vcf-migration \
     --install-mode AllNamespaces \
     --security-context-config restricted \
     --timeout 10m

   oc wait --for=jsonpath='{.status.phase}'=Succeeded "csv/vcf-migration-operator.v${VERSION}" \
     -n openshift-vcf-migration --timeout=10m
   oc rollout status deployment/vcf-migration-operator-controller-manager \
     -n openshift-vcf-migration --timeout=10m
   ```

4. Uninstall the operator:

   ```bash
   operator-sdk cleanup vcf-migration-operator --namespace openshift-vcf-migration
   ```

