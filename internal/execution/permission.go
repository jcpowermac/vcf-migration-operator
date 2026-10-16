// Package execution gates new migration actions without cancelling submitted work.
package execution

import "context"

type permissionKey struct{}

// WithCheck installs a permission read at mutation boundaries.
func WithCheck(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, permissionKey{}, check)
}

// Check revalidates permission when used by a migration; other callers are unchanged.
func Check(ctx context.Context) error {
	if check, ok := ctx.Value(permissionKey{}).(func(context.Context) error); ok {
		return check(ctx)
	}
	return nil
}
