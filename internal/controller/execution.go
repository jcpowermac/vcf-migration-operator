package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/execution"
)

var errExecutionStopped = fmt.Errorf("stopping new migration actions")

// stateContext guards work against deletion, replacement, and state or generation
// changes. Planning callers must restrict work to the planning conditions.
func (r *VmwareCloudFoundationMigrationReconciler) stateContext(ctx context.Context, m *migrationv1alpha1.VmwareCloudFoundationMigration, state migrationv1alpha1.MigrationState) context.Context {
	return execution.WithCheck(ctx, func(ctx context.Context) error {
		latest := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := r.permissionReader().Get(ctx, client.ObjectKeyFromObject(m), latest); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return errExecutionStopped
			}
			return fmt.Errorf("reading migration execution permission: %w", err)
		}
		if !latest.DeletionTimestamp.IsZero() || latest.UID != m.UID || latest.Generation != m.Generation || latest.Spec.State != state {
			return errExecutionStopped
		}
		return nil
	})
}
