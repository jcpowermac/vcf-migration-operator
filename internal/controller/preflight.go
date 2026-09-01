package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

const vsphereCSIDriverName = "csi.vsphere.vmware.com"
const preflightVSphereTimeout = 2 * time.Minute
const rhcosArchAMD64 = "x86_64"

var (
	machineHealthCheckGVR = schema.GroupVersionResource{Group: "machine.openshift.io", Version: "v1beta1", Resource: "machinehealthchecks"}
	clusterAutoscalerGVR  = schema.GroupVersionResource{Group: "autoscaling.openshift.io", Version: "v1", Resource: "clusterautoscalers"}
	machineAutoscalerGVR  = schema.GroupVersionResource{Group: "autoscaling.openshift.io", Version: "v1beta1", Resource: "machineautoscalers"}
	clusterCSIDriverGVR   = schema.GroupVersionResource{Group: "operator.openshift.io", Version: "v1", Resource: "clustercsidrivers"}
	storageOperatorGVR    = schema.GroupVersionResource{Group: "operator.openshift.io", Version: "v1", Resource: "storages"}
)

// platformMachineHealthChecks lists MHCs that do not interfere with cross-vCenter migration.
// machine-api-termination-handler only reacts to cloud provider preemption signals,
// not node health conditions, so it is safe to leave in place during migration.
var platformMachineHealthChecks = map[string]bool{
	"openshift-machine-api/machine-api-termination-handler": true,
}

var (
	rootTagPrivileges = []string{
		"InventoryService.Tagging.AttachTag",
		"InventoryService.Tagging.CreateCategory",
		"InventoryService.Tagging.CreateTag",
	}
	objectAttachPrivileges = []string{
		"InventoryService.Tagging.ObjectAttachable",
	}
	vmFolderPrivileges = []string{
		"Folder.Create",
	}
)

type credentials struct {
	username string
	password string
}

// fdTemplateMissing returns an error naming the first failure domain whose
// topology.template is empty. Called from runPreflightChecks only when spec.image
// is not set, because in that mode the operator does not import a template and
// each failure domain must reference an existing one.
func fdTemplateMissing(fds []configv1.VSpherePlatformFailureDomainSpec) error {
	for i := range fds {
		if fds[i].Topology.Template == "" {
			return fmt.Errorf("spec.failureDomains[%d].topology.template is required when spec.image is not set (failure domain %q)", i, fds[i].Name)
		}
	}
	return nil
}

func (r *VmwareCloudFoundationMigrationReconciler) runPreflightChecks(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (string, error) {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionInfrastructurePrepared

	if len(migration.Spec.FailureDomains) == 0 {
		return "", fmt.Errorf("spec.failureDomains must not be empty")
	}
	if err := validateUniqueFailureDomainNames(migration.Spec.FailureDomains); err != nil {
		return "", err
	}
	if migration.Spec.Image == nil {
		if err := fdTemplateMissing(migration.Spec.FailureDomains); err != nil {
			return "", err
		}
	}

	secretRef := migration.Spec.TargetVCenterCredentialsSecret
	if secretRef.Name == "" {
		return "", fmt.Errorf("spec.targetVCenterCredentialsSecret.name must not be empty")
	}
	ns := secretRef.Namespace
	if ns == "" {
		ns = migration.Namespace
	}
	if _, err := r.KubeClient.CoreV1().Secrets(ns).Get(ctx, secretRef.Name, metav1.GetOptions{}); err != nil {
		return "", fmt.Errorf("target credentials secret %s/%s not found: %w", ns, secretRef.Name, err)
	}

	support, err := openshift.GetVSphereMultiVCenterSupport(ctx, r.ConfigClient)
	if err != nil {
		return "", fmt.Errorf("checking cluster readiness: %w", err)
	}
	if !support.FeatureGateEnabled {
		return "", fmt.Errorf("feature gate VSphereMultiVCenterDay2 is not enabled for OpenShift %s; enable the feature gate before starting migration", support.ClusterVersion)
	}
	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Validating cluster readiness")
	if support.UpgradeInProgress {
		return "", fmt.Errorf("cluster upgrade is in progress; wait for ClusterVersion/version Progressing=False before starting migration")
	}

	opMgr := openshift.NewOperatorManager(r.ConfigClient)
	healthy, unhealthyOperators, err := opMgr.CheckAllOperatorsHealthy(ctx)
	if err != nil {
		return "", fmt.Errorf("checking cluster operator health: %w", err)
	}
	if !healthy {
		return "", fmt.Errorf("cluster operators are not healthy; wait for operators to recover before starting migration: %s", strings.Join(unhealthyOperators, ", "))
	}

	if err := checkNoVSphereCSIPersistentVolumes(ctx, r.KubeClient); err != nil {
		return "", err
	}
	storageWarning, err := checkVSphereStorageManagement(ctx, r.DynamicClient)
	if err != nil {
		return "", err
	}
	if err := checkInterferingRolloutResources(ctx, r.DynamicClient); err != nil {
		return "", err
	}

	vsphereCtx, cancel := context.WithTimeout(ctx, preflightVSphereTimeout)
	defer cancel()
	defer func() {
		log.V(2).Info("clearing cached vSphere sessions after preflight")
		vsphere.ClearSessions(vsphereCtx)
	}()

	if err := r.validatePreflightVSphere(ctx, vsphereCtx, migration); err != nil {
		return "", err
	}

	message := "Preflight validation passed"
	if storageWarning != "" {
		message = fmt.Sprintf("%s; warning: %s", message, storageWarning)
	}
	return message, nil
}

// validatePreflightVSphere connects to the source and target vCenters and
// verifies that the failure domain topology (datacenter, cluster, datastore,
// networks, template) is reachable on each target. When spec.image is set,
// it also performs a best-effort reachability check against the OVA URL.
func (r *VmwareCloudFoundationMigrationReconciler) validatePreflightVSphere(ctx context.Context, vsphereCtx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) error {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionInfrastructurePrepared

	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	sourceVC, err := infraMgr.GetSourceVCenter(ctx)
	if err != nil {
		return fmt.Errorf("getting source vCenter: %w", err)
	}

	sm := openshift.NewSecretManager(r.KubeClient)
	srcUser, srcPass, err := sm.GetCredentials(ctx, sourceVC.Server)
	if err != nil {
		return fmt.Errorf("getting source vCenter credentials: %w", err)
	}

	if len(sourceVC.Datacenters) == 0 {
		return fmt.Errorf("source vCenter has no datacenters configured")
	}
	if len(sourceVC.Datacenters) > 1 {
		return fmt.Errorf("source vCenter must have exactly one datacenter configured, found %d", len(sourceVC.Datacenters))
	}
	srcDC := sourceVC.Datacenters[0]
	srcSession, err := getVSphereSession(vsphereCtx, sourceVC.Server, srcDC, srcUser, srcPass)
	if err != nil {
		return fmt.Errorf("connecting to source vCenter %s: %w", sourceVC.Server, err)
	}
	if _, err := srcSession.Finder.Datacenter(vsphereCtx, srcDC); err != nil {
		return fmt.Errorf("source datacenter %q not accessible: %w", srcDC, err)
	}
	log.V(1).Info("source vCenter connectivity validated", "server", sourceVC.Server)

	targetCredentialsByServer := make(map[string]credentials, len(migration.Spec.FailureDomains))

	for i := range migration.Spec.FailureDomains {
		fd := &migration.Spec.FailureDomains[i]
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, fmt.Sprintf("Validating target failure domain %q", fd.Name))

		creds, ok := targetCredentialsByServer[fd.Server]
		if !ok {
			username, password, err := getTargetCredentials(ctx, r.KubeClient, migration, fd.Server)
			if err != nil {
				return fmt.Errorf("getting credentials for target %s: %w", fd.Server, err)
			}
			creds = credentials{username: username, password: password}
			targetCredentialsByServer[fd.Server] = creds
		}

		if err := validateFailureDomain(vsphereCtx, migration, fd, creds); err != nil {
			return err
		}
		log.V(1).Info("target failure domain validated", "name", fd.Name, "server", fd.Server)
	}

	return nil
}

func validateFailureDomain(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, fd *configv1.VSpherePlatformFailureDomainSpec, creds credentials) error {
	session, err := getVSphereSession(ctx, fd.Server, fd.Topology.Datacenter, creds.username, creds.password)
	if err != nil {
		return fmt.Errorf("connecting to target vCenter %s: %w", fd.Server, err)
	}

	datacenter, err := session.Finder.Datacenter(ctx, fd.Topology.Datacenter)
	if err != nil {
		return fmt.Errorf("target datacenter %q on %s not found: %w", fd.Topology.Datacenter, fd.Server, err)
	}
	cluster, err := session.Finder.ClusterComputeResource(ctx, fd.Topology.ComputeCluster)
	if err != nil {
		return fmt.Errorf("target cluster %q on %s not found: %w", fd.Topology.ComputeCluster, fd.Server, err)
	}
	if _, err := session.Finder.Datastore(ctx, fd.Topology.Datastore); err != nil {
		return fmt.Errorf("target datastore %q on %s not found: %w", fd.Topology.Datastore, fd.Server, err)
	}
	for _, networkName := range fd.Topology.Networks {
		if _, err := session.Finder.Network(ctx, networkName); err != nil {
			return fmt.Errorf("target network %q on %s not found: %w", networkName, fd.Server, err)
		}
	}
	if fd.Topology.ResourcePool != "" {
		if _, err := session.Finder.ResourcePool(ctx, fd.Topology.ResourcePool); err != nil {
			return fmt.Errorf("target resource pool %q on %s not found: %w", fd.Topology.ResourcePool, fd.Server, err)
		}
	}
	if fd.Topology.Folder != "" {
		if _, err := session.Finder.Folder(ctx, fd.Topology.Folder); err != nil {
			return fmt.Errorf("target folder %q on %s not found: %w", fd.Topology.Folder, fd.Server, err)
		}
	}
	// Template check: skip when spec.image is set and topology.template is
	// empty — the template will be created by ensureDestinationImageImported. A
	// missing template with spec.image unset is rejected earlier in
	// runPreflightChecks.
	if fd.Topology.Template != "" {
		if _, err := session.Finder.VirtualMachine(ctx, fd.Topology.Template); err != nil {
			return fmt.Errorf("target template %q on %s not found: %w", fd.Topology.Template, fd.Server, err)
		}
	}

	if err := validateTargetPrivileges(ctx, session, datacenter, cluster); err != nil {
		return fmt.Errorf("validating target privileges for failure domain %q: %w", fd.Name, err)
	}

	if migration.Spec.Image != nil && fd.Topology.Template == "" {
		if err := validateImageImportPrivileges(ctx, session, cluster, fd.Topology.ResourcePool); err != nil {
			return fmt.Errorf("validating image import privileges for failure domain %q: %w", fd.Name, err)
		}
	}

	return nil
}

// imageImportPrivileges are the vCenter privileges required for OVA import.
var imageImportPrivileges = []string{
	"VApp.Import",
	"VirtualMachine.Config.AddNewDisk",
	"VirtualMachine.Inventory.CreateFromExisting",
}

// validateImageImportPrivileges checks that the authenticated user has the
// privileges required for OVA import on the effective resource pool.
// When resourcePoolPath is non-empty, privileges are checked against that
// specific resource pool; otherwise, the cluster's default resource pool is used.
func validateImageImportPrivileges(ctx context.Context, session *vsphere.Session, cluster *object.ClusterComputeResource, resourcePoolPath string) error {
	if session == nil || session.Client == nil || session.Client.Client == nil {
		return fmt.Errorf("session client must not be nil")
	}

	userSession, err := session.Client.SessionManager.UserSession(ctx)
	if err != nil {
		return fmt.Errorf("getting current vSphere user session: %w", err)
	}
	if userSession == nil {
		return fmt.Errorf("current vSphere user session not found")
	}

	authMgr := object.NewAuthorizationManager(session.Client.Client)

	var rp *object.ResourcePool
	if resourcePoolPath != "" {
		rp, err = session.Finder.ResourcePool(ctx, resourcePoolPath)
		if err != nil {
			return fmt.Errorf("finding resource pool %q: %w", resourcePoolPath, err)
		}
	} else {
		rp, err = cluster.ResourcePool(ctx)
		if err != nil {
			return fmt.Errorf("getting cluster resource pool: %w", err)
		}
	}

	results, err := authMgr.HasUserPrivilegeOnEntities(ctx,
		[]types.ManagedObjectReference{rp.Reference()},
		userSession.UserName, imageImportPrivileges)
	if err != nil {
		return fmt.Errorf("checking image import privileges: %w", err)
	}

	if len(results) == 0 {
		return fmt.Errorf("no privilege check results returned for resource pool")
	}

	missing := missingPrivileges(results[0], imageImportPrivileges)
	if len(missing) > 0 {
		return fmt.Errorf("missing required image import privileges on resource pool: %s", strings.Join(missing, ", "))
	}

	return nil
}

func (r *VmwareCloudFoundationMigrationReconciler) hasTargetVCenterConfiguration(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (bool, error) {
	if len(migration.Spec.FailureDomains) == 0 {
		return false, nil
	}

	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	infra, err := infraMgr.Get(ctx)
	if err != nil {
		return false, fmt.Errorf("getting infrastructure for target vCenter check: %w", err)
	}
	if infra.Spec.PlatformSpec.VSphere == nil {
		return false, nil
	}

	targetServers := make(map[string]bool, len(migration.Spec.FailureDomains))
	for _, vc := range infra.Spec.PlatformSpec.VSphere.VCenters {
		targetServers[vc.Server] = true
	}

	targetFailureDomains := make(map[string]bool, len(infra.Spec.PlatformSpec.VSphere.FailureDomains))
	for i := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
		fd := &infra.Spec.PlatformSpec.VSphere.FailureDomains[i]
		targetFailureDomains[fd.Name+"|"+fd.Server] = true
	}

	for i := range migration.Spec.FailureDomains {
		fd := &migration.Spec.FailureDomains[i]
		if !targetServers[fd.Server] {
			return false, nil
		}
		if !targetFailureDomains[fd.Name+"|"+fd.Server] {
			return false, nil
		}
	}

	return true, nil
}

func checkNoVSphereCSIPersistentVolumes(ctx context.Context, kubeClient kubernetes.Interface) error {
	pvs, err := kubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing persistentvolumes: %w", err)
	}

	var blocked []string
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == vsphereCSIDriverName {
			blocked = append(blocked, pv.Name)
		}
	}

	if len(blocked) == 0 {
		return nil
	}

	sort.Strings(blocked)
	return fmt.Errorf("vSphere CSI-backed persistent volumes are not supported for migration; remove PersistentVolumes using driver %q: %s", vsphereCSIDriverName, strings.Join(blocked, ", "))
}

func checkVSphereStorageManagement(ctx context.Context, dynamicClient dynamic.Interface) (string, error) {
	log := klog.FromContext(ctx)

	csiState, err := getManagementState(ctx, dynamicClient, clusterCSIDriverGVR, vsphereCSIDriverName)
	if err != nil {
		return "", fmt.Errorf("vSphere CSI driver is not supported for migration; unable to read ClusterCSIDriver/%s: %w", vsphereCSIDriverName, err)
	}
	log.V(1).Info("checked vSphere CSI driver management state", "clusterCSIDriver", vsphereCSIDriverName, "managementState", csiState)
	if csiState != "Removed" {
		return "", fmt.Errorf("vSphere CSI driver is not supported for migration; set ClusterCSIDriver/%s spec.managementState to Removed (current: %s)", vsphereCSIDriverName, csiState)
	}

	storageState, err := getManagementState(ctx, dynamicClient, storageOperatorGVR, "cluster")
	if err != nil {
		log.V(1).Info("unable to read Storage/cluster management state", "error", err)
		return "unable to read Storage/cluster managementState; consider setting Unmanaged or Removed (only ClusterCSIDriver Removed is required)", nil
	}
	log.V(1).Info("checked Storage operator management state", "storage", "cluster", "managementState", storageState)
	switch storageState {
	case "Unmanaged", "Removed":
		return "", nil
	default:
		return fmt.Sprintf("Storage/cluster managementState is %s; consider setting Unmanaged or Removed (only ClusterCSIDriver Removed is required)", storageState), nil
	}
}

func getManagementState(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, name string) (string, error) {
	obj, err := dynamicClient.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	state, found, err := unstructured.NestedString(obj.Object, "spec", "managementState")
	if err != nil {
		return "", fmt.Errorf("reading spec.managementState: %w", err)
	}
	if !found || state == "" {
		return "", fmt.Errorf("spec.managementState not set")
	}
	return state, nil
}

func checkInterferingRolloutResources(ctx context.Context, dynamicClient dynamic.Interface) error {
	var blockers []string

	mhcs, err := listDynamicResourceNames(ctx, dynamicClient, machineHealthCheckGVR)
	if err != nil {
		return fmt.Errorf("listing machinehealthchecks: %w", err)
	}
	var userMHCs []string
	for _, name := range mhcs {
		if !platformMachineHealthChecks[name] {
			userMHCs = append(userMHCs, name)
		}
	}
	if len(userMHCs) > 0 {
		blockers = append(blockers, fmt.Sprintf("MachineHealthCheck resources: %s", strings.Join(userMHCs, ", ")))
	}

	clusterAutoscalers, err := listDynamicResourceNames(ctx, dynamicClient, clusterAutoscalerGVR)
	if err != nil {
		return fmt.Errorf("listing clusterautoscalers: %w", err)
	}
	if len(clusterAutoscalers) > 0 {
		blockers = append(blockers, fmt.Sprintf("ClusterAutoscaler resources: %s", strings.Join(clusterAutoscalers, ", ")))
	}

	machineAutoscalers, err := listDynamicResourceNames(ctx, dynamicClient, machineAutoscalerGVR)
	if err != nil {
		return fmt.Errorf("listing machineautoscalers: %w", err)
	}
	if len(machineAutoscalers) > 0 {
		blockers = append(blockers, fmt.Sprintf("MachineAutoscaler resources: %s", strings.Join(machineAutoscalers, ", ")))
	}

	if len(blockers) == 0 {
		return nil
	}

	return fmt.Errorf("remove interfering rollout resources before migration: %s", strings.Join(blockers, "; "))
}

func validateTargetPrivileges(ctx context.Context, session *vsphere.Session, datacenter *object.Datacenter, cluster *object.ClusterComputeResource) error {
	if session == nil || session.Client == nil || session.Client.Client == nil {
		return fmt.Errorf("session client must not be nil")
	}

	userSession, err := session.Client.SessionManager.UserSession(ctx)
	if err != nil {
		return fmt.Errorf("getting current vSphere user session: %w", err)
	}
	if userSession == nil {
		return fmt.Errorf("current vSphere user session not found")
	}

	authMgr := object.NewAuthorizationManager(session.Client.Client)
	folders, err := datacenter.Folders(ctx)
	if err != nil {
		return fmt.Errorf("getting datacenter folders: %w", err)
	}

	checks := []struct {
		entity     types.ManagedObjectReference
		privileges []string
		label      string
	}{
		{
			entity:     session.Client.ServiceContent.RootFolder,
			privileges: rootTagPrivileges,
			label:      "root folder",
		},
		{
			entity:     folders.VmFolder.Reference(),
			privileges: vmFolderPrivileges,
			label:      fmt.Sprintf("VM folder %q", folders.VmFolder.InventoryPath),
		},
		{
			entity:     datacenter.Reference(),
			privileges: objectAttachPrivileges,
			label:      fmt.Sprintf("datacenter %q", datacenter.InventoryPath),
		},
		{
			entity:     cluster.Reference(),
			privileges: objectAttachPrivileges,
			label:      fmt.Sprintf("cluster %q", cluster.InventoryPath),
		},
	}

	for _, check := range checks {
		results, err := authMgr.HasUserPrivilegeOnEntities(ctx, []types.ManagedObjectReference{check.entity}, userSession.UserName, check.privileges)
		if err != nil {
			return fmt.Errorf("checking privileges on %s: %w", check.label, err)
		}
		if len(results) == 0 {
			return fmt.Errorf("no privilege results returned for %s", check.label)
		}
		missing := missingPrivileges(results[0], check.privileges)
		if len(missing) > 0 {
			return fmt.Errorf("user %q is missing %s on %s", userSession.UserName, strings.Join(missing, ", "), check.label)
		}
	}

	return nil
}

func missingPrivileges(entityPrivilege types.EntityPrivilege, requested []string) []string {
	available := make(map[string]bool, len(entityPrivilege.PrivAvailability))
	for _, privilege := range entityPrivilege.PrivAvailability {
		available[privilege.PrivId] = privilege.IsGranted
	}

	var missing []string
	for _, privilege := range requested {
		if !available[privilege] {
			missing = append(missing, privilege)
		}
	}
	sort.Strings(missing)
	return missing
}

func validateUniqueFailureDomainNames(failureDomains []configv1.VSpherePlatformFailureDomainSpec) error {
	seen := make(map[string]bool, len(failureDomains))
	duplicates := make(map[string]bool)
	for i := range failureDomains {
		name := failureDomains[i].Name
		if seen[name] {
			duplicates[name] = true
			continue
		}
		seen[name] = true
	}
	if len(duplicates) == 0 {
		return nil
	}

	names := make([]string, 0, len(duplicates))
	for name := range duplicates {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("duplicate failure domain names are not allowed: %s", strings.Join(names, ", "))
}

func listDynamicResourceNames(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource) ([]string, error) {
	resourceList, err := dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			klog.FromContext(ctx).V(2).Info("skipping dynamic resource lookup, resource type not found", "resource", gvr.String())
			return nil, nil
		}
		return nil, err
	}

	names := make([]string, 0, len(resourceList.Items))
	for i := range resourceList.Items {
		item := &resourceList.Items[i]
		name := item.GetName()
		if namespace := item.GetNamespace(); namespace != "" {
			name = namespace + "/" + name
		}
		names = append(names, name)
	}

	sort.Strings(names)
	return names, nil
}
