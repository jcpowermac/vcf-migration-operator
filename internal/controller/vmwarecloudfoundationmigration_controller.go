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
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	"github.com/vmware/govmomi/find"
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

	configv1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	machineclient "github.com/openshift/client-go/machine/clientset/versioned"
	machineconfigclient "github.com/openshift/client-go/machineconfiguration/clientset/versioned"

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
	// MachineConfigClient accesses MachineConfigPool resources.
	MachineConfigClient machineconfigclient.Interface
	DynamicClient       dynamic.Interface
	Recorder            record.EventRecorder

	// lastStallEventKey identifies the set of old worker machines described by the
	// most recent OldWorkersStalled Warning event; lastStallEventTime is when that
	// event was recorded. Together they debounce the event to at most one per
	// five minutes per distinct machine set. In-memory by design: a leader
	// restart may re-emit one event, which is harmless.
	lastStallEventKey  string
	lastStallEventTime time.Time

	// readyStabilityCount tracks consecutive counted stable ensureReady
	// observations in which all cluster operators were stable and all
	// MachineConfigPools converged. It must reach readyStabilityThreshold
	// before Ready=True is committed; any failing check or long gap resets
	// it to zero. In-memory by design: a leader restart conservatively
	// restarts the stability window.
	readyStabilityCount int
	// lastReadyStabilityCheck is the wall-clock time of the most recent
	// ensureReady check. If the next check runs more than
	// readyStabilityCheckGap later, reconciles in between never reached the
	// readiness gate, so readyStabilityCount is reset.
	lastReadyStabilityCheck time.Time
	// lastCountedStabilityCheck is the wall-clock time of the most recent
	// stable observation counted toward readyStabilityCount. Stable
	// observations closer together than readyStabilityObservationInterval
	// are not counted; event-driven reconciles can run far faster than the
	// 30s requeue cadence, and counting each one would reach the threshold
	// within seconds.
	lastCountedStabilityCheck time.Time
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

const stallEventInterval = 5 * time.Minute

// readyStabilityThreshold is the number of consecutive fully stable
// observations required before Ready=True is committed. A single stable
// snapshot can fall in the gap between a finished machine-config revision
// and the start of the next one, which the migration's own final mutations
// can trigger minutes later. Requiring ~3 minutes of sustained stability
// across 30s requeues makes that gap impossible to pass through.
const readyStabilityThreshold = 6

// readyStabilityCheckGap is the maximum elapsed time between two consecutive
// ensureReady checks that the stability window survives. A longer gap means an
// observation is too stale to count toward sustained stability, so the counter
// restarts from zero. It is 3x the 30s requeue cadence.
const readyStabilityCheckGap = 90 * time.Second

// readyStabilityObservationInterval is the minimum elapsed time between two
// stable observations that are both counted toward sustained stability. It
// matches the 30s requeue cadence of the readiness gate.
const readyStabilityObservationInterval = 30 * time.Second

const (
	maxConditionMessageBytes  = 32768
	maxEventNoteBytes         = 1024
	oldWorkerStallEventPrefix = "Old worker deletion stalled: "
)

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
// +kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling.openshift.io,resources=clusterautoscalers;machineautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=clustercsidrivers;storages,verbs=get
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// serverDC identifies a vCenter server and datacenter pair, the unit at which
// VM folders are created on the target.
type serverDC struct {
	server, datacenter string
}

// tagTarget identifies a specific tag attachment (object plus tag value) so
// per-reconcile deduplication can skip already-handled tags.
type tagTarget struct {
	server, datacenter, objectType, objectName, tagValue string
}

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

	// Snapshot the status this reconcile starts from; updateStatus only
	// persists the fields changed relative to this snapshot.
	baseStatus := *migration.Status.DeepCopy()

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
			if err := r.updateStatus(ctx, migration, baseStatus); err != nil {
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
		if err := r.updateStatus(ctx, migration, baseStatus); err != nil {
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
			r.setCondition(migration, condType, metav1.ConditionFalse, reasonForError(err), err.Error())
			r.Recorder.Eventf(migration, "Warning", "ConditionFailed", "Condition %s failed: %v", condType, err)
		}

		// Always persist status after processing a condition.
		if statusErr := r.updateStatus(ctx, migration, baseStatus); statusErr != nil {
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
	folderCreated := make(map[serverDC]bool)
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
		// Skip re-creating and re-validating both when a prior reconcile already
		// attached the tag: EnsureClusterOwnershipTag re-validates the category's
		// associable types on every call, and a second, unnecessary call can fail
		// even though the folder is already fully configured.
		if !folderCreated[key] {
			if err := r.ensureFolderAndOwnership(ctx, migration, session, fd, infraID, key, folderCreated); err != nil {
				return ctrl.Result{}, err
			}
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

// ensureFolderAndOwnership creates the per-target VM folder and ensures the
// installer-style cluster ownership tag is attached to it. When the folder
// already carries the ownership tag, the target is marked done and the method
// returns without re-validating the category.
//
// Correctness under concurrent reconciles (e.g. the brief window during a
// rollout where an outgoing leader finishes an in-flight reconcile while the
// incoming leader starts one) rests entirely on idempotency against vCenter
// state, not on in-process locking: those reconciles run in different
// processes, so a mutex could not serialize them. Every vSphere operation
// below tolerates concurrent/repeat execution — GetVMFolder/CreateVMFolder
// fall back to lookup, and EnsureClusterOwnershipTag/AttachClusterOwnershipTag
// treat already-exists and already-attached as success.
func (r *VmwareCloudFoundationMigrationReconciler) ensureFolderAndOwnership(
	ctx context.Context,
	migration *migrationv1alpha1.VmwareCloudFoundationMigration,
	session *vsphere.Session,
	fd *configv1.VSpherePlatformFailureDomainSpec,
	infraID string,
	key serverDC,
	folderCreated map[serverDC]bool,
) error {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionDestinationInitialized

	existing, getErr := vsphere.GetVMFolder(ctx, session, infraID)
	if getErr != nil {
		var notFound *find.NotFoundError
		if !errors.As(getErr, &notFound) {
			return fmt.Errorf("checking for existing VM folder %q on %s/%s: %w", infraID, fd.Server, fd.Topology.Datacenter, getErr)
		}
	} else {
		hasOwnership, err := vsphere.ObjectHasTagInCategory(ctx, session, vsphere.ClusterOwnershipCategoryName(infraID), existing)
		if err != nil {
			return fmt.Errorf("checking ownership tag on folder %q on %s/%s: %w", infraID, fd.Server, fd.Topology.Datacenter, err)
		}
		if hasOwnership {
			log.V(1).Info("VM folder and ownership tag already configured", "failureDomain", fd.Name)
			folderCreated[key] = true
			return nil
		}
	}

	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
		fmt.Sprintf("Creating VM folder %q on %s/%s", infraID, fd.Server, fd.Topology.Datacenter))

	folder, err := vsphere.CreateVMFolder(ctx, session, infraID)
	if err != nil {
		// Folder may already exist; try to get it.
		folder, getErr = vsphere.GetVMFolder(ctx, session, infraID)
		if getErr != nil {
			return fmt.Errorf("creating VM folder %q on %s/%s: %w", infraID, fd.Server, fd.Topology.Datacenter, err)
		}
		log.V(1).Info("VM folder already exists", "path", folder.InventoryPath)
	} else {
		log.V(1).Info("created VM folder", "path", folder.InventoryPath)
	}

	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
		fmt.Sprintf("Creating cluster ownership tag for %q on %s", infraID, fd.Server))
	ownershipTagID, err := vsphere.EnsureClusterOwnershipTag(ctx, session, infraID)
	if err != nil {
		return fmt.Errorf("ensuring cluster ownership tag for %q on %s: %w", infraID, fd.Server, err)
	}
	if err := vsphere.AttachClusterOwnershipTag(ctx, session, ownershipTagID, folder); err != nil {
		return fmt.Errorf("attaching cluster ownership tag to folder %q on %s/%s: %w", infraID, fd.Server, fd.Topology.Datacenter, err)
	}

	folderCreated[key] = true
	return nil
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
	targetFDNames := failureDomainNames(migration.Spec.FailureDomains)

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
	readiness, err := checkWorkerReadiness(ctx, machineMgr, migration.Spec.FailureDomains, infraID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !readiness.Ready {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			fmt.Sprintf("Waiting for target workers (machines %d/%d ready, nodes %d/%d ready)", readiness.MachinesReady, readiness.MachinesTotal, readiness.NodesReady, readiness.NodesTotal))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Once target workers are ready and the CPMS already targets the target failure
	// domains, the CPMS update step is done: continue from the rollout and scale-down
	// path, derived entirely from cluster state.
	cpmsUpdated, err := machineMgr.IsCPMSUpdatedForFailureDomains(ctx, targetFDNames)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking CPMS update state: %w", err)
	}
	if cpmsUpdated {
		return r.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration)
	}

	// Step 3: Update CPMS with target failure domains and set state to Active.
	// The CPMS is updated in place — no delete/recreate needed. The CPMS operator
	// resolves failure domain topology from the Infrastructure resource and triggers
	// a rolling replacement of control plane machines.
	if err := machineMgr.UpdateCPMSFailureDomain(ctx, targetFDNames); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating CPMS failure domains: %w", err)
	}
	_, generation, observedGeneration, err := machineMgr.IsCPMSGenerationObserved(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking CPMS generation: %w", err)
	}
	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
		fmt.Sprintf("Waiting for control plane rollout to start (CPMS generation %d/%d observed)", generation, observedGeneration))
	r.Recorder.Event(migration, "Normal", "CPMSUpdated", fmt.Sprintf("CPMS updated with failure domains %v", targetFDNames))
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// ensureWorkloadMigratedRolloutAndScaleDown runs Steps 5–8: wait for control plane
// rollout, scale old MachineSets to 0, wait for old machines/nodes to be deleted,
// then delete the empty source MachineSets.
// Progress is derived from cluster state so it is idempotent. Called from
// ensureWorkloadMigrated when target workers are ready and the CPMS already targets
// the target failure domains.
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
	observed, generation, observedGeneration, err := machineMgr.IsCPMSGenerationObserved(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking CPMS generation: %w", err)
	}
	if !observed {
		log.V(1).Info("CPMS generation not yet observed", "generation", generation, "observedGeneration", observedGeneration)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			fmt.Sprintf("Waiting for control plane rollout to start (CPMS generation %d/%d observed)", generation, observedGeneration))
		r.Recorder.Eventf(migration, "Normal", "ControlPlaneRollout", "waiting for rollout to start (CPMS generation %d/%d observed)", generation, observedGeneration)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	complete, replicas, updated, ready, err := machineMgr.CheckControlPlaneRolloutStatus(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking control plane rollout: %w", err)
	}
	if !complete {
		log.V(1).Info("control plane rollout in progress", "replicas", replicas, "updated", updated, "ready", ready)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			fmt.Sprintf("Control plane rolling out (%d/%d updated, %d/%d ready)", updated, replicas, ready, replicas))
		r.Recorder.Eventf(migration, "Normal", "ControlPlaneRollout", "control plane rolling out (%d/%d updated, %d/%d ready)", updated, replicas, ready, replicas)
		if machines, merr := machineMgr.ListControlPlaneMachines(ctx); merr != nil {
			log.V(2).Info("listing control plane machines failed", "err", merr)
		} else {
			for _, machine := range machines {
				logMachineDetail(log, machine)
			}
		}
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
		detail, eventNote, key, err := oldWorkerStallDetail(ctx, log, machineMgr, oldMachineSets)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("building old worker stall detail: %w", err)
		}
		log.V(1).Info("old worker deletion in progress", "detail", detail)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
			boundConditionMessage("Waiting for old worker deletion: "+detail))
		if key != r.lastStallEventKey || time.Since(r.lastStallEventTime) >= stallEventInterval {
			r.Recorder.Event(migration, "Warning", "OldWorkersStalled", eventNote)
			r.lastStallEventKey = key
			r.lastStallEventTime = time.Now()
		}
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

// ensureReady verifies all operators are healthy, every MachineConfigPool has
// converged on its current configuration, and only target vCenters remain in
// the Infrastructure resource. Ready=True is committed only after the full
// check has passed readyStabilityThreshold stable observations spaced at
// least readyStabilityObservationInterval apart.
func (r *VmwareCloudFoundationMigrationReconciler) ensureReady(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionReady

	// A long gap since the last ensureReady check means earlier reconciles
	// never reached this gate (e.g. an earlier condition flapped), so the
	// stability window cannot claim consecutive ~30s observations and must
	// restart.
	if !r.lastReadyStabilityCheck.IsZero() && time.Since(r.lastReadyStabilityCheck) > readyStabilityCheckGap {
		log.V(1).Info("resetting ready stability counter after long gap",
			"lastCheck", r.lastReadyStabilityCheck,
			"gap", readyStabilityCheckGap,
		)
		r.resetReadyStability()
	}
	r.lastReadyStabilityCheck = time.Now()

	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Verifying final cluster state")

	// Check all operators stable.
	opMgr := openshift.NewOperatorManager(r.ConfigClient)
	stable, summary, err := opMgr.CheckAllOperatorsStable(ctx)
	if err != nil {
		// A failed check is not a stable observation; restart the window so
		// the next success cannot immediately complete it.
		r.resetReadyStability()
		return ctrl.Result{}, fmt.Errorf("checking operator stability: %w", err)
	}

	// Check that every MachineConfigPool has converged on its current
	// configuration. This catches node config rollouts (e.g. a new etcd
	// revision triggered by the migration's own final changes) that have
	// started but not finished.
	poolMgr := openshift.NewMachineConfigPoolManager(r.MachineConfigClient)
	converged, poolSummary, err := poolMgr.CheckPoolsConverged(ctx)
	if err != nil {
		// A failed check is not a stable observation; restart the window so
		// the next success cannot immediately complete it.
		r.resetReadyStability()
		return ctrl.Result{}, fmt.Errorf("checking machine config pool convergence: %w", err)
	}

	// Verify only target vCenters remain in Infrastructure.
	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	infra, err := infraMgr.Get(ctx)
	if err != nil {
		// A failed check is not a stable observation; restart the window so
		// the next success cannot immediately complete it.
		r.resetReadyStability()
		return ctrl.Result{}, fmt.Errorf("getting infrastructure for readiness check: %w", err)
	}

	var nonTargetVC string
	if infra.Spec.PlatformSpec.VSphere != nil {
		targetServers := make(map[string]bool)
		for i := range migration.Spec.FailureDomains {
			targetServers[migration.Spec.FailureDomains[i].Server] = true
		}
		for _, vc := range infra.Spec.PlatformSpec.VSphere.VCenters {
			if !targetServers[vc.Server] {
				nonTargetVC = vc.Server
				break
			}
		}
	}

	blockers := make([]string, 0, 4)
	if !stable {
		if len(summary.UnavailableOperators) > 0 {
			blockers = append(blockers, fmt.Sprintf("unavailable=%s", strings.Join(summary.UnavailableOperators, ", ")))
		}
		if len(summary.ProgressingOperators) > 0 {
			blockers = append(blockers, fmt.Sprintf("progressing=%s", strings.Join(summary.ProgressingOperators, ", ")))
		}
		if len(summary.DegradedOperators) > 0 {
			blockers = append(blockers, fmt.Sprintf("degraded=%s", strings.Join(summary.DegradedOperators, ", ")))
		}
	}
	if !converged {
		if len(poolSummary.NotUpdatedPools) > 0 {
			blockers = append(blockers, fmt.Sprintf("pools-not-updated=%s", strings.Join(poolSummary.NotUpdatedPools, ", ")))
		}
		if len(poolSummary.DegradedPools) > 0 {
			blockers = append(blockers, fmt.Sprintf("pools-degraded=%s", strings.Join(poolSummary.DegradedPools, ", ")))
		}
	}
	if nonTargetVC != "" {
		blockers = append(blockers, fmt.Sprintf("non-target vCenter %q still present in Infrastructure", nonTargetVC))
	}

	if len(blockers) > 0 {
		// Any failing check restarts the stability window.
		r.resetReadyStability()
		msg := fmt.Sprintf("Migration not ready: %s", strings.Join(blockers, "; "))
		log.V(1).Info("cluster not yet stable",
			"unavailable", summary.UnavailableOperators,
			"progressing", summary.ProgressingOperators,
			"degraded", summary.DegradedOperators,
			"poolsNotUpdated", poolSummary.NotUpdatedPools,
			"poolsDegraded", poolSummary.DegradedPools,
			"nonTargetVCenter", nonTargetVC,
		)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, msg)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Everything is stable right now. Require sustained stability before
	// committing Ready=True so a config revision triggered by the
	// migration's own final changes cannot start right after this check.
	// Only observations spaced at least
	// readyStabilityObservationInterval apart count, so event-driven
	// reconciles cannot satisfy the threshold within seconds.
	if r.lastCountedStabilityCheck.IsZero() || time.Since(r.lastCountedStabilityCheck) >= readyStabilityObservationInterval {
		r.lastCountedStabilityCheck = time.Now()
		r.readyStabilityCount++
	} else {
		log.V(1).Info("stable observation not counted yet",
			"lastCounted", r.lastCountedStabilityCheck,
			"interval", readyStabilityObservationInterval,
		)
	}
	if r.readyStabilityCount < readyStabilityThreshold {
		msg := fmt.Sprintf("Waiting for sustained cluster stability (%d/%d)", r.readyStabilityCount, readyStabilityThreshold)
		log.V(1).Info("cluster stable, waiting for sustained stability",
			"count", r.readyStabilityCount,
			"threshold", readyStabilityThreshold,
		)
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, msg)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	now := metav1.Now()
	migration.Status.CompletionTime = &now

	r.setCondition(migration, condType, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "Migration complete, all operators healthy and node pools converged")
	r.Recorder.Event(migration, "Normal", "MigrationComplete", "Migration completed successfully")
	return ctrl.Result{}, nil
}

// resetReadyStability restarts the sustained-stability window: the counter
// returns to zero and the next stable observation is eligible to count
// immediately.
func (r *VmwareCloudFoundationMigrationReconciler) resetReadyStability() {
	r.readyStabilityCount = 0
	r.lastCountedStabilityCheck = time.Time{}
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

// statusConditionChanged reports whether two conditions of the same type
// differ in any field this operator writes. It identifies which conditions a
// reconcile actually changed relative to the status it started from.
func statusConditionChanged(prev, next metav1.Condition) bool {
	return prev.Status != next.Status ||
		prev.Reason != next.Reason ||
		prev.Message != next.Message ||
		prev.ObservedGeneration != next.ObservedGeneration ||
		!prev.LastTransitionTime.Equal(&next.LastTransitionTime)
}

// updateStatus persists this reconcile's status changes using optimistic
// concurrency. It re-fetches the latest resource and applies only the
// conditions and timestamps this reconcile changed relative to baseStatus (the
// snapshot taken when the reconcile started), retrying on conflict.
//
// Applying only changed conditions onto the freshly fetched copy merges
// cleanly with a concurrent reconcile — inevitable during a leader handoff,
// where the two reconciles run in separate processes — that committed a
// different condition in the meantime: conditions this reconcile did not touch
// are left as the other writer set them. As a final guard, a committed True is
// never downgraded by a stale False or Unknown from a reconcile that started
// before the success was recorded (it self-heals on the next reconcile anyway,
// but this avoids a visible flap).
func (r *VmwareCloudFoundationMigrationReconciler) updateStatus(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, baseStatus migrationv1alpha1.VmwareCloudFoundationMigrationStatus) error {
	log := klog.FromContext(ctx)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(migration), latest); err != nil {
			return err
		}
		for i := range migration.Status.Conditions {
			cond := migration.Status.Conditions[i]
			// Skip conditions this reconcile did not change, so a concurrent
			// writer's conditions survive.
			if prev := apimeta.FindStatusCondition(baseStatus.Conditions, cond.Type); prev != nil && !statusConditionChanged(*prev, cond) {
				continue
			}
			// Skip deltas from a reconcile that started before the current
			// resource generation; a newer writer may have already committed
			// up-to-date conditions for this type.
			if cond.ObservedGeneration < latest.Generation {
				log.V(1).Info("skipping stale generation condition update", "condition", cond.Type, "observedGeneration", cond.ObservedGeneration, "latestGeneration", latest.Generation)
				continue
			}
			// Never downgrade a success another reconcile already committed.
			if existing := apimeta.FindStatusCondition(latest.Status.Conditions, cond.Type); existing != nil &&
				existing.Status == metav1.ConditionTrue && cond.Status != metav1.ConditionTrue {
				log.V(1).Info("keeping committed condition success over stale update", "condition", cond.Type)
				continue
			}
			apimeta.SetStatusCondition(&latest.Status.Conditions, cond)
		}
		if migration.Status.StartTime != nil && latest.Status.StartTime == nil {
			latest.Status.StartTime = migration.Status.StartTime
		}
		if migration.Status.CompletionTime != nil && latest.Status.CompletionTime == nil {
			latest.Status.CompletionTime = migration.Status.CompletionTime
		}
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

// workerReadinessStatus holds aggregate readiness for target worker MachineSets.
type workerReadinessStatus struct {
	Ready         bool
	MachinesReady int32
	MachinesTotal int32
	NodesReady    int32
	NodesTotal    int32
}

// checkWorkerReadiness verifies that all machines and nodes for the target worker
// MachineSets are in a ready state. It returns aggregate readiness counts across
// all target MachineSets, which callers use to report rollout progress.
func checkWorkerReadiness(ctx context.Context, machineMgr *openshift.MachineManager, fds []configv1.VSpherePlatformFailureDomainSpec, infraID string) (workerReadinessStatus, error) {
	log := klog.FromContext(ctx)
	status := workerReadinessStatus{Ready: true}
	for i := range fds {
		msName := workerMachineSetName(infraID, fds[i].Name)
		machinesOK, machineReadyCount, machineTotalCount, merr := machineMgr.CheckMachinesReady(ctx, msName)
		if merr != nil {
			return workerReadinessStatus{}, fmt.Errorf("checking machines for %q: %w", msName, merr)
		}
		status.MachinesReady += machineReadyCount
		status.MachinesTotal += machineTotalCount
		if !machinesOK {
			log.V(1).Info("machines not ready", "machineSet", msName, "ready", machineReadyCount, "total", machineTotalCount)
			status.Ready = false
		}
		nodesOK, nodeReadyCount, nodeTotalCount, nerr := machineMgr.CheckNodesReady(ctx, msName)
		if nerr != nil {
			return workerReadinessStatus{}, fmt.Errorf("checking nodes for %q: %w", msName, nerr)
		}
		status.NodesReady += nodeReadyCount
		status.NodesTotal += nodeTotalCount
		if !nodesOK {
			log.V(1).Info("nodes not ready", "machineSet", msName, "ready", nodeReadyCount, "total", nodeTotalCount)
			status.Ready = false
		}
	}
	return status, nil
}

type stallDetailPart struct {
	machineSetName string
	text           string
	machineNames   []string
}

// oldWorkerStallDetail formats per-MachineSet detail for old worker machines and
// nodes that are still being deleted (typically a PDB blocking node drain):
// machine names with age and error reason, plus the remaining node count. It also
// returns a bounded Warning event note and a stable key (machine set name plus
// sorted machine names) identifying the exact set of lingering machines, used
// to debounce Warning events.
func oldWorkerStallDetail(ctx context.Context, log klog.Logger, machineMgr *openshift.MachineManager, machineSets []*machinev1beta1.MachineSet) (string, string, string, error) {
	parts := make([]stallDetailPart, 0, len(machineSets))
	keyParts := make([]string, 0, len(machineSets))
	for _, ms := range machineSets {
		machines, err := machineMgr.ListMachinesForMachineSet(ctx, ms.Name)
		if err != nil {
			return "", "", "", err
		}
		if len(machines) == 0 {
			continue
		}
		sort.Slice(machines, func(i, j int) bool {
			return machines[i].Name < machines[j].Name
		})
		names := make([]string, 0, len(machines))
		keyNames := make([]string, 0, len(machines))
		partMachineNames := make([]string, 0, len(machines))
		for _, machine := range machines {
			logStalledWorkerMachine(log, ms.Name, machine)
			age := time.Since(machine.CreationTimestamp.Time).Round(time.Second)
			entry := fmt.Sprintf("%s (%s old", machine.Name, age)
			if machine.Status.ErrorReason != nil {
				entry += fmt.Sprintf(", %s", *machine.Status.ErrorReason)
			}
			entry += ")"
			names = append(names, entry)
			keyNames = append(keyNames, machine.Name)
			partMachineNames = append(partMachineNames, machine.Name)
		}
		_, nodeRemaining, err := machineMgr.CheckNodesDeletedForMachines(ctx, ms.Name)
		if err != nil {
			return "", "", "", err
		}
		parts = append(parts, stallDetailPart{
			machineSetName: ms.Name,
			text:           fmt.Sprintf("%s has %d machine(s) [%s] and %d node(s) remaining", ms.Name, len(machines), strings.Join(names, ", "), nodeRemaining),
			machineNames:   partMachineNames,
		})
		sort.Strings(keyNames)
		keyParts = append(keyParts, ms.Name+"="+strings.Join(keyNames, ","))
	}
	sort.Strings(keyParts)
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].machineSetName < parts[j].machineSetName
	})
	detail := joinStallDetailParts(parts)
	return detail, boundOldWorkerStallEventNote(parts), strings.Join(keyParts, "|"), nil
}

func joinStallDetailParts(parts []stallDetailPart) string {
	texts := make([]string, len(parts))
	for i, part := range parts {
		texts[i] = part.text
	}
	return strings.Join(texts, "; ")
}

// boundOldWorkerStallEventNote formats a Warning event note capped at
// maxEventNoteBytes. Full detail remains in the condition message and
// oldWorkerStallDetail log output; omitted machines are summarized.
func boundOldWorkerStallEventNote(parts []stallDetailPart) string {
	allNames := make([]string, 0)
	for _, part := range parts {
		allNames = append(allNames, part.machineNames...)
	}

	fullDetail := joinStallDetailParts(parts)
	note := oldWorkerStallEventPrefix + fullDetail
	if len(note) <= maxEventNoteBytes {
		return note
	}

	included := make([]stallDetailPart, 0, len(parts))
	includedNameCount := 0
	for _, part := range parts {
		trialIncluded := append(included, part)
		trialDetail := joinStallDetailParts(trialIncluded)
		omitted := allNames[includedNameCount+len(part.machineNames):]
		suffix := stallOmitSuffix(omitted)
		if len(oldWorkerStallEventPrefix+trialDetail+suffix) <= maxEventNoteBytes {
			included = trialIncluded
			includedNameCount += len(part.machineNames)
			continue
		}
		break
	}

	detail := joinStallDetailParts(included)
	omitted := allNames[includedNameCount:]
	maxSuffixBytes := maxEventNoteBytes - len(oldWorkerStallEventPrefix+detail)
	return oldWorkerStallEventPrefix + detail + stallOmitSuffixFit(omitted, maxSuffixBytes)
}

func stallOmitSuffix(omitted []string) string {
	if len(omitted) == 0 {
		return ""
	}
	return fmt.Sprintf("; ... and %d more machine(s) omitted: [%s]", len(omitted), strings.Join(omitted, ", "))
}

func stallOmitSuffixFit(omitted []string, maxSuffixBytes int) string {
	if len(omitted) == 0 {
		return ""
	}
	if maxSuffixBytes <= 0 {
		return truncateToBytes(stallOmitSuffix(omitted), maxSuffixBytes)
	}

	suffix := stallOmitSuffix(omitted)
	if len(suffix) <= maxSuffixBytes {
		return suffix
	}

	countPrefix := fmt.Sprintf("; ... and %d more machine(s) omitted: [", len(omitted))
	suffixSuffix := "]"
	available := maxSuffixBytes - len(countPrefix) - len(suffixSuffix)
	if available <= 0 {
		return truncateToBytes(fmt.Sprintf("; ... and %d more machine(s) omitted", len(omitted)), maxSuffixBytes)
	}

	names := append([]string(nil), omitted...)
	for len(names) > 0 {
		trial := countPrefix + strings.Join(names, ", ") + suffixSuffix
		if len(trial) <= maxSuffixBytes {
			return trial
		}
		names = names[:len(names)-1]
	}

	return truncateToBytes(fmt.Sprintf("; ... and %d more machine(s) omitted", len(omitted)), maxSuffixBytes)
}

// boundConditionMessage ensures a condition message does not exceed
// maxConditionMessageBytes (32768 characters), preserving the message
// when within the limit and truncating with an omitted machine count when exceeded.
func boundConditionMessage(msg string) string {
	if len(msg) <= maxConditionMessageBytes {
		return msg
	}

	totalMachines := countMachinesInMessage(msg)
	const countPrefix = "; ... and "
	const countSuffix = " more machine(s) omitted"
	suffixEst := fmt.Sprintf("%s%d%s", countPrefix, totalMachines, countSuffix)
	maxPrefixLen := maxConditionMessageBytes - len(suffixEst)
	if maxPrefixLen <= 0 {
		return truncateToBytes(suffixEst, maxConditionMessageBytes)
	}

	prefix := msg[:maxPrefixLen]
	if lastSemi := strings.LastIndex(prefix, "; "); lastSemi > 0 {
		prefix = prefix[:lastSemi]
	} else if lastComma := strings.LastIndex(prefix, ", "); lastComma > 0 {
		prefix = prefix[:lastComma]
	}

	preservedMachines := countMachinesInMessage(prefix)
	omitted := totalMachines - preservedMachines
	if omitted <= 0 {
		omitted = 1
	}

	suffix := fmt.Sprintf("%s%d%s", countPrefix, omitted, countSuffix)
	if len(prefix)+len(suffix) > maxConditionMessageBytes {
		prefix = truncateToBytes(prefix, maxConditionMessageBytes-len(suffix))
	}

	return prefix + suffix
}

func countMachinesInMessage(msg string) int {
	count := 0
	start := 0
	for {
		open := strings.Index(msg[start:], "[")
		if open == -1 {
			break
		}
		openIdx := start + open + 1
		close := strings.Index(msg[openIdx:], "]")
		if close == -1 {
			count += countMachineEntries(msg[openIdx:])
			break
		}
		closeIdx := openIdx + close
		count += countMachineEntries(msg[openIdx:closeIdx])
		start = closeIdx + 1
	}
	return count
}

func countMachineEntries(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	c := strings.Count(s, ")")
	if c > 0 {
		return c
	}
	return strings.Count(s, ",") + 1
}

func truncateToBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes]
}

// logStalledWorkerMachine logs allow-listed diagnostics for a stalled old worker
// Machine. Node names and raw error messages are omitted.
func logStalledWorkerMachine(log klog.Logger, machineSetName string, machine *machinev1beta1.Machine) {
	phase := ""
	if machine.Status.Phase != nil {
		phase = *machine.Status.Phase
	}
	kv := []interface{}{
		"machineSet", machineSetName,
		"machine", machine.Name,
		"age", time.Since(machine.CreationTimestamp.Time).Round(time.Second),
		"phase", phase,
	}
	if machine.Status.ErrorReason != nil {
		kv = append(kv, "errorReason", string(*machine.Status.ErrorReason))
	}
	log.V(1).Info("stalled old worker machine", kv...)
}

// logMachineDetail logs the status of a single Machine so that rollout progress
// and stalled machines can be diagnosed from operator logs.
func logMachineDetail(log klog.Logger, machine *machinev1beta1.Machine) {
	phase := ""
	if machine.Status.Phase != nil {
		phase = *machine.Status.Phase
	}
	kv := []interface{}{
		"machine", machine.Name,
		"phase", phase,
		"age", time.Since(machine.CreationTimestamp.Time).Round(time.Second),
	}
	if machine.Status.NodeRef != nil {
		kv = append(kv, "hasNodeRef", true)
	}
	if machine.Status.LastUpdated != nil {
		kv = append(kv, "lastUpdated", machine.Status.LastUpdated.Time)
	}
	if machine.Status.ErrorReason != nil {
		kv = append(kv, "errorReason", string(*machine.Status.ErrorReason))
	}
	if machine.Status.ErrorMessage != nil {
		kv = append(kv, "hasErrorMessage", true)
	}
	log.V(1).Info("machine status", kv...)
}
