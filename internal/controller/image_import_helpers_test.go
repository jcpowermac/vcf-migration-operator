package controller

import (
	"testing"

	configv1 "github.com/openshift/api/config/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func TestNeedsOVAReresolution(t *testing.T) {
	t.Helper()
	tests := []struct {
		name     string
		specURL  string
		resolved string
		source   migrationv1alpha1.ImageURLSource
		want     bool
	}{
		{name: "initial", specURL: "", resolved: "", source: "", want: true},
		{name: "user new", specURL: "https://a", resolved: "", source: "", want: true},
		{name: "user changed", specURL: "https://b", resolved: "https://a", source: migrationv1alpha1.ImageURLSourceUser, want: true},
		{name: "user cleared to auto", specURL: "", resolved: "https://a", source: migrationv1alpha1.ImageURLSourceUser, want: true},
		{name: "auto stable", specURL: "", resolved: "https://a", source: migrationv1alpha1.ImageURLSourceAuto, want: false},
		{name: "user unchanged", specURL: "https://a", resolved: "https://a", source: migrationv1alpha1.ImageURLSourceUser, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsOVAReresolution(tt.specURL, tt.resolved, tt.source); got != tt.want {
				t.Fatalf("needsOVAReresolution(%q, %q, %q) = %v, want %v", tt.specURL, tt.resolved, tt.source, got, tt.want)
			}
		})
	}
}

func TestMergeImageStatus(t *testing.T) {
	t.Helper()
	base := &migrationv1alpha1.ImageStatus{
		ResolvedOVAUrl: "https://example.com/base.ova",
		ImportedTemplates: map[string]string{
			"fd1": "/base/template",
		},
	}

	desired := &migrationv1alpha1.ImageStatus{
		ResolvedOVAUrl: "https://example.com/desired.ova",
		ResolvedSHA256: "desired-digest",
		ImportedTemplates: map[string]string{
			"fd1": "/desired/template",
			"fd2": "/second/template",
		},
	}

	got, changed := mergeImageStatus(nil, nil, desired)
	if !changed {
		t.Fatal("mergeImageStatus() changed = false, want true")
	}
	if got == desired {
		t.Fatal("mergeImageStatus() returned the desired pointer")
	}
	if got.ResolvedOVAUrl != desired.ResolvedOVAUrl || got.ResolvedSHA256 != desired.ResolvedSHA256 {
		t.Fatalf("mergeImageStatus() = %#v, want desired fields", got)
	}

	latest := &migrationv1alpha1.ImageStatus{
		ResolvedOVAUrl: "https://example.com/newer.ova",
		ResolvedSHA256: "newer-digest",
		ImportedTemplates: map[string]string{
			"fd1": "/newer/template",
			"fd3": "/current/template",
		},
	}
	got, changed = mergeImageStatus(latest, nil, desired)
	if changed {
		t.Fatal("mergeImageStatus() changed stale fields, want false")
	}
	if got.ResolvedOVAUrl != "https://example.com/newer.ova" || got.ResolvedSHA256 != "newer-digest" {
		t.Fatalf("mergeImageStatus() overwrote newer scalar fields: %#v", got)
	}
	if got.ImportedTemplates["fd1"] != "/newer/template" || got.ImportedTemplates["fd3"] != "/current/template" {
		t.Fatalf("mergeImageStatus() overwrote newer map entries: %#v", got.ImportedTemplates)
	}

	latest = base.DeepCopy()
	got, changed = mergeImageStatus(latest, base, desired)
	if !changed || got.ResolvedOVAUrl != desired.ResolvedOVAUrl || got.ResolvedSHA256 != desired.ResolvedSHA256 {
		t.Fatalf("mergeImageStatus() did not apply current changes: changed=%v, got=%#v", changed, got)
	}
	if got.ImportedTemplates["fd1"] != "/desired/template" || got.ImportedTemplates["fd2"] != "/second/template" {
		t.Fatalf("mergeImageStatus() did not merge current map changes: %#v", got.ImportedTemplates)
	}
}

func TestPopulateTopologyTemplates(t *testing.T) {
	t.Helper()
	tests := []struct {
		name         string
		imported     map[string]string
		operatorMap  map[string]string
		initial      string
		wantTemplate string
		wantChanged  bool
	}{
		{
			name:         "empty fill",
			imported:     map[string]string{"fd1": "/x/new"},
			operatorMap:  map[string]string{"fd1": "u"},
			initial:      "",
			wantTemplate: "/x/new",
			wantChanged:  true,
		},
		{
			name:         "stale refresh",
			imported:     map[string]string{"fd1": "/x/new"},
			operatorMap:  map[string]string{"fd1": "u"},
			initial:      "/x/old",
			wantTemplate: "/x/new",
			wantChanged:  true,
		},
		{
			name:         "user untouched",
			imported:     map[string]string{"fd1": "/x/user"},
			operatorMap:  map[string]string{},
			initial:      "/x/user",
			wantTemplate: "/x/user",
			wantChanged:  false,
		},
		{
			name:         "no record",
			imported:     map[string]string{},
			operatorMap:  map[string]string{},
			initial:      "/x/keep",
			wantTemplate: "/x/keep",
			wantChanged:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
				Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
					FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
						{
							Name: "fd1",
							Topology: configv1.VSpherePlatformTopology{
								Template: tt.initial,
							},
						},
					},
				},
				Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
					Image: &migrationv1alpha1.ImageStatus{
						ImportedTemplates:         tt.imported,
						OperatorImportedTemplates: tt.operatorMap,
					},
				},
			}
			if got := populateTopologyTemplates(migration); got != tt.wantChanged {
				t.Fatalf("populateTopologyTemplates() = %v, want %v", got, tt.wantChanged)
			}
			if got := migration.Spec.FailureDomains[0].Topology.Template; got != tt.wantTemplate {
				t.Fatalf("fd.Topology.Template = %q, want %q", got, tt.wantTemplate)
			}
		})
	}
}
