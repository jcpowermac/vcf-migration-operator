package storageplan

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

// OwnerLookup reads a supported workload owner by namespace and reference.
// Unknown owner kinds are recorded without being fetched.
type OwnerLookup func(context.Context, string, metav1.OwnerReference) (metav1.Object, error)

// Consumers follows controller references to the outermost supported controller or operator CR.
func Consumers(ctx context.Context, pods []corev1.Pod, namespace string, volumes []migrationv1alpha1.StorageWorkloadMigrationVolume, lookup OwnerLookup) ([]migrationv1alpha1.StorageWorkloadMigrationConsumer, error) {
	found := map[types.UID]migrationv1alpha1.StorageWorkloadMigrationConsumer{}
	for i := range pods {
		pod := &pods[i]
		if !isVolumeConsumer(pod, namespace, volumes) {
			continue
		}
		consumer, err := resolveConsumer(ctx, pod, lookup)
		if err != nil {
			return nil, err
		}
		found[consumer.UID] = consumer
	}
	return sortedConsumers(found), nil
}

// isVolumeConsumer reports whether a running Pod in the namespace mounts any planned volume.
func isVolumeConsumer(pod *corev1.Pod, namespace string, volumes []migrationv1alpha1.StorageWorkloadMigrationVolume) bool {
	if pod.Namespace != namespace {
		return false
	}
	// Terminated Pods no longer hold their volumes.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}
	return len(PodVolumes(pod, volumes)) > 0
}

// resolveConsumer walks the Pod's controller owner chain and returns the outermost
// owner. The walk stops at the first owner kind that cannot be fetched.
func resolveConsumer(ctx context.Context, pod *corev1.Pod, lookup OwnerLookup) (migrationv1alpha1.StorageWorkloadMigrationConsumer, error) {
	consumer := migrationv1alpha1.StorageWorkloadMigrationConsumer{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}

	var owned metav1.Object = pod
	seen := map[types.UID]bool{pod.UID: true}
	for {
		owner := ControllerOwner(owned)
		if owner == nil {
			return consumer, nil
		}
		if seen[owner.UID] {
			return migrationv1alpha1.StorageWorkloadMigrationConsumer{}, fmt.Errorf("following cyclic owner reference for Pod %s/%s", pod.Namespace, pod.Name)
		}
		seen[owner.UID] = true
		consumer = consumerFromOwner(owner)

		if !supportedOwner(*owner) {
			return consumer, nil
		}
		next, err := fetchOwner(ctx, pod.Namespace, owner, lookup)
		if err != nil {
			return migrationv1alpha1.StorageWorkloadMigrationConsumer{}, err
		}
		owned = next
	}
}

// fetchOwner reads an owner and verifies it is the object the reference points to.
func fetchOwner(ctx context.Context, namespace string, owner *metav1.OwnerReference, lookup OwnerLookup) (metav1.Object, error) {
	obj, err := lookup(ctx, namespace, *owner)
	if err != nil {
		return nil, fmt.Errorf("getting %s %s/%s: %w", owner.Kind, namespace, owner.Name, err)
	}
	// A recreated owner with the same name is a different workload.
	if obj.GetUID() != owner.UID {
		return nil, fmt.Errorf("checking owner identity for %s %s/%s: UID changed", owner.Kind, namespace, owner.Name)
	}
	return obj, nil
}

func consumerFromOwner(owner *metav1.OwnerReference) migrationv1alpha1.StorageWorkloadMigrationConsumer {
	return migrationv1alpha1.StorageWorkloadMigrationConsumer{APIVersion: owner.APIVersion, Kind: owner.Kind, Name: owner.Name, UID: owner.UID}
}

// sortedConsumers returns consumers ordered by UID for stable status output.
func sortedConsumers(found map[types.UID]migrationv1alpha1.StorageWorkloadMigrationConsumer) []migrationv1alpha1.StorageWorkloadMigrationConsumer {
	if len(found) == 0 {
		return nil
	}
	result := make([]migrationv1alpha1.StorageWorkloadMigrationConsumer, 0, len(found))
	for _, c := range found {
		result = append(result, c)
	}
	sort.Slice(result, func(i, j int) bool { return string(result[i].UID) < string(result[j].UID) })
	return result
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
