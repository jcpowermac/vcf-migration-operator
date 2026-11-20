package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

// resolveFailureDomainMappings returns the effective source-to-destination
// failure domain mapping. An explicit mapping list must reference only known
// failure domain names and must cover each side exactly once; an omitted list
// is only valid when both sides have exactly one failure domain, in which case
// the inferred pair is returned.
func resolveFailureDomainMappings(mappings []migrationv1alpha1.FailureDomainMapping, sourceFDs, destFDs []configv1.VSpherePlatformFailureDomainSpec) ([]migrationv1alpha1.FailureDomainMapping, error) {
	sourceNames := make(map[string]bool, len(sourceFDs))
	for i := range sourceFDs {
		sourceNames[sourceFDs[i].Name] = true
	}
	destNames := make(map[string]bool, len(destFDs))
	for i := range destFDs {
		destNames[destFDs[i].Name] = true
	}

	if len(mappings) == 0 {
		if len(sourceFDs) == 1 && len(destFDs) == 1 {
			return []migrationv1alpha1.FailureDomainMapping{{Source: sourceFDs[0].Name, Destination: destFDs[0].Name}}, nil
		}
		return nil, fmt.Errorf("spec.failureDomainMappings is required: provide mappings when the source and destination failure domains are not a single pair (got %d source and %d destination failure domains)", len(sourceFDs), len(destFDs))
	}

	for i := range mappings {
		if !sourceNames[mappings[i].Source] {
			return nil, fmt.Errorf("failure domain mapping references unknown source failure domain %q", mappings[i].Source)
		}
		if !destNames[mappings[i].Destination] {
			return nil, fmt.Errorf("failure domain mapping references unknown destination failure domain %q", mappings[i].Destination)
		}
	}

	if len(mappings) != len(sourceFDs) || len(mappings) != len(destFDs) {
		return nil, fmt.Errorf("spec.failureDomainMappings must cover every source and destination failure domain exactly once: got %d mappings for %d source and %d destination failure domains", len(mappings), len(sourceFDs), len(destFDs))
	}

	return mappings, nil
}

// seedFailureDomainMappings recovers the effective mapping for a migration
// that passed preflight before status.failureDomainMappings existed. Preflight
// never re-runs for an existing CR, so an in-flight operator upgrade would
// otherwise dead-end with no mapping recorded. Source failure domains are the
// Infrastructure entries that are not configured destinations; they are
// re-resolved with the same rules preflight applies, so an ambiguous spec still
// fails closed.
func seedFailureDomainMappings(migration *migrationv1alpha1.VmwareCloudFoundationMigration, infraFDs []configv1.VSpherePlatformFailureDomainSpec) ([]migrationv1alpha1.FailureDomainMapping, error) {
	var sourceFDs []configv1.VSpherePlatformFailureDomainSpec
	for i := range infraFDs {
		if !isConfiguredDestination(migration, infraFDs[i].Server) {
			sourceFDs = append(sourceFDs, infraFDs[i])
		}
	}
	return resolveFailureDomainMappings(migration.Spec.FailureDomainMappings, sourceFDs, migration.Spec.FailureDomains)
}

// sourceVCenterServer returns the vCenter server of the mapped source failure
// domain. Unlike InfrastructureManager.GetSourceVCenter it never resolves
// VCenters[0]: AddTargetVCenter appends today, but any reordering would
// otherwise point the destructive steps at the destination.
func sourceVCenterServer(infraFDs []configv1.VSpherePlatformFailureDomainSpec, mappings []migrationv1alpha1.FailureDomainMapping) (string, error) {
	if len(mappings) == 0 {
		return "", fmt.Errorf("no failure domain mappings recorded in status: preflight must complete before workload migration")
	}
	fd := findFailureDomainByName(infraFDs, mappings[0].Source)
	if fd == nil {
		return "", fmt.Errorf("resolving source failure domain %q in infrastructure", mappings[0].Source)
	}
	if fd.Server == "" {
		return "", fmt.Errorf("source failure domain %q has no vCenter server configured", fd.Name)
	}
	return fd.Server, nil
}

// ensureMappingImmutable enforces the immutability of the effective failure
// domain mapping recorded in status once the migration has started. Before
// that, during Pending or Planning, the mapping stays editable: preflight
// re-runs for every spec generation and records the newly resolved mapping, so
// an edit is revalidated rather than rejected. It deliberately does not
// re-resolve from Infrastructure: the Infrastructure legitimately changes
// during migration (destination failure domains added, source ones removed).
// Clearing spec.failureDomainMappings after the start is allowed because the
// recorded effective mapping keeps driving every later phase.
func (r *VmwareCloudFoundationMigrationReconciler) ensureMappingImmutable(migration *migrationv1alpha1.VmwareCloudFoundationMigration) error {
	if migration.Status.StartTime == nil {
		return nil
	}
	recorded := migration.Status.FailureDomainMappings
	if len(recorded) == 0 || len(migration.Spec.FailureDomainMappings) == 0 {
		return nil
	}

	// Compare order-insensitively: reordered but equivalent entries are valid.
	requested := mappingBySource(migration.Spec.FailureDomainMappings)
	bySource := mappingBySource(recorded)
	if len(requested) != len(bySource) {
		return fmt.Errorf("failureDomainMappings are immutable after the migration starts: got %d mappings, want %d as recorded in status", len(requested), len(bySource))
	}
	for source, dest := range requested {
		want, ok := bySource[source]
		if !ok {
			return fmt.Errorf("failureDomainMappings are immutable after the migration starts: source failure domain %q is not part of the recorded mapping", source)
		}
		if want != dest {
			return fmt.Errorf("failureDomainMappings are immutable after the migration starts: source failure domain %q is mapped to %q, want %q as recorded in status", source, dest, want)
		}
	}
	return nil
}

// mappingBySource keys failure domain mappings by source name so lists can be
// compared order-insensitively.
func mappingBySource(mappings []migrationv1alpha1.FailureDomainMapping) map[string]string {
	bySource := make(map[string]string, len(mappings))
	for i := range mappings {
		bySource[mappings[i].Source] = mappings[i].Destination
	}
	return bySource
}

// validateFailureDomainServers verifies that all destination failure domains
// reference the same, non-empty vCenter server. Multiple destination servers
// are out of scope for this release.
func validateFailureDomainServers(fds []configv1.VSpherePlatformFailureDomainSpec) error {
	for i := range fds {
		if fds[i].Server == "" {
			return fmt.Errorf("failure domain %q has no vCenter server configured", fds[i].Name)
		}
		if fds[i].Server != fds[0].Server {
			return fmt.Errorf("spec.failureDomains must use a single vCenter server: %q and %q are both present", fds[0].Server, fds[i].Server)
		}
	}
	return nil
}

// validateSourceVCenterCount verifies that the source Infrastructure has
// exactly one vCenter entry. This holds only at preflight time, before the
// destination vCenter is added.
func validateSourceVCenterCount(infra *configv1.Infrastructure) error {
	if infra.Spec.PlatformSpec.VSphere == nil {
		return fmt.Errorf("infrastructure spec has no vSphere platform configuration")
	}
	if n := len(infra.Spec.PlatformSpec.VSphere.VCenters); n != 1 {
		return fmt.Errorf("source infrastructure must have exactly one vCenter, found %d", n)
	}
	return nil
}

// validateSourceFDTopologyDisjoint verifies that source failure domains
// sharing a datacenter resolve to distinct resource pools, so a source
// MachineSet can match at most one failure domain. An empty
// topology.resourcePool is not a wildcard: it resolves to the compute
// cluster's default pool, so distinct compute clusters are disjoint. A failure
// domain with neither field set resolves to no pool and matches any pool in its
// datacenter, which overlaps every other failure domain there.
func validateSourceFDTopologyDisjoint(sourceFDs []configv1.VSpherePlatformFailureDomainSpec) error {
	for i := range sourceFDs {
		for j := i + 1; j < len(sourceFDs); j++ {
			a, b := &sourceFDs[i], &sourceFDs[j]
			if a.Topology.Datacenter != b.Topology.Datacenter {
				continue
			}
			poolA, poolB := openshift.EffectiveResourcePool(*a), openshift.EffectiveResourcePool(*b)
			if poolA == "" || poolB == "" || poolA == poolB {
				return fmt.Errorf("source failure domains %q and %q have overlapping topology in datacenter %q: each must specify a distinct resource pool", a.Name, b.Name, a.Topology.Datacenter)
			}
		}
	}
	return nil
}

// validateMachineSetNameCollisions verifies that the worker MachineSet names
// generated for the destination failure domains do not collide with existing
// MachineSets. It runs at preflight, before the mappings are recorded as
// immutable, so renaming the destination failure domain is still possible.
func (r *VmwareCloudFoundationMigrationReconciler) validateMachineSetNameCollisions(ctx context.Context, infraID string, mappings []migrationv1alpha1.FailureDomainMapping, sourceFDs, destFDs []configv1.VSpherePlatformFailureDomainSpec) error {
	log := klog.FromContext(ctx)

	msList, err := r.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing machinesets in %s: %w", openshift.MachineAPINamespace, err)
	}
	log.V(2).Info("checking generated MachineSet names for collisions", "machinesets", len(msList.Items))

	generated := make(map[string]bool)
	bases := make([]string, 0, len(mappings))
	for i := range mappings {
		var fd *configv1.VSpherePlatformFailureDomainSpec
		for j := range destFDs {
			if destFDs[j].Name == mappings[i].Destination {
				fd = &destFDs[j]
				break
			}
		}
		if fd == nil {
			return fmt.Errorf("resolving destination failure domain %q for mapping from %q", mappings[i].Destination, mappings[i].Source)
		}
		base := workerMachineSetName(infraID, fd.Name)
		for _, previous := range bases {
			if base == previous || isSuffixedMachineSetName(base, previous) || isSuffixedMachineSetName(previous, base) {
				return fmt.Errorf("generated MachineSet names for %q and %q overlap; rename the destination failure domain", previous, base)
			}
		}
		bases = append(bases, base)
		source := findFailureDomainByName(sourceFDs, mappings[i].Source)
		if source == nil {
			return fmt.Errorf("resolving source failure domain %q", mappings[i].Source)
		}
		sourceSets, err := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient).GetMachineSetsByFailureDomain(ctx, source.Server, source)
		if err != nil {
			return fmt.Errorf("listing machinesets for source failure domain %q: %w", source.Name, err)
		}
		for si := 0; si < max(1, len(sourceSets)); si++ {
			name := suffixedWorkerMachineSetName(base, si)
			if len(name) > maxGeneratedMachineSetNameLen {
				return fmt.Errorf("generated MachineSet name %q exceeds the %d-character Kubernetes label limit; rename the destination failure domain", name, maxGeneratedMachineSetNameLen)
			}
			if generated[name] {
				return fmt.Errorf("generated MachineSet name %q collides with another destination", name)
			}
			generated[name] = true
		}
		for j := range msList.Items {
			name := msList.Items[j].Name
			if name == base || isSuffixedMachineSetName(name, base) {
				return fmt.Errorf("generated MachineSet name %q collides with existing MachineSet; rename the destination failure domain", name)
			}
		}
	}

	return nil
}

// suffixedWorkerMachineSetName returns the generated worker MachineSet name
// for the source MachineSet at index si (0-based) copied into a destination
// failure domain: the base name for the first copy, then base-2, base-3, ...
// so multiple source MachineSets mapped to one failure domain get distinct
// names.
func suffixedWorkerMachineSetName(base string, si int) string {
	if si == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, si+1)
}

// isSuffixedMachineSetName reports whether name is base with a numeric suffix
// (base-2, base-3, ...), as generated when one failure domain receives
// multiple copied source MachineSets.
func isSuffixedMachineSetName(name, base string) bool {
	suffix, ok := strings.CutPrefix(name, base+"-")
	if !ok || suffix == "" {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// recordedWorkerMachineSetNames returns the recorded destination MachineSet
// names matching base (base itself plus suffixed copies), sorted. Recorded
// names survive source cleanup, unlike the source MachineSets they were
// derived from.
func recordedWorkerMachineSetNames(recorded []string, base string) []string {
	var names []string
	for _, name := range recorded {
		if name == base || isSuffixedMachineSetName(name, base) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// workerMachineSetNamesForBase returns the names of the worker MachineSets
// generated for the destination failure domain whose base name is base: base
// itself plus every suffixed copy (base-2, base-3, ...) present in msList.
// base is always included so a missing base MachineSet still surfaces as an
// error from the caller's per-name lookups.
func workerMachineSetNamesForBase(msList []*machinev1beta1.MachineSet, base string) []string {
	names := []string{base}
	for _, ms := range msList {
		if isSuffixedMachineSetName(ms.Name, base) {
			names = append(names, ms.Name)
		}
	}
	return names
}

// expectedDestinationNames returns the destination MachineSet names that must
// exist for one mapped destination failure domain, and whether the aggregate the
// released operator created still supplies them.
//
// Invariant: the result is always
// recordedWorkerMachineSetNames(recorded, base) ∪ the names derived from the
// current source MachineSets, so a recorded name is never dropped. A legacy
// aggregate may only suppress newly derived names beyond the base one: it holds
// the capacity of every mapped source MachineSet already, so per-source copies
// next to it would duplicate capacity.
//
// An ambiguous destination fails here rather than resolving to either layout:
// creating per-source copies next to a released aggregate duplicates capacity,
// and accepting a per-source base as an aggregate drops the added capacity.
//
// The existence check, the creation path and name recovery all use this function
// so they cannot disagree and requeue forever.
func expectedDestinationNames(ctx context.Context, machineMgr *openshift.MachineManager, base string, sourceMSList []*machinev1beta1.MachineSet, fd *configv1.VSpherePlatformFailureDomainSpec, recorded []string) (names []string, legacyAggregate bool, err error) {
	log := klog.FromContext(ctx)

	// Deterministic order keeps the suffixed names (-2, -3, ...) stable across
	// reconciles. Clone so callers keep their own ordering.
	sources := slices.Clone(sourceMSList)
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })

	layout, baseReplicas, sourceReplicas, err := classifyDestinationLayout(ctx, machineMgr, base, sources, fd, recorded)
	if err != nil {
		return nil, false, err
	}

	var derived []string
	switch layout {
	case destinationLayoutAmbiguous:
		return nil, false, fmt.Errorf("destination MachineSet %q has no source MachineSet annotation and %d replicas do not match the %d replicas of its source MachineSets: verify the MachineSet's capacity and, if it is the released operator's aggregate, annotate it with %s=true; otherwise restore the expected source MachineSets before retrying", base, baseReplicas, sourceReplicas, openshift.LegacyAggregateAnnotation)
	case destinationLayoutLegacyAggregate:
		log.V(1).Info("legacy aggregate destination MachineSet detected", "name", base, "failureDomain", fd.Name, "sourceMachineSets", len(sources))
		legacyAggregate = true
		derived = []string{base}
	default:
		derived = make([]string, 0, len(sources))
		for si := range sources {
			name := suffixedWorkerMachineSetName(base, si)
			destination, err := machineMgr.GetMachineSet(ctx, name)
			if err != nil && !apierrors.IsNotFound(err) {
				return nil, false, fmt.Errorf("getting machineset %q: %w", name, err)
			}
			if err == nil && machineSetDesiredReplicas(destination) < machineSetDesiredReplicas(sources[si]) {
				providerSpec, err := openshift.ExtractVSphereProviderSpec(destination)
				if err != nil {
					return nil, false, fmt.Errorf("extracting provider spec from machineset %q: %w", name, err)
				}
				if providerSpec.Workspace == nil || providerSpec.Workspace.Server != fd.Server {
					return nil, false, fmt.Errorf("generated MachineSet name %q collides with existing MachineSet; rename the destination failure domain", name)
				}

				return nil, false, fmt.Errorf("destination MachineSet %q has %d replicas but source MachineSet %q runs %d: restore destination capacity before retrying", name, machineSetDesiredReplicas(destination), sources[si].Name, machineSetDesiredReplicas(sources[si]))
			}
			derived = append(derived, name)
		}
	}

	names = recordedWorkerMachineSetNames(recorded, base)
	for _, name := range derived {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, legacyAggregate, nil
}

// destinationLayout records how the destination MachineSet named base relates to
// the current source MachineSets of its mapped failure domain.
type destinationLayout int

const (
	// destinationLayoutPerSource is the layout this code creates: one destination
	// MachineSet per source MachineSet, named base, base-2, base-3, ...
	destinationLayoutPerSource destinationLayout = iota
	// destinationLayoutLegacyAggregate is the single aggregate MachineSet the
	// released operator created, holding the capacity of every mapped source
	// MachineSet at once.
	destinationLayoutLegacyAggregate
	// destinationLayoutAmbiguous is an unannotated base that fits neither layout,
	// so creating per-source copies could duplicate capacity and accepting it
	// could drop capacity.
	destinationLayoutAmbiguous
)

// classifyDestinationLayout reports how the existing destination MachineSet
// named base relates to the current source MachineSets, plus the replica counts
// needed to explain an ambiguous result. sources must be sorted by name.
//
// The destination MachineSet annotation identifies a per-source copy, because
// every copy this code creates carries it. A base the operator annotated as the
// released operator's aggregate is legacy regardless of its replica count, which
// stops proving provenance once scale-down starts, but it still has to hold the
// capacity of the source MachineSets it replaces. Otherwise an unannotated base
// under a multi-source mapping with no suffixed copy next to it is either the
// released operator's aggregate or a per-source base created before the
// annotation existed, so only the aggregate's own replica count can separate
// them.
func classifyDestinationLayout(ctx context.Context, machineMgr *openshift.MachineManager, base string, sources []*machinev1beta1.MachineSet, fd *configv1.VSpherePlatformFailureDomainSpec, recorded []string) (layout destinationLayout, baseReplicas, sourceReplicas int32, err error) {
	existing, err := machineMgr.GetMachineSet(ctx, base)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return destinationLayoutPerSource, 0, 0, nil
		}
		return destinationLayoutPerSource, 0, 0, fmt.Errorf("getting machineset %q: %w", base, err)
	}
	providerSpec, err := openshift.ExtractVSphereProviderSpec(existing)
	if err != nil {
		return destinationLayoutPerSource, 0, 0, fmt.Errorf("extracting provider spec from machineset %q: %w", base, err)
	}
	// A base targeting another vCenter is a name collision, never a replacement:
	// the per-source layout hands it to the creation path's collision error.
	if providerSpec.Workspace == nil || providerSpec.Workspace.Server != fd.Server {
		return destinationLayoutPerSource, 0, 0, nil
	}
	baseReplicas = machineSetDesiredReplicas(existing)
	for _, source := range sources {
		sourceReplicas += machineSetDesiredReplicas(source)
	}
	// Provenance the operator recorded itself outranks every other signal: after
	// an interrupted Step 6 the source total matches neither the aggregate sum
	// nor a per-source copy, so re-deriving the layout would deadlock Step 1
	// before scale-down could ever retry.
	//
	// This check stays ahead of the single-source shortcut below because one
	// surviving source can be larger than the aggregate after a partial cleanup,
	// so the shortcut must not skip the capacity proof.
	if existing.Annotations[openshift.LegacyAggregateAnnotation] == openshift.LegacyAggregateAnnotationValue {
		if err := validateLegacyAggregateCapacity(base, baseReplicas, sourceReplicas); err != nil {
			return destinationLayoutLegacyAggregate, baseReplicas, sourceReplicas, err
		}
		return destinationLayoutLegacyAggregate, baseReplicas, sourceReplicas, nil
	}
	// With fewer than two current sources the aggregate and the per-source copy
	// are the same MachineSet, so this code always creates the latter.
	if len(sources) < 2 {
		return destinationLayoutPerSource, 0, 0, nil
	}
	// More than one recorded name means the per-source layout already ran.
	if len(recordedWorkerMachineSetNames(recorded, base)) > 1 {
		return destinationLayoutPerSource, 0, 0, nil
	}
	if existing.Annotations[openshift.SourceMachineSetAnnotation] != "" {
		// Every copy this code creates carries the annotation, so an annotated
		// base is a per-source copy and never the aggregate.
		return destinationLayoutPerSource, 0, 0, nil
	}
	// A suffixed copy means the per-source layout already ran next to the base.
	for si := 1; si < len(sources); si++ {
		copyName := suffixedWorkerMachineSetName(base, si)
		if _, err := machineMgr.GetMachineSet(ctx, copyName); err == nil {
			return destinationLayoutPerSource, 0, 0, nil
		} else if !apierrors.IsNotFound(err) {
			return destinationLayoutPerSource, 0, 0, fmt.Errorf("getting machineset %q: %w", copyName, err)
		}
	}
	// Only replicas are left to decide: the aggregate held the sum of the mapped
	// source replicas, while a per-source base copies sources[0]. A per-source
	// base can only equal the sum when every other source is scaled to zero, so
	// that coincidence cannot lose running capacity.
	//
	// Every source at zero replicas is our own post-scale-down state, so an
	// unannotated base cannot be under-provisioned there and ambiguity would
	// deadlock Step 1 before Step 8 deletes the sources. The capacity guard runs
	// on this fallback too, so the two legacy classifications cannot disagree;
	// the equality or zero source total already satisfies it, which leaves this
	// path's behaviour unchanged.
	if sourceReplicas == 0 || baseReplicas == sourceReplicas {
		if err := validateLegacyAggregateCapacity(base, baseReplicas, sourceReplicas); err != nil {
			return destinationLayoutLegacyAggregate, baseReplicas, sourceReplicas, err
		}
		return destinationLayoutLegacyAggregate, baseReplicas, sourceReplicas, nil
	}
	return destinationLayoutAmbiguous, baseReplicas, sourceReplicas, nil
}

// validateLegacyAggregateCapacity fails closed when a legacy aggregate holds less
// capacity than the source MachineSets it replaces. The aggregate annotation is
// provenance, not a bypass: a source MachineSet grown or added after the
// aggregate was annotated would otherwise be accepted by Step 1, measured only by
// the aggregate's own replicas, and then scaled away by Step 6, losing capacity
// silently. An aggregate at or above the source total is accepted, which covers
// this code's own scale-down because the source total only ever decreases.
func validateLegacyAggregateCapacity(base string, aggregateReplicas, sourceReplicas int32) error {
	if aggregateReplicas >= sourceReplicas {
		return nil
	}
	return fmt.Errorf("legacy aggregate destination MachineSet %q has %d replicas but its source MachineSets run %d: scale the destination MachineSet up to at least the source total, or reduce the source MachineSets, before retrying", base, aggregateReplicas, sourceReplicas)
}

// machineSetDesiredReplicas returns a MachineSet's desired replicas. A nil
// value defaults to 1, matching MAPI's defaulting.
func machineSetDesiredReplicas(ms *machinev1beta1.MachineSet) int32 {
	if ms.Spec.Replicas == nil {
		return 1
	}
	return *ms.Spec.Replicas
}

// matchSourceMachineSetFailureDomain resolves the source failure domain a
// MachineSet belongs to. It returns whether the MachineSet runs replicas
// (nil replicas default to 1 in MAPI, so nil counts as running capacity), the
// machine's workspace, and the single matching source failure domain, or nil
// when none matches.
func matchSourceMachineSetFailureDomain(ms *machinev1beta1.MachineSet, sourceFDs []configv1.VSpherePlatformFailureDomainSpec) (bool, *machinev1beta1.Workspace, *configv1.VSpherePlatformFailureDomainSpec, error) {
	providerSpec, err := openshift.ExtractVSphereProviderSpec(ms)
	if err != nil {
		return false, nil, nil, fmt.Errorf("extracting provider spec from machineset %q: %w", ms.Name, err)
	}
	if providerSpec.Workspace == nil {
		return false, nil, nil, fmt.Errorf("extracting provider spec from machineset %q: workspace is nil", ms.Name)
	}
	hasReplicas := ms.Spec.Replicas == nil || *ms.Spec.Replicas > 0
	matched := 0
	var matchedFD *configv1.VSpherePlatformFailureDomainSpec
	for i := range sourceFDs {
		if matchesSourceFailureDomain(providerSpec.Workspace, sourceFDs[i]) {
			matched++
			matchedFD = &sourceFDs[i]
		}
	}
	if matched > 1 {
		return false, nil, nil, fmt.Errorf("validating source machineset coverage: machineset %q matches multiple source failure domains", ms.Name)
	}
	return hasReplicas, providerSpec.Workspace, matchedFD, nil
}

// validateSourceMachineSetMappingCoverage checks that every source-vCenter
// MachineSet with running replicas matches exactly one source failure domain
// that has a mapping. Unlike validateSourceMachineSetCoverage it does not
// require recorded destination MachineSet names, so it runs at preflight
// before the replacements exist. Empty source failure domains and
// zero-replica MachineSets are fine: neither runs capacity that needs a
// replacement.
func validateSourceMachineSetMappingCoverage(ctx context.Context, msList []*machinev1beta1.MachineSet, mappings []migrationv1alpha1.FailureDomainMapping, sourceFDs []configv1.VSpherePlatformFailureDomainSpec) error {
	return forEachRunningSourceMachineSet(ctx, msList, mappings, sourceFDs, func(*machinev1beta1.MachineSet, *migrationv1alpha1.FailureDomainMapping) error {
		return nil
	})
}

// validateSourceMachineSetCoverage fails closed before source worker
// scale-down when a source-vCenter MachineSet with running replicas is not
// covered by the failure domain mapping with a verified replacement
// MachineSet. Zero-replica MachineSets are skipped and logged: they run no
// machines, so no replacement is needed. msList is the fresh source-vCenter
// listing the caller already has.
func validateSourceMachineSetCoverage(ctx context.Context, msList []*machinev1beta1.MachineSet, infraID string, mappings []migrationv1alpha1.FailureDomainMapping, sourceFDs []configv1.VSpherePlatformFailureDomainSpec, recorded []string) error {
	return forEachRunningSourceMachineSet(ctx, msList, mappings, sourceFDs, func(ms *machinev1beta1.MachineSet, mapping *migrationv1alpha1.FailureDomainMapping) error {
		base := workerMachineSetName(infraID, mapping.Destination, recorded...)
		if len(recordedWorkerMachineSetNames(recorded, base)) == 0 {
			return fmt.Errorf("validating source machineset coverage: machineset %q has no recorded destination MachineSet for failure domain %q", ms.Name, mapping.Destination)
		}
		return nil
	})
}

// forEachRunningSourceMachineSet calls fn for every source MachineSet with
// running replicas that matches exactly one mapped source failure domain, and
// fails closed for one that matches none or an unmapped one.
func forEachRunningSourceMachineSet(ctx context.Context, msList []*machinev1beta1.MachineSet, mappings []migrationv1alpha1.FailureDomainMapping, sourceFDs []configv1.VSpherePlatformFailureDomainSpec, fn func(*machinev1beta1.MachineSet, *migrationv1alpha1.FailureDomainMapping) error) error {
	log := klog.FromContext(ctx)

	for _, ms := range msList {
		hasReplicas, workspace, matchedFD, err := matchSourceMachineSetFailureDomain(ms, sourceFDs)
		if err != nil {
			return err
		}
		if !hasReplicas {
			log.V(1).Info("skipping zero-replica source MachineSet for coverage gate", "machineSet", ms.Name)
			continue
		}
		if matchedFD == nil {
			return fmt.Errorf(
				"validating source machineset coverage: machineset %q (datacenter %q, resource pool %q) does not match any source failure domain",
				ms.Name, workspace.Datacenter, workspace.ResourcePool)
		}
		var mapping *migrationv1alpha1.FailureDomainMapping
		for i := range mappings {
			if mappings[i].Source == matchedFD.Name {
				mapping = &mappings[i]
				break
			}
		}
		if mapping == nil {
			return fmt.Errorf("validating source machineset coverage: machineset %q matches source failure domain %q which has no mapping", ms.Name, matchedFD.Name)
		}
		if err := fn(ms, mapping); err != nil {
			return err
		}
	}
	return nil
}

// matchesSourceFailureDomain reports whether a provider spec workspace falls
// within the failure domain: same datacenter and same resource pool, where the
// failure domain's pool is resolved with openshift.EffectiveResourcePool so an
// empty topology.resourcePool means its compute cluster's default pool. Only a
// failure domain with no determinable pool matches every pool in that
// datacenter. This is the same predicate as
// MachineManager.GetMachineSetsByFailureDomain.
func matchesSourceFailureDomain(workspace *machinev1beta1.Workspace, fd configv1.VSpherePlatformFailureDomainSpec) bool {
	if workspace.Datacenter != fd.Topology.Datacenter {
		return false
	}
	pool := openshift.EffectiveResourcePool(fd)
	return pool == "" || workspace.ResourcePool == pool
}

// recoverUnrecordedDestinationNames re-records destination worker MachineSet
// names that were created but lost from status by a crash between creation and
// updateStatus. The destination failure domain list comes from the migration
// spec because sourceFDs holds only infrastructure (source) failure domains.
// Derived names come from expectedDestinationNames, so recovery never requires a
// name the resolver does not require (a legacy aggregate keeps scale-down
// working); each expected name must resolve to a MachineSet that exists and
// targets the destination failure domain's vCenter, so an incomplete creation
// fails closed instead of letting scale-down drop capacity.
func recoverUnrecordedDestinationNames(ctx context.Context, machineMgr *openshift.MachineManager, migration *migrationv1alpha1.VmwareCloudFoundationMigration, msList []*machinev1beta1.MachineSet, infraID string, mappings []migrationv1alpha1.FailureDomainMapping, sourceFDs []configv1.VSpherePlatformFailureDomainSpec) ([]string, error) {
	log := klog.FromContext(ctx)
	// Names are only recorded once every mapping validated, so an error
	// leaves WorkerMachineSetNames untouched.
	var recovered []string

	for i := range mappings {
		sourceFD := findFailureDomainByName(sourceFDs, mappings[i].Source)
		var matched []*machinev1beta1.MachineSet
		if sourceFD != nil {
			for _, ms := range msList {
				providerSpec, err := openshift.ExtractVSphereProviderSpec(ms)
				if err != nil {
					return nil, fmt.Errorf("extracting provider spec from machineset %q: %w", ms.Name, err)
				}
				if providerSpec.Workspace == nil {
					return nil, fmt.Errorf("extracting provider spec from machineset %q: workspace is nil", ms.Name)
				}
				if matchesSourceFailureDomain(providerSpec.Workspace, *sourceFD) {
					matched = append(matched, ms)
				}
			}
		}
		destFD := findFailureDomainByName(migration.Spec.FailureDomains, mappings[i].Destination)
		if destFD == nil {
			return nil, fmt.Errorf("resolving destination failure domain %q for mapping from %q", mappings[i].Destination, mappings[i].Source)
		}
		base, err := resolveWorkerMachineSetName(ctx, machineMgr, infraID, mappings[i].Destination, migration.Status.WorkerMachineSetNames)
		if err != nil {
			return nil, err
		}
		if len(matched) == 0 && len(recordedWorkerMachineSetNames(migration.Status.WorkerMachineSetNames, base)) == 0 {
			// Only an unrecorded empty mapping is intentionally empty.
			log.V(1).Info("skipping name recovery, no source or recorded destination MachineSets", "sourceFailureDomain", mappings[i].Source)
			continue
		}
		// Same resolver as the existence check and the creation path: recovery
		// must not require a name the resolver does not, and must keep recorded
		// names required.
		expected, _, err := expectedDestinationNames(ctx, machineMgr, base, matched, destFD, migration.Status.WorkerMachineSetNames)
		if err != nil {
			return nil, err
		}
		for _, name := range expected {
			existing, err := machineMgr.GetMachineSet(ctx, name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return nil, fmt.Errorf("recovering unrecorded destination MachineSets: machineset %q does not exist; re-run required", name)
				}
				return nil, fmt.Errorf("getting machineset %q: %w", name, err)
			}
			providerSpec, err := openshift.ExtractVSphereProviderSpec(existing)
			if err != nil {
				return nil, fmt.Errorf("extracting provider spec from machineset %q: %w", name, err)
			}
			// Same collision check as the creation loop: a MachineSet under a
			// generated name that targets another vCenter is a collision, not a
			// recovered replacement.
			if providerSpec.Workspace == nil || providerSpec.Workspace.Server != destFD.Server {
				return nil, fmt.Errorf("generated MachineSet name %q collides with existing MachineSet; rename the destination failure domain", name)
			}
			if slices.Contains(migration.Status.WorkerMachineSetNames, name) || slices.Contains(recovered, name) {
				continue
			}
			recovered = append(recovered, name)
			log.V(2).Info("recovered unrecorded destination MachineSet name", "name", name)
		}
	}
	migration.Status.WorkerMachineSetNames = append(migration.Status.WorkerMachineSetNames, recovered...)
	return recovered, nil
}
