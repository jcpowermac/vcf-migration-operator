# API Reference

**Group:** `migration.openshift.io`
**Version:** `v1alpha1`
**Kind:** `VmwareCloudFoundationMigration`
**Short name:** `vcfm`
**Scope:** Namespaced

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `state` | `MigrationState` | Yes | `Pending` | `Pending`, `Running`, or `Paused`. Reconciler only acts when `Running`. |
| `targetVCenterCredentialsSecret` | `SecretReference` | Yes | | Secret with target vCenter credentials. |
| `failureDomains` | `[]VSpherePlatformFailureDomainSpec` | Yes (min 1) | | Target failure domains. Uses the OpenShift `configv1` type. |

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
| `topology.template` | `string` | No | Template path. Defaults to `/<dc>/vm/{infraID}-rhcos-{region}-{zone}` |

## Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Ordered migration phase conditions |
| `startTime` | `*metav1.Time` | When migration entered `Running` |
| `completionTime` | `*metav1.Time` | When migration reached `Ready` |

### Condition Types (in order)

| Type | Description |
|------|-------------|
| `InfrastructurePrepared` | Preflight checks passed, migration path validated |
| `DestinationInitialized` | Target vCenter folders and topology tags created |
| `MultiSiteConfigured` | Cluster recognizes both vCenters |
| `WorkloadMigrated` | Workers created on target, control plane rolled out, source scaled to 0 |
| `SourceCleaned` | Source vCenter fully detached |
| `Ready` | Migration complete, all operators healthy |

### Condition Reasons

`Progressing`, `Completed`, `Failed`, `Paused`, `Pending`

## Print Columns

```text
NAME    STATE     READY   AGE
sample  Running   True    5m
```
