# Development Guide

## Prerequisites

- Go 1.25+
- Podman (or Docker, set `CONTAINER_TOOL=docker`)
- `oc` or `kubectl` CLI
- Access to an OpenShift cluster on vSphere (for integration testing)

## Building

```bash
# Build the operator binary
make build

# Build the container image
make operator-image IMG=<registry>/vcf-migration-operator:dev

# Build the console plugin
make console-plugin-frontend
make console-plugin-backend
make console-plugin-image CONSOLE_PLUGIN_IMG=<registry>/vcf-migration-console-plugin:dev
```

## Code Generation

After modifying types in `api/v1alpha1/`:

```bash
# Regenerate DeepCopy methods and CRD manifests
make generate manifests
```

This runs `controller-gen` to produce:
- `zz_generated.deepcopy.go` — DeepCopy implementations
- `config/crd/bases/` — CRD YAML
- `config/rbac/role.yaml` — ClusterRole from `//+kubebuilder:rbac` markers

## Running Locally

```bash
# Install CRDs, then run the controller against your current kubeconfig
make install
make run
```

The controller listens on `:8081` (health) and `:8443` (metrics).

## Deploying to a Cluster

```bash
# Build, push, and deploy
make operator-image operator-push IMG=<registry>/vcf-migration-operator:dev
make deploy IMG=<registry>/vcf-migration-operator:dev

# Deploy console plugin (optional)
make console-plugin-image console-plugin-push CONSOLE_PLUGIN_IMG=<registry>/vcf-migration-console-plugin:dev
make deploy-console-plugin CONSOLE_PLUGIN_IMG=<registry>/vcf-migration-console-plugin:dev
```

Everything deploys to the `openshift-vcf-migration` namespace.

## Testing

### Unit Tests

```bash
make test
```

Uses Ginkgo/Gomega with `envtest` (local etcd + apiserver, Kubernetes v1.35.0 binaries). vSphere operations are tested against the govmomi simulator.

Test files live alongside the code they test:
- `internal/controller/*_test.go`
- `internal/vsphere/*_test.go`
- `internal/openshift/*_test.go`
- `internal/metadata/*_test.go`

### End-to-End Tests

```bash
# Creates a Kind cluster, builds/loads the image, and runs tests
RUN_E2E=true make test-e2e

# Tear down the Kind cluster afterward
make cleanup-test-e2e
```

### Linting

```bash
make lint        # Check
make lint-fix    # Auto-fix
```

Uses golangci-lint v2.1.0. Config in `.golangci.yml`.

## OLM Bundle

```bash
# Regenerate bundle manifests from current CRDs/RBAC
make bundle

# Build and push bundle + catalog images
make bundle-build bundle-push BUNDLE_IMG=<registry>/vcf-migration-operator-bundle:v0.0.1
make catalog-build catalog-push CATALOG_IMG=<registry>/vcf-migration-operator-catalog:v0.0.1
```

`make bundle-verify` checks that the bundle is up-to-date with the current code.

## Adding a New Reconciliation Phase

1. Add a condition constant in `api/v1alpha1/vmwarecloudfoundationmigration_types.go`:
   ```go
   const ConditionMyPhase = "MyPhase"
   ```

2. Add it to `conditionOrder` in `internal/controller/vmwarecloudfoundationmigration_controller.go`.

3. Write the handler function:
   ```go
   func (r *VmwareCloudFoundationMigrationReconciler) ensureMyPhase(
       ctx context.Context,
       migration *v1alpha1.VmwareCloudFoundationMigration,
   ) (string, ctrl.Result, error) {
       // Do work...
       return "phase complete", ctrl.Result{}, nil
   }
   ```

4. Register the handler in the `handlers` map inside `Reconcile()`.

5. Regenerate manifests: `make generate manifests`

6. Add tests in `internal/controller/`.

## Modifying vSphere Operations

vSphere operations live in `internal/vsphere/`. Tests use govmomi's simulator:

```go
simulator.Test(func(ctx context.Context, c *vim25.Client) {
    // c is a real govmomi client pointed at the in-process simulator
})
```

## Modifying OpenShift Operations

OpenShift operations live in `internal/openshift/`. Each manager type (`InfrastructureManager`, `MachineManager`, etc.) takes injected Kubernetes/OpenShift clients for testability.

## Key Makefile Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `IMG` | `registry.ci.openshift.org/origin/vcf-migration-operator:latest` | Operator image |
| `CONSOLE_PLUGIN_IMG` | `vcf-migration-console-plugin:latest` | Console plugin image |
| `CONTAINER_TOOL` | `podman` | Container build tool |
| `VERSION` | `0.0.1` | Project version |
| `CHANNELS` | `dev-preview` | OLM bundle channel |
| `KIND_CLUSTER` | `vcf-migration-operator-test-e2e` | Kind cluster name for e2e |
| `ENVTEST_K8S_VERSION` | `1.35.0` | Kubernetes version for envtest |

## CI

- **OpenShift CI**: `.ci-operator.yaml` — builds on `rhel-9-release-golang-1.25-openshift-4.22`
- **Tekton/Konflux**: `.tekton/` — pipelines for PR validation and push builds
- `make verify-diff` ensures no uncommitted generated files in CI
