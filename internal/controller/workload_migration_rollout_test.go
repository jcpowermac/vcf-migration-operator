package controller

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/stdr"
	configv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	machineconfigfake "github.com/openshift/client-go/machineconfiguration/clientset/versioned/fake"
	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// machinePhaseRunning is the machinev1beta1 Machine Phase value for a running
// machine; the upstream API package does not export a constant for it.
const machinePhaseRunning = "Running"

// completedMessage marks earlier-phase conditions as done in test fixtures.
const completedMessage = "completed"

const sourceMSB = "source-worker-b"

// assertSourceScaledDownToZero verifies the drained source MachineSet was scaled
// to zero and the workload condition reports the scale-down progress.
func assertSourceScaledDownToZero(t *testing.T, ctx context.Context, reconciler *VmwareCloudFoundationMigrationReconciler, migration *migrationv1alpha1.VmwareCloudFoundationMigration) {
	t.Helper()
	ms, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, "source-worker-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting scaled machineset: %v", err)
	}
	if ms.Spec.Replicas == nil || *ms.Spec.Replicas != 0 {
		t.Fatalf("scaled machineset replicas = %v, want 0", ms.Spec.Replicas)
	}

	cond := findWorkloadCondition(t, migration)
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
	}
	if !strings.Contains(cond.Message, "Old workers scaled down") {
		t.Fatalf("workload condition message = %q, want substring %q", cond.Message, "Old workers scaled down")
	}
}

// assertSourceDeletedAndWorkloadMigrated verifies the drained source MachineSet
// was deleted and the workload condition completed.
func assertSourceDeletedAndWorkloadMigrated(t *testing.T, ctx context.Context, reconciler *VmwareCloudFoundationMigrationReconciler, migration *migrationv1alpha1.VmwareCloudFoundationMigration) {
	t.Helper()
	_, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, "source-worker-a", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected source-worker-a to be deleted, got err %v", err)
	}

	cond := findWorkloadCondition(t, migration)
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionTrue)
	}
	if cond.Reason != migrationv1alpha1.ReasonCompleted {
		t.Fatalf("workload condition reason = %q, want %q", cond.Reason, migrationv1alpha1.ReasonCompleted)
	}
	if !strings.Contains(cond.Message, "Workload migrated") {
		t.Fatalf("workload condition message = %q, want substring %q", cond.Message, "Workload migrated")
	}
}

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
				recorder := resultReconciler.Recorder.(*events.FakeRecorder)
				event := waitForLastEvent(t, recorder)
				if !strings.Contains(event, "Control plane rolling out (1/3 updated, 1/3 ready)") {
					t.Fatalf("last event = %q, want rollout progress substring", event)
				}
			},
		},
		{
			// Regression: the counts can look complete while the CPMS holds the
			// rollout on an unmanaged control plane node. The condition message
			// must name that hold (and its CPMS condition message) instead of
			// reporting progress only, and scale-down must still wait.
			name: "reports the CPMS unmanaged-nodes hold while the rollout is blocked",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newCPMSHeldOnUnmanagedNodesForRollout(),
				newSourceMachineSetForRollout(2),
			},
			wantRequeue: 30 * time.Second,
			assertions: func(t *testing.T, resultReconciler *VmwareCloudFoundationMigrationReconciler, resultMigration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				cond := findWorkloadCondition(t, resultMigration)
				if cond.Status != metav1.ConditionFalse {
					t.Fatalf("workload condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
				}
				for _, want := range []string{
					"held by ControlPlaneMachineSet condition",
					"UnmanagedNodes",
					"Found 1 unmanaged node(s)",
					"3/3 updated, 3/3 ready",
				} {
					if !strings.Contains(cond.Message, want) {
						t.Fatalf("workload condition message %q missing substring %q", cond.Message, want)
					}
				}
				event := waitForLastEvent(t, resultReconciler.Recorder.(*events.FakeRecorder))
				if !strings.Contains(event, "held by ControlPlaneMachineSet condition") {
					t.Fatalf("last event = %q, want the CPMS hold reason", event)
				}
				// The source MachineSets must keep their capacity while the hold
				// applies, even though every rollout count looks complete.
				assertMachineSetReplicas(t, ctx, resultReconciler, "source-worker-a", 2)
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
				assertSourceScaledDownToZero(t, ctx, resultReconciler, resultMigration)
			},
		},
		{
			// nil replicas default to 1 in MAPI: such a MachineSet runs
			// capacity and must be scaled down like any positive one.
			name: "scales source machinesets with nil replicas down to zero",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newCPMSForRollout(true, true),
				newNilReplicaSourceMachineSetForRollout("source-worker-a"),
			},
			wantRequeue: 30 * time.Second,
			assertions: func(t *testing.T, resultReconciler *VmwareCloudFoundationMigrationReconciler, _ *migrationv1alpha1.VmwareCloudFoundationMigration) {
				t.Helper()
				assertMachineSetReplicas(t, ctx, resultReconciler, "source-worker-a", 0)
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
				assertSourceDeletedAndWorkloadMigrated(t, ctx, resultReconciler, resultMigration)
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
			// Step 6 verifies recorded replacements before removing any source capacity.
			machineClientObjects = append(machineClientObjects,
				newTargetMachineSetForRollout(workerMachineSetName("test-infra", "target-fd-1"), "target.example.com", 2))

			reconciler := &VmwareCloudFoundationMigrationReconciler{
				KubeClient:    fakekube.NewClientset(kubeObjects...),
				ConfigClient:  configfake.NewClientset(configClientObjects...),
				MachineClient: fakemachineclient.NewClientset(machineClientObjects...),
				Recorder:      events.NewFakeRecorder(10),
			}
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:       migrationv1alpha1.SingletonName,
					Generation: 1,
				},
				// Spec failure domains are always populated by the time the real
				// flow reaches Step 6; name recovery resolves the destination
				// failure domain from them.
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
				// The coverage gate before scale-down requires the preflight mapping
				// and the worker MachineSets recorded at creation.
				Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
					FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
						{Source: "source-fd-1", Destination: "target-fd-1"},
					},
					WorkerMachineSetNames: []string{workerMachineSetName("test-infra", "target-fd-1")},
				},
			}
			if tt.setup != nil {
				tt.setup(t, reconciler.MachineClient.(*fakemachineclient.Clientset), migration)
			}
			// Scale-down publishes the recorded names before any cleanup, so the
			// reconciler needs a status client on this path.
			reconciler.Client = newMigrationStatusClient(t, migration)

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
		mcpPools    []runtime.Object
		wantRequeue time.Duration
		wantMessage string
		assertions  func(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler)
	}{
		{
			name: "routes to rollout path when CPMS targets failure domains and workers are ready",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
				newReadyWorkerMachineForRollout(targetMSName),
				newReadyNodeForRollout(rolloutReadyNodeName),
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
				newReadyWorkerMachineForRollout(targetMSName),
				newReadyNodeForRollout(rolloutReadyNodeName),
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
			// A failure domain that received multiple source MachineSets generates
			// suffixed copies (base-2, ...); readiness must cover every copy or the
			// source would be scaled down while a copy is still coming up.
			name: "blocks readiness while a suffixed copy is not ready",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
				newReadyWorkerMachineForRollout(targetMSName),
				newReadyNodeForRollout(rolloutReadyNodeName),
				newTargetMachineSetForRollout(targetMSName+"-2", "target.example.com", 1),
				&machinev1beta1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      targetMSName + "-2-pending",
						Namespace: openshift.MachineAPINamespace,
						Labels: map[string]string{
							"machine.openshift.io/cluster-api-machineset": targetMSName + "-2",
						},
					},
					Status: machinev1beta1.MachineStatus{Phase: &provisioning},
				},
				newCPMSUpdatedForRollout([]string{"target-fd-1"}, false),
				newSourceMachineSetForRolloutNamed("source-worker-a", 1),
				newSourceMachineSetForRolloutNamed("source-worker-b", 1),
			},
			// 30s (worker wait), not the 15s rollout requeue: the suffixed copy
			// must gate the migration before any source scale-down.
			wantRequeue: 30 * time.Second,
			wantMessage: "Waiting for target workers (machines 1/2 ready, nodes 1/2 ready)",
		},
		{
			// The base MachineSet alone must not satisfy the creation gate: a
			// missing suffixed copy (e.g. after a transient creation error) must
			// be healed by the idempotent creation loop, never skipped in favor
			// of proceeding toward source scale-down. The base carries this
			// operator's source annotation: an unannotated base under a
			// multi-source mapping is the released operator's legacy aggregate.
			name: "recreates a missing suffixed copy",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newDestinationMachineSetForSource(targetMSName, "source-worker-a", 1),
				newReadyWorkerMachineForRollout(targetMSName),
				newReadyNodeForRollout(rolloutReadyNodeName),
				newCPMSUpdatedForRollout([]string{"target-fd-1"}, false),
				newSourceMachineSetForRolloutNamed("source-worker-a", 1),
				newSourceMachineSetForRolloutNamed("source-worker-b", 2),
			},
			wantRequeue: 30 * time.Second,
			wantMessage: "Workers created, waiting for machines ready",
			assertions: func(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler) {
				t.Helper()
				ms, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, targetMSName+"-2", metav1.GetOptions{})
				if err != nil {
					t.Fatalf("getting recreated suffixed machineset %q: %v", targetMSName+"-2", err)
				}
				if ms.Spec.Replicas == nil || *ms.Spec.Replicas != 2 {
					t.Fatalf("machineset %q replicas = %v, want 2 (copied from source-worker-b)", targetMSName+"-2", ms.Spec.Replicas)
				}
			},
		},
		{
			// A nil-replica source MachineSet runs one replica by MAPI default;
			// the destination copy must not be created at zero. The annotated base
			// keeps the aggregate detection out of the way as above.
			name: "copies nil source replicas as one",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newDestinationMachineSetForSource(targetMSName, "source-worker-a", 1),
				newReadyWorkerMachineForRollout(targetMSName),
				newReadyNodeForRollout(rolloutReadyNodeName),
				newCPMSUpdatedForRollout([]string{"target-fd-1"}, false),
				newSourceMachineSetForRolloutNamed("source-worker-a", 1),
				newNilReplicaSourceMachineSetForRollout("source-worker-b"),
			},
			wantRequeue: 30 * time.Second,
			wantMessage: "Workers created, waiting for machines ready",
			assertions: func(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler) {
				t.Helper()
				assertMachineSetReplicas(t, ctx, reconciler, targetMSName+"-2", 1)
			},
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
		{
			// The migration's own cloud provider and credential changes trigger an
			// MCO machine config pool rollout that reboots the same masters the
			// CPMS replaces. Updating the CPMS while a pool is mid-roll can defeat
			// the etcd quorum guard (member unhealthy mid-reboot), so the CPMS
			// update must hold until every pool has converged.
			name: "holds CPMS update while machine config pools are rolling out",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
				newReadyWorkerMachineForRollout(targetMSName),
				newReadyNodeForRollout(rolloutReadyNodeName),
				newCPMSUpdatedForRollout([]string{"source-fd-1"}, false),
				newSourceMachineSetForRollout(1),
			},
			mcpPools:    []runtime.Object{newUnconvergedReadyTestPool("master"), newConvergedReadyTestPool("worker")},
			wantRequeue: 30 * time.Second,
			wantMessage: "Waiting for machine config pools to converge (not updated: master)",
			assertions: func(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler) {
				t.Helper()
				cpms, err := reconciler.MachineClient.MachineV1().ControlPlaneMachineSets(openshift.MachineAPINamespace).Get(ctx, "cluster", metav1.GetOptions{})
				if err != nil {
					t.Fatalf("getting CPMS: %v", err)
				}
				fds := cpms.Spec.Template.OpenShiftMachineV1Beta1Machine.FailureDomains
				if fds == nil || len(fds.VSphere) != 1 || fds.VSphere[0].Name != "source-fd-1" {
					t.Fatalf("CPMS failure domains = %+v, want source-fd-1 (CPMS must not be updated before pools converge)", fds)
				}
			},
		},
		{
			name: "updates CPMS once machine config pools have converged",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
				newReadyWorkerMachineForRollout(targetMSName),
				newReadyNodeForRollout(rolloutReadyNodeName),
				newCPMSUpdatedForRollout([]string{"source-fd-1"}, false),
				newSourceMachineSetForRollout(1),
			},
			mcpPools:    []runtime.Object{newConvergedReadyTestPool("master"), newConvergedReadyTestPool("worker")},
			wantRequeue: 15 * time.Second,
			wantMessage: "Waiting for control plane rollout to start (CPMS generation 2/1 observed)",
			assertions: func(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler) {
				t.Helper()
				cpms, err := reconciler.MachineClient.MachineV1().ControlPlaneMachineSets(openshift.MachineAPINamespace).Get(ctx, "cluster", metav1.GetOptions{})
				if err != nil {
					t.Fatalf("getting CPMS: %v", err)
				}
				fds := cpms.Spec.Template.OpenShiftMachineV1Beta1Machine.FailureDomains
				if fds == nil || len(fds.VSphere) != 1 || fds.VSphere[0].Name != "target-fd-1" {
					t.Fatalf("CPMS failure domains = %+v, want target-fd-1 (CPMS should be updated once pools converge)", fds)
				}
				if cpms.Spec.State != machinev1.ControlPlaneMachineSetStateActive {
					t.Fatalf("CPMS state = %q, want %q", cpms.Spec.State, machinev1.ControlPlaneMachineSetStateActive)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeObjects, configClientObjects, machineClientObjects := splitRolloutTestObjects(t, tt.objects)

			reconciler := &VmwareCloudFoundationMigrationReconciler{
				KubeClient:          fakekube.NewClientset(kubeObjects...),
				ConfigClient:        configfake.NewClientset(configClientObjects...),
				MachineClient:       fakemachineclient.NewClientset(machineClientObjects...),
				MachineConfigClient: machineconfigfake.NewClientset(tt.mcpPools...),
				Recorder:            events.NewFakeRecorder(20),
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
				Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
					// The effective mapping recorded at preflight drives worker
					// MachineSet creation.
					FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
						{Source: "source-fd-1", Destination: "target-fd-1"},
					},
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

// TestReconcileWorkloadMigratedFailedWorkerMachine is a controller-level
// regression test for a terminally Failed worker Machine behind a target
// MachineSet: the error surfaces from CheckMachinesReady through
// checkWorkerReadiness and ensureWorkloadMigrated, and Reconcile persists
// ConditionWorkloadMigrated False with ReasonFailed (the error is not
// transient, so ReasonRetrying must not be used). The direct manager-level
// behavior is covered by TestCheckMachinesReady in internal/openshift.
func TestReconcileWorkloadMigratedFailedWorkerMachine(t *testing.T) {
	ctx := context.Background()
	targetMSName := workerMachineSetName("test-infra", "target-fd-1")

	failedPhase := machinev1beta1.PhaseFailed
	failureReason := machinev1beta1.CreateMachineError
	failureMessage := "simulated create failure"
	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForRollout(),
		newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
		&machinev1beta1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      targetMSName + "-failed",
				Namespace: openshift.MachineAPINamespace,
				Labels: map[string]string{
					"machine.openshift.io/cluster-api-machineset": targetMSName,
				},
			},
			Status: machinev1beta1.MachineStatus{
				Phase:        &failedPhase,
				ErrorReason:  &failureReason,
				ErrorMessage: &failureMessage,
			},
		},
	})

	start := metav1.Now()
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       migrationv1alpha1.SingletonName,
			Namespace:  migrationv1alpha1.SingletonNamespace,
			Generation: 1,
			// The retention finalizer is normally added by the first reconcile;
			// pre-setting it keeps this test focused on the workload phase.
			Finalizers: []string{migrationv1alpha1.FinalizerName},
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: migrationv1alpha1.MigrationStateRunning,
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
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			StartTime: &start,
			FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
			},
		},
	}
	// A reverted mapping edit must not leave the old rejection persisted.
	reconciler.setCondition(migration, migrationv1alpha1.ConditionAccepted, metav1.ConditionFalse, migrationv1alpha1.ReasonMappingRejected, "previous mapping drift")
	// Earlier phases are complete so Reconcile dispatches straight to
	// ensureWorkloadMigrated.
	reconciler.setCondition(migration, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, completedMessage)
	reconciler.setCondition(migration, migrationv1alpha1.ConditionStoragePlanned, metav1.ConditionTrue, migrationv1alpha1.ReasonNotConfigured, completedMessage)
	reconciler.setCondition(migration, migrationv1alpha1.ConditionDestinationInitialized, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, completedMessage)
	reconciler.setCondition(migration, migrationv1alpha1.ConditionMultiSiteConfigured, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, completedMessage)

	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)})
	if err == nil || !strings.Contains(err.Error(), "is Failed") {
		t.Fatalf("Reconcile() error = %v, want error containing %q", err, "is Failed")
	}

	persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatalf("getting persisted migration: %v", err)
	}
	if accepted := apimeta.FindStatusCondition(persisted.Status.Conditions, migrationv1alpha1.ConditionAccepted); isMappingRejection(accepted) {
		t.Fatalf("stale mapping rejection persisted: %+v", accepted)
	}
	cond := apimeta.FindStatusCondition(persisted.Status.Conditions, migrationv1alpha1.ConditionWorkloadMigrated)
	if cond == nil {
		t.Fatalf("condition %q not found on persisted migration", migrationv1alpha1.ConditionWorkloadMigrated)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
	}
	if cond.Reason != migrationv1alpha1.ReasonFailed {
		t.Fatalf("condition reason = %q, want %q", cond.Reason, migrationv1alpha1.ReasonFailed)
	}
	if !strings.Contains(cond.Message, "is Failed") {
		t.Fatalf("condition message = %q, want containing %q", cond.Message, "is Failed")
	}
}

// TestScaleDownStopsOnUnmappedSourceMachineSet is a controller-level
// regression test for the Step 6 coverage gate: a source-vCenter MachineSet
// with running replicas placed in a datacenter covered by no source failure
// domain must stop the migration before any source MachineSet is scaled
// down, and Reconcile must persist ConditionWorkloadMigrated False. The
// per-case gate behavior is covered by TestValidateSourceMachineSetCoverage.
func TestScaleDownStopsOnUnmappedSourceMachineSet(t *testing.T) {
	ctx := context.Background()
	targetMSName := workerMachineSetName("test-infra", "target-fd-1")
	const outOfTopologyMSName = "source-worker-other"

	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForRollout(),
		newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
		newReadyWorkerMachineForRollout(targetMSName),
		newReadyNodeForRollout(rolloutReadyNodeName),
		newCPMSUpdatedForRollout([]string{"target-fd-1"}, true),
		newSourceMachineSetForRollout(1),
		newOutOfTopologySourceMachineSetForRollout(outOfTopologyMSName, 1),
	})

	start := metav1.Now()
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       migrationv1alpha1.SingletonName,
			Namespace:  migrationv1alpha1.SingletonNamespace,
			Generation: 1,
			// The retention finalizer is normally added by the first reconcile;
			// pre-setting it keeps this test focused on the workload phase.
			Finalizers: []string{migrationv1alpha1.FinalizerName},
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: migrationv1alpha1.MigrationStateRunning,
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
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			StartTime: &start,
			FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
			},
			// Recorded when the target worker MachineSets were created: the
			// mapped source MachineSet has a verified replacement.
			WorkerMachineSetNames: []string{targetMSName},
		},
	}
	// Earlier phases are complete so Reconcile dispatches straight to
	// ensureWorkloadMigrated.
	reconciler.setCondition(migration, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, completedMessage)
	reconciler.setCondition(migration, migrationv1alpha1.ConditionDestinationInitialized, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, completedMessage)
	reconciler.setCondition(migration, migrationv1alpha1.ConditionMultiSiteConfigured, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, completedMessage)

	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)})
	if err == nil || !strings.Contains(err.Error(), "does not match any source failure domain") {
		t.Fatalf("Reconcile() error = %v, want error containing %q", err, "does not match any source failure domain")
	}

	persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatalf("getting persisted migration: %v", err)
	}
	cond := apimeta.FindStatusCondition(persisted.Status.Conditions, migrationv1alpha1.ConditionWorkloadMigrated)
	if cond == nil {
		t.Fatalf("condition %q not found on persisted migration", migrationv1alpha1.ConditionWorkloadMigrated)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("condition status = %q, want %q", cond.Status, metav1.ConditionFalse)
	}
	if !strings.Contains(cond.Message, "does not match any source failure domain") {
		t.Fatalf("condition message = %q, want containing %q", cond.Message, "does not match any source failure domain")
	}

	ms, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, outOfTopologyMSName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting machineset %q: %v", outOfTopologyMSName, err)
	}
	if ms.Spec.Replicas == nil || *ms.Spec.Replicas != 1 {
		t.Fatalf("machineset %q replicas = %v, want 1 (must not be scaled down)", outOfTopologyMSName, ms.Spec.Replicas)
	}
}

// TestScaleDownRecoversUnrecordedDestinationMachineSetNames is a
// controller-level regression test for the restart gap between worker
// MachineSet creation and the updateStatus that records their names: the
// destination MachineSets exist and are ready, but
// status.workerMachineSetNames is empty (lost to a crash). Reconcile must
// re-derive the names from the still-present source MachineSets, verify them,
// record them, and proceed through Step 6 instead of failing the coverage
// gate forever.
func TestScaleDownRecoversUnrecordedDestinationMachineSetNames(t *testing.T) {
	ctx := context.Background()
	targetMSName := workerMachineSetName("test-infra", "target-fd-1")
	const sourceMSA = "source-worker-a"

	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForRollout(),
		newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
		newTargetMachineSetForRollout(targetMSName+"-2", "target.example.com", 1),
		newReadyWorkerMachineForRollout(targetMSName),
		newReadyWorkerMachineForRollout(targetMSName + "-2"),
		newReadyNodeForRollout(rolloutReadyNodeName),
		newCPMSUpdatedForRollout([]string{"target-fd-1"}, true),
		newSourceMachineSetForRolloutNamed(sourceMSA, 1),
		newSourceMachineSetForRolloutNamed(sourceMSB, 1),
	})

	start := metav1.Now()
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       migrationv1alpha1.SingletonName,
			Namespace:  migrationv1alpha1.SingletonNamespace,
			Generation: 1,
			Finalizers: []string{migrationv1alpha1.FinalizerName},
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: migrationv1alpha1.MigrationStateRunning,
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
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			StartTime: &start,
			FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
			},
			// Restart state: destination MachineSets exist, but the crash lost
			// the recording of their names.
			WorkerMachineSetNames: nil,
		},
	}
	reconciler.setCondition(migration, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, completedMessage)
	reconciler.setCondition(migration, migrationv1alpha1.ConditionDestinationInitialized, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, completedMessage)
	reconciler.setCondition(migration, migrationv1alpha1.ConditionMultiSiteConfigured, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, completedMessage)

	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)}); err != nil {
		t.Fatalf("Reconcile() error = %v, want nil (recovered names must let scale-down proceed)", err)
	}

	for _, name := range []string{sourceMSA, sourceMSB} {
		ms, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting machineset %q: %v", name, err)
		}
		if ms.Spec.Replicas == nil || *ms.Spec.Replicas != 0 {
			t.Fatalf("machineset %q replicas = %v, want 0 (flow must proceed through Step 6)", name, ms.Spec.Replicas)
		}
	}

	persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatalf("getting persisted migration: %v", err)
	}
	if want := []string{targetMSName, targetMSName + "-2"}; !slices.Equal(persisted.Status.WorkerMachineSetNames, want) {
		t.Fatalf("persisted WorkerMachineSetNames = %v, want %v", persisted.Status.WorkerMachineSetNames, want)
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
		Recorder:      events.NewFakeRecorder(10),
	}
	migration := newRolloutMigration()

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

func waitForLastEvent(t *testing.T, recorder *events.FakeRecorder) string {
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
					FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
						newSourceFailureDomainForRollout(),
					},
				},
			},
		},
		Status: configv1.InfrastructureStatus{
			InfrastructureName: "test-infra",
		},
	}
}

// newSourceFailureDomainForRollout describes the single source failure domain
// whose topology matches the MachineSets built by newSourceMachineSetForRollout
// and the mappings recorded in the rollout test migrations.
func newSourceFailureDomainForRollout() configv1.VSpherePlatformFailureDomainSpec {
	return configv1.VSpherePlatformFailureDomainSpec{
		Name:   "source-fd-1",
		Server: "source.example.com",
		Topology: configv1.VSpherePlatformTopology{
			Datacenter:   sourceDatacenter,
			ResourcePool: "rp-source-1",
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

// newCPMSHeldOnUnmanagedNodesForRollout builds a CPMS whose counts look
// complete while its Degraded condition holds the rollout on a control plane
// node the machine controller has not associated with a machine yet.
func newCPMSHeldOnUnmanagedNodesForRollout() *machinev1.ControlPlaneMachineSet {
	cpms := newCPMSForRollout(true, true)
	cpms.Status.Conditions = []metav1.Condition{{
		Type:    "Degraded",
		Status:  metav1.ConditionTrue,
		Reason:  reasonUnmanaged,
		Message: "Found 1 unmanaged node(s)",
	}}
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
	return newSourceMachineSetForRolloutNamed("source-worker-a", replicas)
}

// newSourceMachineSetForRolloutNamed builds a source MachineSet with the given
// name in the source failure domain's datacenter and resource pool, so tests
// can place multiple source MachineSets in one failure domain.
func newSourceMachineSetForRolloutNamed(name string, replicas int32) *machinev1beta1.MachineSet {
	ms := newTargetMachineSetForRollout(name, "source.example.com", replicas)
	// The source MachineSet lives in the source failure domain's datacenter and
	// resource pool so failure-domain matching finds it.
	ms.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: []byte(
		`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1","workspace":{"server":"source.example.com","datacenter":"dc-source","resourcePool":"rp-source-1"}}`)}
	return ms
}

// assertMachineSetReplicas verifies the replicas of a MachineSet touched by
// the rollout, treating nil (MAPI default 1) as a mismatch against want.
func assertMachineSetReplicas(t *testing.T, ctx context.Context, reconciler *VmwareCloudFoundationMigrationReconciler, name string, want int32) {
	t.Helper()
	ms, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting machineset %q: %v", name, err)
	}
	if ms.Spec.Replicas == nil || *ms.Spec.Replicas != want {
		t.Fatalf("machineset %q replicas = %v, want %d", name, ms.Spec.Replicas, want)
	}
}

// newNilReplicaSourceMachineSetForRollout builds a source MachineSet with no
// spec.replicas, which MAPI defaults to 1.
func newNilReplicaSourceMachineSetForRollout(name string) *machinev1beta1.MachineSet {
	ms := newSourceMachineSetForRolloutNamed(name, 1)
	ms.Spec.Replicas = nil
	return ms
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

// rolloutReadyNodeName is the node name carried by machines built by
// newReadyWorkerMachineForRollout; pair them with
// newReadyNodeForRollout(rolloutReadyNodeName) so the machine's NodeRef and
// the node object stay in sync.
const rolloutReadyNodeName = "worker-node-1"

func newReadyWorkerMachineForRollout(machineSetName string) *machinev1beta1.Machine {
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
				Name: rolloutReadyNodeName,
			},
		},
	}
}

// newRolloutMigration returns a migration at the rollout step: preflight has
// recorded the effective mapping the destructive steps key on.
func newRolloutMigration() *migrationv1alpha1.VmwareCloudFoundationMigration {
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 1},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{Name: "target-fd-1", Server: "target.example.com"}},
		},
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
			},
			WorkerMachineSetNames: []string{workerMachineSetName("test-infra", "target-fd-1")},
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
			newTargetMachineSetForRollout(workerMachineSetName("test-infra", "target-fd-1"), "target.example.com", 2),
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
	migration := newRolloutMigration()
	// Step 6 publishes the recorded names before any cleanup.
	reconciler.Client = newMigrationStatusClient(t, migration)

	stalled := make([]string, 0, 2)

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
		newTargetMachineSetForRollout(workerMachineSetName("test-infra", "target-fd-1"), "target.example.com", 2),
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
	manyReconciler.Client = newMigrationStatusClient(t, migration)
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
	recorder := reconciler.Recorder.(*events.FakeRecorder)
	stalled := make([]string, 0, 2)
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, "OldWorkersStalled") {
				stalled = append(stalled, event)
			}
		default:
			return stalled
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
		Recorder:      events.NewFakeRecorder(20),
	}
}

// newMigrationStatusClient builds the controller-runtime fake client a reconciler
// needs to publish status. A reconcile that reaches Step 6 persists
// status.workerMachineSetNames before any cleanup, so that path cannot run
// without one.
func newMigrationStatusClient(t *testing.T, migration *migrationv1alpha1.VmwareCloudFoundationMigration) client.Client {
	t.Helper()
	return clientfake.NewClientBuilder().
		WithScheme(newMigrationTestScheme(t)).
		WithObjects(migration).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
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

// TestScaleDownFailsClosedWhenRecoveredNamesCannotPersist covers the restart
// path where recovered destination MachineSet names must be durably published
// before any source MachineSet is scaled down: a failing status write must
// stop the migration with the source capacity untouched.
func TestScaleDownFailsClosedWhenRecoveredNamesCannotPersist(t *testing.T) {
	ctx := context.Background()
	targetMSName := workerMachineSetName("test-infra", "target-fd-1")

	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForRollout(),
		newTargetMachineSetForRollout(targetMSName, "target.example.com", 1),
		newCPMSUpdatedForRollout([]string{"target-fd-1"}, true),
		newSourceMachineSetForRollout(1),
	})

	start := metav1.Now()
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       migrationv1alpha1.SingletonName,
			Namespace:  migrationv1alpha1.SingletonNamespace,
			Generation: 1,
		},
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
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			StartTime: &start,
			FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return apierrors.NewServerTimeout(schema.GroupResource{Group: "migration.openshift.io", Resource: "vmwarecloudfoundationmigrations"}, "update", 1)
			},
		}).
		Build()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme

	machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
	infraMgr := openshift.NewInfrastructureManager(reconciler.ConfigClient)
	sourceVC, err := infraMgr.GetSourceVCenter(ctx)
	if err != nil {
		t.Fatalf("getting source vCenter: %v", err)
	}
	oldMachineSets, err := machineMgr.GetMachineSetsByVCenter(ctx, sourceVC.Server)
	if err != nil {
		t.Fatalf("listing source MachineSets: %v", err)
	}

	_, err = reconciler.scaleDownSourceMachineSets(ctx, migration, machineMgr, infraMgr, oldMachineSets, sourceVC.Server, "test-infra")
	if err == nil {
		t.Fatal("scaleDownSourceMachineSets succeeded, want error when recovered names cannot be persisted")
	}
	if !strings.Contains(err.Error(), "persisting destination MachineSet names before scale-down") {
		t.Fatalf("scaleDownSourceMachineSets error = %v, want containing %q", err, "persisting destination MachineSet names before scale-down")
	}

	ms, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, "source-worker-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting source machineset: %v", err)
	}
	if ms.Spec.Replicas == nil || *ms.Spec.Replicas != 1 {
		t.Fatalf("source machineset replicas = %v, want 1 (must not scale down before persistence succeeds)", ms.Spec.Replicas)
	}
}

// TestScaleDownRefusesDestinationServer covers the destructive-step guard:
// Steps 6 and 8 are server-keyed, so a source server that is also a configured
// destination must stop the rollout before anything is scaled down or deleted.
func TestScaleDownRefusesDestinationServer(t *testing.T) {
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{Name: "target-fd-1", Server: "target.example.com"}},
		},
	}
	r := &VmwareCloudFoundationMigrationReconciler{}
	_, err := r.scaleDownSourceMachineSets(context.Background(), migration, nil, nil, nil, "target.example.com", "test-infra")
	if err == nil || !strings.Contains(err.Error(), "configured destination failure domain") {
		t.Fatalf("scaleDownSourceMachineSets error = %v, want containing %q", err, "configured destination failure domain")
	}
}

// newDestinationRecoveryMigration is the migration state the destination
// recovery tests start from: every earlier phase complete, the effective
// mapping recorded at preflight, and the destination MachineSet names the
// operator published when it created them.
func newDestinationRecoveryMigration(recorded []string) *migrationv1alpha1.VmwareCloudFoundationMigration {
	start := metav1.Now()
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       migrationv1alpha1.SingletonName,
			Namespace:  migrationv1alpha1.SingletonNamespace,
			Generation: 1,
			Finalizers: []string{migrationv1alpha1.FinalizerName},
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: migrationv1alpha1.MigrationStateRunning,
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
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			StartTime: &start,
			FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
			},
			WorkerMachineSetNames: recorded,
		},
	}
	for _, condType := range []string{
		migrationv1alpha1.ConditionInfrastructurePrepared,
		migrationv1alpha1.ConditionDestinationInitialized,
		migrationv1alpha1.ConditionMultiSiteConfigured,
	} {
		apimeta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionTrue,
			Reason:             migrationv1alpha1.ReasonCompleted,
			Message:            completedMessage,
			ObservedGeneration: 1,
		})
	}
	return migration
}

// newDestinationRecoveryClient seeds the migration client Reconcile reads and
// writes status through, returning it with the scheme it was built with.
func newDestinationRecoveryClient(t *testing.T, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (client.WithWatch, *runtime.Scheme) {
	t.Helper()
	scheme := newMigrationTestScheme(t)
	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
	return fakeClient, scheme
}

// TestReconcileRefusesToRecreateMissingDestinationAfterScaleDown is the
// controller-level regression for a destination MachineSet deleted with its
// Machines and Nodes after the source MachineSets were already scaled to 0:
// recreating it from the remaining source would restore zero capacity and let
// the readiness gate pass with no Machines at all.
func TestReconcileRefusesToRecreateMissingDestinationAfterScaleDown(t *testing.T) {
	ctx := context.Background()
	targetMSName := workerMachineSetName("test-infra", "target-fd-1")
	const sourceMS = "source-worker-a"

	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForRollout(),
		newCPMSUpdatedForRollout([]string{"target-fd-1"}, true),
		newDestinationMachineSetForSource(targetMSName, sourceMS, 1),
		newReadyWorkerMachineForRollout(targetMSName),
		newReadyNodeForRollout(rolloutReadyNodeName),
		newSourceMachineSetForRolloutNamed(sourceMS, 1),
	})
	migration := newDestinationRecoveryMigration([]string{targetMSName})
	fakeClient, scheme := newDestinationRecoveryClient(t, migration)
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)}

	// First reconcile: the recorded destination is ready, so Step 6 scales the
	// source MachineSet to 0 and requeues before deleting it.
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	assertMachineSetReplicas(t, ctx, reconciler, sourceMS, 0)

	// The administrator then deletes the recorded destination and its children.
	machineSets := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace)
	if err := machineSets.Delete(ctx, targetMSName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting destination machineset: %v", err)
	}
	machines := reconciler.MachineClient.MachineV1beta1().Machines(openshift.MachineAPINamespace)
	if err := machines.Delete(ctx, targetMSName+"-machine", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting destination machine: %v", err)
	}
	if err := reconciler.KubeClient.CoreV1().Nodes().Delete(ctx, rolloutReadyNodeName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting destination node: %v", err)
	}

	// A fresh reconciler, as after an operator restart, must not rebuild the
	// destination from sources that no longer carry its replica counts.
	fresh := &VmwareCloudFoundationMigrationReconciler{
		KubeClient:    reconciler.KubeClient,
		ConfigClient:  reconciler.ConfigClient,
		MachineClient: reconciler.MachineClient,
		Client:        fakeClient,
		Scheme:        scheme,
		Recorder:      events.NewFakeRecorder(50),
	}
	for i := 0; i < 2; i++ {
		if _, err := fresh.Reconcile(ctx, req); err == nil {
			t.Fatalf("reconcile %d after the destination was deleted succeeded, want a fail-closed error", i+1)
		}
		persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), persisted); err != nil {
			t.Fatalf("getting persisted migration: %v", err)
		}
		cond := apimeta.FindStatusCondition(persisted.Status.Conditions, migrationv1alpha1.ConditionWorkloadMigrated)
		if cond != nil && cond.Status == metav1.ConditionTrue {
			t.Fatalf("reconcile %d published %s=True after the destination was deleted: %+v", i+1, migrationv1alpha1.ConditionWorkloadMigrated, cond)
		}
	}
	assertMachineSetAbsent(t, ctx, fresh, targetMSName)
}

// TestReconcileCreatesOriginallyZeroReplicaDestination is the control for the
// recovery gate above: a mapped source failure domain that always had zero
// replicas is an explicit zero, so its destination MachineSet is created at
// zero replicas and the migration completes normally.
func TestReconcileCreatesOriginallyZeroReplicaDestination(t *testing.T) {
	ctx := context.Background()
	targetMSName := workerMachineSetName("test-infra", "target-fd-1")

	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForRollout(),
		newCPMSUpdatedForRollout([]string{"target-fd-1"}, true),
		newSourceMachineSetForRolloutNamed("source-worker-a", 0),
	})
	migration := newDestinationRecoveryMigration(nil)
	fakeClient, scheme := newDestinationRecoveryClient(t, migration)
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)}

	// First reconcile creates the destination at the source's explicit zero.
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	assertMachineSetReplicas(t, ctx, reconciler, targetMSName, 0)

	// Nothing else to roll out: the zero-replica copy is ready and the source
	// MachineSets are deleted, so the phase completes.
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatalf("getting persisted migration: %v", err)
	}
	cond := apimeta.FindStatusCondition(persisted.Status.Conditions, migrationv1alpha1.ConditionWorkloadMigrated)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition %s = %+v, want True/%s", migrationv1alpha1.ConditionWorkloadMigrated, cond, migrationv1alpha1.ReasonCompleted)
	}
}
