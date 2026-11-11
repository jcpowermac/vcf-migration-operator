package vsphere

import (
	"context"
	"fmt"
	"testing"

	"github.com/vmware/govmomi/vim25/types"

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

// TestDestroyProbeVMStopsBetweenPowerOffAndDestroy verifies the execution
// check between power-off and destroy: once permission is denied after the
// power-off task completed, the probe VM is left powered off but intact.
func TestDestroyProbeVMStopsBetweenPowerOffAndDestroy(t *testing.T) {
	env := newProbeTestEnv(t)
	spec := env.baseSpec("probe-destroy-interrupted")
	vm, err := env.session.CreateProbeVM(env.ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	// Deny once the VM is powered off, so the power-off task proceeds and the
	// next check (before Destroy) aborts.
	ctx := execution.WithCheck(env.ctx, func(context.Context) error {
		state, err := vm.PowerState(env.ctx)
		if err != nil {
			return err
		}
		if state == types.VirtualMachinePowerStatePoweredOff {
			return fmt.Errorf("stopping new migration actions")
		}
		return nil
	})
	if err := DestroyProbeVM(ctx, vm); err == nil {
		t.Fatal("DestroyProbeVM succeeded, want stop between power-off and destroy")
	}
	state, err := vm.PowerState(env.ctx)
	if err != nil {
		t.Fatalf("probe VM was destroyed after execution stopped: %v", err)
	}
	if state != types.VirtualMachinePowerStatePoweredOff {
		t.Fatalf("probe power state = %s, want powered off with destroy blocked", state)
	}
}
