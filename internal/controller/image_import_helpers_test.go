package controller

import (
	"testing"

	configv1 "github.com/openshift/api/config/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func TestNeedsOVAReresolution(t *testing.T) {
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
	base := &migrationv1alpha1.ImageStatus{
		ResolvedOVAUrl:    "https://example.com/base.ova",
		ImportedTemplates: map[string]string{"fd1": "/base/template"},
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
	if !changed || got == desired {
		t.Fatalf("mergeImageStatus(nil, nil, desired) = (%#v, %v), want clone and changed", got, changed)
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
	if changed || got.ResolvedOVAUrl != latest.ResolvedOVAUrl || got.ResolvedSHA256 != latest.ResolvedSHA256 {
		t.Fatalf("mergeImageStatus() overwrote newer scalar fields: changed=%v, got=%#v", changed, got)
	}
	if got.ImportedTemplates["fd1"] != "/newer/template" || got.ImportedTemplates["fd3"] != "/current/template" {
		t.Fatalf("mergeImageStatus() overwrote newer map entries: %#v", got.ImportedTemplates)
	}
	got, changed = mergeImageStatus(base.DeepCopy(), base, desired)
	if !changed || got.ResolvedOVAUrl != desired.ResolvedOVAUrl || got.ImportedTemplates["fd2"] != "/second/template" {
		t.Fatalf("mergeImageStatus() did not apply current changes: changed=%v, got=%#v", changed, got)
	}
}

func TestImageSpecDiffersFromStatus(t *testing.T) {
	tests := []struct {
		name     string
		spec     *migrationv1alpha1.ImageSpec
		status   *migrationv1alpha1.ImageStatus
		wantDiff bool
	}{
		{name: "adding image after no-image completion", spec: &migrationv1alpha1.ImageSpec{}, status: nil, wantDiff: true},
		{name: "changed URL during interrupted import", spec: &migrationv1alpha1.ImageSpec{OVAUrl: "https://b"}, status: &migrationv1alpha1.ImageStatus{ResolvedOVAUrl: "https://a", URLSource: migrationv1alpha1.ImageURLSourceUser}, wantDiff: true},
		{name: "same user URL", spec: &migrationv1alpha1.ImageSpec{OVAUrl: "https://a"}, status: &migrationv1alpha1.ImageStatus{ResolvedOVAUrl: "https://a", URLSource: migrationv1alpha1.ImageURLSourceUser}, wantDiff: false},
		{name: "cleared user URL", spec: &migrationv1alpha1.ImageSpec{}, status: &migrationv1alpha1.ImageStatus{ResolvedOVAUrl: "https://a", URLSource: migrationv1alpha1.ImageURLSourceUser}, wantDiff: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
				Spec:   migrationv1alpha1.VmwareCloudFoundationMigrationSpec{Image: tt.spec},
				Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{Image: tt.status},
			}
			if got := imageSpecDiffersFromStatus(migration); got != tt.wantDiff {
				t.Fatalf("imageSpecDiffersFromStatus() = %v, want %v", got, tt.wantDiff)
			}
		})
	}
}

func TestPopulateTopologyTemplates(t *testing.T) {
	tests := []struct {
		name         string
		imported     map[string]string
		operatorMap  map[string]string
		initial      string
		wantTemplate string
		wantChanged  bool
	}{
		{name: "empty fill", imported: map[string]string{"fd1": "/x/new"}, operatorMap: map[string]string{"fd1": "u"}, wantTemplate: "/x/new", wantChanged: true},
		{name: "stale refresh", imported: map[string]string{"fd1": "/x/new"}, operatorMap: map[string]string{"fd1": "u"}, initial: "/x/old", wantTemplate: "/x/new", wantChanged: true},
		{name: "user untouched", imported: map[string]string{"fd1": "/x/user"}, initial: "/x/user", wantTemplate: "/x/user"},
		{name: "no record", initial: "/x/keep", wantTemplate: "/x/keep"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
				Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
					FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
						Name: "fd1", Topology: configv1.VSpherePlatformTopology{Template: tt.initial},
					}},
				},
				Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{Image: &migrationv1alpha1.ImageStatus{
					ImportedTemplates: tt.imported, OperatorImportedTemplates: tt.operatorMap,
				}},
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

func TestUnknownTemplateProvenanceIsNotOperatorManaged(t *testing.T) {
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
				Name: "fd1", Topology: configv1.VSpherePlatformTopology{Template: "/DC/vm/template"},
			}},
		},
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			Image: &migrationv1alpha1.ImageStatus{
				ImportedTemplates:         map[string]string{"fd1": "/DC/vm/template"},
				OperatorImportedTemplates: map[string]string{},
			},
		},
	}
	if populateTopologyTemplates(migration) {
		t.Fatal("populateTopologyTemplates() changed template without operator provenance")
	}
	if got := migration.Spec.FailureDomains[0].Topology.Template; got != "/DC/vm/template" {
		t.Fatalf("template changed to %q without operator provenance", got)
	}
}
