package controller

import (
	"errors"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

// transientError wraps an error that represents a temporary condition (e.g. an
// external resource is still stabilizing) rather than a permanent failure
// requiring user intervention. The reconciler uses this distinction to choose
// between ReasonFailed and ReasonRetrying when recording condition status.
type transientError struct {
	err error
}

// newTransientError marks err as transient.
func newTransientError(err error) error {
	return &transientError{err: err}
}

func (e *transientError) Error() string {
	return e.err.Error()
}

func (e *transientError) Unwrap() error {
	return e.err
}

// isTransientError reports whether err (or any error it wraps) was marked
// transient via newTransientError.
func isTransientError(err error) bool {
	var te *transientError
	return errors.As(err, &te)
}

// reasonForError returns the condition Reason to record for a handler error:
// ReasonRetrying for transient errors (the operator will automatically retry
// on the next reconcile), ReasonFailed otherwise.
func reasonForError(err error) string {
	if isTransientError(err) {
		return migrationv1alpha1.ReasonRetrying
	}
	return migrationv1alpha1.ReasonFailed
}
