package controller

import (
	"errors"
	"fmt"
	"testing"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error is not transient",
			err:  nil,
			want: false,
		},
		{
			name: "plain error is not transient",
			err:  fmt.Errorf("boom"),
			want: false,
		},
		{
			name: "transient error is transient",
			err:  newTransientError(fmt.Errorf("cluster operators are not healthy")),
			want: true,
		},
		{
			name: "transient error wrapped by additional context is still transient",
			err:  fmt.Errorf("running preflight checks: %w", newTransientError(fmt.Errorf("cluster upgrade is in progress"))),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientError(tt.err); got != tt.want {
				t.Fatalf("isTransientError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestTransientErrorUnwrap(t *testing.T) {
	inner := fmt.Errorf("cluster operators are not healthy")
	transient := newTransientError(inner)

	if transient.Error() != inner.Error() {
		t.Fatalf("transient.Error() = %q, want %q", transient.Error(), inner.Error())
	}
	if !errors.Is(transient, inner) {
		t.Fatalf("errors.Is(transient, inner) = false, want true")
	}
}

func TestReasonForError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "transient error retries",
			err:  newTransientError(fmt.Errorf("cluster operators are not healthy")),
			want: migrationv1alpha1.ReasonRetrying,
		},
		{
			name: "plain error fails",
			err:  fmt.Errorf("spec.failureDomains must not be empty"),
			want: migrationv1alpha1.ReasonFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reasonForError(tt.err); got != tt.want {
				t.Fatalf("reasonForError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
