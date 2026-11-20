package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

const (
	vmotionWorkerRole        = "worker"
	vmotionControlPlaneRole  = "control-plane"
	vmotionMachineAnnotation = "machine.openshift.io/machine"

	vmotionPlanFrozenMessage = "VMotion plan frozen; VM actions remain disabled pending the remaining safety stages"
	// vmotionPlanSteadyStateRequeue spaces out post-freeze drift checks: each one
	// repeats a full vCenter login and datacenter inventory.
	vmotionPlanSteadyStateRequeue = 5 * time.Minute
)

func hasVMotionSelection(migration *migrationv1alpha1.VmwareCloudFoundationMigration) bool {
	return migration.Spec.NodeMigration != nil &&
		(migration.Spec.NodeMigration.Workers.Type == migrationv1alpha1.NodeMigrationTypeVMotion ||
			migration.Spec.NodeMigration.ControlPlane.Type == migrationv1alpha1.NodeMigrationTypeVMotion)
}

func sameVMotionVCenter(a, b string) bool {
	return strings.EqualFold(vsphere.SanitizeServer(a), vsphere.SanitizeServer(b))
}

func completeVMotionReference(ref vsphere.VMotionManagedObjectReference) bool {
	return ref.VCenter != "" && ref.Datacenter != "" && ref.Type != "" && ref.Value != ""
}

func parseVMotionProviderID(providerID string) (string, error) {
	const scheme = "vsphere://"
	if len(providerID) <= len(scheme) || !strings.EqualFold(providerID[:len(scheme)], scheme) {
		return "", fmt.Errorf("parsing node providerID %q: expected vsphere://<BIOS UUID>", providerID)
	}
	biosUUID := providerID[len(scheme):]
	if len(biosUUID) != 36 || biosUUID[8] != '-' || biosUUID[13] != '-' || biosUUID[18] != '-' || biosUUID[23] != '-' {
		return "", fmt.Errorf("parsing node providerID %q: expected a canonical BIOS UUID", providerID)
	}
	bytes, err := hex.DecodeString(strings.ReplaceAll(biosUUID, "-", ""))
	if err != nil {
		return "", fmt.Errorf("parsing node providerID %q: decoding BIOS UUID: %w", providerID, err)
	}
	zero := true
	for _, b := range bytes {
		if b != 0 {
			zero = false
			break
		}
	}
	if zero {
		return "", fmt.Errorf("parsing node providerID %q: BIOS UUID must not be zero", providerID)
	}
	return strings.ToLower(biosUUID), nil
}

func selectVMotionSourceFailureDomainByOwner(node *corev1.Node, owner vsphere.VMotionManagedObjectReference, sourceVCenter, sourceDatacenter string, failureDomains []configv1.VSpherePlatformFailureDomainSpec, references map[string]vsphere.VMotionManagedObjectReference) (*configv1.VSpherePlatformFailureDomainSpec, error) {
	if owner.VCenter == "" || owner.Datacenter == "" || owner.Type == "" || owner.Value == "" {
		return nil, fmt.Errorf("resolving source failure domain for node %q: resource pool owner reference is incomplete", node.Name)
	}
	if !sameVMotionVCenter(owner.VCenter, sourceVCenter) {
		return nil, fmt.Errorf("resolving source failure domain for node %q: resource pool owner belongs to a different vCenter", node.Name)
	}
	if owner.Datacenter != sourceDatacenter {
		return nil, fmt.Errorf("resolving source failure domain for node %q: resource pool owner is outside the configured source datacenter", node.Name)
	}
	var selected *configv1.VSpherePlatformFailureDomainSpec
	for i := range failureDomains {
		reference, ok := references[failureDomains[i].Name]
		if !ok || reference.Type == "" || reference.Value == "" || !sameVMotionVCenter(reference.VCenter, sourceVCenter) || reference.Datacenter != sourceDatacenter {
			return nil, fmt.Errorf("resolving source failure domain %q: resolved compute-resource reference is missing or outside the source scope", failureDomains[i].Name)
		}
		if reference.Type != owner.Type || reference.Value != owner.Value {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("resolving source failure domain for node %q: resource pool owner matches multiple source failure domains", node.Name)
		}
		selected = &failureDomains[i]
	}
	if selected == nil {
		return nil, fmt.Errorf("resolving source failure domain for node %q: resource pool owner does not match a configured source failure domain", node.Name)
	}
	if zone, ok := node.Labels[corev1.LabelTopologyZone]; ok && zone != selected.Zone {
		return nil, fmt.Errorf("resolving source failure domain for node %q: topology zone %q conflicts with the matched source failure domain %q", node.Name, zone, selected.Name)
	}
	return selected, nil
}

// reconcileVMotionWorkflow initializes lifecycle status and dispatches VMotion
// planning. Planning state runs the same non-disruptive VMotion checks but
// leaves StartTime unset so the migration can still be reviewed before Running.
func (r *VmwareCloudFoundationMigrationReconciler) reconcileVMotionWorkflow(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, baseStatus migrationv1alpha1.VmwareCloudFoundationMigrationStatus, state migrationv1alpha1.MigrationState) (ctrl.Result, error) {
	r.seedReadyCondition(migration)

	if state == migrationv1alpha1.MigrationStateRunning {
		if err := r.markMigrationStarted(ctx, migration, baseStatus); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileVMotionPlan(ctx, migration, baseStatus)
}

func (r *VmwareCloudFoundationMigrationReconciler) reconcileVMotionPlan(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, baseStatus migrationv1alpha1.VmwareCloudFoundationMigrationStatus) (ctrl.Result, error) {
	if err := validateVMotionSelection(migration.Spec.NodeMigration); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if r.KubeClient == nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, fmt.Errorf("listing nodes for VMotion planning: Kubernetes client is not configured"))
	}

	nodes, err := r.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, fmt.Errorf("listing nodes for VMotion planning: %w", err))
	}
	if err := validateNoTerminatingVMotionNodes(nodes.Items); err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, err)
	}
	if err := validateVMotionNodeRoles(nodes.Items); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if migration.UID == "" {
		return r.blockVMotionPlan(ctx, migration, baseStatus, "VMotion planning requires a persisted migration UID")
	}
	if len(migration.Spec.FailureDomains) == 0 {
		return r.blockVMotionPlan(ctx, migration, baseStatus, "spec.failureDomains must not be empty for VMotion")
	}
	if stop, err := r.checkMappingForReconcile(ctx, migration, baseStatus); stop || err != nil {
		return ctrl.Result{}, err
	}
	if r.ConfigClient == nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, fmt.Errorf("getting infrastructure for VMotion planning: config client is not configured"))
	}

	infrastructure, err := openshift.NewInfrastructureManager(r.ConfigClient).Get(ctx)
	if err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, fmt.Errorf("getting infrastructure for VMotion planning: %w", err))
	}
	if infrastructure.Spec.PlatformSpec.VSphere == nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, "source Infrastructure has no vSphere platform configuration")
	}
	if err := validateUniqueFailureDomainNames(migration.Spec.FailureDomains); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if err := validateFailureDomainServers(migration.Spec.FailureDomains); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if err := validateFailureDomainTypes(migration.Spec.FailureDomains); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	sourceVC, liveServer, err := vmotionSourceVCenter(migration, infrastructure)
	if err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if sameVMotionVCenter(sourceVC.Server, migration.Spec.FailureDomains[0].Server) {
		return r.blockVMotionPlan(ctx, migration, baseStatus, "VMotion source and destination must be different vCenters")
	}
	if err := validateVMotionVCenterScope(infrastructure, sourceVC.Server, migration.Spec.FailureDomains[0].Server); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	sourceFDs, err := vmotionSourceFailureDomains(migration, infrastructure, sourceVC)
	if err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if err := validateUniqueFailureDomainNames(sourceFDs); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if err := validateFailureDomainTypes(sourceFDs); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if len(migration.Status.FailureDomainMappings) == 0 {
		if err := validateSourceVCenterCount(infrastructure); err != nil {
			return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
		}
		return r.recordEffectiveVMotionMapping(ctx, migration, baseStatus, sourceFDs)
	}
	return r.freezeVMotionPlan(ctx, migration, baseStatus, sourceVC, liveServer, sourceFDs, nodes.Items)
}

func validateVMotionSelection(selection *migrationv1alpha1.NodeMigrationSpec) error {
	if selection == nil || selection.Workers.Type != migrationv1alpha1.NodeMigrationTypeVMotion || selection.ControlPlane.Type != migrationv1alpha1.NodeMigrationTypeVMotion {
		return fmt.Errorf("validating VMotion selection: both roles must explicitly select VMotion; mixed or partial engine selections are unsupported")
	}
	if selection.Workers.VMotion == nil || selection.ControlPlane.VMotion == nil ||
		selection.Workers.VMotion.Mode == "" || selection.ControlPlane.VMotion.Mode == "" ||
		selection.Workers.VMotion.Mode != selection.ControlPlane.VMotion.Mode {
		return fmt.Errorf("validating VMotion selection: both roles must explicitly select the same Hot or Cold mode")
	}
	if selection.Workers.VMotion.Mode != migrationv1alpha1.VMotionModeHot && selection.Workers.VMotion.Mode != migrationv1alpha1.VMotionModeCold {
		return fmt.Errorf("validating VMotion selection: mode must be explicitly Hot or Cold")
	}
	return nil
}

func (r *VmwareCloudFoundationMigrationReconciler) recordEffectiveVMotionMapping(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, baseStatus migrationv1alpha1.VmwareCloudFoundationMigrationStatus, sourceFDs []configv1.VSpherePlatformFailureDomainSpec) (ctrl.Result, error) {
	mappings, err := resolveFailureDomainMappings(
		migration.Spec.FailureDomainMappings,
		sourceFDs,
		migration.Spec.FailureDomains,
	)
	if err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	migration.Status.FailureDomainMappings = mappings
	r.setCondition(migration, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Recorded effective failure-domain mapping for VMotion planning")
	if r.Recorder != nil {
		r.Recorder.Eventf(migration, nil, "Normal", migrationv1alpha1.ReasonProgressing, migrationv1alpha1.ReasonProgressing, "Recorded effective failure-domain mapping for VMotion planning")
	}
	if err := r.updateStatus(ctx, migration, baseStatus); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *VmwareCloudFoundationMigrationReconciler) freezeVMotionPlan(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, baseStatus migrationv1alpha1.VmwareCloudFoundationMigrationStatus, sourceVC *configv1.VSpherePlatformVCenterSpec, liveServer string, sourceFDs []configv1.VSpherePlatformFailureDomainSpec, nodes []corev1.Node) (ctrl.Result, error) {
	destinationServer := migration.Spec.FailureDomains[0].Server
	if err := validateVMotionMappings(migration.Status.FailureDomainMappings, sourceFDs, migration.Spec.FailureDomains); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	identities, err := collectVMotionNodeIdentities(nodes)
	if err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if r.MachineClient == nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, fmt.Errorf("checking Machine ownership for VMotion planning: Machine API client is not configured"))
	}
	machineAPI, err := r.readVMotionMachineAPI(ctx)
	if err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, err)
	}
	if err := machineAPI.validate(identities); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	biosUUIDs := make([]string, 0, len(identities))
	for i := range identities {
		biosUUIDs = append(biosUUIDs, identities[i].biosUUID)
	}
	inventory, err := r.listVMotionVMInventory(ctx, sourceVC, liveServer, sourceFDs, biosUUIDs)
	if err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, err)
	}
	// Inventory may take time. Re-read eligibility before persisting a plan so
	// new phase-less Machines or changed Node associations cannot be overlooked.
	latestNodes, err := r.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, fmt.Errorf("relisting Nodes after VMotion inventory: %w", err))
	}
	// Post-inventory observations are compared, never trusted outright: a Node
	// added, removed, relabelled or deleted during inventory is drift, and the
	// next reconcile sees a settled set.
	if err := validateNoTerminatingVMotionNodes(latestNodes.Items); err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, err)
	}
	if err := validateVMotionNodeRoles(latestNodes.Items); err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, err)
	}
	initialIdentities := identities
	identities, err = collectVMotionNodeIdentities(latestNodes.Items)
	if err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, err)
	}
	machineAPI, err = r.readVMotionMachineAPI(ctx)
	if err != nil {
		return r.retryVMotionPlan(ctx, migration, baseStatus, err)
	}
	if err := machineAPI.validate(identities); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if !sameVMotionNodeIdentities(initialIdentities, identities) {
		return r.retryVMotionPlan(ctx, migration, baseStatus, fmt.Errorf("checking VMotion inventory: Node identities changed during inventory; retrying with fresh observations"))
	}
	if err := validateVMotionSourceFailureDomainReferences(sourceFDs, inventory.SourceFailureDomainReferences, sourceVC.Server, sourceVC.Datacenters[0]); err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	candidate, err := buildVMotionPlan(migration, sourceVC, destinationServer, sourceFDs, migration.Status.FailureDomainMappings, migration.Spec.FailureDomains, identities, inventory)
	if err != nil {
		return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
	}
	if migration.Status.NodeMigration != nil {
		if err := validateVMotionPlanStability(migration.Status.NodeMigration, candidate); err != nil {
			return r.blockVMotionPlan(ctx, migration, baseStatus, err.Error())
		}
		return r.progressVMotionPlan(ctx, migration, baseStatus, vmotionPlanSteadyStateRequeue)
	}

	migration.Status.NodeMigration = candidate
	return r.progressVMotionPlan(ctx, migration, baseStatus, 30*time.Second)
}

// Both inputs are sorted by role and name by collectVMotionNodeIdentities.
func sameVMotionNodeIdentities(a, b []vmotionNodeIdentity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].node.Name != b[i].node.Name || a[i].node.UID != b[i].node.UID ||
			a[i].node.Spec.ProviderID != b[i].node.Spec.ProviderID || a[i].role != b[i].role || a[i].biosUUID != b[i].biosUUID {
			return false
		}
	}
	return true
}

type vmotionNodeIdentity struct {
	node     corev1.Node
	role     string
	biosUUID string
}

type vmotionFingerprintNode struct {
	Name                  string                                         `json:"name"`
	Role                  string                                         `json:"role"`
	UID                   string                                         `json:"uid"`
	ProviderID            string                                         `json:"providerID"`
	BIOSUUID              string                                         `json:"biosUUID"`
	SourceInventoryPath   string                                         `json:"sourceInventoryPath"`
	SourceFailureDomain   string                                         `json:"sourceFailureDomain"`
	ResourcePoolOwner     vsphere.VMotionManagedObjectReference          `json:"resourcePoolOwner"`
	OriginalScheduling    migrationv1alpha1.NodeMigrationSchedulingState `json:"originalSchedulingState"`
	TargetFailureDomain   string                                         `json:"targetFailureDomain"`
	TargetDatacenter      string                                         `json:"targetDatacenter"`
	TargetComputeResource string                                         `json:"targetComputeResource"`
	TargetResourcePool    string                                         `json:"targetResourcePool"`
	TargetDatastore       string                                         `json:"targetDatastore"`
	TargetNetwork         string                                         `json:"targetNetwork"`
}

func collectVMotionNodeIdentities(nodes []corev1.Node) ([]vmotionNodeIdentity, error) {
	identities := make([]vmotionNodeIdentity, 0, len(nodes))
	seenUUIDs := make(map[string]string, len(nodes))
	for i := range nodes {
		node := nodes[i]
		role, err := vmotionNodeRole(&node)
		if err != nil {
			return nil, err
		}
		if node.Name == "" || node.UID == "" {
			return nil, fmt.Errorf("planning VMotion Node %q: Node name and UID must be present", node.Name)
		}
		if !node.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("planning VMotion Node %q: Node is terminating; wait for deletion to settle", node.Name)
		}
		biosUUID, err := parseVMotionProviderID(node.Spec.ProviderID)
		if err != nil {
			return nil, err
		}
		if previous, ok := seenUUIDs[biosUUID]; ok {
			return nil, fmt.Errorf("planning VMotion Nodes %q and %q: both providerIDs identify BIOS UUID %q", previous, node.Name, biosUUID)
		}
		seenUUIDs[biosUUID] = node.Name
		identities = append(identities, vmotionNodeIdentity{node: node, role: role, biosUUID: biosUUID})
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].role == identities[j].role {
			return identities[i].node.Name < identities[j].node.Name
		}
		return identities[i].role < identities[j].role
	})
	return identities, nil
}

// vmotionSourceVCenter returns the frozen identity and the live server spelling for credential lookup and connections.
func vmotionSourceVCenter(migration *migrationv1alpha1.VmwareCloudFoundationMigration, infrastructure *configv1.Infrastructure) (*configv1.VSpherePlatformVCenterSpec, string, error) {
	vsphereSpec := infrastructure.Spec.PlatformSpec.VSphere
	if vsphereSpec == nil || len(vsphereSpec.VCenters) == 0 {
		return nil, "", fmt.Errorf("reading source Infrastructure: no vCenter configuration is present")
	}
	server := ""
	datacenter := ""
	if migration.Status.NodeMigration != nil && migration.Status.NodeMigration.FrozenPlan != nil {
		server = migration.Status.NodeMigration.FrozenPlan.SourceVCenter
		datacenter = migration.Status.NodeMigration.FrozenPlan.SourceDatacenter
	}
	if migration.Status.SourceVCenter != "" {
		if server != "" && !sameVMotionVCenter(migration.Status.SourceVCenter, server) {
			return nil, "", fmt.Errorf("resolving source vCenter for VMotion: persisted source identities disagree")
		}
		if server == "" {
			server = migration.Status.SourceVCenter
		}
	}
	if server == "" {
		if len(vsphereSpec.VCenters) != 1 {
			return nil, "", fmt.Errorf("resolving source vCenter for VMotion: expected one entry before source identity is frozen, found %d", len(vsphereSpec.VCenters))
		}
		server = vsphereSpec.VCenters[0].Server
	}
	var selected *configv1.VSpherePlatformVCenterSpec
	for i := range vsphereSpec.VCenters {
		if !sameVMotionVCenter(vsphereSpec.VCenters[i].Server, server) {
			continue
		}
		if selected != nil {
			return nil, "", fmt.Errorf("resolving source vCenter %q for VMotion: multiple Infrastructure entries match", server)
		}
		selected = &vsphereSpec.VCenters[i]
	}
	if selected == nil {
		return nil, "", fmt.Errorf("resolving source vCenter %q for VMotion: no Infrastructure entry matches the frozen source", server)
	}
	if selected.Server == "" {
		return nil, "", fmt.Errorf("validating source vCenter: server must not be empty")
	}
	if len(selected.Datacenters) != 1 || selected.Datacenters[0] == "" {
		return nil, "", fmt.Errorf("validating source vCenter %q: exactly one datacenter must be configured for VMotion", server)
	}
	if datacenter != "" && selected.Datacenters[0] != datacenter {
		return nil, "", fmt.Errorf("validating source vCenter %q: datacenter changed from frozen value %q to %q", server, datacenter, selected.Datacenters[0])
	}
	// Preserve the frozen spelling in inventory references and plan fingerprints.
	sourceVC := selected.DeepCopy()
	sourceVC.Server = server
	return sourceVC, selected.Server, nil
}

func validateVMotionVCenterScope(infrastructure *configv1.Infrastructure, sourceVCenter, destinationVCenter string) error {
	if infrastructure.Spec.PlatformSpec.VSphere == nil {
		return fmt.Errorf("validating vCenter scope: Infrastructure has no vSphere configuration")
	}
	sources, destinations := 0, 0
	for i := range infrastructure.Spec.PlatformSpec.VSphere.VCenters {
		server := infrastructure.Spec.PlatformSpec.VSphere.VCenters[i].Server
		switch {
		case sameVMotionVCenter(server, sourceVCenter):
			sources++
		case sameVMotionVCenter(server, destinationVCenter):
			destinations++
		default:
			return fmt.Errorf("validating vCenter scope: unexpected vCenter %q is configured", server)
		}
	}
	if sources != 1 || destinations > 1 {
		return fmt.Errorf("validating vCenter scope: expected one source and at most one destination entry, found %d source and %d destination entries", sources, destinations)
	}
	return nil
}

func vmotionSourceFailureDomains(migration *migrationv1alpha1.VmwareCloudFoundationMigration, infrastructure *configv1.Infrastructure, sourceVC *configv1.VSpherePlatformVCenterSpec) ([]configv1.VSpherePlatformFailureDomainSpec, error) {
	destinationVCenter := migration.Spec.FailureDomains[0].Server
	var selected []configv1.VSpherePlatformFailureDomainSpec
	for i := range infrastructure.Spec.PlatformSpec.VSphere.FailureDomains {
		fd := infrastructure.Spec.PlatformSpec.VSphere.FailureDomains[i]
		if sameVMotionVCenter(fd.Server, sourceVC.Server) {
			if fd.Topology.Datacenter != sourceVC.Datacenters[0] {
				return nil, fmt.Errorf("validating source failure domain %q: datacenter %q differs from configured source datacenter %q", fd.Name, fd.Topology.Datacenter, sourceVC.Datacenters[0])
			}
			selected = append(selected, fd)
			continue
		}
		if sameVMotionVCenter(fd.Server, destinationVCenter) {
			continue
		}
		return nil, fmt.Errorf("validating source failure domains: failure domain %q references unexpected vCenter %q", fd.Name, fd.Server)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("reading source Infrastructure: no source failure domains are configured for %s/%s", sourceVC.Server, sourceVC.Datacenters[0])
	}
	return selected, nil
}

func validateVMotionMappings(mappings []migrationv1alpha1.FailureDomainMapping, sourceFDs, destinationFDs []configv1.VSpherePlatformFailureDomainSpec) error {
	if len(mappings) == 0 {
		return fmt.Errorf("validating effective failure-domain mappings: no mappings are recorded")
	}
	sources := make(map[string]bool, len(sourceFDs))
	for i := range sourceFDs {
		sources[sourceFDs[i].Name] = true
	}
	destinations := make(map[string]bool, len(destinationFDs))
	for i := range destinationFDs {
		destinations[destinationFDs[i].Name] = true
	}
	seenSources := make(map[string]bool, len(mappings))
	for i := range mappings {
		if !sources[mappings[i].Source] {
			return fmt.Errorf("validating effective failure-domain mappings: source %q is not configured", mappings[i].Source)
		}
		if !destinations[mappings[i].Destination] {
			return fmt.Errorf("validating effective failure-domain mappings: destination %q is not configured", mappings[i].Destination)
		}
		if seenSources[mappings[i].Source] {
			return fmt.Errorf("validating effective failure-domain mappings: source %q has multiple destinations", mappings[i].Source)
		}
		seenSources[mappings[i].Source] = true
	}
	return nil
}

func validateVMotionSourceFailureDomainReferences(failureDomains []configv1.VSpherePlatformFailureDomainSpec, references map[string]vsphere.VMotionManagedObjectReference, vcenter, datacenter string) error {
	if len(references) != len(failureDomains) {
		return fmt.Errorf("resolving source failure domains: no resolved reference exists for every configured failure domain")
	}
	seen := make(map[string]string, len(references))
	for i := range failureDomains {
		fd := &failureDomains[i]
		ref, ok := references[fd.Name]
		if !ok || !completeVMotionReference(ref) {
			return fmt.Errorf("resolving source failure domain %q: no resolved reference exists", fd.Name)
		}
		if !sameVMotionVCenter(ref.VCenter, vcenter) {
			return fmt.Errorf("resolving source failure domain %q: compute-resource reference belongs to a different vCenter", fd.Name)
		}
		if ref.Datacenter != datacenter {
			return fmt.Errorf("resolving source failure domain %q: compute-resource reference is outside the source datacenter", fd.Name)
		}
		key := ref.Type + "/" + ref.Value
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("resolving source failure domains %q and %q: both share compute-resource reference %q", previous, fd.Name, key)
		}
		seen[key] = fd.Name
	}
	return nil
}

func (r *VmwareCloudFoundationMigrationReconciler) listVMotionVMInventory(ctx context.Context, sourceVC *configv1.VSpherePlatformVCenterSpec, liveServer string, sourceFDs []configv1.VSpherePlatformFailureDomainSpec, biosUUIDs []string) (vsphere.VMotionInventory, error) {
	vsphereCtx, cancel := context.WithTimeout(ctx, preflightVSphereTimeout)
	defer cancel()

	sourceFailureDomains := make([]vsphere.VMotionSourceFailureDomain, 0, len(sourceFDs))
	for i := range sourceFDs {
		sourceFailureDomains = append(sourceFailureDomains, vsphere.VMotionSourceFailureDomain{
			Name: sourceFDs[i].Name, ComputeResourcePath: sourceFDs[i].Topology.ComputeCluster,
		})
	}
	if r.vmotionInventoryFunc != nil {
		return r.vmotionInventoryFunc(vsphereCtx, sourceVC, liveServer, sourceFDs, biosUUIDs)
	}
	username, password, err := openshift.NewSecretManager(r.KubeClient).GetCredentials(vsphereCtx, liveServer)
	if err != nil {
		return vsphere.VMotionInventory{}, fmt.Errorf("getting credentials for source vCenter %s: %w", liveServer, err)
	}
	session, err := vsphere.NewSession(vsphereCtx, vsphere.Params{
		Server: liveServer, Datacenter: sourceVC.Datacenters[0], Username: username, Password: password,
	})
	if err != nil {
		return vsphere.VMotionInventory{}, fmt.Errorf("connecting to source vCenter %s/%s: %w", liveServer, sourceVC.Datacenters[0], err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(vsphereCtx), 30*time.Second)
		defer closeCancel()
		session.Close(closeCtx)
	}()
	return vsphere.ListVMotionVMInventory(vsphereCtx, session, sourceVC.Server, sourceFailureDomains, biosUUIDs)
}

func buildVMotionPlan(migration *migrationv1alpha1.VmwareCloudFoundationMigration, sourceVC *configv1.VSpherePlatformVCenterSpec, destinationServer string, sourceFDs []configv1.VSpherePlatformFailureDomainSpec, mappings []migrationv1alpha1.FailureDomainMapping, destinationFDs []configv1.VSpherePlatformFailureDomainSpec, identities []vmotionNodeIdentity, inventory vsphere.VMotionInventory) (*migrationv1alpha1.NodeMigrationStatus, error) {
	mode := migration.Spec.NodeMigration.Workers.VMotion.Mode
	if mode != migrationv1alpha1.VMotionModeHot && mode != migrationv1alpha1.VMotionModeCold {
		return nil, fmt.Errorf("VMotion mode must be explicitly Hot or Cold")
	}
	if len(mappings) == 0 {
		return nil, fmt.Errorf("effective status.failureDomainMappings is empty for VMotion planning")
	}
	vmMatches := make(map[string][]vsphere.VMotionVM, len(inventory.VMs))
	for i := range inventory.VMs {
		vmMatches[strings.ToLower(inventory.VMs[i].BIOSUUID)] = append(vmMatches[strings.ToLower(inventory.VMs[i].BIOSUUID)], inventory.VMs[i])
	}

	status := &migrationv1alpha1.NodeMigrationStatus{
		Type:         migrationv1alpha1.NodeMigrationTypeVMotion,
		ExecutionID:  string(uuid.NewUUID()),
		MigrationUID: migration.UID,
		FrozenPlan: &migrationv1alpha1.NodeMigrationFrozenPlan{
			SourceVCenter: sourceVC.Server, SourceDatacenter: sourceVC.Datacenters[0],
			DestinationVCenter: destinationServer, Mode: mode,
		},
		ControlPlane: &migrationv1alpha1.RoleMigrationStatus{RequestedMode: mode},
		Workers:      &migrationv1alpha1.RoleMigrationStatus{RequestedMode: mode},
		Progress:     vmotionPlanFrozenMessage,
	}
	fingerprintNodes := make([]vmotionFingerprintNode, 0, len(identities))
	for i := range identities {
		identity := &identities[i]
		vm, err := uniqueVMotionVMForNode(identity.node.Name, identity.biosUUID, vmMatches)
		if err != nil {
			return nil, err
		}
		if vm.InventoryPath == "" {
			return nil, fmt.Errorf("resolving VM for Node %q: source VM BIOS UUID %q has no inventory path", identity.node.Name, identity.biosUUID)
		}
		if vm.NetworkAdapterCount != 1 {
			return nil, fmt.Errorf("validating VM for node %q: expected exactly one network adapter, found %d", identity.node.Name, vm.NetworkAdapterCount)
		}
		if !completeVMotionReference(vm.ResourcePoolRef) ||
			!sameVMotionVCenter(vm.ResourcePoolRef.VCenter, sourceVC.Server) || vm.ResourcePoolRef.Datacenter != sourceVC.Datacenters[0] {
			return nil, fmt.Errorf("resolving VM for node %q: resource pool reference is missing or outside the source scope", identity.node.Name)
		}
		sourceFD, err := selectVMotionSourceFailureDomainByOwner(
			&identity.node, vm.ResourcePoolOwner, sourceVC.Server, sourceVC.Datacenters[0], sourceFDs,
			inventory.SourceFailureDomainReferences,
		)
		if err != nil {
			return nil, err
		}
		destinationFD, err := mappedVMotionDestination(sourceFD.Name, mappings, destinationFDs)
		if err != nil {
			return nil, err
		}
		if err := validateVMotionDestinationFailureDomain(destinationFD, destinationServer); err != nil {
			return nil, err
		}
		// An empty topology.resourcePool means the compute cluster's root pool.
		resourcePool := openshift.EffectiveResourcePool(*destinationFD)
		schedulingState := migrationv1alpha1.NodeMigrationSchedulingStateSchedulable
		if identity.node.Spec.Unschedulable {
			schedulingState = migrationv1alpha1.NodeMigrationSchedulingStateUnschedulable
		}
		biosUUID := strings.ToLower(vm.BIOSUUID)
		progress := migrationv1alpha1.NodeMigrationProgress{
			Name: identity.node.Name, State: migrationv1alpha1.NodeMigrationStatePending, RequestedMode: mode,
			VMBIOSUUID: biosUUID, NodeUID: identity.node.UID, ProviderID: identity.node.Spec.ProviderID,
			OriginalSchedulingState: schedulingState, TargetFailureDomain: destinationFD.Name,
			TargetResourcePool: resourcePool, TargetDatastore: destinationFD.Topology.Datastore,
			TargetNetwork: destinationFD.Topology.Networks[0], SourceInventoryPath: vm.InventoryPath,
		}
		if identity.role == vmotionControlPlaneRole {
			status.ControlPlane.Nodes = append(status.ControlPlane.Nodes, progress)
		} else {
			status.Workers.Nodes = append(status.Workers.Nodes, progress)
		}
		fingerprintNodes = append(fingerprintNodes, vmotionFingerprintNode{
			Name: identity.node.Name, Role: identity.role, UID: string(identity.node.UID), ProviderID: identity.node.Spec.ProviderID,
			BIOSUUID: biosUUID, SourceInventoryPath: vm.InventoryPath, SourceFailureDomain: sourceFD.Name,
			ResourcePoolOwner:  vm.ResourcePoolOwner,
			OriginalScheduling: schedulingState, TargetFailureDomain: destinationFD.Name,
			TargetDatacenter: destinationFD.Topology.Datacenter, TargetComputeResource: destinationFD.Topology.ComputeCluster,
			TargetResourcePool: resourcePool, TargetDatastore: destinationFD.Topology.Datastore,
			TargetNetwork: destinationFD.Topology.Networks[0],
		})
	}
	status.ControlPlane.Total = int32(len(status.ControlPlane.Nodes))
	status.ControlPlane.Pending = status.ControlPlane.Total
	status.Workers.Total = int32(len(status.Workers.Nodes))
	status.Workers.Pending = status.Workers.Total
	fingerprint, err := vmotionPlanFingerprint(migration.UID, *status.FrozenPlan, mappings, fingerprintNodes)
	if err != nil {
		return nil, fmt.Errorf("fingerprinting frozen VMotion plan: %w", err)
	}
	status.PlanFingerprint = fingerprint
	return status, nil
}

func uniqueVMotionVMForNode(nodeName, biosUUID string, inventory map[string][]vsphere.VMotionVM) (vsphere.VMotionVM, error) {
	matches := inventory[strings.ToLower(biosUUID)]
	if len(matches) == 0 {
		return vsphere.VMotionVM{}, fmt.Errorf("resolving VM for Node %q: no source vSphere VM has BIOS UUID %q", nodeName, biosUUID)
	}
	if len(matches) != 1 {
		return vsphere.VMotionVM{}, fmt.Errorf("resolving VM for Node %q: BIOS UUID %q matches %d source vSphere VMs", nodeName, biosUUID, len(matches))
	}
	return matches[0], nil
}

func mappedVMotionDestination(source string, mappings []migrationv1alpha1.FailureDomainMapping, destinationFDs []configv1.VSpherePlatformFailureDomainSpec) (*configv1.VSpherePlatformFailureDomainSpec, error) {
	destination := ""
	for i := range mappings {
		if mappings[i].Source != source {
			continue
		}
		if destination != "" {
			return nil, fmt.Errorf("effective failure-domain mapping selects multiple destinations for source failure domain %q", source)
		}
		destination = mappings[i].Destination
	}
	if destination == "" {
		return nil, fmt.Errorf("effective failure-domain mapping has no destination for source failure domain %q", source)
	}
	var selected *configv1.VSpherePlatformFailureDomainSpec
	for i := range destinationFDs {
		if destinationFDs[i].Name != destination {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("destination failure domain %q is ambiguous", destination)
		}
		selected = &destinationFDs[i]
	}
	if selected == nil {
		return nil, fmt.Errorf("effective failure-domain mapping references missing destination failure domain %q", destination)
	}
	return selected, nil
}

func vmotionPlanFingerprint(uid types.UID, frozen migrationv1alpha1.NodeMigrationFrozenPlan, mappings []migrationv1alpha1.FailureDomainMapping, nodes []vmotionFingerprintNode) (string, error) {
	mappingCopy := append([]migrationv1alpha1.FailureDomainMapping(nil), mappings...)
	sort.Slice(mappingCopy, func(i, j int) bool { return mappingCopy[i].Source < mappingCopy[j].Source })
	nodeCopy := append([]vmotionFingerprintNode(nil), nodes...)
	sort.Slice(nodeCopy, func(i, j int) bool {
		if nodeCopy[i].Role == nodeCopy[j].Role {
			return nodeCopy[i].Name < nodeCopy[j].Name
		}
		return nodeCopy[i].Role < nodeCopy[j].Role
	})
	data, err := json.Marshal(struct {
		MigrationUID types.UID                                 `json:"migrationUID"`
		FrozenPlan   migrationv1alpha1.NodeMigrationFrozenPlan `json:"frozenPlan"`
		Mappings     []migrationv1alpha1.FailureDomainMapping  `json:"mappings"`
		Nodes        []vmotionFingerprintNode                  `json:"nodes"`
	}{MigrationUID: uid, FrozenPlan: frozen, Mappings: mappingCopy, Nodes: nodeCopy})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// validateVMotionPlanStability reports every observed difference so the operator
// can see what drifted; a frozen plan is never republished.
func validateVMotionPlanStability(existing, candidate *migrationv1alpha1.NodeMigrationStatus) error {
	drifted := vmotionFieldDrift([][3]string{
		{"engine type", string(existing.Type), string(candidate.Type)},
		{"migration UID", string(existing.MigrationUID), string(candidate.MigrationUID)},
	})
	if existing.ExecutionID == "" {
		drifted = append(drifted, "execution ID is missing")
	}
	if existing.FrozenPlan == nil {
		drifted = append(drifted, "frozen plan is missing")
	} else {
		drifted = append(drifted, vmotionFieldDrift([][3]string{
			{"source vCenter", existing.FrozenPlan.SourceVCenter, candidate.FrozenPlan.SourceVCenter},
			{"source datacenter", existing.FrozenPlan.SourceDatacenter, candidate.FrozenPlan.SourceDatacenter},
			{"destination vCenter", existing.FrozenPlan.DestinationVCenter, candidate.FrozenPlan.DestinationVCenter},
			{"mode", string(existing.FrozenPlan.Mode), string(candidate.FrozenPlan.Mode)},
		})...)
	}
	if existing.PlanFingerprint != candidate.PlanFingerprint {
		drifted = append(drifted, "plan fingerprint")
	}
	drifted = append(drifted, frozenNodeDrift(vmotionControlPlaneRole, existing.ControlPlane, candidate.ControlPlane)...)
	drifted = append(drifted, frozenNodeDrift(vmotionWorkerRole, existing.Workers, candidate.Workers)...)
	if len(drifted) == 0 {
		return nil
	}
	return fmt.Errorf("frozen VMotion plan drifted (%s); the plan is immutable, so delete and recreate the migration resource to plan again", strings.Join(drifted, "; "))
}

func vmotionFieldDrift(fields [][3]string) []string {
	var drifted []string
	for _, field := range fields {
		if field[1] != field[2] {
			drifted = append(drifted, fmt.Sprintf("%s changed from %q to %q", field[0], field[1], field[2]))
		}
	}
	return drifted
}

func frozenNodeDrift(role string, existing, candidate *migrationv1alpha1.RoleMigrationStatus) []string {
	byName := func(list *migrationv1alpha1.RoleMigrationStatus) map[string]*migrationv1alpha1.NodeMigrationProgress {
		nodes := map[string]*migrationv1alpha1.NodeMigrationProgress{}
		if list == nil {
			return nodes
		}
		for i := range list.Nodes {
			nodes[list.Nodes[i].Name] = &list.Nodes[i]
		}
		return nodes
	}
	frozen, fresh := byName(existing), byName(candidate)
	var drifted []string
	for name := range frozen {
		if _, ok := fresh[name]; !ok {
			drifted = append(drifted, fmt.Sprintf("%s Node %q was removed", role, name))
		}
	}
	for name, node := range fresh {
		previous, ok := frozen[name]
		if !ok {
			drifted = append(drifted, fmt.Sprintf("%s Node %q was added", role, name))
			continue
		}
		for _, field := range vmotionFieldDrift([][3]string{
			{"node UID", string(previous.NodeUID), string(node.NodeUID)},
			{"providerID", previous.ProviderID, node.ProviderID},
			{"VM BIOS UUID", previous.VMBIOSUUID, node.VMBIOSUUID},
			{"requested mode", string(previous.RequestedMode), string(node.RequestedMode)},
			{"original scheduling state", string(previous.OriginalSchedulingState), string(node.OriginalSchedulingState)},
			{"target failure domain", previous.TargetFailureDomain, node.TargetFailureDomain},
			{"target resource pool", previous.TargetResourcePool, node.TargetResourcePool},
			{"target datastore", previous.TargetDatastore, node.TargetDatastore},
			{"target network", previous.TargetNetwork, node.TargetNetwork},
			{"source inventory path", previous.SourceInventoryPath, node.SourceInventoryPath},
		}) {
			drifted = append(drifted, fmt.Sprintf("%s Node %q %s", role, name, field))
		}
	}
	sort.Strings(drifted)
	return drifted
}

func (r *VmwareCloudFoundationMigrationReconciler) retryVMotionPlan(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, baseStatus migrationv1alpha1.VmwareCloudFoundationMigrationStatus, err error) (ctrl.Result, error) {
	message := boundConditionMessage(err.Error())
	condition := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != migrationv1alpha1.ReasonRetrying ||
		condition.Message != message || condition.ObservedGeneration != migration.Generation {
		r.setCondition(migration, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionFalse, migrationv1alpha1.ReasonRetrying, message)
		if r.Recorder != nil {
			r.Recorder.Eventf(migration, nil, "Warning", migrationv1alpha1.ReasonRetrying, migrationv1alpha1.ReasonRetrying, "%s", truncateToBytes(message, maxEventNoteBytes))
		}
	}
	if statusErr := r.updateStatus(ctx, migration, baseStatus); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

func (r *VmwareCloudFoundationMigrationReconciler) progressVMotionPlan(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, baseStatus migrationv1alpha1.VmwareCloudFoundationMigrationStatus, requeue time.Duration) (ctrl.Result, error) {
	message := vmotionPlanFrozenMessage
	condition := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != migrationv1alpha1.ReasonProgressing ||
		condition.Message != message || condition.ObservedGeneration != migration.Generation {
		r.setCondition(migration, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, message)
		if r.Recorder != nil {
			r.Recorder.Eventf(migration, nil, "Normal", migrationv1alpha1.ReasonProgressing, migrationv1alpha1.ReasonProgressing, "%s", message)
		}
	}
	if err := r.updateStatus(ctx, migration, baseStatus); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// validateNoTerminatingVMotionNodes keeps a Node replacement in the retry bucket
// instead of reporting it as an ineligible cluster.
func validateNoTerminatingVMotionNodes(nodes []corev1.Node) error {
	for i := range nodes {
		if !nodes[i].DeletionTimestamp.IsZero() {
			return fmt.Errorf("node %q is terminating; wait for deletion to settle", nodes[i].Name)
		}
	}
	return nil
}

func validateVMotionNodeRoles(nodes []corev1.Node) error {
	controlPlane, workers := 0, 0
	for i := range nodes {
		role, err := vmotionNodeRole(&nodes[i])
		if err != nil {
			return err
		}
		switch role {
		case vmotionControlPlaneRole:
			controlPlane++
		case vmotionWorkerRole:
			workers++
		}
	}
	if controlPlane != 3 {
		return fmt.Errorf("cluster must have exactly three control-plane Nodes for VMotion, found %d", controlPlane)
	}
	if workers == 0 {
		return fmt.Errorf("cluster must have at least one worker Node for VMotion")
	}
	// Only the worker half is reachable: the control-plane count is fixed at three above.
	if workers > 500 {
		return fmt.Errorf("VMotion status supports at most 500 worker Nodes; found %d", workers)
	}
	return nil
}

// validateVMotionDestinationFailureDomain names the fields VMotion freezes for
// destination placement. An empty topology.resourcePool is allowed and resolves
// to the compute cluster's root pool.
func validateVMotionDestinationFailureDomain(fd *configv1.VSpherePlatformFailureDomainSpec, destinationServer string) error {
	missing := make([]string, 0, 4)
	if fd.Server != destinationServer {
		missing = append(missing, fmt.Sprintf("server %q", destinationServer))
	}
	if fd.Topology.Datacenter == "" {
		missing = append(missing, "topology.datacenter")
	}
	if fd.Topology.ComputeCluster == "" {
		missing = append(missing, "topology.computeCluster")
	}
	if fd.Topology.Datastore == "" {
		missing = append(missing, "topology.datastore")
	}
	if len(fd.Topology.Networks) != 1 || fd.Topology.Networks[0] == "" {
		missing = append(missing, "exactly one topology.networks entry")
	}
	if len(missing) > 0 {
		return fmt.Errorf("destination failure domain %q must specify %s for VMotion", fd.Name, strings.Join(missing, ", "))
	}
	return nil
}

func vmotionNodeRole(node *corev1.Node) (string, error) {
	labels := node.GetLabels()
	controlPlane := hasNodeRole(labels, "control-plane") || hasNodeRole(labels, "master")
	worker := hasNodeRole(labels, "worker")
	if controlPlane && worker {
		return "", fmt.Errorf("node %q has both worker and control-plane roles; dual-role Nodes are unsupported for VMotion", node.Name)
	}
	if !controlPlane && !worker {
		return "", fmt.Errorf("node %q has no supported worker or control-plane role; unclassified Nodes are unsupported for VMotion", node.Name)
	}
	if controlPlane {
		return vmotionControlPlaneRole, nil
	}
	return vmotionWorkerRole, nil
}

func hasNodeRole(labels map[string]string, role string) bool {
	_, ok := labels["node-role.kubernetes.io/"+role]
	return ok
}

func validateVMotionMachineEligibility(identities []vmotionNodeIdentity, machines []machinev1beta1.Machine) error {
	indexes := buildVMotionMachineIndexes(identities, machines)
	annotationNodes, err := vmotionMachineAnnotationNodes(identities, indexes.machineRefs)
	if err != nil {
		return err
	}
	machineMatchesByNode := make(map[types.UID]map[string]bool, len(identities))
	for i := range machines {
		machine := &machines[i]
		ref := machine.Namespace + "/" + machine.Name
		linkedNodes, err := vmotionMachineNodeLinks(machine, ref, indexes, annotationNodes[ref])
		if err != nil {
			return err
		}
		if len(linkedNodes) > 1 {
			return fmt.Errorf("machine %s has conflicting associations with multiple planned Nodes; ownership is indeterminate", ref)
		}
		for uid := range linkedNodes {
			if machineMatchesByNode[uid] == nil {
				machineMatchesByNode[uid] = make(map[string]bool)
			}
			machineMatchesByNode[uid][ref] = true
		}
	}

	for i := range identities {
		identity := &identities[i]
		matches := machineMatchesByNode[identity.node.UID]
		if len(matches) > 1 {
			var refs []string
			for ref := range matches {
				refs = append(refs, ref)
			}
			sort.Strings(refs)
			return fmt.Errorf("node %q is associated with multiple Machines %s; ownership is indeterminate", identity.node.Name, strings.Join(refs, ", "))
		}
		if len(matches) == 1 {
			for ref := range matches {
				return fmt.Errorf("node %q is Machine-managed by %s; VMotion requires verified unmanaged Nodes", identity.node.Name, ref)
			}
		}
	}
	for i := range machines {
		if err := validateVMotionMachinePhase(&machines[i]); err != nil {
			return err
		}
	}
	return nil
}

type vmotionMachineIndexes struct {
	machineRefs map[string]struct{}
	nodeByUID   map[types.UID]vmotionNodeIdentity
	nodeByName  map[string]vmotionNodeIdentity
	nodeByBIOS  map[string]vmotionNodeIdentity
	nodeByIP    map[string][]types.UID
}

func buildVMotionMachineIndexes(identities []vmotionNodeIdentity, machines []machinev1beta1.Machine) *vmotionMachineIndexes {
	indexes := &vmotionMachineIndexes{
		machineRefs: make(map[string]struct{}, len(machines)),
		nodeByUID:   make(map[types.UID]vmotionNodeIdentity, len(identities)),
		nodeByName:  make(map[string]vmotionNodeIdentity, len(identities)),
		nodeByBIOS:  make(map[string]vmotionNodeIdentity, len(identities)),
		nodeByIP:    make(map[string][]types.UID),
	}
	for i := range identities {
		identity := identities[i]
		indexes.nodeByUID[identity.node.UID] = identity
		indexes.nodeByName[identity.node.Name] = identity
		indexes.nodeByBIOS[identity.biosUUID] = identity
		for _, address := range identity.node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP && address.Address != "" {
				indexes.nodeByIP[address.Address] = append(indexes.nodeByIP[address.Address], identity.node.UID)
			}
		}
	}
	for i := range machines {
		machine := &machines[i]
		indexes.machineRefs[machine.Namespace+"/"+machine.Name] = struct{}{}
	}
	return indexes
}

func vmotionMachineAnnotationNodes(identities []vmotionNodeIdentity, machineRefs map[string]struct{}) (map[string]map[types.UID]bool, error) {
	annotationNodes := make(map[string]map[types.UID]bool)
	for i := range identities {
		value := identities[i].node.Annotations[vmotionMachineAnnotation]
		if value == "" {
			continue
		}
		ref, err := parseVMotionMachineReference(value)
		if err != nil {
			return nil, fmt.Errorf("resolving Machine annotation on Node %q: %w", identities[i].node.Name, err)
		}
		if _, ok := machineRefs[ref]; !ok {
			continue
		}
		if annotationNodes[ref] == nil {
			annotationNodes[ref] = make(map[types.UID]bool)
		}
		annotationNodes[ref][identities[i].node.UID] = true
	}
	return annotationNodes, nil
}

func vmotionMachineNodeLinks(machine *machinev1beta1.Machine, machineRef string, indexes *vmotionMachineIndexes, annotations map[types.UID]bool) (map[types.UID]bool, error) {
	linkedNodes := make(map[types.UID]bool)
	if machine.Status.NodeRef != nil {
		nodeRef := machine.Status.NodeRef
		if nodeRef.UID != "" {
			if identity, ok := indexes.nodeByUID[nodeRef.UID]; ok {
				if nodeRef.Name != "" && nodeRef.Name != identity.node.Name {
					return nil, fmt.Errorf("machine %s references Node UID %q with conflicting name %q; ownership is indeterminate", machineRef, nodeRef.UID, nodeRef.Name)
				}
				linkedNodes[identity.node.UID] = true
			} else if identity, ok := indexes.nodeByName[nodeRef.Name]; ok {
				return nil, fmt.Errorf("machine %s references planned Node %q with a missing or changed UID; ownership is indeterminate", machineRef, identity.node.Name)
			}
		} else if identity, ok := indexes.nodeByName[nodeRef.Name]; ok {
			return nil, fmt.Errorf("machine %s references planned Node %q without a UID; ownership is indeterminate", machineRef, identity.node.Name)
		}
	}
	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		if biosUUID, err := parseVMotionProviderID(*machine.Spec.ProviderID); err == nil {
			if identity, ok := indexes.nodeByBIOS[biosUUID]; ok {
				linkedNodes[identity.node.UID] = true
			}
		}
	}
	for uid := range annotations {
		linkedNodes[uid] = true
	}
	if err := validateVMotionAssociationEvidence(machine, machineRef, indexes, linkedNodes); err != nil {
		return nil, err
	}
	return linkedNodes, nil
}

func validateVMotionAssociationEvidence(machine *machinev1beta1.Machine, machineRef string, indexes *vmotionMachineIndexes, linkedNodes map[types.UID]bool) error {
	if len(linkedNodes) == 1 {
		for uid := range linkedNodes {
			identity := indexes.nodeByUID[uid]
			if ref := machine.Status.NodeRef; ref != nil && ((ref.UID != "" && ref.UID != uid) || (ref.Name != "" && ref.Name != identity.node.Name)) {
				return fmt.Errorf("machine %s has contradictory NodeRef and association evidence for Node %q; ownership is indeterminate", machineRef, identity.node.Name)
			}
			if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
				biosUUID, err := parseVMotionProviderID(*machine.Spec.ProviderID)
				if err != nil || biosUUID != identity.biosUUID {
					return fmt.Errorf("machine %s has contradictory or invalid providerID evidence for Node %q; ownership is indeterminate", machineRef, identity.node.Name)
				}
			}
		}
	}
	// NodeLink falls back to internal IP after providerID lookup. An IP-only
	// candidate can acquire a link, but is never sufficient VM identity here.
	var unlinked []string
	for _, address := range machine.Status.Addresses {
		if address.Type != corev1.NodeInternalIP || address.Address == "" {
			continue
		}
		for _, uid := range indexes.nodeByIP[address.Address] {
			if !linkedNodes[uid] {
				unlinked = append(unlinked, indexes.nodeByUID[uid].node.Name)
			}
		}
	}
	if len(unlinked) > 0 {
		sort.Strings(unlinked)
		return fmt.Errorf("machine %s has an IP-only association with Nodes %s; ownership is indeterminate", machineRef, strings.Join(unlinked, ", "))
	}
	return nil
}

func parseVMotionMachineReference(value string) (string, error) {
	parts := strings.Split(value, "/")
	namespace, name := openshift.MachineAPINamespace, ""
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		namespace, name = parts[0], parts[1]
	default:
		return "", fmt.Errorf("annotation value %q must be a Machine name or namespace/name", value)
	}
	if len(validation.IsDNS1123Label(namespace)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return "", fmt.Errorf("annotation value %q must be a Machine name or namespace/name with valid Kubernetes names", value)
	}
	return namespace + "/" + name, nil
}

func (r *VmwareCloudFoundationMigrationReconciler) blockVMotionPlan(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, baseStatus migrationv1alpha1.VmwareCloudFoundationMigrationStatus, message string) (ctrl.Result, error) {
	message = boundConditionMessage(message)
	condition := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	alreadyBlocked := condition != nil && condition.Status == metav1.ConditionFalse &&
		condition.Reason == migrationv1alpha1.ReasonFailed && condition.Message == message &&
		condition.ObservedGeneration == migration.Generation
	if !alreadyBlocked {
		r.setCondition(migration, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionFalse, migrationv1alpha1.ReasonFailed, message)
		if r.Recorder != nil {
			note := fmt.Sprintf("Condition %s failed: %s", migrationv1alpha1.ConditionInfrastructurePrepared, message)
			r.Recorder.Eventf(migration, nil, "Warning", "ConditionFailed", "ConditionFailed", "%s", truncateToBytes(note, maxEventNoteBytes))
		}
	}
	if err := r.updateStatus(ctx, migration, baseStatus); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
