/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	machineclient "github.com/openshift/client-go/machine/clientset/versioned"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/metadata"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

// VmwareCloudFoundationMigrationReconciler reconciles a VmwareCloudFoundationMigration object.
// It drives a multi-stage migration workflow from one vCenter to another by
// progressing through a series of conditions in order.
type VmwareCloudFoundationMigrationReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	KubeClient    kubernetes.Interface
	ConfigClient  configclient.Interface
	MachineClient machineclient.Interface
	DynamicClient dynamic.Interface
	Recorder      record.EventRecorder
}

// conditionOrder defines the sequence in which conditions are evaluated.
// The reconciler processes the first condition that is not True and returns.
var conditionOrder = []string{
	migrationv1alpha1.ConditionInfrastructurePrepared,
	migrationv1alpha1.ConditionDestinationInitialized,
	migrationv1alpha1.ConditionMultiSiteConfigured,
	migrationv1alpha1.ConditionWorkloadMigrated,
	migrationv1alpha1.ConditionSourceCleaned,
	migrationv1alpha1.ConditionReady,
}

const reasonWaitingForVSpherePods = "WaitingForVSpherePods"

// +kubebuilder:rbac:groups=migration.openshift.io,resources=vmwarecloudfoundationmigrations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=migration.openshift.io,resources=vmwarecloudfoundationmigrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=migration.openshift.io,resources=vmwarecloudfoundationmigrations/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets;configmaps;pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=config.openshift.io,resources=infrastructures;clusteroperators,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=featuregates,verbs=get;list;watch
// +kubebuilder:rbac:groups=machine.openshift.io,resources=machinesets;machines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=machine.openshift.io,resources=controlplanemachinesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=machine.openshift.io,resources=machinehealthchecks,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling.openshift.io,resources=clusterautoscalers;machineautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=clustercsidrivers;storages,verbs=get
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the migration workflow by checking conditions in order and
// executing the work for the first incomplete condition. It is idempotent and
// safe to restart at any point.
func (r *VmwareCloudFoundationMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := r.Get(ctx, req.NamespacedName, migration); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("migration resource not found, ignoring")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting migration resource: %w", err)
	}

	if migration.Name != migrationv1alpha1.SingletonName {
		cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionAccepted)
		alreadyRecorded := cond != nil &&
			cond.Status == metav1.ConditionFalse &&
			cond.Reason == migrationv1alpha1.ReasonUnsupportedName &&
			cond.ObservedGeneration == migration.Generation
		if !alreadyRecorded {
			log.Info("ignoring VmwareCloudFoundationMigration with unsupported name; only a single resource is reconciled", "expectedName", migrationv1alpha1.SingletonName, "actualName", migration.Name)
			r.Recorder.Eventf(migration, "Warning", migrationv1alpha1.ReasonUnsupportedName, "this operator only reconciles a VmwareCloudFoundationMigration named %q; this resource will be ignored", migrationv1alpha1.SingletonName)
			r.setCondition(migration, migrationv1alpha1.ConditionAccepted, metav1.ConditionFalse, migrationv1alpha1.ReasonUnsupportedName, fmt.Sprintf("only a VmwareCloudFoundationMigration named %q is reconciled by this operator", migrationv1alpha1.SingletonName))
			if err := r.updateStatus(ctx, migration); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if migration.Spec.State != migrationv1alpha1.MigrationStateRunning {
		log.V(1).Info("migration not in Running state, skipping", "state", migration.Spec.State)
		return ctrl.Result{}, nil
	}

	// Set start time on first reconcile in Running state.
	if migration.Status.StartTime == nil {
		now := metav1.Now()
		migration.Status.StartTime = &now
		r.Recorder.Event(migration, "Normal", "MigrationStarted", "Migration workflow started")
		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Walk conditions in order; execute work for the first non-True condition.
	type conditionHandler func(context.Context, *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error)
	handlers := map[string]conditionHandler{
		migrationv1alpha1.ConditionInfrastructurePrepared: r.ensureInfrastructurePrepared,
		migrationv1alpha1.ConditionDestinationInitialized: r.ensureDestinationInitialized,
		migrationv1alpha1.ConditionMultiSiteConfigured:    r.ensureMultiSiteConfigured,
		migrationv1alpha1.ConditionWorkloadMigrated:       r.ensureWorkloadMigrated,
		migrationv1alpha1.ConditionSourceCleaned:          r.ensureSourceCleaned,
		migrationv1alpha1.ConditionReady:                  r.ensureReady,
	}

	for _, condType := range conditionOrder {
		if r.isConditionTrue(migration, condType) {
			continue
		}

		handler, ok := handlers[condType]
		if !ok {
			return ctrl.Result{}, fmt.Errorf("no handler for condition %q", condType)
		}

		log.V(1).Info("processing condition", "condition", condType)
		result, err := handler(ctx, migration)
		if err != nil {
			r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonFailed, err.Error())
			r.Recorder.Eventf(migration, "Warning", "ConditionFailed", "Condition %s failed: %v", condType, err)
		}

		// Always persist status after processing a condition.
		if statusErr := r.updateStatus(ctx, migration); statusErr != nil {
			log.Error(statusErr, "failed to update status")
			return ctrl.Result{}, statusErr
		}

		if err != nil {
			return ctrl.Result{}, err
		}
		return result, nil
	}

	// All conditions True: migration complete.
	log.Info("migration complete")
	return ctrl.Result{}, nil
}

// ensureInfrastructurePrepared validates preflight checks and selects the
// migration path without performing disruptive cluster changes.
func (r *VmwareCloudFoundationMigrationReconciler) ensureInfrastructurePrepared(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {
	condType := migrationv1alpha1.ConditionInfrastructurePrepared

	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Validating preflight checks")
	message, err := r.runPreflightChecks(ctx, migration)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.setCondition(migration, condType, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, message)
	r.Recorder.Event(migration, "Normal", "InfrastructurePrepared", "Preflight validation passed")
	return ctrl.Result{}, nil
}

// ensureDestinationInitialized creates VM folders and region/zone tags on target vCenters.
func (r *VmwareCloudFoundationMigrationReconciler) ensureDestinationInitialized(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionDestinationInitialized

	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Initializing destination vCenter")

	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	infraID, err := infraMgr.GetInfrastructureID(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting infrastructure ID: %w", err)
	}

	// Track which server/datacenter pairs have had folders created to avoid duplicates.
	type serverDC struct {
		server, datacenter string
	}
	folderCreated := make(map[serverDC]bool)
	type tagTarget struct {
		server, datacenter, objectType, objectName, tagValue string
	}
	tagAttached := make(map[tagTarget]bool)

	for i := range migration.Spec.FailureDomains {
		fd := &migration.Spec.FailureDomains[i]
		key := serverDC{server: fd.Server, datacenter: fd.Topology.Datacenter}
		regionAttachmentKey := tagTarget{
			server:     fd.Server,
			datacenter: fd.Topology.Datacenter,
			objectType: "datacenter",
			objectName: fd.Topology.Datacenter,
			tagValue:   fd.Region,
		}
		zoneAttachmentKey := tagTarget{
			server:     fd.Server,
			datacenter: fd.Topology.Datacenter,
			objectType: "cluster",
			objectName: fd.Topology.ComputeCluster,
			tagValue:   fd.Zone,
		}

		username, password, err := getTargetCredentials(ctx, r.KubeClient, migration, fd.Server)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting credentials for %s: %w", fd.Server, err)
		}

		session, err := getVSphereSession(ctx, fd.Server, fd.Topology.Datacenter, username, password)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("connecting to %s/%s: %w", fd.Server, fd.Topology.Datacenter, err)
		}

		// Create VM folder per unique server/datacenter, then ensure the
		// installer-style cluster ownership tag is attached to that folder.
		if !folderCreated[key] {
			r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
				fmt.Sprintf("Creating VM folder %q on %s/%s", infraID, fd.Server, fd.Topology.Datacenter))

			folder, err := vsphere.CreateVMFolder(ctx, session, infraID)
			if err != nil {
				// Folder may already exist; try to get it.
				folder, getErr := vsphere.GetVMFolder(ctx, session, infraID)
				if getErr != nil {
					return ctrl.Result{}, fmt.Errorf("creating VM folder %q on %s/%s: %w", infraID, fd.Server, fd.Topology.Datacenter, err)
				}
				log.V(1).Info("VM folder already exists", "path", folder.InventoryPath)
			} else {
				log.V(1).Info("created VM folder", "path", folder.InventoryPath)
			}

			// Verify folder is accessible.
			folder, err = vsphere.GetVMFolder(ctx, session, infraID)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("verifying VM folder %q on %s/%s: %w", infraID, fd.Server, fd.Topology.Datacenter, err)
			}

			r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
				fmt.Sprintf("Creating cluster ownership tag for %q on %s", infraID, fd.Server))
			ownershipTagID, err := vsphere.EnsureClusterOwnershipTag(ctx, session, infraID)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("ensuring cluster ownership tag for %q on %s: %w", infraID, fd.Server, err)
			}
			if err := vsphere.AttachClusterOwnershipTag(ctx, session, ownershipTagID, folder); err != nil {
				return ctrl.Result{}, fmt.Errorf("attaching cluster ownership tag to folder %q on %s/%s: %w", infraID, fd.Server, fd.Topology.Datacenter, err)
			}

			folderCreated[key] = true
		}

		// Find datacenter and cluster objects for tag checks and attachment.
		dc, err := session.Finder.Datacenter(ctx, fd.Topology.Datacenter)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("finding datacenter %q: %w", fd.Topology.Datacenter, err)
		}

		cluster, err := session.Finder.ClusterComputeResource(ctx, fd.Topology.ComputeCluster)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("finding cluster %q: %w", fd.Topology.ComputeCluster, err)
		}

		// Determine which tags need to be created and attached.
		// Skip tags that were already handled in this reconciliation (dedup map)
		// or that already exist on the target objects.
		needRegion := !tagAttached[regionAttachmentKey]
		needZone := !tagAttached[zoneAttachmentKey]

		if needRegion {
			hasRegion, err := vsphere.ObjectHasTagInCategory(ctx, session, vsphere.TagCategoryRegion, dc)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("checking region tags on datacenter %q: %w", fd.Topology.Datacenter, err)
			}
			if hasRegion {
				log.V(1).Info("datacenter already has region tag, skipping", "datacenter", fd.Topology.Datacenter)
				tagAttached[regionAttachmentKey] = true
				needRegion = false
			}
		}
		if needZone {
			hasZone, err := vsphere.ObjectHasTagInCategory(ctx, session, vsphere.TagCategoryZone, cluster)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("checking zone tags on cluster %q: %w", fd.Topology.ComputeCluster, err)
			}
			if hasZone {
				log.V(1).Info("cluster already has zone tag, skipping", "cluster", fd.Topology.ComputeCluster)
				tagAttached[zoneAttachmentKey] = true
				needZone = false
			}
		}

		if !needRegion && !needZone {
			log.V(1).Info("failure domain initialized", "name", fd.Name)
			continue
		}

		// Create and attach only tags that are missing.
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			fmt.Sprintf("Creating tags for failure domain %q", fd.Name))

		var regionTagID, zoneTagID string
		if needRegion {
			regionCatID, err := vsphere.EnsureTagCategory(ctx, session, vsphere.TagCategoryRegion, vsphere.TagCategoryRegionDescription, "SINGLE")
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create region tag category for failure domain %q: %w", fd.Name, err)
			}
			regionTagID, err = vsphere.EnsureTag(ctx, session, regionCatID, fd.Region, fmt.Sprintf("OpenShift region %s", fd.Region))
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create region tag for failure domain %q: %w", fd.Name, err)
			}
		}
		if needZone {
			zoneCatID, err := vsphere.EnsureTagCategory(ctx, session, vsphere.TagCategoryZone, vsphere.TagCategoryZoneDescription, "SINGLE")
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create zone tag category for failure domain %q: %w", fd.Name, err)
			}
			zoneTagID, err = vsphere.EnsureTag(ctx, session, zoneCatID, fd.Zone, fmt.Sprintf("OpenShift zone %s", fd.Zone))
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to create zone tag for failure domain %q: %w", fd.Name, err)
			}
		}

		if err := vsphere.AttachFailureDomainTags(ctx, session, regionTagID, zoneTagID, dc, cluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("attaching tags for failure domain %q: %w", fd.Name, err)
		}
		if needRegion {
			tagAttached[regionAttachmentKey] = true
		}
		if needZone {
			tagAttached[zoneAttachmentKey] = true
		}

		log.V(1).Info("failure domain initialized", "name", fd.Name)
	}

	r.setCondition(migration, condType, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "Destination vCenter initialized with folders and tags")
	r.Recorder.Event(migration, "Normal", "DestinationInitialized", "VM folders and tags created on target vCenter")
	return ctrl.Result{}, nil
}

// ensureMultiSiteConfigured adds target vCenter to cluster configuration
// (secrets, Infrastructure CRD, cloud-provider-config) and restarts affected pods.
// Once config is applied and pods are restarted, it only waits for vSphere pods to
// become ready on subsequent reconciles (avoids re-restarting pods every requeue).
func (r *VmwareCloudFoundationMigrationReconciler) ensureMultiSiteConfigured(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionMultiSiteConfigured

	configApplied, err := r.hasTargetVCenterConfiguration(ctx, migration)
	if err != nil {
		return ctrl.Result{}, err
	}
	waitingForPods := false
	if cond := apimeta.FindStatusCondition(migration.Status.Conditions, condType); cond != nil {
		waitingForPods = configApplied && cond.Reason == reasonWaitingForVSpherePods
	}

	if !waitingForPods {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Configuring multi-site vCenter")

		sm := openshift.NewSecretManager(r.KubeClient)

		credsSecret, err := sm.GetVSphereCredsSecret(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting vsphere-creds secret: %w", err)
		}

		processedServers := make(map[string]bool)
		for i := range migration.Spec.FailureDomains {
			fd := &migration.Spec.FailureDomains[i]
			if processedServers[fd.Server] {
				continue
			}

			username, password, err := getTargetCredentials(ctx, r.KubeClient, migration, fd.Server)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("getting credentials for %s: %w", fd.Server, err)
			}

			credsSecret, err = sm.AddTargetVCenterCreds(ctx, credsSecret, fd.Server, username, password)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("adding target credentials for %s: %w", fd.Server, err)
			}
			processedServers[fd.Server] = true
		}
		log.V(1).Info("target vCenter credentials added to vsphere-creds")

		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Updating Infrastructure")
		infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
		infra, err := infraMgr.Get(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting infrastructure: %w", err)
		}

		if _, err := infraMgr.AddTargetVCenter(ctx, infra, migration.Spec.FailureDomains); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding target vCenter to infrastructure: %w", err)
		}
		log.V(1).Info("Infrastructure updated with target vCenter")

		// Update cloud-provider-config.
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Updating cloud-provider-config")
		cmMgr := openshift.NewConfigMapManager(r.KubeClient)
		cm, err := cmMgr.GetCloudProviderConfig(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting cloud-provider-config: %w", err)
		}

		if _, err := cmMgr.AddTargetVCenterToConfig(ctx, cm, migration.Spec.FailureDomains); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding target vCenter to cloud-provider-config: %w", err)
		}
		log.V(1).Info("cloud-provider-config updated")

		// Restart MCO pods.
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Restarting MCO pods")
		if err := syncControllerConfig(ctx, r.KubeClient); err != nil {
			return ctrl.Result{}, fmt.Errorf("syncing controller config (MCO restart): %w", err)
		}

		// Restart vSphere pods.
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Restarting vSphere pods")
		podMgr := openshift.NewPodManager(r.KubeClient)
		if err := podMgr.RestartVSpherePods(ctx); err != nil {
			return ctrl.Result{}, fmt.Errorf("restarting vSphere pods: %w", err)
		}

		// Set message so next reconcile only runs readiness check.
		r.setCondition(migration, condType, metav1.ConditionFalse, reasonWaitingForVSpherePods, "Waiting for vSphere pods to become ready")
		// Fall through to readiness check below (no return).
	}

	podMgr := openshift.NewPodManager(r.KubeClient)
	r.setCondition(migration, condType, metav1.ConditionFalse, reasonWaitingForVSpherePods, "Waiting for vSphere pods to become ready")
	podStatus, err := podMgr.CheckVSpherePodsReady(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking vSphere pods readiness: %w", err)
	}
	if !podStatus.AllReady {
		log.V(1).Info("vSphere pods not ready, requeueing", "reason", podStatus.NotReadyReason)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	r.setCondition(migration, condType, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "Multi-site vCenter configured and pods ready")
	r.Recorder.Event(migration, "Normal", "MultiSiteConfigured", "Cluster configured for both source and target vCenters")
	return ctrl.Result{}, nil
}

// ensureWorkloadMigrated creates new worker MachineSets on target, rolls out
// control plane changes, and drains old MachineSets. Progress is derived from
// cluster state (what exists and is ready) so the controller is idempotent
// and safe to restart at any step.
func (r *VmwareCloudFoundationMigrationReconciler) ensureWorkloadMigrated(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionWorkloadMigrated

	if len(migration.Spec.FailureDomains) == 0 {
		return ctrl.Result{}, fmt.Errorf("spec.failureDomains must not be empty")
	}
	for i := range migration.Spec.FailureDomains {
		if migration.Spec.FailureDomains[i].Topology.Template == "" {
			return ctrl.Result{}, fmt.Errorf("spec.failureDomains[%d].topology.template is required (failure domain %q)", i, migration.Spec.FailureDomains[i].Name)
		}
	}

	// If we are past Step 3 (CPMS updated), run Steps 4–6 (rollout and scale-down) from cluster state.
	if c := apimeta.FindStatusCondition(migration.Status.Conditions, condType); c != nil {
		pastCPMSUpdate := strings.HasPrefix(c.Message, "CPMS updated") || strings.Contains(c.Message, "Control plane rollout") || strings.Contains(c.Message, "Old workers")
		if pastCPMSUpdate {
			return r.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration)
		}
	}

	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	infraID, err := infraMgr.GetInfrastructureID(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting infrastructure ID: %w", err)
	}

	sourceVC, err := infraMgr.GetSourceVCenter(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting source vCenter: %w", err)
	}

	machineMgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient)

	// Step 1: Ensure target worker MachineSets exist (idempotent: create only missing ones).
	allTargetMSExist := true
	for i := range migration.Spec.FailureDomains {
		msName := workerMachineSetName(infraID, migration.Spec.FailureDomains[i].Name)
		if _, err := machineMgr.GetMachineSet(ctx, msName); err != nil {
			allTargetMSExist = false
			break
		}
	}
	if !allTargetMSExist {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Creating new worker MachineSets")
		existingMachineSets, err := machineMgr.GetMachineSetsByVCenter(ctx, sourceVC.Server)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting existing MachineSets: %w", err)
		}
		if len(existingMachineSets) == 0 {
			return ctrl.Result{}, fmt.Errorf("no existing MachineSets found for source vCenter %s", sourceVC.Server)
		}
		var totalReplicas int32
		for _, ms := range existingMachineSets {
			if ms.Spec.Replicas != nil {
				totalReplicas += *ms.Spec.Replicas
			}
		}
		replicasPerFD := totalReplicas / int32(len(migration.Spec.FailureDomains))
		if replicasPerFD < 1 {
			replicasPerFD = 1
		}
		template := existingMachineSets[0]
		createdAny := false
		for i := range migration.Spec.FailureDomains {
			fd := &migration.Spec.FailureDomains[i]
			msName := workerMachineSetName(infraID, fd.Name)
			if _, err := machineMgr.GetMachineSet(ctx, msName); err == nil {
				log.V(1).Info("worker MachineSet already exists, skipping", "name", msName)
				continue
			}
			replicas := replicasPerFD
			if i == len(migration.Spec.FailureDomains)-1 {
				replicas = totalReplicas - replicasPerFD*int32(len(migration.Spec.FailureDomains)-1)
				if replicas < 1 {
					replicas = 1
				}
			}
			if _, err := machineMgr.CreateWorkerMachineSet(ctx, msName, replicas, fd, template, infraID); err != nil {
				return ctrl.Result{}, fmt.Errorf("creating worker MachineSet %q: %w", msName, err)
			}
			createdAny = true
			log.V(1).Info("created worker MachineSet", "name", msName, "replicas", replicas)
		}
		if createdAny {
			r.Recorder.Event(migration, "Normal", "WorkersCreated", "New worker MachineSets created on target vCenter")
		}
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Workers created, waiting for machines ready")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 2: Wait for target worker machines and nodes to be ready (cluster state).
	allReady, err := checkWorkerReadiness(ctx, machineMgr, migration.Spec.FailureDomains, infraID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !allReady {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Workers created, waiting for machines ready")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 3: Update CPMS with target failure domains and set state to Active.
	// The CPMS is updated in place — no delete/recreate needed. The CPMS operator
	// resolves failure domain topology from the Infrastructure resource and triggers
	// a rolling replacement of control plane machines.
	targetFDNames := failureDomainNames(migration.Spec.FailureDomains)
	if err := machineMgr.UpdateCPMSFailureDomain(ctx, targetFDNames); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating CPMS failure domains: %w", err)
	}
	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "CPMS updated, waiting for generation observed")
	r.Recorder.Event(migration, "Normal", "CPMSUpdated", fmt.Sprintf("CPMS updated with failure domains %v", targetFDNames))
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// ensureWorkloadMigratedRolloutAndScaleDown runs Steps 5–8: wait for control plane
// rollout, scale old MachineSets to 0, wait for old machines/nodes to be deleted,
// then delete the empty source MachineSets.
// Progress is derived from cluster state so it is idempotent. Call when condition
// message indicates we are past "CPMS updated" (e.g. "Control plane rollout" or
// "Old workers" or we have observed generation and rollout complete).
func (r *VmwareCloudFoundationMigrationReconciler) ensureWorkloadMigratedRolloutAndScaleDown(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionWorkloadMigrated
	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	sourceVC, err := infraMgr.GetSourceVCenter(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting source vCenter: %w", err)
	}
	machineMgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient)

	// Step 5: Wait for CPMS generation observed and rollout complete.
	observed, err := machineMgr.IsCPMSGenerationObserved(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking CPMS generation: %w", err)
	}
	if !observed {
		log.V(1).Info("CPMS generation not yet observed")
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "CPMS updated, waiting for generation observed")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	complete, replicas, updated, ready, err := machineMgr.CheckControlPlaneRolloutStatus(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking control plane rollout: %w", err)
	}
	if !complete {
		log.V(1).Info("control plane rollout in progress", "replicas", replicas, "updated", updated, "ready", ready)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			fmt.Sprintf("CPMS updated, control plane rolling out (%d/%d ready)", ready, replicas))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 6: Scale old MachineSets to 0 (idempotent).
	oldMachineSets, err := machineMgr.GetMachineSetsByVCenter(ctx, sourceVC.Server)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting old MachineSets: %w", err)
	}
	scaledAny := false
	for _, ms := range oldMachineSets {
		if ms.Spec.Replicas != nil && *ms.Spec.Replicas > 0 {
			if err := machineMgr.ScaleMachineSet(ctx, ms.Name, 0); err != nil {
				return ctrl.Result{}, fmt.Errorf("scaling old MachineSet %q to 0: %w", ms.Name, err)
			}
			scaledAny = true
			log.V(1).Info("scaled old MachineSet to 0", "name", ms.Name)
		}
	}
	if scaledAny {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Old workers scaled down, waiting for deletion")
		r.Recorder.Event(migration, "Normal", "ControlPlaneRolledOut", "Control plane rollout complete on target vCenter")
		r.Recorder.Event(migration, "Normal", "OldWorkersScaledDown", "Old worker MachineSets scaled to 0")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 7: Wait for old machines and nodes to be deleted.
	allDeleted := true
	for _, ms := range oldMachineSets {
		machinesGone, remaining, err := machineMgr.CheckMachinesDeleted(ctx, ms.Name)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking machine deletion for %q: %w", ms.Name, err)
		}
		if !machinesGone {
			log.V(1).Info("old machines still present", "machineSet", ms.Name, "remaining", remaining)
			allDeleted = false
			continue
		}
		nodesGone, nodeRemaining, err := machineMgr.CheckNodesDeletedForMachines(ctx, ms.Name)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking node deletion for %q: %w", ms.Name, err)
		}
		if !nodesGone {
			log.V(1).Info("old nodes still present", "machineSet", ms.Name, "remaining", nodeRemaining)
			allDeleted = false
		}
	}
	if !allDeleted {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Old workers scaled down, waiting for deletion")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 8: Delete empty source MachineSets, then confirm none remain.
	deleted, err := machineMgr.DeleteMachineSetsByVCenter(ctx, sourceVC.Server)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("deleting source MachineSets: %w", err)
	}
	if len(deleted) > 0 {
		log.V(1).Info("deleted source MachineSets", "names", deleted)
		r.Recorder.Event(migration, "Normal", "SourceWorkersDeleted", "Source worker MachineSets deleted")
	}

	remaining, err := machineMgr.GetMachineSetsByVCenter(ctx, sourceVC.Server)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing source MachineSets after delete: %w", err)
	}
	if len(remaining) > 0 {
		names := make([]string, 0, len(remaining))
		for _, ms := range remaining {
			names = append(names, ms.Name)
		}
		log.V(1).Info("source MachineSets still present after delete", "names", names)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Deleting source MachineSets")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	r.setCondition(migration, condType, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "Workload migrated to target vCenter")
	r.Recorder.Event(migration, "Normal", "WorkloadMigrated", "All workloads migrated to target vCenter")
	return ctrl.Result{}, nil
}

// ensureSourceCleaned removes the source vCenter from all cluster configuration
// and generates metadata.
func (r *VmwareCloudFoundationMigrationReconciler) ensureSourceCleaned(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionSourceCleaned

	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Cleaning up source vCenter")

	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	infra, err := infraMgr.Get(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting infrastructure: %w", err)
	}

	var sourceServer string
	if infra.Spec.PlatformSpec.VSphere != nil && len(infra.Spec.PlatformSpec.VSphere.VCenters) > 0 {
		sourceServer = infra.Spec.PlatformSpec.VSphere.VCenters[0].Server
	} else {
		log.V(1).Info("source vCenter not found in infrastructure, already removed")
	}

	// Complete all vCenter list changes (Infrastructure, config, secrets, pods).
	if sourceServer != "" {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Removing source vCenter from Infrastructure")
		infra, err := infraMgr.Get(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting infrastructure: %w", err)
		}
		if _, err := infraMgr.RemoveSourceVCenter(ctx, infra, sourceServer); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing source vCenter from infrastructure: %w", err)
		}
		log.V(1).Info("removed source vCenter from infrastructure", "server", sourceServer)

		// Remove source from cloud-provider-config.
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Removing source vCenter from cloud-provider-config")
		cmMgr := openshift.NewConfigMapManager(r.KubeClient)
		cm, err := cmMgr.GetCloudProviderConfig(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting cloud-provider-config: %w", err)
		}

		if _, err := cmMgr.RemoveSourceVCenterFromConfig(ctx, cm, sourceServer); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing source vCenter from cloud-provider-config: %w", err)
		}
		log.V(1).Info("removed source vCenter from cloud-provider-config", "server", sourceServer)

		// Remove source creds from vsphere-creds.
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Removing source credentials from vsphere-creds")
		sm := openshift.NewSecretManager(r.KubeClient)
		credsSecret, err := sm.GetVSphereCredsSecret(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting vsphere-creds secret: %w", err)
		}

		if _, err := sm.RemoveSourceVCenterCreds(ctx, credsSecret, sourceServer); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing source credentials: %w", err)
		}
		log.V(1).Info("removed source credentials from vsphere-creds", "server", sourceServer)

		// Restart vSphere pods to pick up new config.
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Restarting vSphere pods after source removal")
		podMgr := openshift.NewPodManager(r.KubeClient)
		if err := podMgr.RestartVSpherePods(ctx); err != nil {
			return ctrl.Result{}, fmt.Errorf("restarting vSphere pods: %w", err)
		}
	}

	// Generate metadata (after vCenter list is updated).
	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Generating migration metadata")
	infra, err = infraMgr.Get(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting infrastructure for metadata: %w", err)
	}

	// Build credentials map for metadata generation.
	sm := openshift.NewSecretManager(r.KubeClient)
	creds := make(map[string]string)
	for i := range migration.Spec.FailureDomains {
		fd := &migration.Spec.FailureDomains[i]
		if _, exists := creds[fd.Server]; exists {
			continue
		}
		username, password, err := getTargetCredentials(ctx, r.KubeClient, migration, fd.Server)
		if err != nil {
			// Fall back to vsphere-creds.
			username, password, err = sm.GetCredentials(ctx, fd.Server)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("getting credentials for metadata for %s: %w", fd.Server, err)
			}
		}
		creds[fd.Server] = fmt.Sprintf("%s:%s", username, password)
	}

	metadataMgr := metadata.NewMetadataManager(r.KubeClient)
	md, err := metadataMgr.GenerateMetadata(ctx, migration.Spec.FailureDomains, infra, creds)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generating metadata: %w", err)
	}

	secretName := metadata.GetMetadataSecretName(migration.Name)
	if err := metadataMgr.SaveToSecret(ctx, md, migration.Namespace, secretName); err != nil {
		return ctrl.Result{}, fmt.Errorf("saving metadata: %w", err)
	}
	log.V(1).Info("metadata saved", "secret", secretName)

	r.setCondition(migration, condType, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "Source vCenter cleaned")
	r.Recorder.Event(migration, "Normal", "SourceCleaned", "Source vCenter removed from cluster configuration")
	return ctrl.Result{}, nil
}

// ensureReady verifies all operators are healthy and only target vCenters remain
// in the Infrastructure resource.
func (r *VmwareCloudFoundationMigrationReconciler) ensureReady(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionReady

	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Verifying final cluster state")

	// Check all operators stable.
	opMgr := openshift.NewOperatorManager(r.ConfigClient)
	stable, summary, err := opMgr.CheckAllOperatorsStable(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking operator stability: %w", err)
	}
	if !stable {
		blockers := make([]string, 0, 3)
		if len(summary.UnavailableOperators) > 0 {
			blockers = append(blockers, fmt.Sprintf("unavailable=%s", strings.Join(summary.UnavailableOperators, ", ")))
		}
		if len(summary.ProgressingOperators) > 0 {
			blockers = append(blockers, fmt.Sprintf("progressing=%s", strings.Join(summary.ProgressingOperators, ", ")))
		}
		if len(summary.DegradedOperators) > 0 {
			blockers = append(blockers, fmt.Sprintf("degraded=%s", strings.Join(summary.DegradedOperators, ", ")))
		}
		msg := fmt.Sprintf("Operators not stable: %s", strings.Join(blockers, "; "))
		log.V(1).Info("operators not yet stable",
			"unavailable", summary.UnavailableOperators,
			"progressing", summary.ProgressingOperators,
			"degraded", summary.DegradedOperators,
		)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, msg)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Verify only target vCenters remain in Infrastructure.
	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	infra, err := infraMgr.Get(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting infrastructure for readiness check: %w", err)
	}

	if infra.Spec.PlatformSpec.VSphere != nil {
		targetServers := make(map[string]bool)
		for i := range migration.Spec.FailureDomains {
			targetServers[migration.Spec.FailureDomains[i].Server] = true
		}

		for _, vc := range infra.Spec.PlatformSpec.VSphere.VCenters {
			if !targetServers[vc.Server] {
				msg := fmt.Sprintf("Non-target vCenter %q still present in Infrastructure", vc.Server)
				log.V(1).Info("unexpected vCenter in infrastructure", "server", vc.Server)
				r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, msg)
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
		}
	}

	// Set completion time.
	now := metav1.Now()
	migration.Status.CompletionTime = &now

	r.setCondition(migration, condType, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "Migration complete, all operators healthy")
	r.Recorder.Event(migration, "Normal", "MigrationComplete", "Migration completed successfully")
	return ctrl.Result{}, nil
}

// setCondition is a convenience wrapper around apimeta.SetStatusCondition.
func (r *VmwareCloudFoundationMigrationReconciler) setCondition(migration *migrationv1alpha1.VmwareCloudFoundationMigration, conditionType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: migration.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// isConditionTrue checks whether the named condition has status True.
func (r *VmwareCloudFoundationMigrationReconciler) isConditionTrue(migration *migrationv1alpha1.VmwareCloudFoundationMigration, conditionType string) bool {
	cond := apimeta.FindStatusCondition(migration.Status.Conditions, conditionType)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// updateStatus persists the migration status subresource. It retries on conflict by
// re-fetching the latest resource and re-applying the desired status.
func (r *VmwareCloudFoundationMigrationReconciler) updateStatus(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) error {
	desiredStatus := migration.Status.DeepCopy()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(migration), latest); err != nil {
			return err
		}
		latest.Status = *desiredStatus
		return r.Status().Update(ctx, latest)
	})
	if err != nil {
		return fmt.Errorf("updating migration status: %w", err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VmwareCloudFoundationMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Named("vmwarecloudfoundationmigration").
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Second, 5*time.Minute),
		}).
		Complete(r)
}

// failureDomainNames extracts the name from each failure domain spec.
func failureDomainNames(fds []configv1.VSpherePlatformFailureDomainSpec) []string {
	names := make([]string, len(fds))
	for i := range fds {
		names[i] = fds[i].Name
	}
	return names
}

// sanitizeRFC1123 converts s to a valid RFC 1123 subdomain label by lower-casing,
// replacing non-alphanumeric characters with hyphens, collapsing runs of hyphens,
// and trimming leading/trailing hyphens.
func sanitizeRFC1123(name string) string {
	name = strings.ToLower(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, name)
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	return strings.Trim(name, "-")
}

func workerMachineSetName(infraID, fdName string) string {
	sanitized := sanitizeRFC1123(fdName)
	if sanitized == "" {
		sanitized = "default"
	}
	return fmt.Sprintf("%s-worker-%s", infraID, sanitized)
}

// checkWorkerReadiness verifies that all machines and nodes for the target worker
// MachineSets are in a ready state. It returns true when every MachineSet's machines
// are Running with a NodeRef and the corresponding nodes have condition Ready=True.
func checkWorkerReadiness(ctx context.Context, machineMgr *openshift.MachineManager, fds []configv1.VSpherePlatformFailureDomainSpec, infraID string) (bool, error) {
	log := klog.FromContext(ctx)
	for i := range fds {
		msName := workerMachineSetName(infraID, fds[i].Name)
		machinesReady, readyCount, totalCount, err := machineMgr.CheckMachinesReady(ctx, msName)
		if err != nil {
			return false, fmt.Errorf("checking machines for %q: %w", msName, err)
		}
		if !machinesReady {
			log.V(1).Info("machines not ready", "machineSet", msName, "ready", readyCount, "total", totalCount)
			return false, nil
		}
		nodesReady, nodeReadyCount, nodeTotalCount, err := machineMgr.CheckNodesReady(ctx, msName)
		if err != nil {
			return false, fmt.Errorf("checking nodes for %q: %w", msName, err)
		}
		if !nodesReady {
			log.V(1).Info("nodes not ready", "machineSet", msName, "ready", nodeReadyCount, "total", nodeTotalCount)
			return false, nil
		}
	}
	return true, nil
}
