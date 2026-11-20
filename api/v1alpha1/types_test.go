package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMigrationProgressDeepCopy(t *testing.T) {
	now := metav1.Now()
	orig := &VmwareCloudFoundationMigrationStatus{
		Progress: &MigrationProgress{
			Workers: &WorkerMigrationProgress{
				TargetMachinesTotal:     3,
				TargetMachinesReady:     3,
				TargetNodesReady:        3,
				SourceMachinesRemaining: 0,
			},
			ControlPlane: &ControlPlaneProgress{
				Replicas:        3,
				UpdatedReplicas: 3,
				ReadyReplicas:   3,
			},
		},
		StartTime:      &now,
		LastUpdateTime: &now,
		CompletionTime: &now,
	}

	copied := orig.DeepCopy()
	if copied == nil {
		t.Fatal("DeepCopy returned nil")
	}
	if copied.Progress == nil || copied.Progress.Workers == nil || copied.Progress.Workers.TargetMachinesReady != 3 {
		t.Errorf("progress workers not copied properly: %+v", copied.Progress)
	}
	if copied.Progress.ControlPlane == nil || copied.Progress.ControlPlane.UpdatedReplicas != 3 {
		t.Errorf("progress control plane not copied properly: %+v", copied.Progress)
	}
	if copied.LastUpdateTime == nil || !copied.LastUpdateTime.Equal(&now) {
		t.Errorf("lastUpdateTime not copied properly: %+v", copied.LastUpdateTime)
	}
}

func TestMigrationProgressNilDeepCopy(t *testing.T) {
	var orig *MigrationProgress
	copied := orig.DeepCopy()
	if copied != nil {
		t.Errorf("expected nil DeepCopy, got %+v", copied)
	}
}

func TestStorageClassMappingDeepCopy(t *testing.T) {
	orig := &VmwareCloudFoundationMigration{
		Spec: VmwareCloudFoundationMigrationSpec{
			Storage: &StorageMigrationSpec{
				StorageClassMapping: []StorageClassMapping{
					{Source: "legacy-vsphere", Target: "thin-csi"},
				},
			},
		},
	}
	copied := orig.DeepCopy()
	if copied.Spec.Storage == nil || len(copied.Spec.Storage.StorageClassMapping) != 1 {
		t.Fatalf("DeepCopy: storage class mapping was not copied: %+v", copied.Spec.Storage)
	}
	if got := copied.Spec.Storage.StorageClassMapping[0]; got != orig.Spec.Storage.StorageClassMapping[0] {
		t.Fatalf("DeepCopy: got mapping %+v, want %+v", got, orig.Spec.Storage.StorageClassMapping[0])
	}
	copied.Spec.Storage.StorageClassMapping[0].Source = "changed-source"
	copied.Spec.Storage.StorageClassMapping[0].Target = "changed-target"
	if got := orig.Spec.Storage.StorageClassMapping[0]; got.Source != "legacy-vsphere" || got.Target != "thin-csi" {
		t.Fatalf("DeepCopy: mutating the copy changed the original mapping: %+v", got)
	}
}
