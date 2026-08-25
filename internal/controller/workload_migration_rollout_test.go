package controller

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/stdr"
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
	"k8s.io/klog/v2"
)

// machinePhaseRunning is the machinev1beta1 Machine Phase value for a running
// machine; the upstream API package does not export a constant for it.
const machinePhaseRunning = "Running"

func TestEnsureWorkloadMigratedRolloutAndScaleDown(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	running := machinePhaseRunning
	drainErr := machinev1beta1.MachineStatusError("DrainError")
	drainMsg := "unable to drain node: pod disruption budget exceeded"

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
				newInfrastructureForRollout(),
				newCPMSForRollout(false, true),
			},
			wantRequeue: 15 * time.Second,
			assertions: func(t *testing.T, _ *VmwareCloudFoundationMigrationReconciler, resultMigration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				cond := findWorkloadCondition(t, resultMigration)
				if cond.Status != metav1.ConditionFalse {
					t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
				}
				wantMsg := "Waiting for control plane rollout to start (CPMS generation 2/1 observed)"
				if cond.Message != wantMsg {
					t.Fatalf("workload condition message = %q, want %q", cond.Message, wantMsg)
				}
			},
		},
		{
			name: "reports progress while control plane is rolling out",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newCPMSForRollout(true, false),
			},
			wantRequeue: 30 * time.Second,
			assertions: func(t *testing.T, resultReconciler *VmwareCloudFoundationMigrationReconciler, resultMigration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				cond := findWorkloadCondition(t, resultMigration)
				if cond.Status != metav1.ConditionFalse {
					t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
				}
				wantMsg := "Control plane rolling out (1/3 updated, 1/3 ready)"
				if cond.Message != wantMsg {
					t.Fatalf("workload condition message = %q, want %q", cond.Message, wantMsg)
				}
				recorder := resultReconciler.Recorder.(*record.FakeRecorder)
				event := waitForLastEvent(t, recorder)
				if !strings.Contains(event, "control plane rolling out (1/3 updated, 1/3 ready)") {
					t.Fatalf("last event = %q, want rollout progress substring", event)
				}
			},
		},
		{
			name: "scales source machinesets down to zero and requeues",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newCPMSForRollout(true, true),
				newSourceMachineSetForRollout(2),
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
				newInfrastructureForRollout(),
				newCPMSForRollout(true, true),
				newSourceMachineSetForRollout(0),
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
		{
			name: "reports old worker deletion stall detail with machine names and error reasons",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newCPMSForRollout(true, true),
				newSourceMachineSetForRollout(0),
				&machinev1beta1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "source-worker-a-0",
						Namespace:         openshift.MachineAPINamespace,
						CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
						Labels: map[string]string{
							"machine.openshift.io/cluster-api-machineset": "source-worker-a",
						},
					},
					Status: machinev1beta1.MachineStatus{
						Phase:        &running,
						NodeRef:      &corev1.ObjectReference{Name: "source-node-a"},
						ErrorReason:  &drainErr,
						ErrorMessage: &drainMsg,
					},
				},
				newReadyNodeForRollout("source-node-a"),
			},
			wantRequeue: 30 * time.Second,
			assertions: func(t *testing.T, _ *VmwareCloudFoundationMigrationReconciler, resultMigration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				cond := findWorkloadCondition(t, resultMigration)
				if cond.Status != metav1.ConditionFalse {
					t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
				}
				// Assert on stable substrings; the machine age is wall-clock derived
				// and would make an exact match flaky.
				for _, want := range []string{
					"Waiting for old worker deletion: source-worker-a has 1 machine(s) [source-worker-a-0 (",
					" old, DrainError)] and 1 node(s) remaining",
				} {
					if !strings.Contains(cond.Message, want) {
						t.Fatalf("workload condition message %q missing substring %q", cond.Message, want)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeObjects, configClientObjects, machineClientObjects := splitRolloutTestObjects(t, tt.objects)

			reconciler := &VmwareCloudFoundationMigrationReconciler{
				KubeClient:    fakekube.NewClientset(kubeObjects...),
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

func TestEnsureWorkloadMigratedRolloutGate(t *testing.T) {
	ctx := context.Background()
	targetMSName := workerMachineSetName("test-infra", "target-fd-1")
	provisioning := "Provisioning"

	tests := []struct {
		name        string
		objects     []runtime.Object
		wantRequeue time.Duration
		wantMessage string
		assertions  func(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler)
	}{
		{
			name: "routes to rollout path when CPMS targets failure domains and workers are ready",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
				newReadyWorkerMachineForRollout(targetMSName, "worker-node-1"),
				newReadyNodeForRollout("worker-node-1"),
				newCPMSUpdatedForRollout([]string{"target-fd-1"}, false),
				newSourceMachineSetForRollout(1),
			},
			wantRequeue: 15 * time.Second,
			wantMessage: "Waiting for control plane rollout to start (CPMS generation 2/1 observed)",
		},
		{
			name: "stays in worker phase when CPMS not updated",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
				newCPMSUpdatedForRollout([]string{"source-fd-1"}, false),
				newSourceMachineSetForRollout(1),
			},
			wantRequeue: 30 * time.Second,
			wantMessage: "Waiting for target workers (machines 0/0 ready, nodes 0/0 ready)",
		},
		{
			name: "reports worker readiness counts while target workers are pending",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(targetMSName, "target.example.com", 2),
				newReadyWorkerMachineForRollout(targetMSName, "worker-node-1"),
				newReadyNodeForRollout("worker-node-1"),
				&machinev1beta1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      targetMSName + "-pending",
						Namespace: openshift.MachineAPINamespace,
						Labels: map[string]string{
							"machine.openshift.io/cluster-api-machineset": targetMSName,
						},
					},
					Status: machinev1beta1.MachineStatus{Phase: &provisioning},
				},
				newCPMSUpdatedForRollout([]string{"target-fd-1"}, false),
				newSourceMachineSetForRollout(1),
			},
			wantRequeue: 30 * time.Second,
			wantMessage: "Waiting for target workers (machines 1/2 ready, nodes 1/2 ready)",
		},
		{
			name: "stays in worker phase when target machinesets missing",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newCPMSUpdatedForRollout([]string{"target-fd-1"}, false),
				newSourceMachineSetForRollout(2),
			},
			wantRequeue: 30 * time.Second,
			wantMessage: "Workers created, waiting for machines ready",
			assertions: func(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler) {
				t.Helper()
				if _, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, targetMSName, metav1.GetOptions{}); err != nil {
					t.Fatalf("getting created target machineset %q: %v", targetMSName, err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeObjects, configClientObjects, machineClientObjects := splitRolloutTestObjects(t, tt.objects)

			reconciler := &VmwareCloudFoundationMigrationReconciler{
				KubeClient:    fakekube.NewClientset(kubeObjects...),
				ConfigClient:  configfake.NewClientset(configClientObjects...),
				MachineClient: fakemachineclient.NewClientset(machineClientObjects...),
				Recorder:      record.NewFakeRecorder(20),
			}
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
				ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 1},
				Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
					FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
						Name:   "target-fd-1",
						Server: "target.example.com",
						Topology: configv1.VSpherePlatformTopology{
							Template:       "/dc1/vm/target-template",
							Datacenter:     "dc1",
							Datastore:      "ds1",
							ResourcePool:   "rp1",
							ComputeCluster: "cl1",
						},
					}},
				},
			}

			result, err := reconciler.ensureWorkloadMigrated(ctx, migration)
			if err != nil {
				t.Fatalf("ensureWorkloadMigrated: %v", err)
			}
			if result.RequeueAfter != tt.wantRequeue {
				t.Fatalf("result requeueAfter = %s, want %s", result.RequeueAfter, tt.wantRequeue)
			}

			cond := findWorkloadCondition(t, migration)
			if cond.Message != tt.wantMessage {
				t.Fatalf("workload condition message = %q, want %q", cond.Message, tt.wantMessage)
			}
			if tt.assertions != nil {
				tt.assertions(t, reconciler)
			}
		})
	}
}

func TestRolloutLogsMachineLevelDetail(t *testing.T) {
	ctx := context.Background()

	var buf bytes.Buffer
	oldVerbosity := stdr.SetVerbosity(1)
	defer stdr.SetVerbosity(oldVerbosity)
	logger := stdr.NewWithOptions(log.New(&buf, "", 0), stdr.Options{})
	klog.SetLoggerWithOptions(logger, klog.ContextualLogger(true))
	defer klog.ClearLogger()

	now := time.Now()
	running := machinePhaseRunning
	provisioning := "Provisioning"
	createErr := machinev1beta1.CreateMachineError
	errMsg := "vm creation timed out"

	objects := []runtime.Object{
		newInfrastructureForRollout(),
		newCPMSUpdatedForRollout([]string{"target-fd-1"}, true),
		&machinev1beta1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cp-1",
				Namespace: openshift.MachineAPINamespace,
				Labels:    map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"},
			},
			Status: machinev1beta1.MachineStatus{
				Phase: &running,
				NodeRef: &corev1.ObjectReference{
					Name: "cp-node-1",
				},
			},
		},
		&machinev1beta1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cp-2",
				Namespace:         openshift.MachineAPINamespace,
				Labels:            map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"},
				CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Minute)),
			},
			Status: machinev1beta1.MachineStatus{
				Phase:        &provisioning,
				ErrorReason:  &createErr,
				ErrorMessage: &errMsg,
				LastUpdated:  &metav1.Time{Time: now.Add(-1 * time.Minute)},
			},
		},
	}
	cpms := objects[1].(*machinev1.ControlPlaneMachineSet)
	cpms.Status.UpdatedReplicas = 1
	cpms.Status.ReadyReplicas = 1

	kubeObjects, configClientObjects, machineClientObjects := splitRolloutTestObjects(t, objects)
	reconciler := &VmwareCloudFoundationMigrationReconciler{
		KubeClient:    fakekube.NewClientset(kubeObjects...),
		ConfigClient:  configfake.NewClientset(configClientObjects...),
		MachineClient: fakemachineclient.NewClientset(machineClientObjects...),
		Recorder:      record.NewFakeRecorder(10),
	}
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 1},
	}

	result, err := reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration)
	if err != nil {
		t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("result requeueAfter = %s, want %s", result.RequeueAfter, 30*time.Second)
	}

	logOutput := buf.String()
	for _, want := range []string{
		"machine status",
		"cp-1",
		"Running",
		"hasNodeRef",
		"cp-2",
		"Provisioning",
		"CreateError",
		"hasErrorMessage",
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output missing %q; got:\n%s", want, logOutput)
		}
	}
	for _, doNotWant := range []string{
		"cp-node-1",
		"vm creation timed out",
	} {
		if strings.Contains(logOutput, doNotWant) {
			t.Fatalf("log output contains sensitive value %q; got:\n%s", doNotWant, logOutput)
		}
	}
}

func splitRolloutTestObjects(t *testing.T, objects []runtime.Object) (kube []runtime.Object, config []runtime.Object, machine []runtime.Object) {
	t.Helper()
	for _, obj := range objects {
		switch o := obj.(type) {
		case *configv1.Infrastructure:
			config = append(config, o)
		case *machinev1.ControlPlaneMachineSet, *machinev1beta1.MachineSet, *machinev1beta1.Machine:
			machine = append(machine, o)
		case *corev1.Node:
			kube = append(kube, o)
		default:
			t.Fatalf("unsupported test object type %T", obj)
		}
	}
	return kube, config, machine
}

func waitForLastEvent(t *testing.T, recorder *record.FakeRecorder) string {
	t.Helper()
	var last string
	for {
		select {
		case event := <-recorder.Events:
			last = event
		default:
			return last
		}
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

func newInfrastructureForRollout() *configv1.Infrastructure {
	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
		Spec: configv1.InfrastructureSpec{
			PlatformSpec: configv1.PlatformSpec{
				Type: configv1.VSpherePlatformType,
				VSphere: &configv1.VSpherePlatformSpec{
					VCenters: []configv1.VSpherePlatformVCenterSpec{
						{Server: "source.example.com"},
					},
				},
			},
		},
		Status: configv1.InfrastructureStatus{
			InfrastructureName: "test-infra",
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

func newCPMSUpdatedForRollout(fdNames []string, observed bool) *machinev1.ControlPlaneMachineSet {
	replicas := int32(3)
	vsphereFDs := make([]machinev1.VSphereFailureDomain, len(fdNames))
	for i, name := range fdNames {
		vsphereFDs[i] = machinev1.VSphereFailureDomain{Name: name}
	}

	cpms := &machinev1.ControlPlaneMachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster",
			Namespace:  openshift.MachineAPINamespace,
			Generation: 2,
		},
		Spec: machinev1.ControlPlaneMachineSetSpec{
			State:    machinev1.ControlPlaneMachineSetStateActive,
			Replicas: &replicas,
			Template: machinev1.ControlPlaneMachineSetTemplate{
				MachineType: machinev1.OpenShiftMachineV1Beta1MachineType,
				OpenShiftMachineV1Beta1Machine: &machinev1.OpenShiftMachineV1Beta1MachineTemplate{
					FailureDomains: &machinev1.FailureDomains{
						Platform: configv1.VSpherePlatformType,
						VSphere:  vsphereFDs,
					},
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
			UpdatedReplicas: replicas,
			ReadyReplicas:   replicas,
		},
	}
	if observed {
		cpms.Status.ObservedGeneration = cpms.Generation
	} else {
		cpms.Status.ObservedGeneration = cpms.Generation - 1
	}

	return cpms
}

func newSourceMachineSetForRollout(replicas int32) *machinev1beta1.MachineSet {
	return newTargetMachineSetForRollout("source-worker-a", "source.example.com", replicas)
}

func newTargetMachineSetForRollout(name, server string, replicas int32) *machinev1beta1.MachineSet {
	return &machinev1beta1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: openshift.MachineAPINamespace,
		},
		Spec: machinev1beta1.MachineSetSpec{
			Replicas: &replicas,
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"machine.openshift.io/cluster-api-machineset": name,
				},
			},
			Template: machinev1beta1.MachineTemplateSpec{
				ObjectMeta: machinev1beta1.ObjectMeta{
					Labels: map[string]string{
						"machine.openshift.io/cluster-api-machineset": name,
					},
				},
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

func newReadyWorkerMachineForRollout(machineSetName, nodeName string) *machinev1beta1.Machine {
	running := machinePhaseRunning
	return &machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineSetName + "-machine",
			Namespace: openshift.MachineAPINamespace,
			Labels: map[string]string{
				"machine.openshift.io/cluster-api-machineset": machineSetName,
			},
		},
		Status: machinev1beta1.MachineStatus{
			Phase: &running,
			NodeRef: &corev1.ObjectReference{
				Name: nodeName,
			},
		},
	}
}

func newReadyNodeForRollout(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

func TestOldWorkersStallEventDebounce(t *testing.T) {
	ctx := context.Background()
	running := machinePhaseRunning

	newObjects := func(machineName string) []runtime.Object {
		return []runtime.Object{
			newInfrastructureForRollout(),
			newCPMSForRollout(true, true),
			newSourceMachineSetForRollout(0),
			&machinev1beta1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machineName,
					Namespace: openshift.MachineAPINamespace,
					Labels: map[string]string{
						"machine.openshift.io/cluster-api-machineset": "source-worker-a",
					},
				},
				Status: machinev1beta1.MachineStatus{
					Phase:   &running,
					NodeRef: &corev1.ObjectReference{Name: machineName + "-node"},
				},
			},
			newReadyNodeForRollout(machineName + "-node"),
		}
	}

	reconciler := newRolloutReconciler(t, newObjects("source-worker-a-0"))
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 1},
	}

	var stalled []string

	// First stalled reconcile emits exactly one Warning event.
	if _, err := reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration); err != nil {
		t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown: %v", err)
	}
	stalled = append(stalled, stalledEvents(t, reconciler)...)
	if len(stalled) != 1 {
		t.Fatalf("stalled events after first reconcile = %v, want exactly 1", stalled)
	}

	// Second reconcile with the same lingering machines emits no new event.
	if _, err := reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration); err != nil {
		t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown: %v", err)
	}
	stalled = append(stalled, stalledEvents(t, reconciler)...)
	if len(stalled) != 1 {
		t.Fatalf("stalled events after repeat reconcile = %v, want still 1 (debounced)", stalled)
	}

	// A different lingering machine set emits a new event even within the window.
	kubeObjects, configObjects, machineObjects := splitRolloutTestObjects(t, newObjects("source-worker-a-1"))
	reconciler.KubeClient = fakekube.NewClientset(kubeObjects...)
	reconciler.ConfigClient = configfake.NewClientset(configObjects...)
	reconciler.MachineClient = fakemachineclient.NewClientset(machineObjects...)
	if _, err := reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration); err != nil {
		t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown: %v", err)
	}
	stalled = append(stalled, stalledEvents(t, reconciler)...)
	if len(stalled) != 2 {
		t.Fatalf("stalled events after changed machine set = %v, want 2", stalled)
	}

	// Many lingering machines produce a bounded event note with an omission summary.
	const manyMachineCount = 40
	manyObjects := []runtime.Object{
		newInfrastructureForRollout(),
		newCPMSForRollout(true, true),
		newSourceMachineSetForRollout(0),
	}
	for i := 0; i < manyMachineCount; i++ {
		machineName := fmt.Sprintf("source-worker-a-very-long-machine-name-for-stall-event-bounding-test-%03d", i)
		manyObjects = append(manyObjects,
			&machinev1beta1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machineName,
					Namespace: openshift.MachineAPINamespace,
					Labels: map[string]string{
						"machine.openshift.io/cluster-api-machineset": "source-worker-a",
					},
				},
				Status: machinev1beta1.MachineStatus{
					Phase:   &running,
					NodeRef: &corev1.ObjectReference{Name: machineName + "-node"},
				},
			},
			newReadyNodeForRollout(machineName+"-node"),
		)
	}
	manyReconciler := newRolloutReconciler(t, manyObjects)
	if _, err := manyReconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration); err != nil {
		t.Fatalf("ensureWorkloadMigratedRolloutAndScaleDown: %v", err)
	}
	manyEvents := stalledEvents(t, manyReconciler)
	if len(manyEvents) != 1 {
		t.Fatalf("stalled events with many machines = %v, want exactly 1", manyEvents)
	}
	eventNote := stallEventNote(manyEvents[0])
	if len(eventNote) > 1024 {
		t.Fatalf("event note length = %d, want <= 1024; note = %q", len(eventNote), eventNote)
	}
	if !strings.Contains(eventNote, "omitted") {
		t.Fatalf("event note %q missing omission summary", eventNote)
	}
}

// stalledEvents drains the recorder and returns events carrying the
// OldWorkersStalled Warning reason.
func stalledEvents(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler) []string {
	t.Helper()
	recorder := reconciler.Recorder.(*record.FakeRecorder)
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "OldWorkersStalled") {
				events = append(events, event)
			}
		default:
			return events
		}
	}
}

// stallEventNote returns the message portion of a FakeRecorder event string.
func stallEventNote(event string) string {
	const prefix = "Warning OldWorkersStalled "
	if strings.HasPrefix(event, prefix) {
		return strings.TrimPrefix(event, prefix)
	}
	return event
}

// newRolloutReconciler builds a reconciler wired to fake clients populated with
// the given rollout objects.
func newRolloutReconciler(t *testing.T, objects []runtime.Object) *VmwareCloudFoundationMigrationReconciler {
	t.Helper()
	kubeObjects, configObjects, machineObjects := splitRolloutTestObjects(t, objects)
	return &VmwareCloudFoundationMigrationReconciler{
		KubeClient:    fakekube.NewClientset(kubeObjects...),
		ConfigClient:  configfake.NewClientset(configObjects...),
		MachineClient: fakemachineclient.NewClientset(machineObjects...),
		Recorder:      record.NewFakeRecorder(20),
	}
}

func TestBoundConditionMessage(t *testing.T) {
	t.Run("preserves short message within limit", func(t *testing.T) {
		shortMsg := "Waiting for old worker deletion: source-worker-a has 1 machine(s) [source-worker-a-0 (10s old)] and 0 node(s) remaining"
		got := boundConditionMessage(shortMsg)
		if got != shortMsg {
			t.Fatalf("boundConditionMessage(%q) = %q, want %q", shortMsg, got, shortMsg)
		}
	})

	t.Run("truncates oversized message within 32768 limit with omitted machine count", func(t *testing.T) {
		var parts []string
		for i := 0; i < 1000; i++ {
			parts = append(parts, fmt.Sprintf("machine-%04d (10s old, CreateError)", i))
		}
		longMsg := fmt.Sprintf("Waiting for old worker deletion: source-worker-a has 1000 machine(s) [%s] and 0 node(s) remaining", strings.Join(parts, ", "))
		if len(longMsg) <= maxConditionMessageBytes {
			t.Fatalf("test precondition failed: longMsg length %d <= %d", len(longMsg), maxConditionMessageBytes)
		}

		bounded := boundConditionMessage(longMsg)
		if len(bounded) > maxConditionMessageBytes {
			t.Fatalf("bounded message length = %d, want <= %d", len(bounded), maxConditionMessageBytes)
		}
		if !strings.Contains(bounded, "omitted") {
			t.Fatalf("bounded message %q missing omission summary", bounded)
		}
		if !strings.HasPrefix(bounded, "Waiting for old worker deletion: source-worker-a has 1000 machine(s) [") {
			t.Fatalf("bounded message %q missing expected prefix", bounded)
		}
	})
}

func TestOldWorkerStallDetailSorting(t *testing.T) {
	ctx := context.Background()
	running := machinePhaseRunning

	objects := []runtime.Object{
		newTargetMachineSetForRollout("worker-z", "source.example.com", 0),
		newTargetMachineSetForRollout("worker-a", "source.example.com", 0),
		&machinev1beta1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker-z-machine-2",
				Namespace: openshift.MachineAPINamespace,
				Labels:    map[string]string{"machine.openshift.io/cluster-api-machineset": "worker-z"},
			},
			Status: machinev1beta1.MachineStatus{Phase: &running},
		},
		&machinev1beta1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker-z-machine-1",
				Namespace: openshift.MachineAPINamespace,
				Labels:    map[string]string{"machine.openshift.io/cluster-api-machineset": "worker-z"},
			},
			Status: machinev1beta1.MachineStatus{Phase: &running},
		},
		&machinev1beta1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker-a-machine-1",
				Namespace: openshift.MachineAPINamespace,
				Labels:    map[string]string{"machine.openshift.io/cluster-api-machineset": "worker-a"},
			},
			Status: machinev1beta1.MachineStatus{Phase: &running},
		},
	}

	kubeObjects, _, machineObjects := splitRolloutTestObjects(t, objects)
	kubeClient := fakekube.NewClientset(kubeObjects...)
	machineClient := fakemachineclient.NewClientset(machineObjects...)
	machineMgr := openshift.NewMachineManager(kubeClient, machineClient, nil)

	msList := []*machinev1beta1.MachineSet{
		objects[0].(*machinev1beta1.MachineSet),
		objects[1].(*machinev1beta1.MachineSet),
	}

	var buf bytes.Buffer
	logger := stdr.NewWithOptions(log.New(&buf, "", 0), stdr.Options{})

	detail, eventNote, key, err := oldWorkerStallDetail(ctx, logger, machineMgr, msList)
	if err != nil {
		t.Fatalf("oldWorkerStallDetail: %v", err)
	}

	// detail and parts should be sorted by machineSet name (worker-a before worker-z)
	idxA := strings.Index(detail, "worker-a")
	idxZ := strings.Index(detail, "worker-z")
	if idxA == -1 || idxZ == -1 || idxA > idxZ {
		t.Fatalf("detail parts not sorted by machineSet name: %q", detail)
	}

	// within worker-z, machine-1 should come before machine-2
	idxM1 := strings.Index(detail, "worker-z-machine-1")
	idxM2 := strings.Index(detail, "worker-z-machine-2")
	if idxM1 == -1 || idxM2 == -1 || idxM1 > idxM2 {
		t.Fatalf("machines within machineSet not sorted by name: %q", detail)
	}

	// event note should also preserve machineSet sort order
	idxEventA := strings.Index(eventNote, "worker-a")
	idxEventZ := strings.Index(eventNote, "worker-z")
	if idxEventA == -1 || idxEventZ == -1 || idxEventA > idxEventZ {
		t.Fatalf("event note not sorted by machineSet name: %q", eventNote)
	}

	// key should have worker-a before worker-z
	wantKey := "worker-a=worker-a-machine-1|worker-z=worker-z-machine-1,worker-z-machine-2"
	if key != wantKey {
		t.Fatalf("key = %q, want %q", key, wantKey)
	}
}
