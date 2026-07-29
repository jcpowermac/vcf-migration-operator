# AGENTS.md — vcf-migration-operator

Kubernetes operator (Go / Kubebuilder v4) for orchestrating OpenShift cluster migration between VMware vCenters. Operator uses controller-runtime, govmomi, OpenShift client-go, and Ginkgo/Gomega for tests.

## Build / Lint / Test Commands

### Operator

```bash
# Build
make build                # builds bin/manager (runs manifests, generate, fmt, vet first)
go build -o bin/manager cmd/main.go  # build only, skip codegen

# Lint
make lint                 # run golangci-lint (v2.1.0, installs if missing)
make lint-fix             # lint with auto-fix
make lint-config          # verify golangci-lint config

# Format
make fmt                  # go fmt ./...
make vet                  # go vet ./...

# Unit + integration tests (excludes e2e, requires envtest binaries)
make test

# Run a single test by name (regex match)
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" \
  go test ./internal/vsphere/ -run TestCreateVMFolder -v

# Run a single Ginkgo test by description
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" \
  go test ./internal/controller/ -v -ginkgo.focus="should successfully reconcile"

# Run all tests in one package
go test ./internal/vsphere/ -v

# E2E tests (requires Kind cluster, Docker)
make test-e2e

# Code generation
make manifests            # generate CRDs, RBAC, webhooks
make generate             # generate DeepCopy methods
```

## Project Layout

```
api/v1alpha1/          CRD types (VmwareCloudFoundationMigration)
cmd/main.go            Operator entrypoint
internal/
  controller/          Reconciler and helpers
  openshift/           OpenShift resource managers (secrets, infra, pods, machines, configmaps, operators)
  vsphere/             vSphere operations (session, folder, tags)
  metadata/            Installer metadata generation
config/                Kustomize manifests (CRDs, RBAC, manager deployment)
hack/                  Boilerplate license header
test/e2e/              End-to-end tests (Kind cluster)
test/utils/            Test utility helpers
```

## Code Style

### Imports

Three groups separated by blank lines: (1) stdlib, (2) third-party, (3) project-internal. Each group alphabetically sorted. Enforced by `goimports` via golangci-lint.

```go
import (
    "context"
    "fmt"

    apierrors "k8s.io/apimachinery/pkg/api/errors"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"

    migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
    "github.com/openshift/vcf-migration-operator/internal/openshift"
)
```

Standard aliases: `apierrors`, `apimeta`, `metav1`, `corev1`, `ctrl`, `migrationv1alpha1`. Ginkgo/Gomega use dot imports in test files only.

### Formatting

Go default formatting (`gofmt`). No `.editorconfig`. Golangci-lint enforces `gofmt` and `goimports` as formatters.

### Naming Conventions

- **Types**: PascalCase. Manager pattern: `SecretManager`, `InfrastructureManager`, `PodManager`.
- **Constructors**: `NewSecretManager(client) *SecretManager`.
- **Getters**: `Get` prefix: `GetVSphereCredsSecret`, `GetSourceVCenter`.
- **Boolean checks**: `is` prefix (unexported): `isConditionTrue`, `isPodReady`.
- **Reconciler sub-steps**: `ensure` prefix: `ensureInfrastructurePrepared`, `ensureReady`.
- **Constants**: Exported PascalCase (`VSphereCredsSecretName`), unexported camelCase (`cvoNamespace`).
- **Enum types**: `type MigrationState string` with `MigrationStateRunning` etc.
- **Receivers**: Single letter (`r` for Reconciler, `s` for SecretManager, `m` for InfrastructureManager).
- **Loop variables**: Short names (`fd`, `ms`, `vc`, `pod`). Use `&slice[i]` for pointer access.
- **Test variables**: `tt` for table cases, `got`/`want` for actual/expected.

### Error Handling

Wrap errors with `fmt.Errorf` using `%w`. Messages are lowercase, start with a gerund (present participle), and include contextual identifiers:

```go
return fmt.Errorf("creating vim25 client for %s: %w", server, err)
return fmt.Errorf("key %q not found in secret %s/%s", key, ns, name)
```

Use `apierrors.IsNotFound(err)` for Kubernetes not-found checks. Non-critical errors are logged and skipped, not returned. No custom error types or `errors.New()` — always `fmt.Errorf`.

### Logging

Use `klog/v2` exclusively. Obtain logger from context:

```go
log := klog.FromContext(ctx)
```

Verbosity levels:
- `log.Info(...)` — significant milestones only (migration complete)
- `log.V(1).Info(...)` — primary operational logging (condition processing, validation)
- `log.V(2).Info(...)` — debug detail (credential lookups, API calls, tag creation)
- `log.Error(err, "msg")` — non-fatal errors that are noted but not returned

Use structured key-value pairs with camelCase keys:

```go
log.V(1).Info("machines not ready", "machineSet", name, "ready", count, "total", total)
```

### Context

Always the first parameter. Never stored in structs. Passed through the entire call chain. Use `context.Background()` in tests.

### Comments

Godoc on all exported identifiers, starting with the identifier name. Multi-line descriptions for complex functions. Inline comments for implementation notes above the relevant code.

### License Header

Kubebuilder-scaffolded files (controller, types, cmd/main, suite_test) use the Apache 2.0 block comment from `hack/boilerplate.go.txt`. Hand-written internal packages omit it.

### Types and API Conventions

- CRD types in `api/v1alpha1/` follow kubebuilder conventions with markers (`+kubebuilder:rbac`, `+kubebuilder:validation`, `+optional`).
- Manager structs hold an interface-typed client field (unexported), constructed via `NewXxxManager`.
- Reconciler uses embedded `client.Client` and exported fields for injected dependencies.

### Testing

- **Unit tests** (`internal/vsphere/`): Standard `testing.T`, same package (white-box). Use govmomi `simulator.Test()`. Table-driven tests with `t.Run`.
- **Controller tests** (`internal/controller/`): Ginkgo/Gomega with envtest. `Describe/Context/It` structure. `Expect(...).NotTo(HaveOccurred())`.
- **E2E tests** (`test/e2e/`): Ginkgo/Gomega with Kind cluster. Use `utils.Run()` for shell commands.
- Test helper functions call `t.Helper()`.
- Fatal assertions: `t.Fatalf("FunctionName: %v", err)` with function name prefix.

### Events

Record events alongside condition changes:

```go
r.Recorder.Event(migration, "Normal", "InfrastructurePrepared", "Preflight validation passed")
r.Recorder.Eventf(migration, "Warning", "ConditionFailed", "Condition %s failed: %v", cond, err)
```

### Requeue Patterns

- 10s for quick retries (resource deletion)
- 15s for medium waits (pod readiness, CVO checks)
- 30s for long waits (machine readiness, rollout stability)

### Linters Enabled

golangci-lint v2 with: `copyloopvar`, `dupl`, `errcheck`, `ginkgolinter`, `goconst`, `gocyclo`, `govet`, `ineffassign`, `lll`, `misspell`, `nakedret`, `prealloc`, `revive`, `staticcheck`, `unconvert`, `unparam`, `unused`. Revive rules: `comment-spacings`, `import-shadowing`.
