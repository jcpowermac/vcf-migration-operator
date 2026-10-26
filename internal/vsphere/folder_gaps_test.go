package vsphere

import (
	"context"
	"strings"
	"testing"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
)

func TestVMFolderNilGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("CreateVMFolder nil session", func(t *testing.T) {
		if _, err := CreateVMFolder(ctx, nil, "f"); err == nil || !strings.Contains(err.Error(), "session and Finder must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("CreateVMFolder nil finder", func(t *testing.T) {
		s := &Session{Datacenter: "DC0"}
		if _, err := CreateVMFolder(ctx, s, "f"); err == nil || !strings.Contains(err.Error(), "session and Finder must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("GetVMFolder nil session", func(t *testing.T) {
		if _, err := GetVMFolder(ctx, nil, "f"); err == nil || !strings.Contains(err.Error(), "session and Finder must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("DeleteVMFolder nil folder", func(t *testing.T) {
		s := &Session{Datacenter: "DC0"}
		if err := DeleteVMFolder(ctx, s, nil); err == nil || !strings.Contains(err.Error(), "folder must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCreateVMFolderErrors(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		s := newTestSession(ctx, t, c)

		t.Run("duplicate folder", func(t *testing.T) {
			if _, err := CreateVMFolder(ctx, s, "dup-folder"); err != nil {
				t.Fatalf("first CreateVMFolder: %v", err)
			}
			_, err := CreateVMFolder(ctx, s, "dup-folder")
			if err == nil || !strings.Contains(err.Error(), "creating folder") {
				t.Fatalf("second CreateVMFolder: error = %v, want 'creating folder'", err)
			}
		})

		t.Run("missing datacenter", func(t *testing.T) {
			s2 := newTestSession(ctx, t, c)
			s2.Datacenter = missingDatacenter
			if _, err := CreateVMFolder(ctx, s2, "f"); err == nil || !strings.Contains(err.Error(), "finding datacenter") {
				t.Fatalf("error = %v, want 'finding datacenter'", err)
			}
		})

		t.Run("empty folder part skipped", func(t *testing.T) {
			// "parent//child" parses to parts [parent, "", child]; the empty part
			// exercises the continue branch.
			if _, err := CreateVMFolder(ctx, s, "part//skip"); err != nil {
				t.Fatalf("CreateVMFolder: %v", err)
			}
		})
	})
}

func TestDeleteVMFolderStale(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		s := newTestSession(ctx, t, c)

		folder, err := CreateVMFolder(ctx, s, "victim")
		if err != nil {
			t.Fatalf("CreateVMFolder: %v", err)
		}
		if err := DeleteVMFolder(ctx, s, folder); err != nil {
			t.Fatalf("first DeleteVMFolder: %v", err)
		}
		// The folder reference is now stale; destroying it again should fail.
		if err := DeleteVMFolder(ctx, s, folder); err == nil {
			t.Fatal("expected destroy error for stale folder")
		}
	})
}

func TestParseFolderNameNoSlashPrefix(t *testing.T) {
	got := parseFolderName("DC0", "DC0/vm/my-folder")
	if got != "my-folder" {
		t.Fatalf("parseFolderName = %q, want my-folder", got)
	}
}

// Uncovered defensive branches in folder.go (left for a future non-simulator
// harness):
//   - CreateVMFolder "getting datacenter folders" (dc.Folders): only fails on a
//     property-collector fault, which the simulator does not produce here.
//   - CreateVMFolder "has no VM folder" (folders.VmFolder == nil): object.Folder
//     from dc.Folders is never nil (NewFolder always returns &Folder{}).
//   - CreateVMFolder "looking up created folder" (GetVMFolder after create):
//     the finder normalizes the freshly-created path, so the re-lookup succeeds.
//   - DeleteVMFolder "waiting for folder destroy" (task.Wait): the simulator's
//     folder DestroyTask child loop asserts a 1-arg DestroyTask interface, but
//     every simulator entity defines a 2-arg DestroyTask, so children are always
//     skipped and the child loop never faults. The task.Wait branch itself is
//     reachable via a TaskDelay fault injection (not set up here).
