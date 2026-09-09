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
| `nodeMigration` | `NodeMigrationSpec` | No | | Controls how nodes move to the target vCenter. When omitted, uses MachineSet/CPMS replacement. See [Node Migration API](node-migration-api.md). |

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

## Singleton Enforcement

The operator reconciles exactly one resource: `openshift-vcf-migration/cluster` (`SingletonName` /
`SingletonNamespace`, matching the operator deployment namespace). Enforcement is at reconcile
time, not admission: any other `VmwareCloudFoundationMigration` object (wrong name or wrong
namespace) is ignored with `Accepted=False` and a warning event, and no migration side effects
run for it.

## Deletion and Record Retention

The singleton carries the finalizer `migration.openshift.io/record-retention` from its first
reconcile onward. Deleting the resource stops new migration actions only: nothing is cancelled,
powered off, deleted, or rolled back — in-flight relocation and VMs are left untouched.

Without completion evidence tied to the current spec, the finalizer
holds the object in a terminating state so the execution record survives. The `Accepted`
condition is set to `False` with reason `DeletionBlocked` and a warning event explains the
retention.

The finalizer is released (and the record actually removed) only when either:

1. the migration has `Ready=True`, `completionTime`, and `completionSpecHash` matching its
   current spec. The hash is lowercase hexadecimal SHA-256 of Go `encoding/json.Marshal` of
   the typed spec, recorded atomically when a current-generation reconcile establishes
   completion. This permits deletion's metadata-only generation increment, but not a changed
   spec. A timestamp alone or historical Ready condition is insufficient; or
2. an administrator explicitly completes the handoff by setting the annotation
   `migration.openshift.io/allow-deletion: "true"` (any other value is ignored).

Legacy completed records without `completionSpecHash` are retained conservatively and need
administrator handoff; the operator does not infer missing evidence during deletion.

Execution permission is read uncached before each workflow handler and new mutation, including
loop iterations and retries. There is an unavoidable narrow race between the final permission
check and submitting a request to another API: Kubernetes and vCenter cannot atomically check
the migration and accept that request. Already-submitted operations are not cancelled and may
continue through other controllers (including MachineSet and control-plane rollouts).

After deletion the operator does not run readiness/progress checks to observe unfinished work
to completion. Even if external work later finishes, the retained record may require an
administrator to verify the outcome and explicitly authorize the handoff above. Deletion also
stops new probe VM cleanup requests; any remaining probes must be handled during that handoff.

### VSpherePlatformFailureDomainSpec (from openshift/api configv1)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Unique failure domain name (1-256 chars) |
| `region` | `string` | Yes | Region tag name, category `openshift-region` (1-80 chars). Reused as-is if the target datacenter already carries an `openshift-region` tag. |
| `zone` | `string` | Yes | Zone tag name, category `openshift-zone` (1-80 chars). Reused as-is if the target cluster already carries an `openshift-zone` tag. |
| `server` | `string` | Yes | Target vCenter FQDN (1-255 chars) |
| `topology.datacenter` | `string` | Yes | Datacenter name (max 80 chars) |
| `topology.computeCluster` | `string` | Yes | Path: `/<dc>/host/<cluster>` |
| `topology.datastore` | `string` | Yes | Path: `/<dc>/datastore/<ds>` |
| `topology.networks` | `[]string` | Yes (min 1) | Port group network names |
| `topology.resourcePool` | `string` | No | Path: `/<dc>/host/<cluster>/Resources/<pool>` |
| `topology.folder` | `string` | No | Path: `/<dc>/vm/<folder>` |
| `topology.template` | `string` | Yes | Existing VM/template inventory path on the target vCenter; required for every failure domain at admission and verified during preflight |

## Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Ordered migration phase conditions |
| `startTime` | `*metav1.Time` | When migration entered `Running` |
| `completionTime` | `*metav1.Time` | When migration reached `Ready` |
| `nodeMigration` | `NodeMigrationStatus` | Per-node move progress. Nil when using MachineSet/CPMS replacement. |

### Condition Types (in order)

| Type | Description |
|------|-------------|
| `Accepted` | Admission gate: `True` for the singleton instance (`openshift-vcf-migration/cluster`), `False` for unsupported object names or namespaces, and `False` with `DeletionBlocked` while deletion is retained |
| `InfrastructurePrepared` | Preflight checks passed, migration path validated |
| `DestinationInitialized` | Target vCenter folders and topology tags created |
| `MultiSiteConfigured` | Cluster recognizes both vCenters |
| `WorkloadMigrated` | Workers created on target, control plane rolled out, source scaled to 0 |
| `SourceCleaned` | Source vCenter fully detached |
| `Ready` | Migration complete, all operators healthy |

### Condition Reasons

`Progressing`, `Completed`, `Failed`, `Paused`, `UnsupportedName`, `UnsupportedNamespace`, `DeletionBlocked`

`Paused` is applied to the `Ready` condition while `spec.state` is `Paused` (message explains how to resume); it is cleared and replaced with `Progressing` when the migration is resumed.

## Print Columns

```text
NAME    STATE     READY   AGE
sample  Running   True    5m
```
