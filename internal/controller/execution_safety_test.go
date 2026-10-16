package controller

import (
	"context"
	"testing"

	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	machinefake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
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
			c := retentionTestClient(t, m)
			staleCache := retentionTestClient(t, m.DeepCopy())
			second := newSourceMachineSetForRollout(2)
			second.Name = "source-worker-b"
			machines := machinefake.NewClientset(newCPMSForRollout(true, true), newSourceMachineSetForRollout(2), second)
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
				_, err := r.ensureWorkloadMigratedRolloutAndScaleDown(r.executionContext(ctx, m), m)
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
	_, _ = openshift.NewPodManager(kube).DeletePodsByLabel(r.executionContext(ctx, m), "test", map[string]string{"app": "test"})
	if deletes != 1 {
		t.Fatalf("submitted %d pod deletions, want 1", deletes)
	}
}
