package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	machinefake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/execution"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

func TestExecutionStopsAfterDeletionDuringPreparation(t *testing.T) {
	for _, between := range []bool{false, true} {
		name := "blocked preparatory read"
		if between {
			name = "between scale-down iterations"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStateRunning)
			m.Finalizers = []string{migrationv1alpha1.FinalizerName}
			// The scale-down coverage gate requires the preflight mapping and
			// recorded worker MachineSets on any migration reaching Step 6.
			m.Status.FailureDomainMappings = []migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
			}
			m.Status.WorkerMachineSetNames = []string{workerMachineSetName("test-infra", "target-fd-1")}
			c := retentionTestClient(t, m)
			staleCache := retentionTestClient(t, m.DeepCopy())
			second := newSourceMachineSetForRollout(2)
			second.Name = sourceMSB
			// Name recovery before scale-down requires every derived destination
			// MachineSet to exist, which the real flow guarantees via Step 1.
			destBase2 := newTargetMachineSetForRollout(workerMachineSetName("test-infra", "target-fd-1")+"-2", "vcenter-target.example.com", 2)
			destBase := newTargetMachineSetForRollout(m.Status.WorkerMachineSetNames[0], "vcenter-target.example.com", 2)
			machines := machinefake.NewClientset(newCPMSForRollout(true, true), newSourceMachineSetForRollout(2), second, destBase, destBase2)
			r := &VmwareCloudFoundationMigrationReconciler{Client: staleCache, APIReader: c, KubeClient: fakekube.NewClientset(), ConfigClient: configfake.NewClientset(newInfrastructureForRollout()), MachineClient: machines, Recorder: events.NewFakeRecorder(20)}
			blocked, resume := make(chan struct{}), make(chan struct{})
			updates := 0
			machines.PrependReactor("update", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
				updates++
				if between && updates == 1 {
					if err := c.Delete(ctx, m); err != nil {
						return true, nil, err
					}
				}
				return false, nil, nil
			})
			if !between {
				firstList := true
				machines.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
					if firstList {
						firstList = false
						close(blocked)
						<-resume
					}
					return false, nil, nil
				})
			}
			done := make(chan error, 1)
			go func() {
				_, err := r.ensureWorkloadMigratedRolloutAndScaleDown(r.stateContext(ctx, m, migrationv1alpha1.MigrationStateRunning), m)
				done <- err
			}()
			if !between {
				<-blocked
				if err := c.Delete(ctx, m); err != nil {
					t.Fatal(err)
				}
				close(resume)
			}
			<-done
			want := 0
			if between {
				want = 1
			}
			if updates != want {
				t.Fatalf("submitted %d scale-down requests, want %d", updates, want)
			}
			latest := m.DeepCopy()
			if err := c.Get(ctx, client.ObjectKeyFromObject(m), latest); err != nil {
				t.Fatal(err)
			}
			if latest.DeletionTimestamp.IsZero() {
				t.Fatal("deletion not observed by test")
			}
		})
	}
}

func TestReconcileStopsAtUncachedHandlerBoundary(t *testing.T) {
	ctx := context.Background()
	m := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStateRunning)
	m.Finalizers = []string{migrationv1alpha1.FinalizerName}
	now := metav1.Now()
	m.Status.StartTime = &now
	cache := retentionTestClient(t, m.DeepCopy())
	live := retentionTestClient(t, m)
	if err := live.Delete(ctx, m); err != nil {
		t.Fatal(err)
	}
	r := &VmwareCloudFoundationMigrationReconciler{Client: cache, APIReader: live, Recorder: events.NewFakeRecorder(10)}
	// No workflow clients are installed: entering the preflight handler would panic.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}); err != nil {
		t.Fatal(err)
	}
	latest := m.DeepCopy()
	if err := cache.Get(ctx, client.ObjectKeyFromObject(m), latest); err != nil {
		t.Fatal(err)
	}
	for _, cond := range latest.Status.Conditions {
		if cond.Reason == migrationv1alpha1.ReasonFailed {
			t.Fatal("deletion reported as migration failure")
		}
	}
}

// TestExecutionContextCheckOutcomes verifies each permission outcome of the
// execution context check: identity or state drift stops execution without
// surfacing an error, while a permission read failure is reported.
func TestExecutionContextCheckOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		mutateLive func(*migrationv1alpha1.VmwareCloudFoundationMigration)
		dropLive   bool
		readErr    error
	}{
		{name: "live object missing", dropLive: true},
		{name: "live UID differs", mutateLive: func(live *migrationv1alpha1.VmwareCloudFoundationMigration) {
			live.UID = types.UID("replacement-live-uid")
		}},
		{name: "live generation bumped", mutateLive: func(live *migrationv1alpha1.VmwareCloudFoundationMigration) {
			live.Generation++
		}},
		{name: "live state pending", mutateLive: func(live *migrationv1alpha1.VmwareCloudFoundationMigration) {
			live.Spec.State = migrationv1alpha1.MigrationStatePending
		}},
		{name: "live state paused", mutateLive: func(live *migrationv1alpha1.VmwareCloudFoundationMigration) {
			live.Spec.State = migrationv1alpha1.MigrationStatePaused
		}},
		{name: "permission read fails", readErr: fmt.Errorf("api server unreachable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			stale := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStateRunning)
			stale.UID = types.UID("stale-uid")
			liveClient := retentionTestClient(t, stale.DeepCopy())
			if tt.mutateLive != nil {
				mutated := stale.DeepCopy()
				tt.mutateLive(mutated)
				liveClient = retentionTestClient(t, mutated)
			}
			if tt.dropLive {
				if err := liveClient.Delete(ctx, stale.DeepCopy()); err != nil {
					t.Fatal(err)
				}
			}
			if tt.readErr != nil {
				liveClient = interceptor.NewClient(liveClient, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return tt.readErr
				}})
			}
			r := &VmwareCloudFoundationMigrationReconciler{Client: liveClient, APIReader: liveClient}
			err := execution.Check(r.stateContext(ctx, stale, migrationv1alpha1.MigrationStateRunning))
			if tt.readErr == nil {
				// Drift stops execution silently; no error is surfaced.
				if !errors.Is(err, errExecutionStopped) {
					t.Fatalf("Check error = %v, want errExecutionStopped", err)
				}
				return
			}
			if err == nil || errors.Is(err, errExecutionStopped) {
				t.Fatalf("Check error = %v, want wrapped read error", err)
			}
			if !errors.Is(err, tt.readErr) {
				t.Fatalf("Check error = %v, want it wrapping %v", err, tt.readErr)
			}
			if !strings.Contains(err.Error(), "reading migration execution permission") {
				t.Fatalf("Check error = %v, want read-failure context", err)
			}
		})
	}
}

// TestReconcileDeniedFromStartWritesNothing runs a full reconcile whose
// permission check denies from the start and asserts no write is submitted to
// the kube client, not even a status update.
func TestReconcileDeniedFromStartWritesNothing(t *testing.T) {
	ctx := context.Background()
	m := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStateRunning)
	m.Finalizers = []string{migrationv1alpha1.FinalizerName}
	now := metav1.Now()
	m.Status.StartTime = &now
	writes := 0
	counting := interceptor.NewClient(retentionTestClient(t, m), interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			writes++
			return underlying.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes++
			return underlying.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, underlying client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes++
			return underlying.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			writes++
			return underlying.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, underlying client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return underlying.Status().Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, underlying client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			writes++
			return underlying.Status().Patch(ctx, obj, patch, opts...)
		},
	})
	live := retentionTestClient(t, m.DeepCopy())
	if err := live.Delete(ctx, m); err != nil {
		t.Fatal(err)
	}
	r := &VmwareCloudFoundationMigrationReconciler{Client: counting, APIReader: live, Recorder: events.NewFakeRecorder(10)}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("reconcile submitted %d writes while execution was denied from the start", writes)
	}
}

func TestPodRestartStopsBetweenDeletes(t *testing.T) {
	ctx := context.Background()
	m := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStateRunning)
	m.Finalizers = []string{migrationv1alpha1.FinalizerName}
	c := retentionTestClient(t, m)
	r := &VmwareCloudFoundationMigrationReconciler{Client: c, APIReader: c}
	kube := fakekube.NewClientset()
	for _, name := range []string{"a", "b"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test", Labels: map[string]string{"app": "test"}}}
		if _, err := kube.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	deletes := 0
	kube.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { deletes++; return false, nil, c.Delete(ctx, m) })
	_, _ = openshift.NewPodManager(kube).DeletePodsByLabel(r.stateContext(ctx, m, migrationv1alpha1.MigrationStateRunning), "test", map[string]string{"app": "test"})
	if deletes != 1 {
		t.Fatalf("submitted %d pod deletions, want 1", deletes)
	}
}
