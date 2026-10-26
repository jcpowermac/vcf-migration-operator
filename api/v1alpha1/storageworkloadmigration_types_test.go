package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestStorageWorkloadMigrationScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	for _, kind := range []string{"StorageWorkloadMigration", "StorageWorkloadMigrationList"} {
		if _, err := scheme.New(GroupVersion.WithKind(kind)); err != nil {
			t.Fatalf("Scheme.New(%s): %v", kind, err)
		}
	}
}

func TestStorageWorkloadMigrationDeepCopy(t *testing.T) {
	orig := &StorageWorkloadMigrationList{
		Items: []StorageWorkloadMigration{{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"site": "source"}},
			Spec: StorageWorkloadMigrationSpec{
				MigrationRef: MigrationObjectReference{Name: "cluster", UID: "migration-uid"},
			},
			Status: StorageWorkloadMigrationStatus{
				Phase: StorageWorkloadMigrationPhasePlanning,
				Volumes: []StorageWorkloadMigrationVolume{{
					SourcePVC: MigrationObjectReference{Name: "data", UID: "pvc-uid"},
					Blockers:  []StorageWorkloadMigrationBlocker{{Reason: "RawBlockUnsupported", Message: "original"}},
					SourcePV: StorageWorkloadMigrationSourcePV{
						Name: "pv", UID: "pv-uid", VolumeHandle: "source-volume",
					},
				}},
				Consumers: []StorageWorkloadMigrationConsumer{{
					APIVersion: "apps/v1", Kind: "StatefulSet", Name: "database", UID: "consumer-uid",
				}},
				Conditions: []metav1.Condition{{Type: "Ready", Message: "planning"}},
			},
		}},
	}
	copied := orig.DeepCopyObject().(*StorageWorkloadMigrationList)
	copied.Items[0].Status.Volumes[0].Blockers[0].Message = "changed"
	copied.Items[0].Labels["site"] = "target"
	copied.Items[0].Spec.MigrationRef.UID = "replacement-migration-uid"
	copied.Items[0].Status.Volumes[0].SourcePV.VolumeHandle = "target-volume"
	copied.Items[0].Status.Consumers[0].UID = "replacement-uid"
	copied.Items[0].Status.Conditions[0].Message = "changed"
	got := orig.Items[0]
	if got.Status.Volumes[0].Blockers[0].Message != "original" || got.Labels["site"] != "source" || got.Spec.MigrationRef.UID != "migration-uid" ||
		got.Status.Volumes[0].SourcePV.VolumeHandle != "source-volume" ||
		got.Status.Consumers[0].UID != "consumer-uid" || got.Status.Conditions[0].Message != "planning" {
		t.Fatalf("DeepCopyObject: mutating the copy changed the original: %+v", got)
	}
}
