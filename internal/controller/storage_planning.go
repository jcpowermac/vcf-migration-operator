package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/execution"
	"github.com/openshift/vcf-migration-operator/internal/storageplan"
	"github.com/openshift/vcf-migration-operator/internal/util"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

const planningResync = 5 * time.Minute
const migrationUIDLabel = "migration.openshift.io/migration-uid"

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list
// +kubebuilder:rbac:groups=apps,resources=replicasets;statefulsets;daemonsets,verbs=get
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get
// +kubebuilder:rbac:groups=migration.openshift.io,resources=storageworkloadmigrations,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=migration.openshift.io,resources=storageworkloadmigrations/status,verbs=get;update;patch

func isPlanningOrPausedReason(reason string) bool {
	switch reason {
	case migrationv1alpha1.ReasonPaused, migrationv1alpha1.ReasonPlanning, migrationv1alpha1.ReasonPlanComplete, migrationv1alpha1.ReasonPlanBlocked, migrationv1alpha1.ReasonPlanningAfterStart:
		return true
	default:
		return false
	}
}

// blockUnsupportedStorageExecution keeps the guard at the execution boundary
// until the complete storage workflow is implemented, preserving valid plans.
func (r *VmwareCloudFoundationMigrationReconciler) blockUnsupportedStorageExecution(migration *migrationv1alpha1.VmwareCloudFoundationMigration, condition string) bool {
	if isPlanningCondition(condition) || !migration.Spec.MigrateStorage {
		return false
	}
	message := "Storage migration execution is not implemented; only non-disruptive storage planning is available"
	changed := r.setCondition(migration, migrationv1alpha1.ConditionReady, metav1.ConditionFalse, migrationv1alpha1.ReasonStorageExecutionUnsupported, message)
	if changed && r.Recorder != nil {
		r.Recorder.Eventf(migration, nil, "Warning", migrationv1alpha1.ReasonStorageExecutionUnsupported, migrationv1alpha1.ConditionReady, "%s", message)
	}
	return true
}

func isPlanningCondition(condition string) bool {
	return condition == migrationv1alpha1.ConditionInfrastructurePrepared || condition == migrationv1alpha1.ConditionStoragePlanned
}

// isConditionComplete requires the named condition to be True. Before execution
// starts, planning conditions must also reflect the current migration generation.
func isConditionComplete(m *migrationv1alpha1.VmwareCloudFoundationMigration, conditionType string) bool {
	c := apimeta.FindStatusCondition(m.Status.Conditions, conditionType)
	if c == nil || c.Status != metav1.ConditionTrue {
		return false
	}
	if isPlanningCondition(conditionType) && m.Status.StartTime == nil {
		return c.ObservedGeneration == m.Generation
	}
	return true
}

// canRefreshPlan allows a same-generation inventory change only if no newer writer
// changed the condition since this reconcile read it, and disruption has not started.
func canRefreshPlan(latest *migrationv1alpha1.VmwareCloudFoundationMigration, base migrationv1alpha1.VmwareCloudFoundationMigrationStatus, condition string, existing metav1.Condition) bool {
	if condition != migrationv1alpha1.ConditionStoragePlanned {
		return false
	}
	if latest.Status.StartTime != nil {
		return false
	}

	previous := apimeta.FindStatusCondition(base.Conditions, condition)
	if previous == nil {
		return false
	}
	if statusConditionChanged(*previous, existing) {
		return false
	}
	return true
}

func checkStorageManagementForPlan(ctx context.Context, dyn dynamic.Interface, configured bool) (string, error) {
	if !configured {
		return checkVSphereStorageManagement(ctx, dyn)
	}
	state, err := getManagementState(ctx, dyn, clusterCSIDriverGVR, vsphereCSIDriverName)
	if err != nil {
		return "", fmt.Errorf("reading vSphere CSI management state: %w", err)
	}
	if state != "Managed" {
		return "", fmt.Errorf("configuring storage migration requires ClusterCSIDriver/%s spec.managementState Managed (current: %s)", vsphereCSIDriverName, state)
	}
	return "", nil
}

// storagePlanSummary aggregates volume counts across all planned namespaces.
type storagePlanSummary struct {
	namespaces        int
	volumes           int
	blockedVolumes    int
	blockedNamespaces int
}

func (r *VmwareCloudFoundationMigrationReconciler) ensureStoragePlanned(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {

	var previous *metav1.Condition
	if condition := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned); condition != nil {
		previous = condition.DeepCopy()
	}

	defer r.recordStoragePlannedEvent(m, previous)

	if !m.Spec.MigrateStorage {
		r.setCondition(m, migrationv1alpha1.ConditionStoragePlanned, metav1.ConditionTrue, migrationv1alpha1.ReasonNotConfigured, "Storage migration is not enabled")
		r.clearStoragePlanBlocked(m)
		return ctrl.Result{}, nil
	}

	in, err := r.collectStorageInventory(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}

	plan := storageplan.Build(in)
	if err := r.checkPlanPolicies(ctx, m, in.Infrastructure, plan, in.StorageClasses, in.PVCs); err != nil {
		return ctrl.Result{}, err
	}

	summary, err := r.writeNamespacePlans(ctx, m, plan, in.Pods)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteStaleStoragePlans(ctx, m, plan); err != nil {
		return ctrl.Result{}, err
	}

	if summary.blockedVolumes > 0 || len(plan.Blockers) > 0 {
		message := storagePlanBlockedMessage(summary, plan.Blockers)
		r.setCondition(m, migrationv1alpha1.ConditionStoragePlanned, metav1.ConditionFalse, migrationv1alpha1.ReasonPlanBlocked, message)
		r.setCondition(m, migrationv1alpha1.ConditionReady, metav1.ConditionFalse, migrationv1alpha1.ReasonPlanBlocked, message)
		return ctrl.Result{RequeueAfter: planningResync}, nil
	}
	r.setCondition(m, migrationv1alpha1.ConditionStoragePlanned, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, fmt.Sprintf("Planned %d volumes in %d namespaces", summary.volumes, summary.namespaces))
	r.clearStoragePlanBlocked(m)
	return ctrl.Result{}, nil
}

// clearStoragePlanBlocked releases a Running migration's Ready condition from
// PlanBlocked once storage planning passes. reconcileWorkflow keeps that
// reason in place so a blocked plan does not flip Ready on every reconcile.
func (r *VmwareCloudFoundationMigrationReconciler) clearStoragePlanBlocked(m *migrationv1alpha1.VmwareCloudFoundationMigration) {
	if m.Spec.State != migrationv1alpha1.MigrationStateRunning {
		return
	}
	ready := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != migrationv1alpha1.ReasonPlanBlocked {
		return
	}
	r.setReadyRunning(m)
}

// recordStoragePlannedEvent emits an event when the StoragePlanned condition
// was added or changed relative to previous.
func (r *VmwareCloudFoundationMigrationReconciler) recordStoragePlannedEvent(m *migrationv1alpha1.VmwareCloudFoundationMigration, previous *metav1.Condition) {
	if r.Recorder == nil {
		return
	}
	condition := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned)
	if condition == nil {
		return
	}
	if previous != nil && !statusConditionChanged(*previous, *condition) {
		return
	}
	eventType := "Normal"
	if condition.Status == metav1.ConditionFalse {
		eventType = "Warning"
	}
	r.Recorder.Eventf(m, nil, eventType, condition.Reason, migrationv1alpha1.ConditionStoragePlanned, "%s", truncateToBytes(condition.Message, maxEventNoteBytes))
}

// collectStorageInventory lists the cluster resources needed to build a storage plan.
func (r *VmwareCloudFoundationMigrationReconciler) collectStorageInventory(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration) (storageplan.Input, error) {
	pvs, err := r.KubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return storageplan.Input{}, fmt.Errorf("listing PV inventory: %w", err)
	}
	pvcs, err := r.KubeClient.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return storageplan.Input{}, fmt.Errorf("listing PVC inventory: %w", err)
	}
	pods, err := r.KubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return storageplan.Input{}, fmt.Errorf("listing storage consumers: %w", err)
	}
	classes, err := r.KubeClient.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return storageplan.Input{}, fmt.Errorf("listing StorageClasses: %w", err)
	}
	infra, err := r.ConfigClient.ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return storageplan.Input{}, fmt.Errorf("getting infrastructure for storage planning: %w", err)
	}
	categories, err := r.getCSITopologyCategories(ctx)
	if err != nil {
		return storageplan.Input{}, err
	}
	return storageplan.Input{
		Migration:          m,
		Infrastructure:     infra,
		PVs:                pvs.Items,
		PVCs:               pvcs.Items,
		Pods:               pods.Items,
		StorageClasses:     classes.Items,
		TopologyCategories: categories,
	}, nil
}

// getCSITopologyCategories returns the topology categories configured on the
// vSphere ClusterCSIDriver.
func (r *VmwareCloudFoundationMigrationReconciler) getCSITopologyCategories(ctx context.Context) ([]string, error) {
	csi, err := r.DynamicClient.Resource(clusterCSIDriverGVR).Get(ctx, vsphereCSIDriverName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting CSI topology categories: %w", err)
	}
	categories, _, err := unstructured.NestedStringSlice(csi.Object, "spec", "driverConfig", "vSphere", "topologyCategories")
	if err != nil {
		return nil, fmt.Errorf("reading CSI topology categories: %w", err)
	}
	return categories, nil
}

// writeNamespacePlans resolves storage consumers for each planned namespace,
// writes its StorageWorkloadMigration, and summarizes volume and blocker counts.
// Namespaces are processed in sorted order so writes are deterministic.
func (r *VmwareCloudFoundationMigrationReconciler) writeNamespacePlans(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration, plan *storageplan.Plan, pods []corev1.Pod) (storagePlanSummary, error) {
	namespaces := make([]string, 0, len(plan.Namespaces))
	for ns := range plan.Namespaces {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	summary := storagePlanSummary{namespaces: len(namespaces)}
	for _, namespace := range namespaces {

		nsPlan := plan.Namespaces[namespace]

		consumers, err := storageplan.Consumers(ctx, pods, namespace, nsPlan.Volumes, r.lookupStorageOwner)

		if err != nil {
			return storagePlanSummary{}, err
		}
		nsPlan.Consumers = consumers

		blocked := countBlockedVolumes(nsPlan.Volumes)
		summary.volumes += len(nsPlan.Volumes)
		summary.blockedVolumes += blocked

		if blocked > 0 {
			summary.blockedNamespaces++
		}

		if err := r.writeStoragePlan(ctx, m, namespace, nsPlan, blocked); err != nil {
			return storagePlanSummary{}, err
		}
	}
	return summary, nil
}

// countBlockedVolumes returns the number of volumes with at least one blocker.
func countBlockedVolumes(volumes []migrationv1alpha1.StorageWorkloadMigrationVolume) int {
	count := 0
	for _, v := range volumes {
		if len(v.Blockers) > 0 {
			count++
		}
	}
	return count
}

// storagePlanBlockedMessage builds the StoragePlanned condition message for a
// plan with blocked volumes or plan-wide blockers.
func storagePlanBlockedMessage(summary storagePlanSummary, blockers []migrationv1alpha1.StorageWorkloadMigrationBlocker) string {
	parts := make([]string, 0, len(blockers)+2)
	parts = append(parts, fmt.Sprintf("%d volumes blocked in %d namespaces", summary.blockedVolumes, summary.blockedNamespaces))
	for _, b := range blockers {
		parts = append(parts, b.Reason+": "+b.Message)
	}
	parts = append(parts, "see StorageWorkloadMigration status")
	return boundConditionMessage(strings.Join(parts, "; "))
}

func (r *VmwareCloudFoundationMigrationReconciler) lookupStorageOwner(ctx context.Context, ns string, owner metav1.OwnerReference) (metav1.Object, error) {
	opts := metav1.GetOptions{}
	switch owner.Kind {
	case "ReplicaSet":
		return r.KubeClient.AppsV1().ReplicaSets(ns).Get(ctx, owner.Name, opts)
	case "Deployment":
		return r.KubeClient.AppsV1().Deployments(ns).Get(ctx, owner.Name, opts)
	case "StatefulSet":
		return r.KubeClient.AppsV1().StatefulSets(ns).Get(ctx, owner.Name, opts)
	case "DaemonSet":
		return r.KubeClient.AppsV1().DaemonSets(ns).Get(ctx, owner.Name, opts)
	case "Job":
		return r.KubeClient.BatchV1().Jobs(ns).Get(ctx, owner.Name, opts)
	case "CronJob":
		return r.KubeClient.BatchV1().CronJobs(ns).Get(ctx, owner.Name, opts)
	default:
		return nil, fmt.Errorf("looking up unsupported controller kind %s", owner.Kind)
	}
}

func storagePlanName(m *migrationv1alpha1.VmwareCloudFoundationMigration) string {
	uid := string(m.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return "vcf-storage-" + uid
}

func (r *VmwareCloudFoundationMigrationReconciler) writeStoragePlan(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration, namespace string, plan *storageplan.NamespacePlan, blocked int) error {
	key := client.ObjectKey{Namespace: namespace, Name: storagePlanName(m)}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		swm := &migrationv1alpha1.StorageWorkloadMigration{}
		err := r.Get(ctx, key, swm)
		ref := migrationv1alpha1.MigrationObjectReference{Name: m.Name, UID: m.UID}
		if apierrors.IsNotFound(err) {
			swm = &migrationv1alpha1.StorageWorkloadMigration{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: namespace, Labels: map[string]string{migrationUIDLabel: string(m.UID)}}, Spec: migrationv1alpha1.StorageWorkloadMigrationSpec{MigrationRef: ref}}
			if err := execution.Check(ctx); err != nil {
				return err
			}
			if err = r.Create(ctx, swm); err != nil {
				return fmt.Errorf("creating SWM %s: %w", key, err)
			}
		} else if err != nil {
			return fmt.Errorf("getting SWM %s: %w", key, err)
		}
		if swm.Spec.MigrationRef != ref {
			return fmt.Errorf("checking SWM %s: migration identity conflicts", key)
		}
		if swm.Status.Phase != "" && swm.Status.Phase != migrationv1alpha1.StorageWorkloadMigrationPhasePlanning {
			return nil
		}
		if swm.Labels[migrationUIDLabel] != string(m.UID) {
			if swm.Labels == nil {
				swm.Labels = map[string]string{}
			}
			swm.Labels[migrationUIDLabel] = string(m.UID)
			if err := execution.Check(ctx); err != nil {
				return err
			}
			if err := r.Update(ctx, swm); err != nil {
				return err
			}
		}
		previous := swm.Status.DeepCopy()
		swm.Status.Phase = migrationv1alpha1.StorageWorkloadMigrationPhasePlanning
		swm.Status.Volumes = plan.Volumes
		swm.Status.Consumers = plan.Consumers
		status, reason, message := metav1.ConditionTrue, "NoBlockers", "All volumes can be migrated"
		if blocked > 0 {
			status = metav1.ConditionFalse
			reason = "Blocked"
			message = fmt.Sprintf("%d of %d volumes blocked", blocked, len(plan.Volumes))
		}
		apimeta.SetStatusCondition(&swm.Status.Conditions, metav1.Condition{Type: migrationv1alpha1.StorageWorkloadMigrationConditionPlanned, Status: status, Reason: reason, Message: message, ObservedGeneration: swm.Generation})
		if reflect.DeepEqual(*previous, swm.Status) {
			return nil
		}
		if err := execution.Check(ctx); err != nil {
			return err
		}
		if err := r.Status().Update(ctx, swm); err != nil {
			return fmt.Errorf("updating SWM %s status: %w", key, err)
		}
		return nil
	})
}

func (r *VmwareCloudFoundationMigrationReconciler) deleteStaleStoragePlans(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration, plan *storageplan.Plan) error {
	var list migrationv1alpha1.StorageWorkloadMigrationList
	if err := r.List(ctx, &list, client.MatchingLabels{migrationUIDLabel: string(m.UID)}); err != nil {
		return fmt.Errorf("listing SWMs: %w", err)
	}
	for i := range list.Items {
		swm := &list.Items[i]
		if swm.Spec.MigrationRef.UID != m.UID || swm.Status.Phase != migrationv1alpha1.StorageWorkloadMigrationPhasePlanning || plan.Namespaces[swm.Namespace] != nil {
			continue
		}
		if err := execution.Check(ctx); err != nil {
			return err
		}
		if err := r.Delete(ctx, swm, client.Preconditions{UID: &swm.UID, ResourceVersion: &swm.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting stale SWM %s/%s: %w", swm.Namespace, swm.Name, err)
		}
	}
	return nil
}

func (r *VmwareCloudFoundationMigrationReconciler) checkPlanPolicies(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration, infra *configv1.Infrastructure, plan *storageplan.Plan, classes []storagev1.StorageClass, pvcs []corev1.PersistentVolumeClaim) error {
	policyCtx, cancel := context.WithTimeout(ctx, preflightVSphereTimeout)
	defer cancel()
	defer func() {
		// Log out on a fresh bounded context, as preflight does, so cached
		// session handles are released even when policyCtx has expired.
		logoutCtx, logoutCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer logoutCancel()
		vsphere.ClearSessions(logoutCtx)
	}()

	classMap := map[string]*storagev1.StorageClass{}
	for i := range classes {
		classMap[classes[i].Name] = &classes[i]
	}
	claims := map[string]*corev1.PersistentVolumeClaim{}
	for i := range pvcs {
		claims[util.GetObjectKey(pvcs[i].Name, pvcs[i].Namespace)] = &pvcs[i]
	}

	checker := newPolicyChecker(r, m)
	for namespace, nsPlan := range plan.Namespaces {
		for i := range nsPlan.Volumes {
			v := &nsPlan.Volumes[i]
			if v.TargetFailureDomain == "" {
				continue
			}
			pvc := claims[util.GetObjectKey(v.SourcePVC.Name, namespace)]
			if pvc == nil {
				continue
			}
			policy := customerStoragePolicy(pvc, classMap, infra.Status.InfrastructureName)
			if policy == "" {
				continue
			}
			fd := findFailureDomainByName(m.Spec.FailureDomains, v.TargetFailureDomain)
			if fd == nil {
				continue
			}
			compatible, err := checker.isCompatible(policyCtx, fd, policy)
			if err != nil {
				return err
			}
			if !compatible {
				storageplan.AddBlocker(v, "StoragePolicyMissing", fmt.Sprintf("policy %s is missing or incompatible with target datastore %s on %s", policy, fd.Topology.Datastore, fd.Server))
			}
		}
	}
	return nil
}

// customerStoragePolicy returns the storage policy named by the PVC's
// StorageClass. It returns "" when there is no policy to check: the PVC has no
// known StorageClass, the class names no policy, or the class uses the default
// policy that the CSI driver operator creates in every vCenter.
func customerStoragePolicy(pvc *corev1.PersistentVolumeClaim, classes map[string]*storagev1.StorageClass, infraName string) string {
	if pvc.Spec.StorageClassName == nil {
		return ""
	}
	sc := classes[*pvc.Spec.StorageClassName]
	if sc == nil {
		return ""
	}
	policy, _ := storageplan.Parameter(sc, "storagepolicyname")
	if infraName != "" && policy == "openshift-storage-policy-"+infraName {
		return ""
	}
	return policy
}

// profileKey identifies a storage policy on a target vCenter.
type profileKey struct {
	server string
	policy string
}

// datastoreKey identifies a storage policy checked against one target datastore.
type datastoreKey struct {
	profileKey
	datacenter string
	datastore  string
}

// policyChecker checks storage policies against target vCenters. It caches
// credentials, policy profile IDs, and datastore compatibility so volumes
// sharing a policy and failure domain are checked once.
type policyChecker struct {
	reconciler *VmwareCloudFoundationMigrationReconciler
	migration  *migrationv1alpha1.VmwareCloudFoundationMigration
	creds      map[string]credentials
	profiles   map[profileKey]string
	results    map[datastoreKey]bool
}

// newPolicyChecker returns a policyChecker with empty caches.
func newPolicyChecker(r *VmwareCloudFoundationMigrationReconciler, m *migrationv1alpha1.VmwareCloudFoundationMigration) *policyChecker {
	return &policyChecker{
		reconciler: r,
		migration:  m,
		creds:      map[string]credentials{},
		profiles:   map[profileKey]string{},
		results:    map[datastoreKey]bool{},
	}
}

// isCompatible reports whether policy exists in fd's vCenter and matches fd's datastore.
func (c *policyChecker) isCompatible(ctx context.Context, fd *configv1.VSpherePlatformFailureDomainSpec, policy string) (bool, error) {
	key := datastoreKey{
		profileKey: profileKey{server: fd.Server, policy: policy},
		datacenter: fd.Topology.Datacenter,
		datastore:  fd.Topology.Datastore,
	}
	if compatible, ok := c.results[key]; ok {
		return compatible, nil
	}

	s, err := c.session(ctx, fd)
	if err != nil {
		return false, err
	}
	profile, err := c.profileID(ctx, s, key.profileKey)
	if err != nil {
		return false, err
	}
	compatible := false
	if profile != "" {
		compatible, err = vsphere.IsDatastoreCompatible(ctx, s, profile, fd.Topology.Datastore)
		if err != nil {
			return false, err
		}
	}
	c.results[key] = compatible
	return compatible, nil
}

// session returns a cached session for fd's vCenter and datacenter, resolving
// credentials once per server.
func (c *policyChecker) session(ctx context.Context, fd *configv1.VSpherePlatformFailureDomainSpec) (*vsphere.Session, error) {
	creds, ok := c.creds[fd.Server]
	if !ok {
		username, password, err := getTargetCredentials(ctx, c.reconciler.KubeClient, c.migration, fd.Server)
		if err != nil {
			return nil, err
		}
		creds = credentials{username: username, password: password}
		c.creds[fd.Server] = creds
	}
	s, err := getVSphereSession(ctx, fd.Server, fd.Topology.Datacenter, creds.username, creds.password)
	if err != nil {
		return nil, fmt.Errorf("connecting to target vCenter %s for storage policy checks: %w", fd.Server, err)
	}
	return s, nil
}

// profileID returns the policy's profile ID, or "" when the policy does not exist.
func (c *policyChecker) profileID(ctx context.Context, s *vsphere.Session, key profileKey) (string, error) {
	if profile, ok := c.profiles[key]; ok {
		return profile, nil
	}
	profile, err := vsphere.GetProfileIDByName(ctx, s, key.policy)
	if err != nil {
		return "", err
	}
	c.profiles[key] = profile
	return profile, nil
}
