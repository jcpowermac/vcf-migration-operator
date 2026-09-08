# API Reference

**Group:** `migration.openshift.io`
**Version:** `v1alpha1`
**Kind:** `VmwareCloudFoundationMigration`
**Short name:** `vcfm`
**Scope:** Namespaced

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `state` | `MigrationState` | No | `Pending` | `Pending`, `Running`, or `Paused`. Reconciler only acts when `Running`. |
| `targetVCenterCredentialsSecret` | `SecretReference` | Yes | | Secret with target vCenter credentials. |
| `failureDomains` | `[]VSpherePlatformFailureDomainSpec` | Yes (min 1) | | Target failure domains. Uses the OpenShift `configv1` type. |
| `image` | `ImageSpec` | No | | RHCOS OVA resolution and import controls. |

### MigrationState Enum

| Value | Meaning |
|-------|---------|
| `Pending` | Migration has not started |
| `Running` | Migration is actively progressing |
| `Paused` | Migration is paused by the user |

### SecretReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Secret name |
| `namespace` | `string` | No | Secret namespace (defaults to migration namespace) |

The secret must contain keys in the format `<vcenter-fqdn>.username` and `<vcenter-fqdn>.password`.

### ImageSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `ovaUrl` | `string` | No | Direct `https://` URL to the RHCOS OVA (must end in `.ova`; optional query string allowed). Overrides auto-resolution from the `coreos-bootimages` ConfigMap. Required for air-gapped environments. |
| `diskProvisioning` | `DiskProvisioningMode` | No | VMDK provisioning type for imported templates: `thin`, `thick`, or `eagerZeroedThick`. Defaults to the OVF descriptor's type. |

When `image` is set, the operator imports the OVA during the `DestinationImageImported` phase and populates `topology.template` for each failure domain. When omitted, `topology.template` must be set manually.

### VSpherePlatformFailureDomainSpec (from openshift/api configv1)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Unique failure domain name (1-256 chars) |
| `region` | `string` | Yes | Region tag name, category `openshift-region` (1-80 chars) |
| `zone` | `string` | Yes | Zone tag name, category `openshift-zone` (1-80 chars) |
| `server` | `string` | Yes | Target vCenter FQDN (1-255 chars) |
| `topology.datacenter` | `string` | Yes | Datacenter name (max 80 chars) |
| `topology.computeCluster` | `string` | Yes | Path: `/<dc>/host/<cluster>` |
| `topology.datastore` | `string` | Yes | Path: `/<dc>/datastore/<ds>` |
| `topology.networks` | `[]string` | Yes (min 1) | Port group network names |
| `topology.resourcePool` | `string` | No | Path: `/<dc>/host/<cluster>/Resources/<pool>` |
| `topology.folder` | `string` | No | Path: `/<dc>/vm/<folder>` |
| `topology.template` | `string` | Conditional | Template path. **Required when `spec.image` is unset** (rejected otherwise); when `spec.image` is set, the operator imports the OVA and populates it per failure domain |

## Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Ordered migration phase conditions |
| `startTime` | `*metav1.Time` | When migration entered `Running` |
| `completionTime` | `*metav1.Time` | When migration reached `Ready` |
| `image` | `ImageStatus` | RHCOS OVA import state |

### ImageStatus

| Field | Type | Description |
|-------|------|-------------|
| `resolvedOVAUrl` | `string` | URL the OVA was (or will be) downloaded from |
| `resolvedSHA256` | `string` | Expected sha256 digest (empty for user-provided URLs) |
| `importedTemplates` | `map[string]string` | Failure domain name → imported VM template inventory path |
| `operatorImportedTemplates` | `map[string]string` | Failure domain name → OVA URL used by the operator for its imports |
| `urlSource` | `ImageURLSource` | How `resolvedOVAUrl` was populated: `user`, `auto`, or empty (unresolved) |

If the OVA URL changes after import, the operator deletes and re-imports only the templates it imported itself (`operatorImportedTemplates`); user-pre-configured templates are never touched.

### Condition Types (in order)

| Type | Description |
|------|-------------|
| `Accepted` | Admission gate: `True` for the singleton instance (`cluster`), `False` for unsupported object names |
| `InfrastructurePrepared` | Preflight checks passed, migration path validated |
| `DestinationInitialized` | Target vCenter folders and topology tags created |
| `DestinationImageImported` | RHCOS OVA imported as a VM template on the destination vCenter (immediately `True` when `spec.image` is nil) |
| `MultiSiteConfigured` | Cluster recognizes both vCenters |
| `WorkloadMigrated` | Workers created on target, control plane rolled out, source scaled to 0 |
| `SourceCleaned` | Source vCenter fully detached |
| `Ready` | Migration complete, all operators healthy |

### Condition Reasons

`Progressing`, `Completed`, `Failed`, `Paused`, `UnsupportedName`

`Paused` is applied to the `Ready` condition while `spec.state` is `Paused` (message explains how to resume); it is cleared and replaced with `Progressing` when the migration is resumed.

## Print Columns

```text
NAME    STATE     READY   AGE
sample  Running   True    5m
```
