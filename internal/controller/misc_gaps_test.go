package controller

// Gap coverage for the controller's small helpers: helpers.go
// (getTargetCredentials, syncControllerConfig), retention.go
// (updateRetentionFinalizer), and the failuredomains.go error/mismatch
// branches that the existing table tests do not exercise.

import (
	"context"
	"errors"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	machinefake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/execution"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

// Shared literals reused across controller tests, hoisted for goconst.
const (
	testInfraName     = "cluster" // singleton Infrastructure object name
	testSourceDC      = "DC0"
	testTargetDC      = "DC1"
	sourceDatacenter  = "dc-source"
	folderTemplate    = "/dc/vm/t"
	reasonUnmanaged   = "UnmanagedNodes"
	unreachableVCAddr = "127.0.0.1:1"
	testVCServer      = "10.0.0.5"
	ccoNamespace      = "openshift-cloud-controller-manager"
	tgtCredsName      = "tgt-creds"
	doneMsg           = "done"
	zoneName          = "zone"
)

func TestGetTargetCredentials(t *testing.T) {
	const (
		user = "u"
		pass = "p"
	)
	secret := newTargetCredentialsSecret("default", "creds", testVCServer, user, pass)
	kube := fakekube.NewClientset(secret)

	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{Name: "creds", Namespace: "default"},
		},
	}

	t.Run("success", func(t *testing.T) {
		u, p, err := getTargetCredentials(context.Background(), kube, migration, testVCServer)
		if err != nil {
			t.Fatalf("getTargetCredentials = %v, want nil", err)
		}
		if u != user || p != pass {
			t.Fatalf("got (%q,%q), want (%q,%q)", u, p, user, pass)
		}
	})
	t.Run("unknown server key", func(t *testing.T) {
		_, _, err := getTargetCredentials(context.Background(), kube, migration, "9.9.9.9")
		if err == nil || !strings.Contains(err.Error(), "getting target credentials for 9.9.9.9") {
			t.Fatalf("err = %v, want unknown-server error", err)
		}
	})
	t.Run("empty secret name", func(t *testing.T) {
		m := migration.DeepCopy()
		m.Spec.TargetVCenterCredentialsSecret.Name = ""
		_, _, err := getTargetCredentials(context.Background(), kube, m, testVCServer)
		if err == nil || !strings.Contains(err.Error(), "spec.targetVCenterCredentialsSecret.name must not be empty") {
			t.Fatalf("err = %v, want empty-name error", err)
		}
	})
	t.Run("empty namespace falls back to migration namespace", func(t *testing.T) {
		m := migration.DeepCopy()
		m.Spec.TargetVCenterCredentialsSecret.Namespace = ""
		u, _, err := getTargetCredentials(context.Background(), kube, m, testVCServer)
		if err != nil || u != user {
			t.Fatalf("got u=%q err=%v, want fallback to migration namespace", u, err)
		}
	})
}

func TestSyncControllerConfig(t *testing.T) {
	mcoPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: mcoNamespace, Name: mcoPodPrefix + "abc123"}}
	otherPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: mcoNamespace, Name: "not-mco"}}

	t.Run("deletes mco pods, skips others", func(t *testing.T) {
		kube := fakekube.NewClientset(mcoPod, otherPod)
		if err := syncControllerConfig(context.Background(), kube); err != nil {
			t.Fatalf("syncControllerConfig = %v, want nil", err)
		}
		remaining, _ := kube.CoreV1().Pods(mcoNamespace).List(context.Background(), metav1.ListOptions{})
		if len(remaining.Items) != 1 || remaining.Items[0].Name != "not-mco" {
			names := make([]string, 0, len(remaining.Items))
			for i := range remaining.Items {
				names = append(names, remaining.Items[i].Name)
			}
			t.Fatalf("remaining pods = %v, want [not-mco]", names)
		}
	})
	t.Run("list error", func(t *testing.T) {
		kube := fakekube.NewClientset()
		kube.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		if err := syncControllerConfig(context.Background(), kube); err == nil || !strings.Contains(err.Error(), "listing pods in") {
			t.Fatalf("err = %v, want list error", err)
		}
	})
	t.Run("delete error is non-fatal", func(t *testing.T) {
		kube := fakekube.NewClientset(mcoPod)
		kube.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		// A failed delete is logged and skipped, not returned.
		if err := syncControllerConfig(context.Background(), kube); err != nil {
			t.Fatalf("syncControllerConfig = %v, want nil (delete error is logged)", err)
		}
	})
	t.Run("execution check error", func(t *testing.T) {
		kube := fakekube.NewClientset(mcoPod)
		sentinel := errors.New("check failed")
		faultCtx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		err := syncControllerConfig(faultCtx, kube)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want the execution check error", err)
		}
	})
}

func newMigrationWithScheme(t *testing.T) (*runtime.Scheme, *migrationv1alpha1.VmwareCloudFoundationMigration) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding migration to scheme: %v", err)
	}
	return scheme, &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
	}
}

// The two uncovered retention.go statements are the json.Marshal error branch
// in completionSpecHash (retention.go:29-30) and the isCompletionRecordedForSpec
// error return in updateRetentionFinalizer (retention.go:56-57). Both are
// reachable only if json.Marshal fails on VmwareCloudFoundationMigrationSpec,
// whose fields are all plain serialisable types, so the branches are defensive
// and treated as unreachable here.
func TestUpdateRetentionFinalizer(t *testing.T) {
	scheme, migration := newMigrationWithScheme(t)
	now := metav1.Now()

	t.Run("object not in client (get not-found) is a no-op", func(t *testing.T) {
		c := clientfake.NewClientBuilder().WithScheme(scheme).Build()
		r := &VmwareCloudFoundationMigrationReconciler{Client: c}
		if err := r.updateRetentionFinalizer(context.Background(), migration); err != nil {
			t.Fatalf("updateRetentionFinalizer = %v, want nil (not-found ignored)", err)
		}
	})
	t.Run("adds finalizer when present", func(t *testing.T) {
		c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(migration).WithStatusSubresource(migration).Build()
		r := &VmwareCloudFoundationMigrationReconciler{Client: c}
		if err := r.updateRetentionFinalizer(context.Background(), migration); err != nil {
			t.Fatalf("updateRetentionFinalizer = %v, want nil", err)
		}
		got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(migration), got); err != nil {
			t.Fatalf("re-getting migration: %v", err)
		}
		if !containsString(got.Finalizers, migrationv1alpha1.FinalizerName) {
			t.Fatalf("finalizers = %v, want %q", got.Finalizers, migrationv1alpha1.FinalizerName)
		}
	})
	t.Run("removes finalizer when complete during deletion", func(t *testing.T) {
		m := migration.DeepCopy()
		m.DeletionTimestamp = &now
		m.Finalizers = []string{migrationv1alpha1.FinalizerName}
		m.Status.CompletionTime = &now
		hash, err := completionSpecHash(m.Spec)
		if err != nil {
			t.Fatalf("computing completion hash: %v", err)
		}
		m.Status.CompletionSpecHash = hash
		m.Status.Conditions = []metav1.Condition{{Type: migrationv1alpha1.ConditionReady, Status: metav1.ConditionTrue}}
		c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(m).WithStatusSubresource(m).Build()
		r := &VmwareCloudFoundationMigrationReconciler{Client: c}
		if err := r.updateRetentionFinalizer(context.Background(), m); err != nil {
			t.Fatalf("updateRetentionFinalizer = %v, want nil", err)
		}
		// With the last finalizer removed and a DeletionTimestamp set, the fake
		// client garbage-collects the object.
		got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(m), got); !apierrors.IsNotFound(err) {
			t.Fatalf("re-getting migration: err = %v, want NotFound (finalizer removed + deleted)", err)
		}
	})
	t.Run("no change when deleting and not complete", func(t *testing.T) {
		m := migration.DeepCopy()
		m.DeletionTimestamp = &now
		m.Finalizers = []string{migrationv1alpha1.FinalizerName}
		c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(m).WithStatusSubresource(m).Build()
		r := &VmwareCloudFoundationMigrationReconciler{Client: c}
		if err := r.updateRetentionFinalizer(context.Background(), m); err != nil {
			t.Fatalf("updateRetentionFinalizer = %v, want nil", err)
		}
		got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
			t.Fatalf("re-getting migration: %v", err)
		}
		// Not complete and no allow-deletion annotation: finalizer retained.
		if !containsString(got.Finalizers, migrationv1alpha1.FinalizerName) {
			t.Fatalf("finalizers = %v, want finalizer retained", got.Finalizers)
		}
	})
}

func TestSourceVCenterServer(t *testing.T) {
	t.Run("no mappings", func(t *testing.T) {
		_, err := sourceVCenterServer(fdSpecs("s1"), nil)
		if err == nil || !strings.Contains(err.Error(), "no failure domain mappings recorded") {
			t.Fatalf("err = %v, want no-mappings error", err)
		}
	})
	t.Run("source fd not found", func(t *testing.T) {
		_, err := sourceVCenterServer(fdSpecs("s1"), mappingList([2]string{"s2", "d1"}))
		if err == nil || !strings.Contains(err.Error(), "resolving source failure domain") {
			t.Fatalf("err = %v, want fd-not-found error", err)
		}
	})
	t.Run("source fd has no server", func(t *testing.T) {
		fd := fdSpec("s1")
		_, err := sourceVCenterServer([]configv1.VSpherePlatformFailureDomainSpec{fd}, mappingList([2]string{"s1", "d1"}))
		if err == nil || !strings.Contains(err.Error(), "has no vCenter server configured") {
			t.Fatalf("err = %v, want no-server error", err)
		}
	})
	t.Run("success", func(t *testing.T) {
		fd := fdSpec("s1")
		fd.Server = testVCServer
		server, err := sourceVCenterServer([]configv1.VSpherePlatformFailureDomainSpec{fd}, mappingList([2]string{"s1", "d1"}))
		if err != nil || server != testVCServer {
			t.Fatalf("got (%q,%v), want (10.0.0.5, nil)", server, err)
		}
	})
}

func TestEnsureMappingImmutableGaps(t *testing.T) {
	r := &VmwareCloudFoundationMigrationReconciler{}
	// Mappings are only immutable once the migration has started.
	startTime := metav1.Now()
	newMigration := func(spec, recorded []migrationv1alpha1.FailureDomainMapping) *migrationv1alpha1.VmwareCloudFoundationMigration {
		return &migrationv1alpha1.VmwareCloudFoundationMigration{
			Spec:   migrationv1alpha1.VmwareCloudFoundationMigrationSpec{FailureDomainMappings: spec},
			Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{FailureDomainMappings: recorded, StartTime: &startTime},
		}
	}
	both := mappingList([2]string{"s1", "d1"}, [2]string{"s2", "d2"})

	if err := r.ensureMappingImmutable(newMigration(both, nil)); err != nil {
		t.Fatalf("no recorded mappings should be a no-op, got %v", err)
	}
	if err := r.ensureMappingImmutable(newMigration(both, mappingList([2]string{"s1", "d1"}))); err == nil ||
		!strings.Contains(err.Error(), "got 2 mappings, want 1") {
		t.Fatalf("err = %v, want length-mismatch error", err)
	}
	if err := r.ensureMappingImmutable(newMigration(both, mappingList([2]string{"s1", "d1"}, [2]string{"s3", "d3"}))); err == nil ||
		!strings.Contains(err.Error(), "is not part of the recorded mapping") {
		t.Fatalf("err = %v, want source-not-recorded error", err)
	}
	if err := r.ensureMappingImmutable(newMigration(both, mappingList([2]string{"s1", "d1"}, [2]string{"s2", "dX"}))); err == nil ||
		!strings.Contains(err.Error(), "is mapped to") {
		t.Fatalf("err = %v, want destination-changed error", err)
	}
	if err := r.ensureMappingImmutable(newMigration(both, mappingList([2]string{"s2", "d2"}, [2]string{"s1", "d1"}))); err != nil {
		t.Fatalf("reordered equivalent mappings should be valid, got %v", err)
	}
}

func TestValidateFailureDomainServersGaps(t *testing.T) {
	a, b := fdSpec("a"), fdSpec("b")
	a.Server, b.Server = testVCServer, testVCServer
	if err := validateFailureDomainServers([]configv1.VSpherePlatformFailureDomainSpec{a, b}); err != nil {
		t.Fatalf("same server should be valid, got %v", err)
	}
	a.Server = ""
	if err := validateFailureDomainServers([]configv1.VSpherePlatformFailureDomainSpec{a, b}); err == nil ||
		!strings.Contains(err.Error(), "has no vCenter server configured") {
		t.Fatalf("err = %v, want empty-server error", err)
	}
	a.Server, b.Server = testVCServer, "10.0.0.6"
	if err := validateFailureDomainServers([]configv1.VSpherePlatformFailureDomainSpec{a, b}); err == nil ||
		!strings.Contains(err.Error(), "must use a single vCenter server") {
		t.Fatalf("err = %v, want mismatched-server error", err)
	}
}

func TestValidateSourceVCenterCountGaps(t *testing.T) {
	one := []configv1.VSpherePlatformVCenterSpec{{Server: "s"}}
	if err := validateSourceVCenterCount(newTestInfraWithVCenters(one)); err != nil {
		t.Fatalf("one vCenter should be valid, got %v", err)
	}
	if err := validateSourceVCenterCount(newTestInfraWithVCenters(nil)); err == nil ||
		!strings.Contains(err.Error(), "must have exactly one vCenter, found 0") {
		t.Fatalf("err = %v, want zero-vCenter error", err)
	}
	if err := validateSourceVCenterCount(newTestInfraWithVCenters([]configv1.VSpherePlatformVCenterSpec{{Server: "a"}, {Server: "b"}})); err == nil ||
		!strings.Contains(err.Error(), "must have exactly one vCenter, found 2") {
		t.Fatalf("err = %v, want two-vCenter error", err)
	}
	infra := newTestInfraWithVCenters(one)
	infra.Spec.PlatformSpec.VSphere = nil
	if err := validateSourceVCenterCount(infra); err == nil ||
		!strings.Contains(err.Error(), "has no vSphere platform configuration") {
		t.Fatalf("err = %v, want no-vSphere error", err)
	}
}

func TestValidateMachineSetNameCollisions(t *testing.T) {
	const infraID = "test-infra"
	sourceFDs := []configv1.VSpherePlatformFailureDomainSpec{{Name: "s1", Server: testVCServer}}
	destFDs := []configv1.VSpherePlatformFailureDomainSpec{{Name: "d1"}}
	mappings := mappingList([2]string{"s1", "d1"})

	newReconciler := func(objects ...runtime.Object) *VmwareCloudFoundationMigrationReconciler {
		ms := make([]runtime.Object, 0, len(objects))
		ms = append(ms, objects...)
		return &VmwareCloudFoundationMigrationReconciler{
			KubeClient:    fakekube.NewClientset(),
			MachineClient: machinefake.NewSimpleClientset(ms...),
			DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		}
	}

	t.Run("listing machinesets error", func(t *testing.T) {
		r := newReconciler()
		machineClientset(t, r).PrependReactor("list", "machinesets", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		if err := r.validateMachineSetNameCollisions(context.Background(), infraID, mappings, sourceFDs, destFDs); err == nil ||
			!strings.Contains(err.Error(), "listing machinesets in") {
			t.Fatalf("err = %v, want list error", err)
		}
	})
	t.Run("destination fd not found", func(t *testing.T) {
		r := newReconciler()
		badMappings := mappingList([2]string{"s1", "no-such-dest"})
		if err := r.validateMachineSetNameCollisions(context.Background(), infraID, badMappings, sourceFDs, destFDs); err == nil ||
			!strings.Contains(err.Error(), "resolving destination failure domain") {
			t.Fatalf("err = %v, want dest-not-found error", err)
		}
	})
	t.Run("source fd not found", func(t *testing.T) {
		r := newReconciler()
		badMappings := mappingList([2]string{"no-such-source", "d1"})
		if err := r.validateMachineSetNameCollisions(context.Background(), infraID, badMappings, sourceFDs, destFDs); err == nil ||
			!strings.Contains(err.Error(), "resolving source failure domain") {
			t.Fatalf("err = %v, want source-not-found error", err)
		}
	})
	t.Run("collides with existing machineset", func(t *testing.T) {
		base := workerMachineSetName(infraID, "d1")
		existing := &machinev1beta1.MachineSet{ObjectMeta: metav1.ObjectMeta{Name: base, Namespace: openshift.MachineAPINamespace}}
		r := newReconciler(existing)
		if err := r.validateMachineSetNameCollisions(context.Background(), infraID, mappings, sourceFDs, destFDs); err == nil ||
			!strings.Contains(err.Error(), "collides with existing MachineSet") {
			t.Fatalf("err = %v, want existing-collision error", err)
		}
	})
	t.Run("generated names overlap between mappings", func(t *testing.T) {
		// Two mappings to the same destination failure domain generate the same
		// base name -> overlap error.
		r := newReconciler()
		overlapping := mappingList([2]string{"s1", "d1"}, [2]string{"s2", "d1"})
		moreSourceFDs := append(append([]configv1.VSpherePlatformFailureDomainSpec{}, sourceFDs...), configv1.VSpherePlatformFailureDomainSpec{Name: "s2", Server: testVCServer})
		if err := r.validateMachineSetNameCollisions(context.Background(), infraID, overlapping, moreSourceFDs, destFDs); err == nil ||
			!strings.Contains(err.Error(), "overlap") {
			t.Fatalf("err = %v, want overlap error", err)
		}
	})
	t.Run("happy path no collision", func(t *testing.T) {
		r := newReconciler()
		if err := r.validateMachineSetNameCollisions(context.Background(), infraID, mappings, sourceFDs, destFDs); err != nil {
			t.Fatalf("validateMachineSetNameCollisions = %v, want nil", err)
		}
	})
}

func TestMatchSourceMachineSetFailureDomain(t *testing.T) {
	ms := func(providerSpecJSON string) *machinev1beta1.MachineSet {
		m := &machinev1beta1.MachineSet{ObjectMeta: metav1.ObjectMeta{Name: "ms-1"}}
		m.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: []byte(providerSpecJSON)}
		return m
	}
	fd := configv1.VSpherePlatformFailureDomainSpec{
		Name:   "s1",
		Server: "source.example.com",
		Topology: configv1.VSpherePlatformTopology{
			Datacenter:   sourceDatacenter,
			ResourcePool: "rp-source-1",
		},
	}
	t.Run("unparseable provider spec", func(t *testing.T) {
		_, _, _, err := matchSourceMachineSetFailureDomain(ms(`not-json`), []configv1.VSpherePlatformFailureDomainSpec{fd})
		if err == nil || !strings.Contains(err.Error(), "extracting provider spec") {
			t.Fatalf("err = %v, want extract error", err)
		}
	})
	t.Run("workspace is nil", func(t *testing.T) {
		_, _, _, err := matchSourceMachineSetFailureDomain(ms(`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1"}`), []configv1.VSpherePlatformFailureDomainSpec{fd})
		if err == nil || !strings.Contains(err.Error(), "workspace is nil") {
			t.Fatalf("err = %v, want workspace-nil error", err)
		}
	})
	t.Run("no matching failure domain", func(t *testing.T) {
		ms := ms(`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1","workspace":{"server":"source.example.com","datacenter":"other","resourcePool":"rp"}}`)
		hasReplicas, _, matchedFD, err := matchSourceMachineSetFailureDomain(ms, []configv1.VSpherePlatformFailureDomainSpec{fd})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !hasReplicas || matchedFD != nil {
			t.Fatalf("hasReplicas=%v matchedFD=%v, want (true, nil) for no match", hasReplicas, matchedFD)
		}
	})
}

func containsString(list []string, target string) bool { //nolint:unparam
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}
