package execution

import (
	"context"
	"errors"
	"testing"
)

func TestCheckPermission(t *testing.T) {
	type ctxKey int
	var key ctxKey
	sentinelErr := errors.New("denied")
	enriched := context.WithValue(context.Background(), key, "set")

	tests := []struct {
		name         string
		ctx          context.Context
		wantErr      bool
		wantSentinel bool
	}{
		{
			name: "bare context has no check",
			ctx:  context.Background(),
		},
		{
			name: "check passes",
			ctx:  WithCheck(context.Background(), func(context.Context) error { return nil }),
		},
		{
			name:         "check denies",
			ctx:          WithCheck(context.Background(), func(context.Context) error { return sentinelErr }),
			wantErr:      true,
			wantSentinel: true,
		},
		{
			name: "check receives enriched ctx",
			ctx: WithCheck(enriched, func(ctx context.Context) error {
				// The check must be invoked with the ctx that passed through
				// WithCheck. If the enriched value reached it, return the
				// sentinel so the caller can observe that propagation.
				if ctx.Value(key) == "set" {
					return sentinelErr
				}
				return nil
			}),
			wantErr:      true,
			wantSentinel: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Check(tt.ctx)
			if tt.wantErr && err == nil {
				t.Fatalf("Check() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Check() = %v, want nil", err)
			}
			if tt.wantSentinel && !errors.Is(err, sentinelErr) {
				t.Errorf("Check() error = %v, want sentinel %q", err, "denied")
			}
		})
	}
}
