package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func completionHashForTest(t interface {
	Helper()
	Fatal(...any)
}, spec migrationv1alpha1.VmwareCloudFoundationMigrationSpec) string {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func retentionTestClient(t *testing.T, migration *migrationv1alpha1.VmwareCloudFoundationMigration) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(migration).WithStatusSubresource(migration).Build()
}

func TestRetentionRejectsStaleCompletion(t *testing.T) {
	for _, evidence := range []string{"timestamp", "stale ready", "legacy current ready", "matching spec after deletion generation bump", "changed spec", "hash without ready"} {
		t.Run(evidence, func(t *testing.T) {
			ctx := context.Background()
			m := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStatePending)
			m.Generation = 2
			m.Finalizers = []string{migrationv1alpha1.FinalizerName, "test/other"}
			now := metav1.Now()
			m.DeletionTimestamp = &now
			if evidence == "timestamp" || evidence == "hash without ready" {
				m.Status.CompletionTime = &now
			} else {
				generation := int64(1)
				if evidence == "legacy current ready" {
					generation = 2
				}
				apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: migrationv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed", ObservedGeneration: generation})
			}
			if evidence == "matching spec after deletion generation bump" || evidence == "changed spec" || evidence == "hash without ready" {
				m.Status.CompletionTime = &now
				m.Status.CompletionSpecHash = completionHashForTest(t, m.Spec)
				if evidence == "changed spec" {
					m.Spec.State = migrationv1alpha1.MigrationStatePaused
				}
			}
			c := retentionTestClient(t, m)
			r := &VmwareCloudFoundationMigrationReconciler{Client: c, Recorder: events.NewFakeRecorder(10)}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
				t.Fatal(err)
			}
			wantHeld := evidence != "matching spec after deletion generation bump"
			if got := controllerutil.ContainsFinalizer(m, migrationv1alpha1.FinalizerName); got != wantHeld {
				t.Fatalf("retention held = %v, want %v", got, wantHeld)
			}
		})
	}
}

func TestUpdateStatusRejectsStaleCompletionWriter(t *testing.T) {
	ctx := context.Background()
	m := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStateRunning)
	m.Generation = 2
	c := retentionTestClient(t, m)
	stale := m.DeepCopy()
	stale.Generation = 1
	base := stale.Status.DeepCopy()
	now := metav1.Now()
	stale.Status.CompletionTime = &now
	stale.Status.CompletionSpecHash = completionHashForTest(t, stale.Spec)
	apimeta.SetStatusCondition(&stale.Status.Conditions, metav1.Condition{Type: migrationv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed", ObservedGeneration: 1})
	r := &VmwareCloudFoundationMigrationReconciler{Client: c}
	if err := r.updateStatus(ctx, stale, *base); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
		t.Fatal(err)
	}
	if m.Status.CompletionSpecHash != "" {
		t.Fatal("stale writer published completion hash")
	}
	if m.Status.CompletionTime != nil {
		t.Fatal("stale writer published completion timestamp")
	}
}

func TestUpdateStatusPublishesCompletionHash(t *testing.T) {
	ctx := context.Background()
	m := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStateRunning)
	m.Generation = 2
	c := retentionTestClient(t, m)
	base := m.Status.DeepCopy()
	now := metav1.Now()
	m.Status.CompletionTime = &now
	apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: migrationv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed", ObservedGeneration: m.Generation})
	r := &VmwareCloudFoundationMigrationReconciler{Client: c}
	if err := r.updateStatus(ctx, m, *base); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
		t.Fatal(err)
	}
	if m.Status.CompletionSpecHash != completionHashForTest(t, m.Spec) {
		t.Fatal("current completion did not bind spec hash")
	}
}

func TestRetentionFinalizerConflicts(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "preserves concurrently added finalizer"
		if revoke {
			name = "rechecks revoked handoff"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStatePending)
			if revoke {
				now := metav1.Now()
				m.DeletionTimestamp = &now
				m.Finalizers = []string{migrationv1alpha1.FinalizerName, "test/other"}
				m.Annotations = map[string]string{migrationv1alpha1.AnnotationAllowDeletion: "true"}
			}
			c := retentionTestClient(t, m)
			injected := false
			wrapped := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, underlying client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if !injected {
					injected = true
					latest := m.DeepCopy()
					if err := c.Get(ctx, client.ObjectKeyFromObject(m), latest); err != nil {
						return err
					}
					if revoke {
						delete(latest.Annotations, migrationv1alpha1.AnnotationAllowDeletion)
					} else {
						latest.Finalizers = append(latest.Finalizers, "test/other")
					}
					if err := c.Update(ctx, latest); err != nil {
						return err
					}
				}
				return underlying.Patch(ctx, obj, patch, opts...)
			}})
			r := &VmwareCloudFoundationMigrationReconciler{Client: wrapped, Recorder: events.NewFakeRecorder(10)}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}); err != nil {
				t.Fatal(err)
			}
			if !injected {
				t.Fatal("conflict was not injected")
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
				t.Fatal(err)
			}
			if !controllerutil.ContainsFinalizer(m, "test/other") {
				t.Fatal("lost concurrent finalizer")
			}
			if !controllerutil.ContainsFinalizer(m, migrationv1alpha1.FinalizerName) {
				t.Fatal("lost retention after revoked handoff")
			}
		})
	}
}
