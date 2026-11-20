package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machineconfigurationv1 "github.com/openshift/api/machineconfiguration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	machinefake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	machineconfigfake "github.com/openshift/client-go/machineconfiguration/clientset/versioned/fake"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

// reconcileScheme builds a scheme with the migration type and the core types
// the controller watches, which is all the plain (non-envtest) Reconcile tests need.
func reconcileScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding migration to scheme: %v", err)
	}
	// SetupWithManager watches PVC metadata to refresh storage plans.
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("adding core/v1 to scheme: %v", err)
	}
	return s
}

// reconcileErrReconciler builds a reconciler backed by a fully in-memory
// client plus empty openshift fakes, so Reconcile's guards run without a real
// cluster. The base client is returned so tests can wrap it with interceptors
// to inject faults.
func reconcileErrReconciler(t *testing.T, objs ...client.Object) (*VmwareCloudFoundationMigrationReconciler, client.Client) {
	t.Helper()
	c := clientfake.NewClientBuilder().
		WithScheme(reconcileScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
	r := &VmwareCloudFoundationMigrationReconciler{
		Client:        c,
		ConfigClient:  configfake.NewClientset(),
		KubeClient:    fakekube.NewClientset(),
		MachineClient: machinefake.NewSimpleClientset(),
		Recorder:      events.NewFakeRecorder(50),
	}
	return r, c
}

// errReconcileFault is a plain error used to fault client calls. It must not
// be context.DeadlineExceeded/Canceled: k8s wait.Interrupted treats those as
// interrupts and drops them inside retry.RetryOnConflict.
var errReconcileFault = errors.New("reconcile test fault")

func faultClient(base client.Client, funcs interceptor.Funcs) client.Client {
	// interceptor.NewClient requires the Watch method, which the in-memory fake
	// client provides even though Build() hands it back as client.Client.
	withWatch, ok := base.(client.WithWatch)
	if !ok {
		panic("reconcile test client does not implement client.WithWatch")
	}
	return interceptor.NewClient(withWatch, funcs)
}

// singletonMigration builds a migration with the given identity fields. The
// UID and Generation are fixed so the execution/retention checks that compare
// object identity behave deterministically.
func singletonMigration(name, ns string, gen int64, finalizers []string, deleteTS *metav1.Time, annotations map[string]string) *migrationv1alpha1.VmwareCloudFoundationMigration { //nolint:unparam
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			UID:               types.UID("uid-test"),
			Generation:        gen,
			Finalizers:        finalizers,
			DeletionTimestamp: deleteTS,
			Annotations:       annotations,
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{State: migrationv1alpha1.MigrationStateRunning},
	}
}

func reconcileRequest(name, ns string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func TestReconcileNotFound(t *testing.T) {
	r, _ := reconcileErrReconciler(t)
	if _, err := r.Reconcile(context.Background(), reconcileRequest(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace)); err != nil {
		t.Fatalf("Reconcile(not found) err = %v, want nil", err)
	}
}

func TestReconcileGetError(t *testing.T) {
	r, c := reconcileErrReconciler(t)
	r.Client = faultClient(c, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errReconcileFault
		},
	})
	_, err := r.Reconcile(context.Background(), reconcileRequest(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace))
	if err == nil || !strings.Contains(err.Error(), "getting migration resource") {
		t.Fatalf("Reconcile(get error) err = %v, want 'getting migration resource'", err)
	}
}

// TestReconcileUnsupportedIdentity covers the singleton gate: a resource with a
// wrong name or namespace is ignored with Accepted=False and no side effects.
func TestReconcileUnsupportedIdentity(t *testing.T) {
	t.Run("unsupported name", func(t *testing.T) {
		m := singletonMigration("other", migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
		r, c := reconcileErrReconciler(t, m)
		if _, err := r.Reconcile(context.Background(), reconcileRequest("other", migrationv1alpha1.SingletonNamespace)); err != nil {
			t.Fatalf("Reconcile err = %v, want nil", err)
		}
		got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
			t.Fatalf("re-getting migration: %v", err)
		}
		cond := apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionAccepted)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != migrationv1alpha1.ReasonUnsupportedName {
			t.Fatalf("Accepted = %+v, want False/UnsupportedName", cond)
		}
	})
	t.Run("unsupported namespace", func(t *testing.T) {
		m := singletonMigration(migrationv1alpha1.SingletonName, "wrong-ns", 1, nil, nil, nil)
		r, c := reconcileErrReconciler(t, m)
		if _, err := r.Reconcile(context.Background(), reconcileRequest(migrationv1alpha1.SingletonName, "wrong-ns")); err != nil {
			t.Fatalf("Reconcile err = %v, want nil", err)
		}
		got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
			t.Fatalf("re-getting migration: %v", err)
		}
		cond := apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionAccepted)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != migrationv1alpha1.ReasonUnsupportedNamespace {
			t.Fatalf("Accepted = %+v, want False/UnsupportedNamespace", cond)
		}
	})
}

func TestReconcileAddFinalizer(t *testing.T) {
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	r, c := reconcileErrReconciler(t, m)
	if _, err := r.Reconcile(context.Background(), reconcileRequest(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace)); err != nil {
		t.Fatalf("Reconcile err = %v, want nil (finalizer add is a no-error short-circuit)", err)
	}
	got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
		t.Fatalf("re-getting migration: %v", err)
	}
	if !containsString(got.Finalizers, migrationv1alpha1.FinalizerName) {
		t.Fatalf("finalizers = %v, want %q", got.Finalizers, migrationv1alpha1.FinalizerName)
	}
}

func TestReconcileFinalizerAddError(t *testing.T) {
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	r, c := reconcileErrReconciler(t, m)
	r.Client = faultClient(c, interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errReconcileFault
		},
	})
	_, err := r.Reconcile(context.Background(), reconcileRequest(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace))
	if err == nil || !strings.Contains(err.Error(), "adding finalizer") {
		t.Fatalf("Reconcile(finalizer add error) err = %v, want 'adding finalizer'", err)
	}
}

func TestReconcileDeletionBlocked(t *testing.T) {
	now := metav1.Now()
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1,
		[]string{migrationv1alpha1.FinalizerName}, &now, nil)
	r, c := reconcileErrReconciler(t, m)
	if _, err := r.Reconcile(context.Background(), reconcileRequest(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace)); err != nil {
		t.Fatalf("Reconcile err = %v, want nil (deletion block is a no-error short-circuit)", err)
	}
	got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
		t.Fatalf("re-getting migration: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionAccepted)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != migrationv1alpha1.ReasonDeletionBlocked {
		t.Fatalf("Accepted = %+v, want False/DeletionBlocked", cond)
	}
	if !containsString(got.Finalizers, migrationv1alpha1.FinalizerName) {
		t.Fatalf("finalizers = %v, want finalizer retained", got.Finalizers)
	}
}

func TestReconcileRemoveFinalizerOnDeletion(t *testing.T) {
	now := metav1.Now()
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1,
		[]string{migrationv1alpha1.FinalizerName}, &now,
		map[string]string{migrationv1alpha1.AnnotationAllowDeletion: "true"})
	r, c := reconcileErrReconciler(t, m)
	if _, err := r.Reconcile(context.Background(), reconcileRequest(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace)); err != nil {
		t.Fatalf("Reconcile err = %v, want nil", err)
	}
	got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	// The admin annotation releases the last finalizer; with a DeletionTimestamp
	// set the fake client garbage-collects the object.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(m), got); !apierrors.IsNotFound(err) {
		t.Fatalf("re-getting migration: err = %v, want NotFound (finalizer removed + deleted)", err)
	}
}

func TestReconcileRemoveFinalizerError(t *testing.T) {
	now := metav1.Now()
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1,
		[]string{migrationv1alpha1.FinalizerName}, &now,
		map[string]string{migrationv1alpha1.AnnotationAllowDeletion: "true"})
	r, c := reconcileErrReconciler(t, m)
	r.Client = faultClient(c, interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errReconcileFault
		},
	})
	_, err := r.Reconcile(context.Background(), reconcileRequest(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace))
	if err == nil || !strings.Contains(err.Error(), "removing finalizer") {
		t.Fatalf("Reconcile(remove finalizer error) err = %v, want 'removing finalizer'", err)
	}
}

// reconcileWorkflow residuals (documented): the remaining uncovered rows are
// defensive sub-branches, not missed main paths:
//   - the updateStatus error returns in the DeletionBlocked (264-266),
//     paused (295-296), resume-from-paused (310-311), and start-time (320-321)
//     early-return blocks; each fires only if the status persist itself faults.
//   - checkMappingForReconcile's stop/error return (335-337): reached on mapping
//     drift, which the failure-domain-mapping tests exercise separately.
//   - the "no handler for condition" guard (348-350): unreachable because every
//     conditionOrder entry has a handler registered in the map.
//   - the pre-handler execution.Check errExecutionStopped return (362-363) and the
//     post-handler execution.Check recheck (365-366): fire only if the migration
//     is deleted/rolled-back mid-handler.
//   - the updateStatus error after a handler runs (374-377): a status-persist fault
//     on top of an already-failed handler.
//
// TestReconcileWorkflowConditionFailure routes the workflow to the first
// condition's handler and makes it fail (a migration with no failure domains
// fails preflight), exercising the shared handler-error, status-persist and
// warning-event rows in reconcileWorkflow.
func TestReconcileWorkflowConditionFailure(t *testing.T) {
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	// No FailureDomains: ensureInfrastructurePrepared (the first condition)
	// fails preflight with "spec.failureDomains must not be empty".
	r, _ := reconcileErrReconciler(t, m)
	rec := r.Recorder.(*events.FakeRecorder)
	baseStatus := *m.Status.DeepCopy()
	_, err := r.reconcileWorkflow(context.Background(), m, baseStatus)
	if err == nil || !strings.Contains(err.Error(), "spec.failureDomains must not be empty") {
		t.Fatalf("reconcileWorkflow err = %v, want 'spec.failureDomains must not be empty'", err)
	}
	cond := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("InfrastructurePrepared = %+v, want False", cond)
	}
	sawWarning := drainRecorder(rec, "ConditionFailed")
	if !sawWarning {
		t.Fatalf("expected a ConditionFailed warning event, recorder had no matching event")
	}
}

// TestReconcileWorkflowComplete sets every condition True so the loop skips all
// handlers and reaches the migration-complete tail.
func TestReconcileWorkflowComplete(t *testing.T) {
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	for _, ct := range conditionOrder {
		apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type: ct, Status: metav1.ConditionTrue, Reason: migrationv1alpha1.ReasonCompleted,
			Message: doneMsg, ObservedGeneration: 1,
		})
	}
	r, _ := reconcileErrReconciler(t, m)
	baseStatus := *m.Status.DeepCopy()
	if _, err := r.reconcileWorkflow(context.Background(), m, baseStatus); err != nil {
		t.Fatalf("reconcileWorkflow(all true) err = %v, want nil", err)
	}
}

// TestReconcileWorkflowPaused covers the not-Running early return: a Paused
// migration records Ready=Paused and returns without touching any handler.
func TestReconcileWorkflowPaused(t *testing.T) {
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	m.Spec.State = migrationv1alpha1.MigrationStatePaused
	r, _ := reconcileErrReconciler(t, m)
	res, err := r.reconcileWorkflow(context.Background(), m, *m.Status.DeepCopy())
	if err != nil {
		t.Fatalf("reconcileWorkflow(paused) err = %v, want nil", err)
	}
	if !res.IsZero() {
		t.Fatalf("reconcileWorkflow(paused) res = %+v, want empty", res)
	}
	cond := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionReady)
	if cond == nil || cond.Reason != migrationv1alpha1.ReasonPaused {
		t.Fatalf("Ready = %+v, want reason Paused", cond)
	}
}

// TestReconcileWorkflowResumeFromPaused covers the Paused->Running resume: the
// Ready condition is cleared from Paused to Progressing, then the workflow
// proceeds to the first condition and fails (0 failure domains).
func TestReconcileWorkflowResumeFromPaused(t *testing.T) {
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type: migrationv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: migrationv1alpha1.ReasonPaused, ObservedGeneration: 1,
	})
	r, _ := reconcileErrReconciler(t, m)
	_, err := r.reconcileWorkflow(context.Background(), m, *m.Status.DeepCopy())
	if err == nil || !strings.Contains(err.Error(), "spec.failureDomains must not be empty") {
		t.Fatalf("reconcileWorkflow(resume) err = %v, want preflight failure", err)
	}
	cond := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionReady)
	if cond == nil || cond.Reason == migrationv1alpha1.ReasonPaused {
		t.Fatalf("Ready reason = %+v, want resumed (not Paused)", cond)
	}
}

// TestReconcileWorkflowExecutionStopped covers the in-flight permission check:
// the client copy drifts to Paused after the reconcile started, so the
// pre-handler execution check returns errExecutionStopped and the workflow
// stops cleanly (no error, no requeue).
func TestReconcileWorkflowExecutionStopped(t *testing.T) {
	paused := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	paused.Spec.State = migrationv1alpha1.MigrationStatePaused
	r, _ := reconcileErrReconciler(t, paused)
	// The passed object is still Running; the client copy it is checked against
	// is Paused, so the execution check stops the run before any handler runs.
	running := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	res, err := r.reconcileWorkflow(context.Background(), running, *running.Status.DeepCopy())
	if err != nil {
		t.Fatalf("reconcileWorkflow(exec stopped) err = %v, want nil", err)
	}
	if !res.IsZero() {
		t.Fatalf("reconcileWorkflow(exec stopped) res = %+v, want empty", res)
	}
}

// TestReconcileWorkflowCompleteStatusError covers the all-conditions-True tail
// when the final status persist fails: StartTime is preset so only the final
// updateStatus runs, and it faults.
func TestReconcileWorkflowCompleteStatusError(t *testing.T) {
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	for _, ct := range conditionOrder {
		apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
			Type: ct, Status: metav1.ConditionTrue, Reason: migrationv1alpha1.ReasonCompleted, Message: doneMsg, ObservedGeneration: 1,
		})
	}
	now := metav1.Now()
	m.Status.StartTime = &now
	r, c := reconcileErrReconciler(t, m)
	r.Client = faultClient(c, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errReconcileFault
		},
	})
	_, err := r.reconcileWorkflow(context.Background(), m, *m.Status.DeepCopy())
	if err == nil || !strings.Contains(err.Error(), "updating migration status") {
		t.Fatalf("reconcileWorkflow(complete status error) err = %v, want 'updating migration status'", err)
	}
}

func TestUpdateStatusError(t *testing.T) {
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type: migrationv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "Progressing", ObservedGeneration: 1,
	})
	r, c := reconcileErrReconciler(t, m)
	r.Client = faultClient(c, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errReconcileFault
		},
	})
	baseStatus := *m.Status.DeepCopy()
	if err := r.updateStatus(context.Background(), m, baseStatus); err == nil || !strings.Contains(err.Error(), "updating migration status") {
		t.Fatalf("updateStatus err = %v, want 'updating migration status'", err)
	}
}

func TestUpdateStatusNoop(t *testing.T) {
	m := singletonMigration(migrationv1alpha1.SingletonName, migrationv1alpha1.SingletonNamespace, 1, nil, nil, nil)
	now := metav1.Now()
	m.Status.LastUpdateTime = &now
	r, c := reconcileErrReconciler(t, m)
	updateCount := 0
	r.Client = faultClient(c, interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			updateCount++
			return nil
		},
	})
	baseStatus := *m.Status.DeepCopy()
	if err := r.updateStatus(context.Background(), m, baseStatus); err != nil {
		t.Fatalf("updateStatus(noop) err = %v, want nil", err)
	}
	if updateCount != 0 {
		t.Fatalf("updateStatus called the status subresource %d times, want 0 (unchanged status)", updateCount)
	}
}

// TestEnsureReadyBlockers drives ensureReady to the blocker-reporting path with
// each of the four blocker families: a non-target vCenter, an unavailable
// operator, a degraded operator, and a degraded machine config pool.
func TestEnsureReadyBlockers(t *testing.T) {
	ctx := context.Background()
	stableOp := newStableReadyTestOperator("etcd")
	// (a) a non-target vCenter is still present in Infrastructure.
	t.Run("non-target vCenter", func(t *testing.T) {
		infra := newInfrastructureForReadyTest([]string{"target.example.com", "leftover.example.com"})
		cfg := configfake.NewClientset(infra, stableOp.DeepCopy())
		mc := machineconfigfake.NewSimpleClientset(newConvergedReadyTestPool("master"))
		r := newReadyTestReconciler(cfg, mc)
		m := newMigrationForReadyTest([]string{"target.example.com"})
		_, err := r.ensureReady(ctx, m)
		assertReadyBlocker(t, m, err, `non-target vCenter "leftover.example.com"`)
	})
	// (b) an operator is unavailable.
	t.Run("operator unavailable", func(t *testing.T) {
		op := stableOp.DeepCopy()
		op.Status.Conditions[0].Status = configv1.ConditionFalse
		infra := newInfrastructureForReadyTest([]string{"target.example.com"})
		cfg := configfake.NewClientset(infra, op)
		mc := machineconfigfake.NewSimpleClientset(newConvergedReadyTestPool("master"))
		r := newReadyTestReconciler(cfg, mc)
		m := newMigrationForReadyTest([]string{"target.example.com"})
		_, err := r.ensureReady(ctx, m)
		assertReadyBlocker(t, m, err, "unavailable=etcd")
	})
	// (c) an operator is degraded.
	t.Run("operator degraded", func(t *testing.T) {
		op := stableOp.DeepCopy()
		for i := range op.Status.Conditions {
			if op.Status.Conditions[i].Type == configv1.OperatorDegraded {
				op.Status.Conditions[i].Status = configv1.ConditionTrue
			}
		}
		infra := newInfrastructureForReadyTest([]string{"target.example.com"})
		cfg := configfake.NewClientset(infra, op)
		mc := machineconfigfake.NewSimpleClientset(newConvergedReadyTestPool("master"))
		r := newReadyTestReconciler(cfg, mc)
		m := newMigrationForReadyTest([]string{"target.example.com"})
		_, err := r.ensureReady(ctx, m)
		assertReadyBlocker(t, m, err, "degraded=etcd")
	})
	// (d) a machine config pool is degraded.
	t.Run("pool degraded", func(t *testing.T) {
		pool := newConvergedReadyTestPool("master")
		pool.Status.Conditions = append(pool.Status.Conditions,
			machineconfigurationv1.MachineConfigPoolCondition{Type: machineconfigurationv1.MachineConfigPoolDegraded, Status: corev1.ConditionTrue})
		infra := newInfrastructureForReadyTest([]string{"target.example.com"})
		cfg := configfake.NewClientset(infra, stableOp.DeepCopy())
		mc := machineconfigfake.NewSimpleClientset(pool)
		r := newReadyTestReconciler(cfg, mc)
		m := newMigrationForReadyTest([]string{"target.example.com"})
		_, err := r.ensureReady(ctx, m)
		assertReadyBlocker(t, m, err, "pools-degraded=master")
	})
}

// drainRecorder non-blockingly drains a FakeRecorder's Events channel (which is
// never closed, so ranging over it blocks) and reports whether any event
// contains wantSubstr.
func drainRecorder(rec *events.FakeRecorder, wantSubstr string) bool {
found:
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, wantSubstr) {
				return true
			}
		default:
			break found
		}
	}
	return false
}

// assertReadyBlocker asserts that ensureReady returned a nil error with a Ready=False
// condition whose message contains wantSubstr.
func assertReadyBlocker(t *testing.T, m *migrationv1alpha1.VmwareCloudFoundationMigration, err error, wantSubstr string) {
	t.Helper()
	if err != nil {
		t.Fatalf("ensureReady err = %v, want nil (blockers requeue, not error)", err)
	}
	cond := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", cond)
	}
	if !strings.Contains(cond.Message, wantSubstr) {
		t.Fatalf("Ready message = %q, want substring %q", cond.Message, wantSubstr)
	}
}

// TestEnsureInfrastructurePreparedAlreadyReady builds a fully-passing preflight
// and confirms ensureInfrastructurePrepared marks the condition complete and
// records the Normal event without any error.
func TestEnsureInfrastructurePreparedAlreadyReady(t *testing.T) {
	e := newNetcheckTestEnv(t)
	migration := newMigrationForPreflight(e.host, e.inv)
	infra := newInfrastructureForPreflight(e.host, e.dcName)
	kube := fakekube.NewClientset(
		newTargetCredentialsSecret("default", "target-vcenter-creds", e.host, e.user, e.pass),
		newTargetCredentialsSecret(openshift.VSphereCredsSecretNamespace, openshift.VSphereCredsSecretName, e.host, e.user, e.pass),
	)
	cfg := configfake.NewClientset(infra, newClusterVersionForPreflight("4.18.0", false), newFeatureGateForPreflight("4.18.0", true))
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), preflightListKinds(),
		newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
		newManagementStateResource("Storage", "cluster", "Removed"),
	)
	rec := events.NewFakeRecorder(10)
	r := &VmwareCloudFoundationMigrationReconciler{
		KubeClient:    kube,
		ConfigClient:  cfg,
		DynamicClient: dyn,
		MachineClient: machinefake.NewSimpleClientset(),
		checkNetworkingViaProbeVMsFunc: func(context.Context, *migrationv1alpha1.VmwareCloudFoundationMigration, *configv1.VSpherePlatformVCenterSpec) error {
			return nil
		},
		Recorder: rec,
	}

	if _, err := r.ensureInfrastructurePrepared(e.ctx, migration); err != nil {
		t.Fatalf("ensureInfrastructurePrepared err = %v, want nil (preflight passes)", err)
	}
	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("InfrastructurePrepared = %+v, want True", cond)
	}
	if !drainRecorder(rec, "Preflight validation passed") {
		t.Fatalf("expected a 'Preflight validation passed' Normal event, recorder had none")
	}
}

// TestSetupWithManager builds a manager from a dummy rest.Config (Complete only
// registers the controller; it never starts the manager or dials the cluster) and
// confirms SetupWithManager wires it without error.
func TestSetupWithManager(t *testing.T) {
	s := reconcileScheme(t)
	mgr, err := manager.New(&rest.Config{Host: "http://127.0.0.1:1"}, manager.Options{Scheme: s})
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	r := &VmwareCloudFoundationMigrationReconciler{Client: mgr.GetClient(), Scheme: s, KubeClient: fakekube.NewClientset()}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}
}
