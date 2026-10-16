package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func (r *VmwareCloudFoundationMigrationReconciler) permissionReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	// Unit tests may use a non-caching fake client; production injects APIReader.
	return r.Client
}

func completionSpecHash(spec migrationv1alpha1.VmwareCloudFoundationMigrationSpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encoding completion spec: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func isCompletionRecordedForSpec(m *migrationv1alpha1.VmwareCloudFoundationMigration) (bool, error) {
	ready := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || m.Status.CompletionTime == nil || m.Status.CompletionSpecHash == "" {
		return false, nil
	}
	hash, err := completionSpecHash(m.Spec)
	return err == nil && hash == m.Status.CompletionSpecHash, err
}

func (r *VmwareCloudFoundationMigrationReconciler) updateRetentionFinalizer(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := r.permissionReader().Get(ctx, client.ObjectKeyFromObject(m), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		base := client.MergeFromWithOptions(latest.DeepCopy(), client.MergeFromWithOptimisticLock{})
		changed := false
		if latest.DeletionTimestamp.IsZero() {
			changed = controllerutil.AddFinalizer(latest, migrationv1alpha1.FinalizerName)
		} else {
			complete, err := isCompletionRecordedForSpec(latest)
			if err != nil {
				return err
			}
			if complete || latest.Annotations[migrationv1alpha1.AnnotationAllowDeletion] == "true" {
				changed = controllerutil.RemoveFinalizer(latest, migrationv1alpha1.FinalizerName)
			}
		}
		if !changed {
			return nil
		}
		return r.Patch(ctx, latest, base)
	})
}
