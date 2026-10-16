package vsphere

import (
	"context"
	"fmt"
	"testing"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

func TestProbeCleanupRespectsExecutionPermission(t *testing.T) {
	env := newProbeTestEnv(t)
	spec := env.baseSpec("probe-retained-after-deletion")
	vm, err := env.session.CreateProbeVM(env.ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	before, err := vm.PowerState(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	ctx := execution.WithCheck(env.ctx, func(context.Context) error { checks++; return fmt.Errorf("migration deleted") })
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	waitErr := fmt.Errorf("clone wait timed out")
	if got := env.session.cleanupFailedClone(ctx, spec, waitErr); got != waitErr {
		t.Fatalf("cleanup changed submitted task's wait error: %v", got)
	}
	if checks == 0 {
		t.Fatal("cleanup bypassed execution permission")
	}
	after, err := vm.PowerState(env.ctx)
	if err != nil {
		t.Fatalf("probe was destroyed after deletion: %v", err)
	}
	if after != before {
		t.Fatalf("probe power changed after deletion: %s -> %s", before, after)
	}
}
