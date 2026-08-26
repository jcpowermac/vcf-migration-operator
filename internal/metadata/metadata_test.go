package metadata

import (
	"context"
	"encoding/json"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fakekubeclient "k8s.io/client-go/kubernetes/fake"
)

func TestGenerateMetadata(t *testing.T) {
	tests := []struct {
		name           string
		failureDomains []configv1.VSpherePlatformFailureDomainSpec
		infra          *configv1.Infrastructure
		credentials    map[string]string
		wantErr        bool
		wantInfraID    string
		wantVCCount    int
		wantPrimary    string
	}{
		{
			name:    "nil infrastructure",
			infra:   nil,
			wantErr: true,
		},
		{
			name: "empty infrastructure name",
			infra: &configv1.Infrastructure{
				Status: configv1.InfrastructureStatus{InfrastructureName: ""},
			},
			wantErr: true,
		},
		{
			name: "no failure domains",
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: types.UID("test-uid")},
				Status:     configv1.InfrastructureStatus{InfrastructureName: "my-cluster-abc"},
			},
			wantErr: true,
		},
		{
			name: "single failure domain",
			failureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "fd1",
					Server: "vcenter.example.com",
					Topology: configv1.VSpherePlatformTopology{
						Datacenter:     "dc1",
						ComputeCluster: "/dc1/host/cluster1",
						Datastore:      "/dc1/datastore/ds1",
						Networks:       []string{"VM Network"},
						ResourcePool:   "/dc1/host/cluster1/Resources",
						Folder:         "/dc1/vm/my-cluster",
					},
				},
			},
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: types.UID("test-uid")},
				Status:     configv1.InfrastructureStatus{InfrastructureName: "my-cluster-abc"},
			},
			credentials: map[string]string{
				"vcenter.example.com": "admin:secret",
			},
			wantInfraID: "my-cluster-abc",
			wantVCCount: 1,
			wantPrimary: "vcenter.example.com",
		},
		{
			name: "multiple distinct vcenters",
			failureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd1", Server: "vc1.example.com", Topology: configv1.VSpherePlatformTopology{Datacenter: "dc1"}},
				{Name: "fd2", Server: "vc2.example.com", Topology: configv1.VSpherePlatformTopology{Datacenter: "dc2"}},
			},
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: types.UID("uid2a")},
				Status:     configv1.InfrastructureStatus{InfrastructureName: "infra2a"},
			},
			credentials: map[string]string{
				"vc1.example.com": "user1:pass1",
				"vc2.example.com": "user2:pass2",
			},
			wantInfraID: "infra2a",
			wantVCCount: 2,
			wantPrimary: "vc1.example.com",
		},
		{
			name: "deduplicates vcenters by server",
			failureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd1", Server: "vc1.example.com", Topology: configv1.VSpherePlatformTopology{Datacenter: "dc1"}},
				{Name: "fd2", Server: "vc1.example.com", Topology: configv1.VSpherePlatformTopology{Datacenter: "dc2"}},
			},
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: types.UID("uid2")},
				Status:     configv1.InfrastructureStatus{InfrastructureName: "infra2"},
			},
			credentials: map[string]string{
				"vc1.example.com": "user1:pass1",
			},
			wantInfraID: "infra2",
			wantVCCount: 1,
			wantPrimary: "vc1.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewMetadataManager(fakekubeclient.NewClientset())
			got, err := mgr.GenerateMetadata(context.Background(), tt.failureDomains, tt.infra, tt.credentials)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("GenerateMetadata: expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("GenerateMetadata: %v", err)
			}
			if got.InfraID != tt.wantInfraID {
				t.Errorf("InfraID = %q, want %q", got.InfraID, tt.wantInfraID)
			}
			if got.VSphere == nil {
				t.Fatal("VSphere metadata is nil")
			}
			if len(got.VSphere.VCenters) != tt.wantVCCount {
				t.Errorf("VCenters count = %d, want %d", len(got.VSphere.VCenters), tt.wantVCCount)
			}
			if got.VSphere.VCenter != tt.wantPrimary {
				t.Errorf("VCenter = %q, want %q", got.VSphere.VCenter, tt.wantPrimary)
			}
			if got.VSphere.TerraformPlatform != "vsphere" {
				t.Errorf("TerraformPlatform = %q, want %q", got.VSphere.TerraformPlatform, "vsphere")
			}
		})
	}
}

func TestGenerateMetadataCredentialErrors(t *testing.T) {
	infra := &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: types.UID("test-uid")},
		Status:     configv1.InfrastructureStatus{InfrastructureName: "my-cluster-abc"},
	}
	failureDomains := []configv1.VSpherePlatformFailureDomainSpec{
		{Name: "fd1", Server: "vc1.example.com", Topology: configv1.VSpherePlatformTopology{Datacenter: "dc1"}},
	}

	tests := []struct {
		name        string
		credentials map[string]string
	}{
		{
			name:        "missing credentials for server",
			credentials: map[string]string{},
		},
		{
			name: "malformed credentials without colon",
			credentials: map[string]string{
				"vc1.example.com": "user1",
			},
		},
		{
			name: "empty username after parsing",
			credentials: map[string]string{
				"vc1.example.com": ":pass1",
			},
		},
		{
			name: "empty password after parsing",
			credentials: map[string]string{
				"vc1.example.com": "user1:",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewMetadataManager(fakekubeclient.NewClientset())
			if _, err := mgr.GenerateMetadata(context.Background(), failureDomains, infra, tt.credentials); err == nil {
				t.Fatal("GenerateMetadata: expected error, got nil")
			}
		})
	}
}

func TestGenerateMetadataJSONContract(t *testing.T) {
	mgr := NewMetadataManager(fakekubeclient.NewClientset())
	md, err := mgr.GenerateMetadata(
		context.Background(),
		[]configv1.VSpherePlatformFailureDomainSpec{
			{
				Name:   "fd1",
				Server: "vc1.example.com",
				Topology: configv1.VSpherePlatformTopology{
					Datacenter:     "dc1",
					ComputeCluster: "/dc1/host/cluster1",
					Datastore:      "/dc1/datastore/ds1",
					Networks:       []string{"VM Network"},
					ResourcePool:   "/dc1/host/cluster1/Resources",
					Folder:         "/dc1/vm/my-cluster",
				},
			},
		},
		&configv1.Infrastructure{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: types.UID("uid4")},
			Status:     configv1.InfrastructureStatus{InfrastructureName: "infra4"},
		},
		map[string]string{"vc1.example.com": "user1:pass1"},
	)
	if err != nil {
		t.Fatalf("GenerateMetadata: %v", err)
	}

	data, err := json.Marshal(md)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	assertMapKeys(t, payload, "clusterName", "clusterID", "infraID", "vsphere")

	vsphereRaw, ok := payload["vsphere"]
	if !ok {
		t.Fatal("vsphere key missing")
	}
	vspherePayload, ok := vsphereRaw.(map[string]any)
	if !ok {
		t.Fatalf("vsphere = %T, want map[string]any", vsphereRaw)
	}
	assertMapKeys(t, vspherePayload, "vCenter", "username", "password", "terraform_platform", "VCenters")

	vcentersRaw, ok := vspherePayload["VCenters"]
	if !ok {
		t.Fatal("VCenters key missing")
	}
	vcenters, ok := vcentersRaw.([]any)
	if !ok {
		t.Fatalf("VCenters = %T, want []any", vcentersRaw)
	}
	if len(vcenters) != 1 {
		t.Fatalf("len(VCenters) = %d, want 1", len(vcenters))
	}
	entry, ok := vcenters[0].(map[string]any)
	if !ok {
		t.Fatalf("VCenters[0] = %T, want map[string]any", vcenters[0])
	}
	assertMapKeys(t, entry, "vCenter", "username", "password")
}

func TestSaveToSecretAndGet(t *testing.T) {
	ctx := context.Background()
	client := fakekubeclient.NewClientset()
	mgr := NewMetadataManager(client)

	md := &ClusterMetadata{
		ClusterName: "test-cluster",
		ClusterID:   "test-uid",
		InfraID:     "test-infra",
		VSphere: &VSphereMetadata{
			VCenter:           "vc.example.com",
			Username:          "admin",
			Password:          "secret",
			TerraformPlatform: "vsphere",
			VCenters: []VCenter{
				{VCenter: "vc.example.com", Username: "admin", Password: "secret"},
			},
		},
	}

	// Save
	if err := mgr.SaveToSecret(ctx, md, "test-ns", "test-metadata"); err != nil {
		t.Fatalf("SaveToSecret: %v", err)
	}

	// Verify Secret exists with correct label
	secret, err := client.CoreV1().Secrets("test-ns").Get(ctx, "test-metadata", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get Secret: %v", err)
	}
	if secret.Labels[metadataSecretLabelKey] != metadataSecretLabelValue {
		t.Error("Secret missing metadata label")
	}
	if len(secret.OwnerReferences) != 0 {
		t.Errorf("Secret.OwnerReferences = %#v, want empty", secret.OwnerReferences)
	}

	// Get metadata back
	data, err := mgr.GetMetadataFromSecret(ctx, "test-ns", "test-metadata")
	if err != nil {
		t.Fatalf("GetMetadataFromSecret: %v", err)
	}

	var roundTripped ClusterMetadata
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if roundTripped.InfraID != md.InfraID {
		t.Errorf("roundTripped.InfraID = %q, want %q", roundTripped.InfraID, md.InfraID)
	}
	if roundTripped.VSphere == nil {
		t.Fatal("roundTripped.VSphere is nil")
	}
	if roundTripped.VSphere.VCenter != md.VSphere.VCenter {
		t.Errorf("roundTripped.VSphere.VCenter = %q, want %q", roundTripped.VSphere.VCenter, md.VSphere.VCenter)
	}

	// Update existing
	secret.Labels = map[string]string{}
	secret.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: "migration.openshift.io/v1alpha1",
			Kind:       "VmwareCloudFoundationMigration",
			Name:       "test-migration",
			UID:        types.UID("owner-uid"),
		},
	}
	if _, err := client.CoreV1().Secrets("test-ns").Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update Secret labels: %v", err)
	}
	md.InfraID = "updated-infra"
	if err := mgr.SaveToSecret(ctx, md, "test-ns", "test-metadata"); err != nil {
		t.Fatalf("SaveToSecret (update): %v", err)
	}
	data, err = mgr.GetMetadataFromSecret(ctx, "test-ns", "test-metadata")
	if err != nil {
		t.Fatalf("GetMetadataFromSecret (after update): %v", err)
	}
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("Unmarshal (after update): %v", err)
	}
	if roundTripped.InfraID != "updated-infra" {
		t.Errorf("after update InfraID = %q, want %q", roundTripped.InfraID, "updated-infra")
	}
	secret, err = client.CoreV1().Secrets("test-ns").Get(ctx, "test-metadata", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get Secret after update: %v", err)
	}
	if secret.Labels[metadataSecretLabelKey] != metadataSecretLabelValue {
		t.Error("Secret missing metadata label after update")
	}
	if len(secret.OwnerReferences) != 0 {
		t.Errorf("Secret.OwnerReferences after update = %#v, want empty", secret.OwnerReferences)
	}
}

func TestSaveToSecretAndGetNotFound(t *testing.T) {
	mgr := NewMetadataManager(fakekubeclient.NewClientset())
	_, err := mgr.GetMetadataFromSecret(context.Background(), "test-ns", "nonexistent")
	if err == nil {
		t.Fatal("GetMetadataFromSecret: expected error for nonexistent secret")
	}
}

func TestSaveToSecretMissingKey(t *testing.T) {
	ctx := context.Background()
	client := fakekubeclient.NewClientset()
	// Create a secret without the metadata key
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-secret", Namespace: "test-ns"},
		Data:       map[string][]byte{},
	}
	if _, err := client.CoreV1().Secrets("test-ns").Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create Secret: %v", err)
	}

	mgr := NewMetadataManager(client)
	_, err := mgr.GetMetadataFromSecret(ctx, "test-ns", "empty-secret")
	if err == nil {
		t.Fatal("GetMetadataFromSecret: expected error for missing key")
	}
}

func TestGetMetadataSecretName(t *testing.T) {
	got := GetMetadataSecretName("my-migration")
	want := "my-migration-metadata"
	if got != want {
		t.Errorf("GetMetadataSecretName = %q, want %q", got, want)
	}
}

func TestParseCredentials(t *testing.T) {
	tests := []struct {
		input    string
		wantUser string
		wantPass string
	}{
		{"admin:secret", "admin", "secret"},
		{"user:pass:with:colons", "user", "pass:with:colons"},
		{"", "", ""},
		{"nocolon", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			u, p := parseCredentials(tt.input)
			if u != tt.wantUser || p != tt.wantPass {
				t.Errorf("parseCredentials(%q) = (%q, %q), want (%q, %q)", tt.input, u, p, tt.wantUser, tt.wantPass)
			}
		})
	}
}

func assertMapKeys(t *testing.T, got map[string]any, wantKeys ...string) {
	t.Helper()

	want := make(map[string]struct{}, len(wantKeys))
	for _, key := range wantKeys {
		want[key] = struct{}{}
	}

	if len(got) != len(wantKeys) {
		t.Fatalf("map has %d keys, want %d: %#v", len(got), len(wantKeys), got)
	}

	for key := range got {
		if _, ok := want[key]; !ok {
			t.Fatalf("unexpected key %q in map %#v", key, got)
		}
	}
}
