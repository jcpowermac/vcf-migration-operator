# Preflight: vSphere CSI Driver Must Be Removed

## Goal

For the first release, vSphere storage is not supported during migration. Preflight must enforce that the vSphere CSI driver operator is disabled (`Removed`), while still inspecting and reporting the Cluster Storage Operator (`Storage`) management state.

This mirrors what CI already does in [openshift/release#79788](https://github.com/openshift/release/pull/79788): patch `ClusterCSIDriver/csi.vsphere.vmware.com` to `managementState: Removed`.

## Requirements

### Hard blocker

- Read `clustercsidrivers.operator.openshift.io/csi.vsphere.vmware.com`.
- Require `spec.managementState == "Removed"`.
- Fail preflight (keep `InfrastructurePrepared=False`) when:
  - the object is missing
  - `managementState` is anything other than `Removed` (including `Managed` and `Unmanaged`)
  - the API/type is unavailable / read errors (except treat missing CRD the same as missing object: fail with an actionable message)

### Advisory (do not block)

- Read `storages.operator.openshift.io/cluster`.
- Always log `spec.managementState` at V(1).
- If `managementState` is `Managed` (or missing/unexpected), include a warning in the preflight success message so it surfaces in status.
- If `managementState` is `Unmanaged` or `Removed`, log and proceed with no warning.
- Missing Storage object / unreadable Storage API: log a warning and proceed (CSI remains the only hard gate).

### Logging

- Log both objects’ names and `managementState` on every preflight pass (success or failure path after the reads).

## Design

### Approach

Use the existing dynamic client pattern already used for MHC / autoscaler preflight checks. No new typed operator client dependency.

### New helper

Add a focused helper in `internal/controller/preflight.go`, for example:

```go
func checkVSphereStorageManagement(ctx context.Context, dynamicClient dynamic.Interface) (warning string, err error)
```

Behavior:

1. Get `ClusterCSIDriver` `csi.vsphere.vmware.com`.
2. If not `Removed` → return error (hard fail).
3. Get `Storage` `cluster`.
4. Log both states.
5. If Storage is `Managed` / unexpected → return `(warning, nil)`.
6. Otherwise return `("", nil)`.

### Integration into `runPreflightChecks`

Call the helper after the existing CSI PV check and before interfering-resource / vSphere target checks:

1. feature gate / upgrade / operator health (unchanged)
2. no vSphere CSI PVs (unchanged)
3. **vSphere CSI/Storage managementState check (new)**
4. interfering rollout resources (unchanged)
5. source/target vSphere validation (unchanged)

- On error: return the error unchanged (existing condition failure path).
- On warning: continue preflight; append the warning to the final success message returned by `runPreflightChecks` (e.g. `"Preflight validation passed; warning: Storage/cluster managementState is Managed ..."`).

Do not introduce a new condition type.

Assumption: with `ClusterCSIDriver` set to `Removed`, the `storage` ClusterOperator remains healthy enough to pass the existing operator-health gate. If CI/runtime proves otherwise, follow up by excluding `storage` from that health check rather than weakening this gate.

### RBAC

Add controller RBAC markers:

- `operator.openshift.io` / `clustercsidrivers` → `get`
- `operator.openshift.io` / `storages` → `get`

Regenerate manifests (`make manifests`) so ClusterRole picks up the verbs.

### GVRs

```go
clusterCSIDriverGVR = schema.GroupVersionResource{
  Group: "operator.openshift.io", Version: "v1", Resource: "clustercsidrivers",
}
storageOperatorGVR = schema.GroupVersionResource{
  Group: "operator.openshift.io", Version: "v1", Resource: "storages",
}
```

Read `spec.managementState` via unstructured nested string access.

### Error / warning copy (actionable)

Hard fail example:

> vSphere CSI driver is not supported for migration; set ClusterCSIDriver/csi.vsphere.vmware.com spec.managementState to Removed (current: Managed)

Advisory warning example:

> Storage/cluster managementState is Managed; consider setting Unmanaged or Removed (only ClusterCSIDriver Removed is required)

## Testing (TDD)

Unit tests for the helper first (failing), then implementation:

| Case | CSI state | Storage state | Expect |
|------|-----------|---------------|--------|
| happy | Removed | Removed/Unmanaged | pass, no warning |
| advisory | Removed | Managed | pass + warning |
| hard fail Managed | Managed | any | error |
| hard fail Unmanaged | Unmanaged | any | error |
| hard fail missing CSI | missing | any | error |
| Storage missing | Removed | missing | pass + warning (advisory) |

Also add one `runPreflightChecks` case proving CSI `Managed` short-circuits before target validation.

## Out of scope

- Operator does not patch/remove CSI or Storage itself (admin/CI prerequisite).
- No change to the existing “no vSphere CSI PVs” check (kept as a separate hard gate).
- No new CRD status fields beyond the existing condition message.
- No typed `openshift/api/operator` client wiring.

## Success criteria

- Migration cannot proceed past `InfrastructurePrepared` while vSphere `ClusterCSIDriver` is not `Removed`.
- Storage management state is always logged; `Managed` produces a visible status warning without blocking.
- Unit coverage for hard fail, advisory warning, and happy path.
- RBAC regenerated for the new GETs.
