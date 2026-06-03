# Installer-Compatible Metadata Secret

## Overview

Align the operator's metadata.json output with the installer's `ClusterMetadata` structure so that `openshift-install destroy cluster` can consume it directly. The current operator produces a flat JSON schema with wrong field names and missing nesting; the installer expects a specific shape with a nested `vsphere` key and installer-convention JSON tags.

## Current Baseline

- `internal/metadata/metadata.go` defines a custom `Metadata` struct with flat vCenter fields (`vcenter`, `username`, `password`, `terraformPlatform`) and an operator-specific `VCenters` array carrying topology details (Port, Datacenters, DefaultDC, Cluster, Datastore, Network, ResourcePool, Folder).
- The controller calls `GenerateMetadata()` during the `ConditionSourceCleaned` phase, saves the result to a Secret named `{migration-name}-metadata` with key `metadata.json`.
- `SaveToSecret()` creates-or-updates the Secret with label `migration.openshift.io/metadata: true`.
- Tests in `metadata_test.go` cover generation, round-trip, not-found, and credential parsing.

## Incompatibilities with Installer

The installer's `ClusterMetadata` (`installer/pkg/types/clustermetadata.go`) expects:

| Field | Installer JSON | Operator JSON (current) |
|-------|---------------|------------------------|
| Top-level structure | `clusterName`, `clusterID`, `infraID`, then `vsphere: {...}` nested | All fields flat at top level |
| vCenter address | `vsphere.vCenter` (capital C) | `vcenter` (lowercase) |
| Terraform platform | `vsphere.terraform_platform` (snake_case) | `terraformPlatform` (camelCase) |
| VCenters array | `vsphere.VCenters` (capital V, no json tag = Go default) with `{vCenter, username, password}` | `vcenters` (lowercase) with topology fields the installer doesn't use |

The installer's vSphere destroyer (`installer/pkg/destroy/vsphere/vsphere.go:29-51`) reads:
- `metadata.VSphere.VCenters[].VCenter/Username/Password` for multi-vCenter
- `metadata.VSphere.VCenter/Username/Password` as legacy fallback
- `metadata.VSphere.TerraformPlatform`
- `metadata.ClusterID`, `metadata.InfraID`

## Plan

### 1. Replace `Metadata` and `VCenters` types with installer-compatible structure

In `internal/metadata/metadata.go`, replace the two custom structs with types that produce the exact JSON the installer expects. The operator cannot import `github.com/openshift/installer/pkg/types` directly (it would pull the entire installer dependency tree), so define local types with matching JSON tags.

**New types:**

```go
type ClusterMetadata struct {
    ClusterName string         `json:"clusterName"`
    ClusterID   string         `json:"clusterID"`
    InfraID     string         `json:"infraID"`
    VSphere     *VSphereMetadata `json:"vsphere,omitempty"`
}

type VSphereMetadata struct {
    VCenter           string     `json:"vCenter,omitempty"`
    Username          string     `json:"username,omitempty"`
    Password          string     `json:"password,omitempty"`
    TerraformPlatform string     `json:"terraform_platform"`
    VCenters          []VCenter  `json:"VCenters"`
}

type VCenter struct {
    VCenter  string `json:"vCenter"`
    Username string `json:"username"`
    Password string `json:"password"`
}
```

Note: the `VCenters` field in `VSphereMetadata` uses `json:"VCenters"` (no `omitempty`) to match the installer's untagged field — Go defaults to the field name when no tag is present, and the installer never omits the key. Rename the element type from `VCenters` to `VCenter` (singular) for Go convention clarity — only the JSON tag matters for wire compatibility.

Remove the old `Metadata` and `VCenters` types entirely. Remove topology fields (`Port`, `Datacenters`, `DefaultDC`, `Cluster`, `Datastore`, `Network`, `ResourcePool`, `Folder`) — these are not consumed by the installer's destroy path and were operator-internal additions.

### 2. Update `GenerateMetadata` signature and implementation

Updated logic:
- Build `VSphereMetadata` from failure domains and credentials:
  - Set legacy root fields (`VCenter`, `Username`, `Password`) from the first failure domain (matches installer behavior).
  - Set `TerraformPlatform` to `"vsphere"`.
  - Build `VCenters` array with one entry per **unique server** (deduplicate across failure domains that share the same vCenter).
- **Fail-fast on missing or malformed credentials.** If `parseCredentials` returns empty username or password for any server, return an error immediately rather than serializing blank values into the Secret. This surfaces misconfiguration at reconciliation time instead of deferring failure to the destroy path.
- Return `*ClusterMetadata` instead of `*Metadata`.

**Known limitation — `clusterName`:** The installer populates `ClusterName` from `InstallConfig.Config.ObjectMeta.Name` (the user-chosen cluster name), which is not stored on a running cluster. The operator uses `infra.Name`, which is always `"cluster"` (the singleton Infrastructure object name). The vSphere destroyer does not read `ClusterName` — only IBMCloud and PowerVS destroyers use it — so this is cosmetically wrong but functionally harmless for vSphere destroy compatibility.

### 3. Update `SaveToSecret` to accept `*ClusterMetadata`

Change the parameter type from `*Metadata` to `*ClusterMetadata`. The serialization logic (`json.Marshal`) remains the same.

On the update path, reconcile `existing.Labels` (re-assert `migration.openshift.io/metadata: true`) in addition to `existing.Data`. The current code only replaces `Data`, so a manually-edited Secret could lose its label and become invisible to any label-based lookups.

### 4. Secret lifecycle — no owner reference

The metadata Secret must **not** have an owner reference to the migration CR. It contains the vCenter credentials needed for `openshift-install destroy cluster`, which may be run long after the migration CR is deleted. An owner reference would garbage-collect it on migration CR deletion, losing the deprovisioning data.

Cleanup is the user's responsibility after successful cluster teardown. Document this in the secret layout documentation (step 8).

### 5. Validate JSON output against installer expectations

Add a test that:
1. Generates metadata with known inputs.
2. Marshals to JSON.
3. Unmarshals into a map and asserts the exact key structure:
   - Top-level keys: `clusterName`, `clusterID`, `infraID`, `vsphere`.
   - `vsphere` sub-keys: `vCenter`, `username`, `password`, `terraform_platform`, `VCenters`.
   - Each `VCenters` entry: `vCenter`, `username`, `password`.
4. Verify no extra keys leak (no `port`, `datacenters`, `defaultDC`, etc.).

This serves as a contract test — if someone changes a JSON tag, this test breaks.

### 6. Update existing tests

- `TestGenerateMetadata`: Update struct literals and assertions to use `ClusterMetadata`/`VSphereMetadata`/`VCenter` types. Assert `md.VSphere.VCenter` instead of `md.VCenter`. Assert `md.VSphere.TerraformPlatform`.
- `TestSaveToSecretAndGet`: Update `Metadata{}` literal to `ClusterMetadata{}` with nested `VSphere` field. Assert labels are re-applied on update.
- Add deduplication test: two failure domains with the same server should produce one `VCenters` entry.

### 7. Add credential validation tests

- Missing credentials for a server (empty string in credentials map) — `GenerateMetadata` returns error.
- Malformed credential string (no colon separator) — `GenerateMetadata` returns error.
- Empty username or password after parsing — `GenerateMetadata` returns error.

### 8. Document secret layout

Add a doc comment block at the top of `internal/metadata/metadata.go` explaining:
- The Secret is named `{migration-name}-metadata` in the migration's namespace.
- The Secret contains a single key `metadata.json`.
- The JSON structure matches `installer/pkg/types.ClusterMetadata` for vSphere.
- The Secret is created/updated during the `ConditionSourceCleaned` phase after source vCenter removal.
- The label `migration.openshift.io/metadata: true` identifies metadata secrets.
- Consumers: `openshift-install destroy cluster` reads this file to connect to vCenter(s) and delete tagged resources.
- The Secret has no owner reference and intentionally outlives the migration CR — it must remain available for manual cluster teardown after migration completes. Cleanup is the user's responsibility after successful deprovisioning.

## Ordering (TDD)

Tests are written before their corresponding implementation so the implementation is forced to meet the contract.

1. **Write JSON contract test** (step 5) — defines the exact key structure the Secret must produce. Fails until types are implemented.
2. **Write credential validation tests** (step 7) — missing credentials, malformed format. Fails until `GenerateMetadata` tightens `parseCredentials`.
3. **Write deduplication test** (step 6) — two failure domains with the same server produce one `VCenters` entry. Fails until `GenerateMetadata` adds dedup logic.
4. **Implement type changes** (step 1) and **`GenerateMetadata`** (step 2) — contract test, credential tests, and dedup test go green.
5. **Implement `SaveToSecret` label reconciliation** (step 3) and **Secret lifecycle** (step 4).
6. **Update remaining tests** (step 6) and **document** (step 8).

## Files Changed

| File | Change |
|------|--------|
| `internal/metadata/metadata.go` | Replace types, update `GenerateMetadata`/`SaveToSecret` signatures, credential validation, label reconciliation |
| `internal/metadata/metadata_test.go` | Update all tests, add JSON contract test, add dedup test, add credential validation tests |
| `internal/controller/vmwarecloudfoundationmigration_controller.go` | Call `GenerateMetadata` with the installer-compatible signature |

## Out of Scope

- Feature set and custom feature gate data in the metadata secret — the installer's vSphere destroy path does not consume them.
- Topology fields in the VCenters array — the installer destroyer does not use them. If a future consumer needs datacenter/cluster/folder info, it can read it from the Infrastructure resource directly.
- Downloadable metadata from the console UI — tracked separately in `downloadable-installer-metadata.md`.
