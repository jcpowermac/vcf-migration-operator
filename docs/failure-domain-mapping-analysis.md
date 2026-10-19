# Failure Domain Mapping Analysis

Investigation: what happens today when a customer has multiple existing (source) failure
domains and/or multiple destination failure domains, given there is no mapping between
the Infrastructure spec's failure domains and the destination failure domains in
`spec.failureDomains`.

## Scope for this release

**This release does not support multiple source vCenters or multiple destination
vCenters.** The operator assumes exactly one source vCenter and one destination
vCenter. Destination failure domains may still be multiple, but they must all reference
the same destination vCenter server.

## Short answer

The destination side is already list-based and handles multiple destination FDs
reasonably well. The source side does not, and the sharp edges are exactly where you'd
expect: `[0]`-indexing on the source and name-only identity between source and
destination.

## Multiple destination failure domains — mostly works

`spec.failureDomains` is a list everywhere:

- Preflight validates each entry (unique names, template present, topology reachable
  per FD) — `internal/controller/preflight.go`.
- `ensureWorkloadMigrated` creates one worker MachineSet per destination FD
  (`workerMachineSetName(infraID, fdName)`), evenly splitting `totalReplicas`, with the
  remainder going to the last FD — `vmwarecloudfoundationmigration_controller.go`
  (~line 685). Each destination MachineSet is clamped to at least one replica, so a
  zero or negative source total — or fewer source replicas than destination failure
  domains — yields one replica per destination FD and the resulting total can exceed
  the source total. The proposed mapping behavior below instead preserves a zero
  replica total exactly.
- `UpdateCPMSFailureDomain` (`internal/openshift/machines.go`) replaces the CPMS
  template's FD list wholesale; MAPI spreads the 3 masters across them.
- `AddTargetVCenter` (`internal/openshift/infrastructure.go`) adds all destination FDs
  and deduped vCenter entries.

But with no source→destination mapping, three concrete gaps:

1. **Name collision = silent stall.** MachineSet identity is name-only
   (`<infraID>-worker-<sanitized-fdName>`). If a destination FD has the same name as an
   existing source FD, `GetMachineSet(msName)` finds the *source* MachineSet, creation
   is skipped ("already exists, skipping"), and the operator then waits forever for
   machines on the target that were never created. Nothing validates destination FD
   names against source FD names — only destination-vs-destination uniqueness.
2. **Arbitrary template.** All destination MachineSets are cloned from
   `existingMachineSets[0]` — whichever source MachineSet the API returns first. If
   source FDs had different VM sizes, every destination FD gets one arbitrary size. No
   per-FD mapping of instance type.
3. **Blind replica split.** `totalReplicas / len(FDs)` ignores how workers were actually
   distributed across source FDs.

## Multiple existing (source) failure domains — three scenarios

1. **Multiple FDs, one vCenter, one datacenter** (clusters as FDs): works. All
   source-side operations are keyed by vCenter *server*, not FD —
   `GetMachineSetsByVCenter`, scale-to-0, `DeleteMachineSetsByVCenter`, and
   `RemoveSourceVCenter` (which unlinks every FD referencing that server).

2. **Multiple FDs across multiple datacenters on one vCenter**: **hard preflight
   failure.** `validatePreflightVSphere` rejects `len(sourceVC.Datacenters) > 1` with
   "source vCenter must have exactly one datacenter" — `internal/controller/preflight.go`
   (~line 262). Rejected up front, not mishandled — but it is a common multi-zone
   topology, so it is a real supportability gap.

3. **Multiple source vCenters** (multi-vCenter vSphere, supported by OpenShift since
   4.17): **stalls silently mid-migration.** `GetSourceVCenter` returns `VCenters[0]`
   with no check that there is only one, and every source-side operation — credentials
   lookup, MachineSet enumeration/scale-down/deletion, `RemoveSourceVCenter`,
   cloud-provider-config cleanup — runs against only that first server. Machinesets,
   FDs, and creds on the second vCenter are never touched, so:

   - target MachineSets get created and rolled out (built from a template on vCenter #1),
     control plane moves, workers on vCenter #1 scale down…
   - then `ensureReady` blocks permanently on its own gate: "non-target vCenter %q still
     present in Infrastructure" — `vmwarecloudfoundationmigration_controller.go`
     (~line 1120). The migration never completes rather than completing wrongly — which
     is the right failure mode, but only by accident of that final check.

Also note `GetSourceFailureDomain` (`FailureDomains[0]`) is currently dead code in the
controller — only the unit tests call it — so the `[0]`-indexing habit is latent, not
yet load-bearing.

## Recommended first fix

Given the release scope above, `GetSourceVCenter` should **reject `len(VCenters) > 1`
at preflight** (cheap, honest) rather than silently using `VCenters[0]`. Preflight should
also reject destination failure domains spanning more than one server.

## Failure domain mapping API design (source A ↔ destination B)

Requirement: map Source FD A ↔ Destination FD B by name when more than one failure
domain exists on either side; when there is exactly one source FD and one destination
FD, the mapping is omitted entirely and inferred.

### Chosen design: additive `spec.failureDomainMappings`

> **Status: implemented** on branch `failure-domain-mapping` (Tasks 1–4 of
> `docs/superpowers/plans/2026-10-15-failure-domain-mapping.md`).

A separate, optional list of name pairs referencing existing failure domains by name.
It is purely additive — `spec.failureDomains` keeps its current shape
(`configv1.VSpherePlatformFailureDomainSpec` entries), so existing CRs and manifests
are unaffected.

```go
// FailureDomainMapping maps one source failure domain to one destination failure domain.
type FailureDomainMapping struct {
	// source is the name of the failure domain in the cluster's Infrastructure
	// resource that this entry migrates workloads from.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Source string `json:"source"`

	// destination is the name of the entry in spec.failureDomains that receives
	// the workloads migrated from source.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Destination string `json:"destination"`
}
```

Field on the spec:

```go
// failureDomainMappings maps source failure domains to destination failure
// domains by name. When omitted, exactly one source failure domain and one
// destination failure domain must exist, and they are mapped to each other
// implicitly.
// +optional
// +listType=map
// +listMapKey=source
// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:XValidation:rule="self.map(m, m.destination).distinct().size() == self.size()",message="destination must be unique across mappings"
// +kubebuilder:validation:MaxItems=64
FailureDomainMappings []FailureDomainMapping `json:"failureDomainMappings,omitempty"`
```

Validated with `api_lint_types`: 0 errors, 0 warnings.

### Validation split

**Schema-enforced (CRD validation):**

- `listType=map` + `listMapKey=source` → source names unique within the list.
- XValidation → destination names unique within the list (mapping is injective).
- `MinItems=1` → the field is either absent or non-empty; no meaningful empty list.
- `MaxItems=64` → bounds the quadratic cost of the CEL uniqueness rule and keeps the
  list within a realistic failure-domain count.
- MinLength/MaxLength match `configv1.VSpherePlatformFailureDomainSpec.Name`
  (1–256) so any name accepted here can exist in Infrastructure.

**Reconcile/preflight-enforced (schema cannot see the Infrastructure CR):**

1. If `failureDomainMappings` is set: every `source` must name a failure domain in
   the cluster's Infrastructure spec, and every `destination` must name an entry in
   `spec.failureDomains`. Each must be covered exactly once (complete bijection).
2. If `failureDomainMappings` is omitted: require exactly one source FD (in
   Infrastructure) and exactly one entry in `spec.failureDomains`; anything else is
   rejected at preflight with a message telling the user to provide mappings.
3. Map entries are immutable for the lifetime of the migration (drift would strand
   machinesets mid-rollout).

### What the mapping is used for (semantics)

The mapping fixes the three destination-side gaps identified above:

1. **Identity** — MachineSet correspondence is derived from the mapping, never from
   name collision or `[0]`-indexing. The destination FD-name collision stall becomes
   impossible because the operator never guesses which source machineset a
   destination machineset replaces.
2. **Template inheritance** — each destination worker MachineSet is copied from one source MachineSet in the mapped source FD (one destination MachineSet per source MachineSet), inheriting its provider spec (VM size, etc.). The VM template itself always comes from the destination failure domain's `topology.template`; it is never inherited from the source.
3. **Replica inheritance** — each destination MachineSet starts with its mapped
   source FD's replica count instead of the blind even split with remainder. The sum
   is exact: a source FD deliberately scaled to zero produces zero destination
   replicas (distinct from having no matching MachineSets, which is an error).

Scale-down and cleanup remain vCenter-keyed (unchanged — they already handle many
FDs per vCenter).

### Rejected alternatives

- **Inline wrapper on `spec.failureDomains` entries** (e.g. wrapping
  `configv1.VSpherePlatformFailureDomainSpec` with a `sourceFailureDomain` field):
  changes the element type of an existing list — a breaking schema change for every
  existing CR, just to avoid a second list. Also couples the mapping to every
  destination FD entry even when 1:1 makes it noise.
- **Name convention** (destination FD named like source FD = mapped): fully implicit,
  cannot express renames, and is exactly the collision hazard described above.
- **`map[string]string`**: Go maps are unordered and have no per-item validation
  story; k8s APIs use list types with `listType=map` instead.

## Open questions / not verified

- How the cloud-provider-config ConfigMap handles multiple source vCenters.
- The exact multi-vCenter stall behavior depends on OpenShift's multi-vCenter FD layout
  (faultDomainNames in Infrastructure status) — not verified against a real cluster.
- Whether destination-vs-source FD name collisions occur in practice with common
  naming schemes (e.g., `east`/`west` reused on the target).
