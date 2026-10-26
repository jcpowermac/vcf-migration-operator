package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	"github.com/vmware/govmomi/find"
	_ "github.com/vmware/govmomi/pbm/simulator"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/storageplan"
)

func planningFixture(t interface {
	Helper()
	Fatal(...any)
}) (*VmwareCloudFoundationMigrationReconciler, *migrationv1alpha1.VmwareCloudFoundationMigration) {
	t.Helper()
	m := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       migrationv1alpha1.SingletonName,
			Namespace:  migrationv1alpha1.SingletonNamespace,
			Finalizers: []string{migrationv1alpha1.FinalizerName},
			UID:        "12345678-migration",
			Generation: 1,
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: migrationv1alpha1.MigrationStatePlanning,
			TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
				Name: "target-creds",
			},
			MigrateStorage: true,
			Storage:        &migrationv1alpha1.StorageMigrationSpec{},
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "target",
					Region: "new-region",
					Zone:   "new-zone",
					Server: "target.example.com",
					Topology: configv1.VSpherePlatformTopology{
						Datacenter:     "dc",
						ComputeCluster: "/dc/host/cluster",
						Datastore:      "/dc/datastore/ds",
						Networks: []string{
							"network",
						},
						ResourcePool: "/dc/host/cluster/Resources",
						Template:     "/dc/vm/template",
					},
				},
			},
		},
	}
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &VmwareCloudFoundationMigrationReconciler{Scheme: scheme, Recorder: events.NewFakeRecorder(100)}
	r.setCondition(m, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "validated")
	r.Client = clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(m).WithStatusSubresource(m, &migrationv1alpha1.StorageWorkloadMigration{}).Build()
	r.KubeClient = fakekube.NewClientset(
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pv",
				UID:  "pv-uid",
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       vsphereCSIDriverName,
						VolumeHandle: "disk",
					},
				},
				ClaimRef: &corev1.ObjectReference{
					Namespace: "app",
					Name:      "data",
					UID:       "pvc-uid",
				},
			},
			Status: corev1.PersistentVolumeStatus{
				Phase: corev1.VolumeBound,
			},
		},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app", UID: "pvc-uid"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv", StorageClassName: ptr.To("thin-csi")}},
		&storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: "thin-csi",
			},
			VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
			Parameters: map[string]string{
				"storagepolicyname": "openshift-storage-policy-infra",
			},
		},
	)
	r.ConfigClient = configfake.NewClientset(&configv1.Infrastructure{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.InfrastructureSpec{PlatformSpec: configv1.PlatformSpec{VSphere: &configv1.VSpherePlatformSpec{FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{Name: "source", Region: "old-region", Zone: "old-zone"}}}}}, Status: configv1.InfrastructureStatus{InfrastructureName: "infra"}})
	r.DynamicClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "operator.openshift.io/v1", "kind": "ClusterCSIDriver", "metadata": map[string]interface{}{"name": vsphereCSIDriverName}, "spec": map[string]interface{}{"managementState": "Managed"}}})
	return r, m
}

func reconcilePlan(t *testing.T, r *VmwareCloudFoundationMigrationReconciler, m *migrationv1alpha1.VmwareCloudFoundationMigration) *migrationv1alpha1.VmwareCloudFoundationMigration {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(m), got); err != nil {
		t.Fatal(err)
	}
	return got
}

func requirePlanCondition(t *testing.T, m *migrationv1alpha1.VmwareCloudFoundationMigration, kind, reason string, status metav1.ConditionStatus) {
	t.Helper()
	c := apimeta.FindStatusCondition(m.Status.Conditions, kind)
	if c == nil || c.Status != status || c.Reason != reason {
		t.Fatalf("condition %s: %+v; want %s %s", kind, c, status, reason)
	}
}

func TestPlanningBoundaryAndRefresh(t *testing.T) {
	r, m := planningFixture(t)
	got := reconcilePlan(t, r, m)
	requirePlanCondition(t, got, migrationv1alpha1.ConditionReady, migrationv1alpha1.ReasonPlanComplete, metav1.ConditionFalse)
	if got.Status.StartTime != nil || apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionDestinationInitialized) != nil {
		t.Fatal("Planning crossed disruptive boundary")
	}
	var list migrationv1alpha1.StorageWorkloadMigrationList
	if err := r.List(context.Background(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("SWM inventory: %+v %v", list, err)
	}
	if list.Items[0].Name != "vcf-storage-12345678" || len(list.Items[0].OwnerReferences) != 0 || list.Items[0].Status.Volumes[0].TargetFailureDomain != "target" {
		t.Fatalf("SWM: %+v", list.Items[0])
	}
	unchanged := reconcilePlan(t, r, m)
	if unchanged.ResourceVersion != got.ResourceVersion {
		t.Fatal("unchanged plan caused a status update")
	}
	// PVC metadata changes do not increment the migration generation.
	pvc, err := r.KubeClient.CoreV1().PersistentVolumeClaims("app").Get(context.Background(), "data", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pvc.Annotations = map[string]string{storageplan.TargetFailureDomainAnnotation: "missing"}
	if _, err := r.KubeClient.CoreV1().PersistentVolumeClaims("app").Update(context.Background(), pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	got = reconcilePlan(t, r, m)
	requirePlanCondition(t, got, migrationv1alpha1.ConditionStoragePlanned, migrationv1alpha1.ReasonPlanBlocked, metav1.ConditionFalse)
	requirePlanCondition(t, got, migrationv1alpha1.ConditionReady, migrationv1alpha1.ReasonPlanBlocked, metav1.ConditionFalse)
	pvc.Annotations[storageplan.TargetFailureDomainAnnotation] = "target"
	if _, err := r.KubeClient.CoreV1().PersistentVolumeClaims("app").Update(context.Background(), pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	got = reconcilePlan(t, r, m)
	requirePlanCondition(t, got, migrationv1alpha1.ConditionReady, migrationv1alpha1.ReasonPlanComplete, metav1.ConditionFalse)
}

func TestPlanningStopsWhenPermissionChanges(t *testing.T) {
	for _, change := range []string{"deletion", "paused", "generation"} {
		t.Run(change, func(t *testing.T) {
			r, m := planningFixture(t)
			live := retentionTestClient(t, m.DeepCopy())
			r.APIReader = live
			r.KubeClient.(*fakekube.Clientset).PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
				latest := m.DeepCopy()
				if err := live.Get(context.Background(), client.ObjectKeyFromObject(m), latest); err != nil {
					return true, nil, err
				}
				if change == "deletion" {
					return false, nil, live.Delete(context.Background(), latest)
				}
				if change == "paused" {
					latest.Spec.State = migrationv1alpha1.MigrationStatePaused
				} else {
					latest.Generation++
				}
				return false, nil, live.Update(context.Background(), latest)
			})
			got := reconcilePlan(t, r, m)
			var plans migrationv1alpha1.StorageWorkloadMigrationList
			if err := r.List(context.Background(), &plans); err != nil {
				t.Fatal(err)
			}
			if len(plans.Items) != 0 || got.Status.StartTime != nil || apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned) != nil {
				t.Fatalf("planning continued after permission changed: plans=%d status=%+v", len(plans.Items), got.Status)
			}
		})
	}
}

func TestPlanningStopsAfterInventoryCreation(t *testing.T) {
	r, m := planningFixture(t)
	live := retentionTestClient(t, m.DeepCopy())
	r.APIReader = live
	underlying := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(underlying, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			return live.Delete(ctx, m.DeepCopy())
		},
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return fmt.Errorf("writing status after deletion")
		},
	})
	reconcilePlan(t, r, m)
	var plans migrationv1alpha1.StorageWorkloadMigrationList
	if err := r.List(context.Background(), &plans); err != nil {
		t.Fatal(err)
	}
	if len(plans.Items) != 1 || len(plans.Items[0].Status.Volumes) != 0 {
		t.Fatalf("unexpected inventory after deletion: %+v", plans.Items)
	}
}

func TestRunningBlockedPlan(t *testing.T) {
	r, m := planningFixture(t)
	m.Spec.State = migrationv1alpha1.MigrationStateRunning
	if err := r.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	pv, err := r.KubeClient.CoreV1().PersistentVolumes().Get(context.Background(), "pv", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pv.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
	if _, err := r.KubeClient.CoreV1().PersistentVolumes().Update(context.Background(), pv, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	got := reconcilePlan(t, r, m)
	requirePlanCondition(t, got, migrationv1alpha1.ConditionStoragePlanned, migrationv1alpha1.ReasonPlanBlocked, metav1.ConditionFalse)
	if got.Status.StartTime != nil {
		t.Fatal("blocked Running migration started")
	}
	unchanged := reconcilePlan(t, r, m)
	if unchanged.ResourceVersion != got.ResourceVersion {
		t.Fatal("unchanged blocked plan caused a status update")
	}
}

func TestPlanningAfterStart(t *testing.T) {
	for _, state := range []migrationv1alpha1.MigrationState{migrationv1alpha1.MigrationStatePlanning, migrationv1alpha1.MigrationStatePending} {
		t.Run(string(state), func(t *testing.T) {
			r, m := planningFixture(t)
			m.Spec.State = state
			if err := r.Update(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			now := metav1.Now()
			m.Status.StartTime = &now
			if err := r.Status().Update(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			got := reconcilePlan(t, r, m)
			requirePlanCondition(t, got, migrationv1alpha1.ConditionReady, migrationv1alpha1.ReasonPlanningAfterStart, metav1.ConditionFalse)
			if apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned) != nil {
				t.Fatal("planning ran after migration start")
			}
		})
	}
}

func TestPlanningGenerationFreshness(t *testing.T) {
	r, m := planningFixture(t)
	got := reconcilePlan(t, r, m)
	got.Spec.State = migrationv1alpha1.MigrationStateRunning
	got.Generation++
	if err := r.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	// The changed generation must rerun preflight, which fails without target credentials.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	if err == nil || !strings.Contains(err.Error(), "target credentials secret") {
		t.Fatalf("Reconcile did not rerun preflight: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.StartTime != nil {
		t.Fatal("failed preflight started migration")
	}
	c := apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if c == nil || c.Status != metav1.ConditionFalse || c.ObservedGeneration != got.Generation {
		t.Fatalf("preflight downgrade: %+v", c)
	}
}

func TestStorageManagementForPlan(t *testing.T) {
	for _, state := range []string{"Managed", "Removed", "Unmanaged"} {
		t.Run(state, func(t *testing.T) {
			r, _ := planningFixture(t)
			obj, err := r.DynamicClient.Resource(clusterCSIDriverGVR).Get(context.Background(), vsphereCSIDriverName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedField(obj.Object, state, "spec", "managementState"); err != nil {
				t.Fatal(err)
			}
			if _, err := r.DynamicClient.Resource(clusterCSIDriverGVR).Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err = checkStorageManagementForPlan(context.Background(), r.DynamicClient, true)
			if (err == nil) != (state == "Managed") {
				t.Fatalf("checkStorageManagementForPlan(%s): %v", state, err)
			}
		})
	}
}

func TestStaleStoragePlans(t *testing.T) {
	r, m := planningFixture(t)
	reconcilePlan(t, r, m)
	swm := &migrationv1alpha1.StorageWorkloadMigration{}
	key := client.ObjectKey{Namespace: "app", Name: storagePlanName(m)}
	if err := r.Get(context.Background(), key, swm); err != nil {
		t.Fatal(err)
	}
	// Simulate an execution phase that is not yet part of the API.
	swm.Status.Phase = migrationv1alpha1.StorageWorkloadMigrationPhase("PreparingTargets")
	if err := r.Status().Update(context.Background(), swm); err != nil {
		t.Fatal(err)
	}
	if err := r.deleteStaleStoragePlans(context.Background(), m, &storageplan.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), key, swm); err != nil {
		t.Fatalf("advanced SWM deleted: %v", err)
	}
	swm.Status.Phase = migrationv1alpha1.StorageWorkloadMigrationPhasePlanning
	if err := r.Status().Update(context.Background(), swm); err != nil {
		t.Fatal(err)
	}
	if err := r.deleteStaleStoragePlans(context.Background(), m, &storageplan.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), key, swm); err == nil {
		t.Fatal("stale Planning SWM survived")
	}
}

func TestRunningPersistsStartBeforeDisruption(t *testing.T) {
	r, m := planningFixture(t)
	m.Spec.State = migrationv1alpha1.MigrationStateRunning
	if err := r.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	// Destination initialization fails on the incomplete compute fixture, after planning passes.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	if err == nil {
		t.Fatal("expected destination initialization to fail")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(m), m); err != nil {
		t.Fatal(err)
	}
	if m.Status.StartTime == nil {
		t.Fatal("disruptive handler ran without persisting StartTime")
	}
	requirePlanCondition(t, m, migrationv1alpha1.ConditionStoragePlanned, migrationv1alpha1.ReasonCompleted, metav1.ConditionTrue)
}

func TestStatusRejectsStalePlanDowngrade(t *testing.T) {
	r, m := planningFixture(t)
	m = reconcilePlan(t, r, m)
	base := *m.Status.DeepCopy()
	stale := m.DeepCopy()
	// A concurrent writer changes the committed plan after this reconcile read it.
	r.setCondition(m, migrationv1alpha1.ConditionStoragePlanned, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "newer inventory")
	if err := r.Status().Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	r.setCondition(stale, migrationv1alpha1.ConditionStoragePlanned, metav1.ConditionFalse, migrationv1alpha1.ReasonPlanBlocked, "stale blocker")
	if err := r.updateStatus(context.Background(), stale, base); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(m), m); err != nil {
		t.Fatal(err)
	}
	requirePlanCondition(t, m, migrationv1alpha1.ConditionStoragePlanned, migrationv1alpha1.ReasonCompleted, metav1.ConditionTrue)
}

func TestStatusRejectsStartingWithChangedSpec(t *testing.T) {
	r, m := planningFixture(t)
	m = reconcilePlan(t, r, m)
	base := *m.Status.DeepCopy()
	stale := m.DeepCopy()
	stale.Spec.State = migrationv1alpha1.MigrationStateRunning
	m.Spec.State = migrationv1alpha1.MigrationStatePaused
	m.Generation++
	if err := r.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	stale.Status.StartTime = &now
	if err := r.updateStatus(context.Background(), stale, base); err == nil {
		t.Fatal("stale reconcile persisted StartTime")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(m), m); err != nil {
		t.Fatal(err)
	}
	if m.Status.StartTime != nil {
		t.Fatal("stale reconcile started migration")
	}
}

func TestCustomerStoragePolicyPlanning(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		r, m := planningFixture(t)
		finder := find.NewFinder(c, true)
		dc, err := finder.DefaultDatacenter(ctx)
		if err != nil {
			t.Fatal(err)
		}
		finder.SetDatacenter(dc)
		ds, err := finder.DefaultDatastore(ctx)
		if err != nil {
			t.Fatal(err)
		}
		fd := &m.Spec.FailureDomains[0]
		fd.Server = c.URL().Host
		fd.Topology.Datacenter = dc.Name()
		fd.Topology.Datastore = ds.InventoryPath
		password, _ := simulator.DefaultLogin.Password()
		_, err = r.KubeClient.CoreV1().Secrets(m.Namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: m.Spec.TargetVCenterCredentialsSecret.Name}, Data: map[string][]byte{fd.Server + ".username": []byte(simulator.DefaultLogin.Username()), fd.Server + ".password": []byte(password)}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		sc, err := r.KubeClient.StorageV1().StorageClasses().Get(ctx, "thin-csi", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, tt := range []struct {
			policy  string
			blocked bool
		}{{"vSAN Default Storage Policy", false}, {"missing-policy", true}} {
			sc.Parameters["storagepolicyname"] = tt.policy
			if _, err := r.KubeClient.StorageV1().StorageClasses().Update(ctx, sc, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err := r.ensureStoragePlanned(ctx, m)
			if err != nil {
				t.Fatalf("ensureStoragePlanned: %v", err)
			}
			condition := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned)
			if (condition.Status == metav1.ConditionFalse) != tt.blocked {
				t.Fatalf("policy %s: %+v", tt.policy, condition)
			}
		}
	})
}
