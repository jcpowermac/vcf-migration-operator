package storageplan

import (
	"context"
	"fmt"
	"sort"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// OwnerLookup reads a supported workload owner by namespace and reference.
// Unknown owner kinds are recorded without being fetched.
type OwnerLookup func(context.Context, string, metav1.OwnerReference) (metav1.Object, error)

// Consumers follows controller references to the outermost supported controller or operator CR.
func Consumers(ctx context.Context, pods []corev1.Pod, namespace string, volumes []migrationv1alpha1.StorageWorkloadMigrationVolume, lookup OwnerLookup) ([]migrationv1alpha1.StorageWorkloadMigrationConsumer, error) {
	found := map[types.UID]migrationv1alpha1.StorageWorkloadMigrationConsumer{}
	for i := range pods {
		pod := &pods[i]
		if pod.Namespace != namespace || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || len(PodVolumes(pod, volumes)) == 0 {
			continue
		}
		consumer := migrationv1alpha1.StorageWorkloadMigrationConsumer{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}
		var meta metav1.Object = pod
		seen := map[types.UID]bool{pod.UID: true}
		for {
			owner := ControllerOwner(meta)
			if owner == nil {
				break
			}
			if seen[owner.UID] {
				return nil, fmt.Errorf("following cyclic owner reference for Pod %s/%s", pod.Namespace, pod.Name)
			}
			seen[owner.UID] = true
			consumer = migrationv1alpha1.StorageWorkloadMigrationConsumer{APIVersion: owner.APIVersion, Kind: owner.Kind, Name: owner.Name, UID: owner.UID}
			if !supportedOwner(*owner) {
				break
			}
			next, err := lookup(ctx, pod.Namespace, *owner)
			if err != nil {
				return nil, fmt.Errorf("getting %s %s/%s: %w", owner.Kind, pod.Namespace, owner.Name, err)
			}
			if next.GetUID() != owner.UID {
				return nil, fmt.Errorf("checking owner identity for %s %s/%s: UID changed", owner.Kind, pod.Namespace, owner.Name)
			}
			meta = next
		}
		found[consumer.UID] = consumer
	}
	var result []migrationv1alpha1.StorageWorkloadMigrationConsumer
	for _, c := range found {
		result = append(result, c)
	}
	sort.Slice(result, func(i, j int) bool { return string(result[i].UID) < string(result[j].UID) })
	return result, nil
}

func supportedOwner(owner metav1.OwnerReference) bool {
	switch owner.APIVersion {
	case "apps/v1":
		return owner.Kind == "ReplicaSet" || owner.Kind == "Deployment" || owner.Kind == "StatefulSet" || owner.Kind == "DaemonSet"
	case "batch/v1":
		return owner.Kind == "Job" || owner.Kind == "CronJob"
	default:
		return false
	}
}
