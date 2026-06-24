# Architecture

## Package Layout

```text
cmd/main.go                     Manager entrypoint
api/v1alpha1/                   CRD types, condition/reason constants
internal/
  controller/                   Reconciler, phase handlers, preflight checks
  vsphere/                      vCenter session management, folder/tag operations
  openshift/
    infrastructure.go           Infrastructure CR (add/remove vCenters)
    machines.go                 MachineSets, CPMS, rollout status
    secrets.go                  vsphere-creds secret management
    configmaps.go               cloud-provider-config ConfigMap
    pods.go                     vSphere pod restart/readiness checks
    operators.go                ClusterOperator health checks
    version.go                  Cluster version, feature gate detection
  metadata/                     Migration metadata generation and storage
console-plugin/
  cmd/plugin/                   Console plugin backend (Go HTTP server)
  pkg/                          Plugin API handlers and SSE
  web/                          React/PatternFly frontend
config/                         Kustomize manifests (CRDs, RBAC, manager, samples)
bundle/                         OLM bundle (CSV, annotations)
test/e2e/                       End-to-end tests (Kind cluster)
```

## Reconciliation Flow

The reconciler watches `VmwareCloudFoundationMigration` resources. When `spec.state` is `Running`, it walks a fixed condition order. For each condition not yet `True`, it runs the corresponding handler and returns. One phase per reconcile loop.

```text
spec.state == Running?
  │ no → skip
  │ yes
  ▼
InfrastructurePrepared? ──no──→ runPreflightChecks()
  │ yes
DestinationInitialized? ─no──→ create folders, ensure tags, attach to objects
  │ yes
MultiSiteConfigured? ────no──→ update vsphere-creds, Infrastructure CR,
  │                             cloud-provider-config; restart pods; poll readiness
  │ yes
WorkloadMigrated? ───────no──→ create target MachineSets; wait for nodes;
  │                             update CPMS; wait for rollout; scale down source
  │ yes
SourceCleaned? ──────────no──→ remove source from Infrastructure, config, creds;
  │                             restart pods; generate metadata secret
  │ yes
Ready? ──────────────────no──→ check operator health; verify only target vCenters
  │ yes
  ▼
Done (set completionTime)
```

### Requeue Strategy

| Context | Interval |
|---------|----------|
| Pod readiness polling | 15s |
| CPMS generation observation | 15s |
| Infrastructure validation | 15s |
| Machine creation/readiness | 30s |
| Control plane rollout | 30s |
| Old machine deletion | 30s |
| Operator health check | 30s |
| Error backoff | 5s base, 5m max (exponential) |

### Error Handling

On error in any phase handler:
1. Condition set to `False` with `ReasonFailed`
2. Warning event recorded
3. Status persisted (with conflict retry)
4. Error returned to controller-runtime for backoff requeue

### Status Updates

Status is always persisted after each phase, even on error. Uses `retry.RetryOnConflict` with `DefaultRetry` (exponential backoff, up to 7 retries).

## vSphere Session Management

Sessions are cached in a thread-safe map keyed by `server#datacenter#username`. `GetOrCreate()` returns a cached session or creates a new one. `ClearSessions()` logs out and clears all cached sessions.

Each `Session` holds a govmomi SOAP client, an inventory `Finder` scoped to a datacenter, and a REST-based `TagManager`.

## Key External Dependencies

| Dependency | Version | Purpose |
|------------|---------|---------|
| controller-runtime | v0.21.0 | Operator framework |
| openshift/api | v0.0.0-20260511191110-9b69e5fa27e9 | OpenShift CRD types (Infrastructure, FeatureGate) |
| openshift/client-go | v0.0.0-20260512113608-deb4dc54551a | OpenShift API clients |
| govmomi | v0.52.0 | vSphere SOAP/REST client |
| cloud-provider-vsphere | v1.35.0 | Cloud provider config types |
| k8s.io/api | v0.36.0-alpha.0 | Kubernetes API types |
| ginkgo/v2 | v2.27.2 | Test framework |
