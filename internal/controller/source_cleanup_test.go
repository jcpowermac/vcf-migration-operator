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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
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
			Namespace: "default",
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

func newCleanupTestReconciler(t *testing.T, migration *migrationv1alpha1.VmwareCloudFoundationMigration, cfgClient *configfake.Clientset, kubeClient *fakekube.Clientset) (*VmwareCloudFoundationMigrationReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
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

	if _, err := retryKube.CoreV1().Secrets("default").Get(ctx, metadata.GetMetadataSecretName(migration.Name), metav1.GetOptions{}); err != nil {
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

	reconciler, _ := newCleanupTestReconciler(t, migration, cfgClient, kubeClient)

	_, err := reconciler.ensureSourceCleaned(ctx, migration)
	if err == nil {
		t.Fatal("expected cleanup to refuse removing a destination vCenter")
	}
	if !strings.Contains(err.Error(), "destination") {
		t.Fatalf("error = %v, want it to identify the destination guard", err)
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
