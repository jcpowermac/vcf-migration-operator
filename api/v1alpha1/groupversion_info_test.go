package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestAddToScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	migration := schema.GroupVersionKind{Group: "migration.openshift.io", Version: "v1alpha1", Kind: "VmwareCloudFoundationMigration"}
	if !scheme.Recognizes(migration) {
		t.Errorf("scheme does not recognize %v", migration)
	}

	migrationList := schema.GroupVersionKind{Group: "migration.openshift.io", Version: "v1alpha1", Kind: "VmwareCloudFoundationMigrationList"}
	if !scheme.Recognizes(migrationList) {
		t.Errorf("scheme does not recognize %v", migrationList)
	}
}
