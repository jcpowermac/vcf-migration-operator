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

func (r *VmwareCloudFoundationMigrationReconciler) ensureStoragePlanned(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {

	var previous *metav1.Condition
	if condition := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned); condition != nil {
		previous = condition.DeepCopy()
	}

	defer func() {
		condition := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned)
		if r.Recorder == nil || condition == nil || previous != nil && !statusConditionChanged(*previous, *condition) {
			return
		}
		eventType := "Normal"
		if condition.Status == metav1.ConditionFalse {
			eventType = "Warning"
		}
		r.Recorder.Eventf(m, nil, eventType, condition.Reason, migrationv1alpha1.ConditionStoragePlanned, "%s", truncateToBytes(condition.Message, maxEventNoteBytes))
	}()

	if !m.Spec.MigrateStorage {
		r.setCondition(m, migrationv1alpha1.ConditionStoragePlanned, metav1.ConditionTrue, migrationv1alpha1.ReasonNotConfigured, "Storage migration is not enabled")
		return ctrl.Result{}, nil
	}

	pvs, err := r.KubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing PV inventory: %w", err)
	}
	pvcs, err := r.KubeClient.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing PVC inventory: %w", err)
	}
	pods, err := r.KubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing storage consumers: %w", err)
	}
	classes, err := r.KubeClient.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing StorageClasses: %w", err)
	}
	infra, err := r.ConfigClient.ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting infrastructure for storage planning: %w", err)
	}
	csi, err := r.DynamicClient.Resource(clusterCSIDriverGVR).Get(ctx, vsphereCSIDriverName, metav1.GetOptions{})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting CSI topology categories: %w", err)
	}

	categories, _, err := unstructured.NestedStringSlice(csi.Object, "spec", "driverConfig", "vSphere", "topologyCategories")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading CSI topology categories: %w", err)
	}

	plan := storageplan.Build(storageplan.Input{Migration: m, Infrastructure: infra, PVs: pvs.Items, PVCs: pvcs.Items, Pods: pods.Items, StorageClasses: classes.Items, TopologyCategories: categories})

	if err := r.checkPlanPolicies(ctx, m, infra, plan, classes.Items, pvcs.Items); err != nil {
		return ctrl.Result{}, err
	}

	namespaces := make([]string, 0, len(plan.Namespaces))
	for ns := range plan.Namespaces {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	total, blocked, blockedNamespaces := 0, 0, 0
	for _, namespace := range namespaces {
		ns := plan.Namespaces[namespace]
		ns.Consumers, err = storageplan.Consumers(ctx, pods.Items, namespace, ns.Volumes, r.lookupStorageOwner)
		if err != nil {
			return ctrl.Result{}, err
		}
		count := 0
		for _, v := range ns.Volumes {
			if len(v.Blockers) > 0 {
				count++
			}
		}
		total += len(ns.Volumes)
		blocked += count
		if count > 0 {
			blockedNamespaces++
		}
		if err := r.writeStoragePlan(ctx, m, namespace, ns, count); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.deleteStaleStoragePlans(ctx, m, plan); err != nil {
		return ctrl.Result{}, err
	}
	if blocked > 0 || len(plan.Blockers) > 0 {
		parts := []string{fmt.Sprintf("%d volumes blocked in %d namespaces", blocked, blockedNamespaces)}
		for _, b := range plan.Blockers {
			parts = append(parts, b.Reason+": "+b.Message)
		}
		parts = append(parts, "see StorageWorkloadMigration status")
		message := boundConditionMessage(strings.Join(parts, "; "))
		r.setCondition(m, migrationv1alpha1.ConditionStoragePlanned, metav1.ConditionFalse, migrationv1alpha1.ReasonPlanBlocked, message)
		r.setCondition(m, migrationv1alpha1.ConditionReady, metav1.ConditionFalse, migrationv1alpha1.ReasonPlanBlocked, message)
		return ctrl.Result{RequeueAfter: planningResync}, nil
	}
	r.setCondition(m, migrationv1alpha1.ConditionStoragePlanned, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, fmt.Sprintf("Planned %d volumes in %d namespaces", total, len(namespaces)))
	return ctrl.Result{}, nil
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

func (r *VmwareCloudFoundationMigrationReconciler) writeStoragePlan(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration, namespace string, plan *storageplan.Namespace, blocked int) error {
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
	sessions := map[string]*vsphere.Session{}
	defer func() {
		logoutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, s := range sessions {
			s.Close(logoutCtx)
		}
	}()
	results := map[string]bool{}
	profiles := map[string]string{}
	classMap := map[string]*storagev1.StorageClass{}
	for i := range classes {
		classMap[classes[i].Name] = &classes[i]
	}
	claims := map[string]*corev1.PersistentVolumeClaim{}
	for i := range pvcs {
		claims[pvcs[i].Namespace+"/"+pvcs[i].Name] = &pvcs[i]
	}
	for namespace, ns := range plan.Namespaces {
		for i := range ns.Volumes {
			v := &ns.Volumes[i]
			pvc := claims[namespace+"/"+v.SourcePVC.Name]
			if pvc.Spec.StorageClassName == nil {
				continue
			}
			sc := classMap[*pvc.Spec.StorageClassName]
			if sc == nil {
				continue
			}
			policy, _ := storageplan.Parameter(sc, "storagepolicyname")
			if policy == "" || v.TargetFailureDomain == "" {
				continue
			}
			if infra.Status.InfrastructureName != "" && policy == "openshift-storage-policy-"+infra.Status.InfrastructureName {
				continue
			}
			var fd configv1.VSpherePlatformFailureDomainSpec
			for _, target := range m.Spec.FailureDomains {
				if target.Name == v.TargetFailureDomain {
					fd = target
					break
				}
			}
			key := fd.Server + "\x00" + fd.Topology.Datacenter
			resultKey := key + "\x00" + policy + "\x00" + fd.Topology.Datastore
			compatible, found := results[resultKey]
			if !found {
				s := sessions[key]
				if s == nil {
					username, password, err := getTargetCredentials(policyCtx, r.KubeClient, m, fd.Server)
					if err != nil {
						return err
					}
					s, err = vsphere.NewSession(policyCtx, vsphere.Params{Server: fd.Server, Datacenter: fd.Topology.Datacenter, Username: username, Password: password, Insecure: true})
					if err != nil {
						return fmt.Errorf("connecting to target for storage policy checks: %w", err)
					}
					sessions[key] = s
				}
				profileKey := fd.Server + "\x00" + policy
				profile, ok := profiles[profileKey]
				if !ok {
					var err error
					profile, err = vsphere.GetProfileIDByName(policyCtx, s, policy)
					if err != nil {
						return err
					}
					profiles[profileKey] = profile
				}
				if profile != "" {
					var err error
					compatible, err = vsphere.IsDatastoreCompatible(policyCtx, s, profile, fd.Topology.Datastore)
					if err != nil {
						return err
					}
				}
				results[resultKey] = compatible
			}
			if !compatible {
				storageplan.AddBlocker(v, "StoragePolicyMissing", fmt.Sprintf("policy %s is missing or incompatible with target datastore %s on %s", policy, fd.Topology.Datastore, fd.Server))
			}
		}
	}
	return nil
}
