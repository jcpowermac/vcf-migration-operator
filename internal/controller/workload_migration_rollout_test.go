package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
)

func TestEnsureWorkloadMigratedRolloutAndScaleDown(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		objects     []runtime.Object
		wantErr     string
		wantRequeue time.Duration
		setup       func(t *testing.T, machineClient *fakemachineclient.Clientset, migration *migrationv1alpha1.VmwareCloudFoundationMigration)
		assertions  func(t *testing.T, resultReconciler *VmwareCloudFoundationMigrationReconciler, resultMigration *migrationv1alpha1.VmwareCloudFoundationMigration)
	}{
		{
			name: "requeues while CPMS generation is not observed",
			objects: []runtime.Object{
				newInfrastructureForRollout("source.example.com"),
				newCPMSForRollout(false, true),
			},
			wantRequeue: 15 * time.Second,
			assertions: func(t *testing.T, _ *VmwareCloudFoundationMigrationReconciler, resultMigration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				cond := findWorkloadCondition(t, resultMigration)
				if cond.Status != metav1.ConditionFalse {
					t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
				}
				if !strings.Contains(cond.Message, "generation observed") {
					t.Fatalf("workload condition message = %q, want substring %q", cond.Message, "generation observed")
				}
			},
		},
		{
			name: "scales source machinesets down to zero and requeues",
			objects: []runtime.Object{
				newInfrastructureForRollout("source.example.com"),
				newCPMSForRollout(true, true),
				newSourceMachineSetForRollout("source-worker-a", "source.example.com", 2),
			},
			wantRequeue: 30 * time.Second,
			assertions: func(t *testing.T, resultReconciler *VmwareCloudFoundationMigrationReconciler, resultMigration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				ms, err := resultReconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, "source-worker-a", metav1.GetOptions{})
				if err != nil {
					t.Fatalf("getting scaled machineset: %v", err)
				}
				if ms.Spec.Replicas == nil || *ms.Spec.Replicas != 0 {
					t.Fatalf("scaled machineset replicas = %v, want 0", ms.Spec.Replicas)
				}

				cond := findWorkloadCondition(t, resultMigration)
				if cond.Status != metav1.ConditionFalse {
					t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
				}
				if !strings.Contains(cond.Message, "Old workers scaled down") {
					t.Fatalf("workload condition message = %q, want substring %q", cond.Message, "Old workers scaled down")
				}
			},
		},
		{
			name: "completes workload migration and deletes zero-replica source machinesets",
			objects: []runtime.Object{
				newInfrastructureForRollout("source.example.com"),
				newCPMSForRollout(true, true),
				newSourceMachineSetForRollout("source-worker-a", "source.example.com", 0),
			},
			setup: func(t *testing.T, machineClient *fakemachineclient.Clientset, migration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				machineClient.PrependReactor("delete", "machinesets", func(action k8stesting.Action) (bool, runtime.Object, error) {
					deleteAction, ok := action.(k8stesting.DeleteAction)
					if !ok || deleteAction.GetName() != "source-worker-a" {
						return false, nil, nil
					}

					for _, cond := range migration.Status.Conditions {
						if cond.Type == migrationv1alpha1.ConditionWorkloadMigrated && cond.Status == metav1.ConditionTrue {
							t.Fatalf("condition %q became true before deleting source-worker-a", migrationv1alpha1.ConditionWorkloadMigrated)
						}
					}

					return false, nil, nil
				})
			},
			assertions: func(t *testing.T, resultReconciler *VmwareCloudFoundationMigrationReconciler, resultMigration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				_, err := resultReconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, "source-worker-a", metav1.GetOptions{})
				if !apierrors.IsNotFound(err) {
					t.Fatalf("expected source-worker-a to be deleted, got err %v", err)
				}

				cond := findWorkloadCondition(t, resultMigration)
				if cond.Status != metav1.ConditionTrue {
					t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionTrue)
				}
				if cond.Reason != migrationv1alpha1.ReasonCompleted {
					t.Fatalf("workload condition reason = %q, want %q", cond.Reason, migrationv1alpha1.ReasonCompleted)
				}
				if !strings.Contains(cond.Message, "Workload migrated") {
					t.Fatalf("workload condition message = %q, want substring %q", cond.Message, "Workload migrated")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configClientObjects := []runtime.Object{}
			machineClientObjects := []runtime.Object{}
			for _, obj := range tt.objects {
				switch o := obj.(type) {
				case *configv1.Infrastructure:
					configClientObjects = append(configClientObjects, o)
				case *machinev1.ControlPlaneMachineSet:
					machineClientObjects = append(machineClientObjects, o)
				case *machinev1beta1.MachineSet:
					machineClientObjects = append(machineClientObjects, o)
				default:
					t.Fatalf("unsupported test object type %T", obj)
				}
			}

			reconciler := &VmwareCloudFoundationMigrationReconciler{
				KubeClient:    fakekube.NewClientset(&corev1.NodeList{}),
				ConfigClient:  configfake.NewClientset(configClientObjects...),
				MachineClient: fakemachineclient.NewClientset(machineClientObjects...),
				Recorder:      record.NewFakeRecorder(10),
			}
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:       migrationv1alpha1.SingletonName,
					Generation: 1,
				},
			}
			if tt.setup != nil {
				tt.setup(t, reconciler.MachineClient.(*fakemachineclient.Clientset), migration)
			}

			result, err := reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown succeeded, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown: %v", err)
			}
			if result.RequeueAfter != tt.wantRequeue {
				t.Fatalf("result requeueAfter = %s, want %s", result.RequeueAfter, tt.wantRequeue)
			}

			tt.assertions(t, reconciler, migration)
		})
	}
}

func findWorkloadCondition(t *testing.T, migration *migrationv1alpha1.VmwareCloudFoundationMigration) metav1.Condition {
	t.Helper()
	for _, cond := range migration.Status.Conditions {
		if cond.Type == migrationv1alpha1.ConditionWorkloadMigrated {
			return cond
		}
	}
	t.Fatalf("condition %q not found", migrationv1alpha1.ConditionWorkloadMigrated)
	return metav1.Condition{}
}

func newInfrastructureForRollout(sourceVCenter string) *configv1.Infrastructure {
	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
		Spec: configv1.InfrastructureSpec{
			PlatformSpec: configv1.PlatformSpec{
				Type: configv1.VSpherePlatformType,
				VSphere: &configv1.VSpherePlatformSpec{
					VCenters: []configv1.VSpherePlatformVCenterSpec{
						{Server: sourceVCenter},
					},
				},
			},
		},
	}
}

func newCPMSForRollout(observed, complete bool) *machinev1.ControlPlaneMachineSet {
	replicas := int32(3)
	updated := replicas
	ready := replicas
	if !complete {
		updated = 1
		ready = 1
	}

	cpms := &machinev1.ControlPlaneMachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster",
			Namespace:  openshift.MachineAPINamespace,
			Generation: 2,
		},
		Spec: machinev1.ControlPlaneMachineSetSpec{
			Replicas: &replicas,
			Template: machinev1.ControlPlaneMachineSetTemplate{
				MachineType: machinev1.OpenShiftMachineV1Beta1MachineType,
				OpenShiftMachineV1Beta1Machine: &machinev1.OpenShiftMachineV1Beta1MachineTemplate{
					Spec: machinev1beta1.MachineSpec{
						ProviderSpec: machinev1beta1.ProviderSpec{
							Value: &runtime.RawExtension{Raw: []byte(`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1"}`)},
						},
					},
				},
			},
		},
		Status: machinev1.ControlPlaneMachineSetStatus{
			Replicas:        replicas,
			UpdatedReplicas: updated,
			ReadyReplicas:   ready,
		},
	}
	if observed {
		cpms.Status.ObservedGeneration = cpms.Generation
	} else {
		cpms.Status.ObservedGeneration = cpms.Generation - 1
	}

	return cpms
}

func newSourceMachineSetForRollout(name, server string, replicas int32) *machinev1beta1.MachineSet {
	return &machinev1beta1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: openshift.MachineAPINamespace,
		},
		Spec: machinev1beta1.MachineSetSpec{
			Replicas: &replicas,
			Template: machinev1beta1.MachineTemplateSpec{
				Spec: machinev1beta1.MachineSpec{
					ProviderSpec: machinev1beta1.ProviderSpec{
						Value: &runtime.RawExtension{
							Raw: []byte(fmt.Sprintf(`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1","workspace":{"server":%q}}`, server)),
						},
					},
				},
			},
		},
	}
}
