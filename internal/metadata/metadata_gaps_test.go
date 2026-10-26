package metadata

import (
	"context"
	"errors"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekubeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

// Shared literals across metadata tests, hoisted for goconst.
const (
	primaryVCenter   = "primary.example.com"
	terraformVSphere = "vsphere"
	adminCreds       = "admin:secret"
)

// testMetadata returns a fully-populated ClusterMetadata for SaveToSecret tests.
func testMetadata() *ClusterMetadata {
	return &ClusterMetadata{
		ClusterName: "cluster",
		ClusterID:   "uid-1",
		InfraID:     "infra-1",
		VSphere: &VSphereMetadata{
			VCenter:           primaryVCenter,
			Username:          "admin",
			Password:          "secret",
			TerraformPlatform: terraformVSphere,
			VCenters: []VCenter{
				{VCenter: primaryVCenter, Username: "admin", Password: "secret"},
			},
		},
	}
}

func TestGenerateMetadataErrors(t *testing.T) {
	ctx := context.Background()
	m := NewMetadataManager(fakekubeclient.NewSimpleClientset())

	t.Run("infrastructure status has no InfrastructureName", func(t *testing.T) {
		infra := &configv1.Infrastructure{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Status:     configv1.InfrastructureStatus{},
		}
		fds := []configv1.VSpherePlatformFailureDomainSpec{
			{Name: "fd-0", Server: primaryVCenter, Topology: configv1.VSpherePlatformTopology{Datacenter: "DC"}},
		}
		_, err := m.GenerateMetadata(ctx, fds, infra, map[string]string{primaryVCenter: adminCreds})
		if err == nil || !strings.Contains(err.Error(), "infrastructure status has no InfrastructureName") {
			t.Errorf("error = %v, want InfrastructureName message", err)
		}
	})

	t.Run("primary vcenter credentials incomplete", func(t *testing.T) {
		infra := &configv1.Infrastructure{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Status:     configv1.InfrastructureStatus{InfrastructureName: "infra-1"},
		}
		// An empty server with valid creds parses fine, so the primary
		// VCenter field stays empty and trips the incomplete check.
		fds := []configv1.VSpherePlatformFailureDomainSpec{
			{Name: "fd-0", Server: "", Topology: configv1.VSpherePlatformTopology{Datacenter: "DC"}},
		}
		_, err := m.GenerateMetadata(ctx, fds, infra, map[string]string{"": adminCreds})
		if err == nil || !strings.Contains(err.Error(), "primary vcenter credentials are incomplete") {
			t.Errorf("error = %v, want incomplete creds message", err)
		}
	})
}

// TestSaveToSecretErrors covers the update/create/get error branches, the two
// execution-check error branches, and the nil-labels initialization.
//
// Two branches are left uncovered and are defensive:
//   - the "no vcenters available" return (metadata.go:150-151) — the first
//     failure domain always appends a vcenter, so VCenters is never empty past
//     the primary-credentials check.
//   - the json.Marshal error (metadata.go:167-168) — all ClusterMetadata fields
//     are plain JSON-encodable types, so marshal failure is not reachable.
func TestSaveToSecretErrors(t *testing.T) {
	ctx := context.Background()
	ns := "mig-ns"
	name := "test-metadata"
	md := testMetadata()

	t.Run("existing secret with nil labels initializes map", func(t *testing.T) {
		existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}} // no labels
		client := fakekubeclient.NewSimpleClientset(existing)
		m := NewMetadataManager(client)
		if err := m.SaveToSecret(ctx, md, ns, name); err != nil {
			t.Fatalf("SaveToSecret: %v", err)
		}
		got, err := client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get secret: %v", err)
		}
		if got.Labels["migration.openshift.io/metadata"] != "true" {
			t.Errorf("expected metadata label set, got %v", got.Labels)
		}
	})

	t.Run("update error", func(t *testing.T) {
		existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		client := fakekubeclient.NewSimpleClientset(existing)
		client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("update sentinel")
		})
		m := NewMetadataManager(client)
		err := m.SaveToSecret(ctx, md, ns, name)
		if err == nil || !strings.Contains(err.Error(), "updating metadata Secret") {
			t.Fatalf("error = %v, want update error message", err)
		}
	})

	t.Run("create error", func(t *testing.T) {
		client := fakekubeclient.NewSimpleClientset() // no existing secret
		client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("create sentinel")
		})
		m := NewMetadataManager(client)
		err := m.SaveToSecret(ctx, md, ns, name)
		if err == nil || !strings.Contains(err.Error(), "creating metadata Secret") {
			t.Fatalf("error = %v, want create error message", err)
		}
	})

	t.Run("get error non not-found", func(t *testing.T) {
		client := fakekubeclient.NewSimpleClientset()
		client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("get sentinel")
		})
		m := NewMetadataManager(client)
		err := m.SaveToSecret(ctx, md, ns, name)
		if err == nil || !strings.Contains(err.Error(), "getting metadata Secret") {
			t.Fatalf("error = %v, want get error message", err)
		}
	})

	t.Run("execution check error in update path", func(t *testing.T) {
		sentinel := errors.New("exec sentinel")
		execCtx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		client := fakekubeclient.NewSimpleClientset(existing)
		m := NewMetadataManager(client)
		if err := m.SaveToSecret(execCtx, md, ns, name); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("execution check error in create path", func(t *testing.T) {
		sentinel := errors.New("exec sentinel")
		execCtx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		client := fakekubeclient.NewSimpleClientset() // no secret → create path
		m := NewMetadataManager(client)
		if err := m.SaveToSecret(execCtx, md, ns, name); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})
}
