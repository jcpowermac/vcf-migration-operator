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
| `failureDomainMappings` | `[]FailureDomainMapping` | No | | Maps source failure domain names (from `Infrastructure`) to destination `failureDomains` entries. Required unless exactly one source and one destination failure domain exist; must cover every failure domain exactly once. |
| `nodeMigration` | `NodeMigrationSpec` | No | | Selects Recreate or the VMotion API shape. When omitted, retains MachineSet/CPMS replacement. VMotion requires both roles, explicit matching Hot/Cold modes, and serial worker availability. API shape does not mean controller execution is implemented. See [Node Migration API](node-migration-api.md). |

- `state` (`MigrationState`, optional, default `Pending`): accepts `Pending`, `Planning`, `Running`, or `Paused`. The migration workflow only executes when `Running`.
- `targetVCenterCredentialsSecret` (`SecretReference`, required): Secret with target vCenter credentials.
- `failureDomains` (`[]VSpherePlatformFailureDomainSpec`, required, minimum one): target failure domains, using the OpenShift `configv1` type.
- `failureDomainMappings` (`[]FailureDomainMapping`, optional): maps source failure domain names (from `Infrastructure`) to destination `failureDomains` entries. Required unless exactly one source and one destination failure domain exist; must cover every failure domain exactly once.
>>>>>>> 4b9daa3f (Implement support for storage planning)

### MigrationState Enum

- `Pending`: migration has not started.
- `Planning`: the cluster administrator has requested migration planning. The API accepts this value; planning execution is not yet implemented, so the reconciler skips migration work in this state.
- `Running`: migration is actively progressing.
- `Paused`: migration is paused by the user.

### SecretReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Secret name |
| `namespace` | `string` | No | Secret namespace (defaults to migration namespace) |

The secret must contain keys in the format `<vcenter-fqdn>.username` and `<vcenter-fqdn>.password`.

### FailureDomainMapping

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `source` | `string` | Yes | Source failure domain name, from the cluster's `Infrastructure` resource |
| `destination` | `string` | Yes | Destination failure domain name, matching an entry in `spec.failureDomains` |

### Failure Domain Mapping Behavior

The effective mapping is resolved at preflight and recorded in
`status.failureDomainMappings`; every later phase uses the recorded value. Until the
migration starts (`status.startTime` is unset, i.e. during `Pending` or `Planning`), the
mapping stays editable: preflight re-runs for each spec generation and replaces the
recorded mapping with the newly resolved one. Once the migration has started,
`spec.failureDomainMappings` is only checked for equivalence with the recorded
mapping: reordering the same source→destination pairs is accepted (the comparison ignores
order), and clearing the field is tolerated (the recorded mapping keeps driving every
later phase, and an empty recorded list short-circuits the check too). Changing an
association, or adding or removing mappings, no longer matches the recorded mapping and is
rejected with `Accepted=False` and reason `MappingRejected`, and no phase work runs until
the spec matches the recorded mapping again. A migration created before the field existed
(in-flight operator upgrade) recovers the mapping preflight would have recorded, so
preflight does not need to re-run.

A mapped source failure domain with no source MachineSets is valid and stays empty: no
destination MachineSets are created for it. A mapped failure domain that does carry
capacity but has no verified replacement fails closed before any source MachineSet is
scaled down. Source MachineSets are matched to a failure domain by datacenter and
resource pool, where an empty `topology.resourcePool` means the compute cluster's
`<computeCluster>/Resources` pool, not any pool in the datacenter.

Destination failure domains must not use the source vCenter server. The rollout resolves
the source server from the recorded mapping (never from `Infrastructure.vCenters[0]`) and
refuses to create, scale down, or delete MachineSets on a server configured as a
destination. A generated worker MachineSet name is
`<infrastructureID>-worker-<sanitized destination failure domain name>`, capped at 59
characters (the 63-character Kubernetes label limit less a 4-character reserve for the
`-2`, `-3`, … suffixed copies), so whether the cap is reached depends on the
infrastructure ID length as well as the destination failure domain name. Because the
capped name is derived from the destination failure domain, renaming a destination
failure domain across an operator upgrade can change the generated names.

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
| `topology.template` | `string` | Yes | Existing VM/template inventory path on the target vCenter; required for every failure domain in this release, including VMotion |

## Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Ordered migration phase conditions |
| `failureDomainMappings` | `[]FailureDomainMapping` | Effective source-to-destination failure domain mapping, recorded once at preflight and immutable afterwards; it drives every later phase |
| `workerMachineSetNames` | `[]string` | Append-only inventory of destination worker MachineSet names (max 256), used to require the replacements after the source MachineSets are deleted |
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

`Progressing`, `Completed`, `Failed`, `Paused`, `UnsupportedName`, `UnsupportedNamespace`, `DeletionBlocked`, `MappingRejected`

`Paused` is applied to the `Ready` condition while `spec.state` is `Paused` (message explains how to resume); it is cleared and replaced with `Progressing` when the migration is resumed.

## Print Columns

```text
NAME    STATE     READY   AGE
sample  Running   True    5m
```
