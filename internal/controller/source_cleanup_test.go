package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/metadata"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	cleanupTestSource = "source.example.com"
	cleanupTestDest   = "dest.example.com"
	// cloudProviderConfigKey mirrors the key the vSphere CCM config is stored
	// under; it is unexported in the openshift package.
	cloudProviderConfigKey = "config"
)

func newCleanupTestMigration() *migrationv1alpha1.VmwareCloudFoundationMigration {
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationv1alpha1.SingletonName,
			Namespace: migrationv1alpha1.SingletonNamespace,
			// The retention finalizer is normally added by the first reconcile;
			// pre-setting it keeps these tests focused on the cleanup flow.
			Finalizers: []string{migrationv1alpha1.FinalizerName},
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
				Name:      "target-creds",
				Namespace: "default",
			},
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
				Name:   "fd-dest",
				Server: cleanupTestDest,
				Topology: configv1.VSpherePlatformTopology{
					Datacenter: "DC1",
				},
			}},
		},
	}
}

func newCleanupTestInfrastructure(servers ...string) *configv1.Infrastructure {
	infra := newInfrastructureForReadyTest(servers)
	infra.UID = types.UID("test-infra-uid")
	infra.Status.InfrastructureName = "test-cluster-abcd"
	return infra
}

func newCleanupTestConfigMap(servers ...string) *corev1.ConfigMap {
	var b strings.Builder
	b.WriteString("global:\n  secretName: vsphere-creds\n  secretNamespace: kube-system\nvcenter:\n")
	for _, s := range servers {
		fmt.Fprintf(&b, "  %s:\n    server: %s\n    datacenters:\n      - DC0\n", s, s)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      openshift.CloudProviderConfigMapName,
			Namespace: openshift.CloudProviderConfigMapNamespace,
		},
		Data: map[string]string{cloudProviderConfigKey: b.String()},
	}
}

func newCleanupTestCredsSecret(servers ...string) *corev1.Secret {
	data := map[string][]byte{}
	for _, s := range servers {
		data[s+".username"] = []byte("admin@" + s)
		data[s+".password"] = []byte("password")
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      openshift.VSphereCredsSecretName,
			Namespace: openshift.VSphereCredsSecretNamespace,
		},
		Data: data,
	}
}

func newCleanupTestTargetSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "target-creds", Namespace: "default"},
		Data: map[string][]byte{
			cleanupTestDest + ".username": []byte("admin@" + cleanupTestDest),
			cleanupTestDest + ".password": []byte("password"),
		},
	}
}

func newCleanupTestReconciler(t *testing.T, migration *migrationv1alpha1.VmwareCloudFoundationMigration, cfgClient *configfake.Clientset, kubeClient *fakekube.Clientset, interceptorFuncs ...interceptor.Funcs) (*VmwareCloudFoundationMigrationReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	builder := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{})
	if len(interceptorFuncs) > 0 {
		builder = builder.WithInterceptorFuncs(interceptorFuncs[0])
	}
	fakeClient := builder.Build()
	return &VmwareCloudFoundationMigrationReconciler{
		Client:       fakeClient,
		Scheme:       scheme,
		KubeClient:   kubeClient,
		ConfigClient: cfgClient,
		Recorder:     events.NewFakeRecorder(10),
	}, fakeClient
}

// cleanupTestInfraServers returns the servers currently in the Infrastructure
// vCenters list.
func cleanupTestInfraServers(t *testing.T, cfgClient *configfake.Clientset) []string {
	t.Helper()
	infra, err := cfgClient.ConfigV1().Infrastructures().Get(context.Background(), openshift.InfrastructureName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting infrastructure: %v", err)
	}
	if infra.Spec.PlatformSpec.VSphere == nil {
		return nil
	}
	servers := make([]string, 0, len(infra.Spec.PlatformSpec.VSphere.VCenters))
	for _, vc := range infra.Spec.PlatformSpec.VSphere.VCenters {
		servers = append(servers, vc.Server)
	}
	return servers
}

// cleanupTestConfigHas reports whether the cloud-provider-config still
// references the given server.
func cleanupTestConfigHas(t *testing.T, kubeClient *fakekube.Clientset, server string) bool {
	t.Helper()
	cm, err := kubeClient.CoreV1().ConfigMaps(openshift.CloudProviderConfigMapNamespace).Get(context.Background(), openshift.CloudProviderConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting cloud-provider-config: %v", err)
	}
	return strings.Contains(cm.Data[cloudProviderConfigKey], server)
}

// cleanupTestCredsHas reports whether vsphere-creds still holds credentials
// for the given server.
func cleanupTestCredsHas(t *testing.T, kubeClient *fakekube.Clientset, server string) bool {
	t.Helper()
	secret, err := kubeClient.CoreV1().Secrets(openshift.VSphereCredsSecretNamespace).Get(context.Background(), openshift.VSphereCredsSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting vsphere-creds: %v", err)
	}
	_, hasUser := secret.Data[server+".username"]
	_, hasPass := secret.Data[server+".password"]
	return hasUser && hasPass
}

// TestEnsureSourceCleanedRetryKeepsDestination covers OCPBUGS-131831: when a
// cleanup step fails after the Infrastructure update and the reconcile is
// retried, the retry must stay bound to the original source vCenter and must
// never remove the destination.
func TestEnsureSourceCleanedRetryKeepsDestination(t *testing.T) {
	ctx := context.Background()
	migration := newCleanupTestMigration()

	// World at first cleanup entry: source and destination both configured.
	cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestSource, cleanupTestDest))
	kubeClient := fakekube.NewSimpleClientset(
		newCleanupTestConfigMap(cleanupTestSource, cleanupTestDest),
		newCleanupTestCredsSecret(cleanupTestSource, cleanupTestDest),
		newCleanupTestTargetSecret(),
	)
	// Inject a vsphere-creds update failure to force a mid-cleanup retry.
	kubeClient.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected vsphere-creds update failure")
	})

	reconciler, fakeClient := newCleanupTestReconciler(t, migration, cfgClient, kubeClient)

	// First attempt: infrastructure and config updates succeed, creds fail.
	if _, err := reconciler.ensureSourceCleaned(ctx, migration); err == nil {
		t.Fatal("expected first cleanup attempt to fail on injected creds error")
	}

	// The source identity must be persisted before any side effect.
	if migration.Status.SourceVCenter != cleanupTestSource {
		t.Fatalf("persisted sourceVCenter = %q, want %q", migration.Status.SourceVCenter, cleanupTestSource)
	}

	// The first attempt correctly removed the source from infrastructure and
	// cloud-provider-config but not from vsphere-creds (step failed).
	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 1 || got[0] != cleanupTestDest {
		t.Fatalf("after first attempt infra servers = %v, want [%s]", got, cleanupTestDest)
	}
	if cleanupTestConfigHas(t, kubeClient, cleanupTestSource) {
		t.Fatal("cloud-provider-config still references source after successful config removal step")
	}
	if !cleanupTestConfigHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("cloud-provider-config lost the destination during first attempt")
	}
	if !cleanupTestCredsHas(t, kubeClient, cleanupTestSource) {
		t.Fatal("vsphere-creds lost source before the failing step; reactor setup wrong")
	}

	// Retry without the injected failure, starting from persisted state.
	cm, err := kubeClient.CoreV1().ConfigMaps(openshift.CloudProviderConfigMapNamespace).Get(ctx, openshift.CloudProviderConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting configmap for retry: %v", err)
	}
	creds, err := kubeClient.CoreV1().Secrets(openshift.VSphereCredsSecretNamespace).Get(ctx, openshift.VSphereCredsSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting vsphere-creds for retry: %v", err)
	}
	retryKube := fakekube.NewSimpleClientset(cm, creds, newCleanupTestTargetSecret())
	retryReconciler := &VmwareCloudFoundationMigrationReconciler{
		Client:       fakeClient,
		Scheme:       reconciler.Scheme,
		KubeClient:   retryKube,
		ConfigClient: cfgClient,
		Recorder:     events.NewFakeRecorder(10),
	}

	// Simulate the operator restart: reload the migration from the cluster so
	// only the persisted status drives the retry.
	retried := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), retried); err != nil {
		t.Fatalf("getting migration for retry: %v", err)
	}
	if retried.Status.SourceVCenter != cleanupTestSource {
		t.Fatalf("persisted sourceVCenter on API object = %q, want %q", retried.Status.SourceVCenter, cleanupTestSource)
	}

	if _, err := retryReconciler.ensureSourceCleaned(ctx, retried); err != nil {
		t.Fatalf("cleanup retry error: %v", err)
	}

	// The destination must survive everywhere.
	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 1 || got[0] != cleanupTestDest {
		t.Fatalf("after retry infra servers = %v, want [%s]; the retry removed the destination", got, cleanupTestDest)
	}
	if !cleanupTestConfigHas(t, retryKube, cleanupTestDest) {
		t.Fatal("cleanup retry removed the destination from cloud-provider-config")
	}
	if cleanupTestConfigHas(t, retryKube, cleanupTestSource) {
		t.Fatal("cleanup retry left the source in cloud-provider-config")
	}
	if !cleanupTestCredsHas(t, retryKube, cleanupTestDest) {
		t.Fatal("cleanup retry removed the destination credentials")
	}
	if cleanupTestCredsHas(t, retryKube, cleanupTestSource) {
		t.Fatal("cleanup retry left the source credentials in vsphere-creds")
	}

	cond := apimeta.FindStatusCondition(retried.Status.Conditions, migrationv1alpha1.ConditionSourceCleaned)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("sourceCleaned condition = %+v, want True", cond)
	}

	if _, err := retryKube.CoreV1().Secrets(migration.Namespace).Get(ctx, metadata.GetMetadataSecretName(migration.Name), metav1.GetOptions{}); err != nil {
		t.Fatalf("getting generated metadata secret: %v", err)
	}
}

// TestEnsureSourceCleanedRetryCompletesConfigAndSecrets covers the retry path
// where the source was already removed from the Infrastructure but the
// cloud-provider-config and vsphere-creds steps never ran: the retry must
// complete those steps using the persisted source identity instead of
// skipping them.
func TestEnsureSourceCleanedRetryCompletesConfigAndSecrets(t *testing.T) {
	ctx := context.Background()
	migration := newCleanupTestMigration()
	// Source already removed from Infrastructure and identity persisted.
	migration.Status.SourceVCenter = cleanupTestSource

	cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestDest))
	kubeClient := fakekube.NewSimpleClientset(
		newCleanupTestConfigMap(cleanupTestSource, cleanupTestDest),
		newCleanupTestCredsSecret(cleanupTestSource, cleanupTestDest),
		newCleanupTestTargetSecret(),
	)

	reconciler, _ := newCleanupTestReconciler(t, migration, cfgClient, kubeClient)

	if _, err := reconciler.ensureSourceCleaned(ctx, migration); err != nil {
		t.Fatalf("cleanup retry error: %v", err)
	}

	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 1 || got[0] != cleanupTestDest {
		t.Fatalf("infra servers = %v, want [%s]", got, cleanupTestDest)
	}
	if cleanupTestConfigHas(t, kubeClient, cleanupTestSource) {
		t.Fatal("retry skipped cloud-provider-config cleanup")
	}
	if !cleanupTestConfigHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("retry removed the destination from cloud-provider-config")
	}
	if cleanupTestCredsHas(t, kubeClient, cleanupTestSource) {
		t.Fatal("retry skipped vsphere-creds cleanup")
	}
	if !cleanupTestCredsHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("retry removed the destination credentials")
	}

	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionSourceCleaned)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("sourceCleaned condition = %+v, want True", cond)
	}
}

// TestEnsureSourceCleanedRefusesDestinationAsSource covers the guard: when
// the resolved source vCenter matches a destination failure domain (e.g. a
// retry deriving VCenters[0] after the source was already removed), cleanup
// must refuse instead of removing the destination.
func TestEnsureSourceCleanedRefusesDestinationAsSource(t *testing.T) {
	ctx := context.Background()
	migration := newCleanupTestMigration()
	// No persisted identity; Infrastructure only holds the destination, so
	// VCenters[0] would resolve to the destination.
	cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestDest))
	kubeClient := fakekube.NewSimpleClientset(
		newCleanupTestConfigMap(cleanupTestDest),
		newCleanupTestCredsSecret(cleanupTestDest),
		newCleanupTestTargetSecret(),
	)

	reconciler, fakeClient := newCleanupTestReconciler(t, migration, cfgClient, kubeClient)

	_, err := reconciler.ensureSourceCleaned(ctx, migration)
	if err == nil {
		t.Fatal("expected cleanup to refuse removing a destination vCenter")
	}
	if !strings.Contains(err.Error(), "destination") {
		t.Fatalf("error = %v, want it to identify the destination guard", err)
	}

	// The refused destination must not be recorded as the source identity.
	after := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), after); err != nil {
		t.Fatalf("getting migration after refusal: %v", err)
	}
	if after.Status.SourceVCenter != "" {
		t.Fatalf("status.sourceVCenter = %q, want empty; the destination must not be recorded as the source", after.Status.SourceVCenter)
	}

	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 1 || got[0] != cleanupTestDest {
		t.Fatalf("infra servers = %v, want [%s]; cleanup must not touch the destination", got, cleanupTestDest)
	}
	if !cleanupTestConfigHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("cleanup removed the destination from cloud-provider-config")
	}
	if !cleanupTestCredsHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("cleanup removed the destination credentials")
	}
}

// TestEnsureSourceCleanedRecoversIdentityFromStatus covers the retry path
// where the in-memory migration has no recorded source identity and
// VCenters[0] resolves to the destination because the source was already
// removed from Infrastructure: cleanup must recover the identity from the
// persisted status instead of refusing or rebinding.
func TestEnsureSourceCleanedRecoversIdentityFromStatus(t *testing.T) {
	ctx := context.Background()

	// The API object records the source identity, but a stale in-memory
	// copy does not, and Infrastructure only holds the destination.
	persisted := newCleanupTestMigration()
	persisted.Status.SourceVCenter = cleanupTestSource

	cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestDest))
	kubeClient := fakekube.NewSimpleClientset(
		newCleanupTestConfigMap(cleanupTestSource, cleanupTestDest),
		newCleanupTestCredsSecret(cleanupTestSource, cleanupTestDest),
		newCleanupTestTargetSecret(),
	)

	reconciler, _ := newCleanupTestReconciler(t, persisted, cfgClient, kubeClient)

	stale := newCleanupTestMigration()
	if _, err := reconciler.ensureSourceCleaned(ctx, stale); err != nil {
		t.Fatalf("cleanup with recovered identity error: %v", err)
	}
	if stale.Status.SourceVCenter != cleanupTestSource {
		t.Fatalf("recovered sourceVCenter = %q, want %q", stale.Status.SourceVCenter, cleanupTestSource)
	}

	// The recovered identity must drive the remaining cleanup steps.
	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 1 || got[0] != cleanupTestDest {
		t.Fatalf("infra servers = %v, want [%s]", got, cleanupTestDest)
	}
	if cleanupTestConfigHas(t, kubeClient, cleanupTestSource) {
		t.Fatal("recovery skipped cloud-provider-config cleanup")
	}
	if !cleanupTestConfigHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("recovery removed the destination from cloud-provider-config")
	}
	if cleanupTestCredsHas(t, kubeClient, cleanupTestSource) {
		t.Fatal("recovery skipped vsphere-creds cleanup")
	}
	if !cleanupTestCredsHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("recovery removed the destination credentials")
	}

	cond := apimeta.FindStatusCondition(stale.Status.Conditions, migrationv1alpha1.ConditionSourceCleaned)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("sourceCleaned condition = %+v, want True", cond)
	}
}

// TestEnsureSourceCleanedPersistFailurePreventsCleanup covers the safety
// gate: when the source identity cannot be persisted to the migration
// status, no cleanup side effects may run — otherwise a later retry could
// resolve the destination as the source.
func TestEnsureSourceCleanedPersistFailurePreventsCleanup(t *testing.T) {
	ctx := context.Background()
	migration := newCleanupTestMigration()

	cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestSource, cleanupTestDest))
	kubeClient := fakekube.NewSimpleClientset(
		newCleanupTestConfigMap(cleanupTestSource, cleanupTestDest),
		newCleanupTestCredsSecret(cleanupTestSource, cleanupTestDest),
		newCleanupTestTargetSecret(),
	)

	// Fail every status subresource write so the identity persist step errors.
	reconciler, _ := newCleanupTestReconciler(t, migration, cfgClient, kubeClient, interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return fmt.Errorf("injected status update failure")
		},
	})

	if _, err := reconciler.ensureSourceCleaned(ctx, migration); err == nil {
		t.Fatal("expected cleanup to fail when the source identity cannot be persisted")
	}

	// No cleanup step may have run while the identity is not durable.
	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 2 {
		t.Fatalf("infra servers = %v, want both source and destination; cleanup ran without a persisted identity", got)
	}
	if !cleanupTestConfigHas(t, kubeClient, cleanupTestSource) || !cleanupTestConfigHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("cleanup modified cloud-provider-config despite failed identity persistence")
	}
	if !cleanupTestCredsHas(t, kubeClient, cleanupTestSource) || !cleanupTestCredsHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("cleanup modified vsphere-creds despite failed identity persistence")
	}
	if migration.Status.SourceVCenter != "" {
		t.Fatalf("in-memory sourceVCenter = %q, want empty after failed persist", migration.Status.SourceVCenter)
	}
}

// TestEnsureSourceCleanedRefusesSourceRebind covers the write-once rule for
// the persisted identity: when the status already records a different source
// vCenter, cleanup must refuse to rebind to the newly resolved one instead
// of silently switching targets.
func TestEnsureSourceCleanedRefusesSourceRebind(t *testing.T) {
	ctx := context.Background()

	// The API object already records a different source identity.
	persisted := newCleanupTestMigration()
	persisted.Status.SourceVCenter = "old.example.com"

	cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestSource, cleanupTestDest))
	kubeClient := fakekube.NewSimpleClientset(
		newCleanupTestConfigMap(cleanupTestSource, cleanupTestDest),
		newCleanupTestCredsSecret(cleanupTestSource, cleanupTestDest),
		newCleanupTestTargetSecret(),
	)

	reconciler, _ := newCleanupTestReconciler(t, persisted, cfgClient, kubeClient)

	// A stale in-memory copy without the recorded identity resolves the
	// source from VCenters[0]; the persisted identity must win.
	stale := newCleanupTestMigration()

	_, err := reconciler.ensureSourceCleaned(ctx, stale)
	if err == nil {
		t.Fatal("expected cleanup to refuse rebinding the source vCenter")
	}
	if !strings.Contains(err.Error(), "refusing to rebind") {
		t.Fatalf("error = %v, want a rebind refusal", err)
	}

	// Nothing may have been cleaned up.
	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 2 {
		t.Fatalf("infra servers = %v, want both source and destination; cleanup ran despite rebind refusal", got)
	}
	if !cleanupTestConfigHas(t, kubeClient, cleanupTestSource) || !cleanupTestConfigHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("cleanup modified cloud-provider-config despite rebind refusal")
	}
	if !cleanupTestCredsHas(t, kubeClient, cleanupTestSource) || !cleanupTestCredsHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("cleanup modified vsphere-creds despite rebind refusal")
	}
}

// TestEnsureSourceCleanedPodRestartFailureRetried covers the final cleanup
// step: when restarting vSphere pods fails, the retry must still complete
// the remaining cleanup and actually restart (delete) the vSphere pods.
func TestEnsureSourceCleanedPodRestartFailureRetried(t *testing.T) {
	ctx := context.Background()
	migration := newCleanupTestMigration()

	ccmPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vsphere-cloud-controller-manager-1",
			Namespace: "openshift-cloud-controller-manager",
			Labels:    map[string]string{"k8s-app": "vsphere-cloud-controller-manager"},
		},
	}

	cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestSource, cleanupTestDest))
	kubeClient := fakekube.NewSimpleClientset(
		newCleanupTestConfigMap(cleanupTestSource, cleanupTestDest),
		newCleanupTestCredsSecret(cleanupTestSource, cleanupTestDest),
		newCleanupTestTargetSecret(),
		ccmPod,
	)
	// Fail pod listing so the pod restart step errors after infrastructure,
	// cloud-provider-config, and credential cleanup already succeeded.
	kubeClient.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected pod list failure")
	})

	reconciler, fakeClient := newCleanupTestReconciler(t, migration, cfgClient, kubeClient)

	// First attempt: everything succeeds until the vSphere pod restart.
	if _, err := reconciler.ensureSourceCleaned(ctx, migration); err == nil {
		t.Fatal("expected first cleanup attempt to fail on injected pod restart error")
	}
	if migration.Status.SourceVCenter != cleanupTestSource {
		t.Fatalf("persisted sourceVCenter = %q, want %q", migration.Status.SourceVCenter, cleanupTestSource)
	}
	if _, err := kubeClient.CoreV1().Pods(ccmPod.Namespace).Get(ctx, ccmPod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("first attempt must not delete pods when the restart step fails: %v", err)
	}

	// Retry without the injected failure, starting from persisted state.
	cm, err := kubeClient.CoreV1().ConfigMaps(openshift.CloudProviderConfigMapNamespace).Get(ctx, openshift.CloudProviderConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting configmap for retry: %v", err)
	}
	creds, err := kubeClient.CoreV1().Secrets(openshift.VSphereCredsSecretNamespace).Get(ctx, openshift.VSphereCredsSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting vsphere-creds for retry: %v", err)
	}
	retryKube := fakekube.NewSimpleClientset(cm, creds, newCleanupTestTargetSecret(), ccmPod)
	retryReconciler := &VmwareCloudFoundationMigrationReconciler{
		Client:       fakeClient,
		Scheme:       reconciler.Scheme,
		KubeClient:   retryKube,
		ConfigClient: cfgClient,
		Recorder:     events.NewFakeRecorder(10),
	}

	// Simulate the operator restart: reload the migration from the cluster so
	// only the persisted status drives the retry.
	retried := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), retried); err != nil {
		t.Fatalf("getting migration for retry: %v", err)
	}

	if _, err := retryReconciler.ensureSourceCleaned(ctx, retried); err != nil {
		t.Fatalf("cleanup retry error: %v", err)
	}

	// The vSphere pod that existed before the failed attempt must be
	// restarted (deleted) by the retry, not skipped as already cleaned up.
	if _, err := retryKube.CoreV1().Pods(ccmPod.Namespace).Get(ctx, ccmPod.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected CCM pod to be deleted on retry, got err %v", err)
	}

	// The destination must survive the retry and the source must be gone.
	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 1 || got[0] != cleanupTestDest {
		t.Fatalf("after retry infra servers = %v, want [%s]", got, cleanupTestDest)
	}
	if !cleanupTestCredsHas(t, retryKube, cleanupTestDest) || cleanupTestCredsHas(t, retryKube, cleanupTestSource) {
		t.Fatal("retry vsphere-creds state is wrong")
	}
}

// TestEnsureSourceCleanedConflictRetriesPersist covers the conflict path of
// the identity persist step: unlike the generic failure injected by
// TestEnsureSourceCleanedPersistFailurePreventsCleanup, a status update
// conflict must be retried by persistSourceVCenter and cleanup must complete
// in the same call.
func TestEnsureSourceCleanedConflictRetriesPersist(t *testing.T) {
	ctx := context.Background()
	migration := newCleanupTestMigration()

	cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestSource, cleanupTestDest))
	kubeClient := fakekube.NewSimpleClientset(
		newCleanupTestConfigMap(cleanupTestSource, cleanupTestDest),
		newCleanupTestCredsSecret(cleanupTestSource, cleanupTestDest),
		newCleanupTestTargetSecret(),
	)

	// Fail only the first status subresource write with a conflict; later
	// writes delegate to the fake client.
	conflict := apierrors.NewConflict(schema.GroupResource{
		Group:    migrationv1alpha1.GroupVersion.Group,
		Resource: "vmwarecloudfoundationmigrations",
	}, migration.Name, fmt.Errorf("injected conflict"))
	updates := 0
	reconciler, fakeClient := newCleanupTestReconciler(t, migration, cfgClient, kubeClient, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			updates++
			if updates == 1 {
				return conflict
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})

	if _, err := reconciler.ensureSourceCleaned(ctx, migration); err != nil {
		t.Fatalf("cleanup must survive a status update conflict: %v", err)
	}
	if updates != 2 {
		t.Fatalf("status subresource updates = %d, want 2 (one conflict, one success)", updates)
	}

	// The identity must be durable on the API object despite the first
	// write conflicting.
	persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatalf("getting migration: %v", err)
	}
	if persisted.Status.SourceVCenter != cleanupTestSource {
		t.Fatalf("persisted sourceVCenter = %q, want %q", persisted.Status.SourceVCenter, cleanupTestSource)
	}

	// Cleanup completed in the same call after the retried persist.
	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 1 || got[0] != cleanupTestDest {
		t.Fatalf("infra servers = %v, want [%s]", got, cleanupTestDest)
	}
	if cleanupTestConfigHas(t, kubeClient, cleanupTestSource) {
		t.Fatal("conflict retry skipped cloud-provider-config cleanup")
	}
	if cleanupTestCredsHas(t, kubeClient, cleanupTestSource) {
		t.Fatal("conflict retry skipped vsphere-creds cleanup")
	}
}

// TestReconcileSourceCleanedFailureRetried drives the full Reconcile loop
// through the source cleanup phase: the first reconcile persists the source
// identity, fails partway through cleanup, and records the failure as a
// condition on the API object; the second reconcile loads the persisted
// status and completes the remaining cleanup.
func TestReconcileSourceCleanedFailureRetried(t *testing.T) {
	ctx := context.Background()

	// Every phase before source cleanup has already completed.
	migration := newCleanupTestMigration()
	migration.Spec.State = migrationv1alpha1.MigrationStateRunning
	for _, condType := range []string{
		migrationv1alpha1.ConditionInfrastructurePrepared,
		migrationv1alpha1.ConditionDestinationInitialized,
		migrationv1alpha1.ConditionMultiSiteConfigured,
		migrationv1alpha1.ConditionWorkloadMigrated,
	} {
		apimeta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionTrue,
			Reason:             migrationv1alpha1.ReasonCompleted,
			Message:            "done",
			ObservedGeneration: migration.Generation,
		})
	}

	cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestSource, cleanupTestDest))
	kubeClient := fakekube.NewSimpleClientset(
		newCleanupTestConfigMap(cleanupTestSource, cleanupTestDest),
		newCleanupTestCredsSecret(cleanupTestSource, cleanupTestDest),
		newCleanupTestTargetSecret(),
	)
	// Fail every secret update so the credentials step errors after the
	// infrastructure and cloud-provider-config cleanup already succeeded.
	kubeClient.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected credentials update failure")
	})

	reconciler, fakeClient := newCleanupTestReconciler(t, migration, cfgClient, kubeClient)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)}

	// First reconcile: fails partway through cleanup.
	if _, err := reconciler.Reconcile(ctx, req); err == nil {
		t.Fatal("expected first reconcile to fail on injected credentials update error")
	}

	// The retry identity must already be durable, and the failure must be
	// recorded as a condition on the API object.
	persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatalf("getting migration after failed reconcile: %v", err)
	}
	if persisted.Status.SourceVCenter != cleanupTestSource {
		t.Fatalf("persisted sourceVCenter = %q, want %q", persisted.Status.SourceVCenter, cleanupTestSource)
	}
	cond := apimeta.FindStatusCondition(persisted.Status.Conditions, migrationv1alpha1.ConditionSourceCleaned)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("sourceCleaned condition = %+v, want False", cond)
	}
	if cond != nil && !strings.Contains(cond.Message, "injected credentials update failure") {
		t.Fatalf("sourceCleaned condition message = %q, want the injected failure", cond.Message)
	}
	if ready := apimeta.FindStatusCondition(persisted.Status.Conditions, migrationv1alpha1.ConditionReady); ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("ready condition = %+v, want False while source cleanup is failing", ready)
	}

	// Partial cleanup: infrastructure and cloud-provider-config were already
	// cleaned, credentials were not.
	if got := cleanupTestInfraServers(t, cfgClient); len(got) != 1 || got[0] != cleanupTestDest {
		t.Fatalf("after failed reconcile infra servers = %v, want [%s]", got, cleanupTestDest)
	}
	if cleanupTestConfigHas(t, kubeClient, cleanupTestSource) {
		t.Fatal("failed reconcile skipped cloud-provider-config cleanup")
	}
	if !cleanupTestCredsHas(t, kubeClient, cleanupTestSource) || !cleanupTestCredsHas(t, kubeClient, cleanupTestDest) {
		t.Fatal("failed reconcile must leave both credentials in vsphere-creds")
	}

	// Second reconcile: retry from the persisted status without the injected
	// failure, using the kube state the first attempt left behind.
	cm, err := kubeClient.CoreV1().ConfigMaps(openshift.CloudProviderConfigMapNamespace).Get(ctx, openshift.CloudProviderConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting configmap for retry: %v", err)
	}
	creds, err := kubeClient.CoreV1().Secrets(openshift.VSphereCredsSecretNamespace).Get(ctx, openshift.VSphereCredsSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting vsphere-creds for retry: %v", err)
	}
	retryKube := fakekube.NewSimpleClientset(cm, creds, newCleanupTestTargetSecret())
	retryReconciler := &VmwareCloudFoundationMigrationReconciler{
		Client:       fakeClient,
		Scheme:       reconciler.Scheme,
		KubeClient:   retryKube,
		ConfigClient: cfgClient,
		Recorder:     events.NewFakeRecorder(10),
	}

	if _, err := retryReconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("retry reconcile error: %v", err)
	}

	// The retry must have completed the cleanup and persisted True.
	reloaded := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), reloaded); err != nil {
		t.Fatalf("getting migration after retry: %v", err)
	}
	cond = apimeta.FindStatusCondition(reloaded.Status.Conditions, migrationv1alpha1.ConditionSourceCleaned)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("sourceCleaned condition after retry = %+v, want True", cond)
	}
	if reloaded.Status.SourceVCenter != cleanupTestSource {
		t.Fatalf("reloaded sourceVCenter = %q, want %q", reloaded.Status.SourceVCenter, cleanupTestSource)
	}
	if !cleanupTestCredsHas(t, retryKube, cleanupTestDest) || cleanupTestCredsHas(t, retryKube, cleanupTestSource) {
		t.Fatal("retry vsphere-creds state is wrong")
	}
	if !cleanupTestConfigHas(t, retryKube, cleanupTestDest) {
		t.Fatal("retry removed the destination from cloud-provider-config")
	}
}
