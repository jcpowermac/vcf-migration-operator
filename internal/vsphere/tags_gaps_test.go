package vsphere

// Residual (uncovered) branches in tags.go and why they are not covered in
// the govmomi vpx/simulator environment:
//
//   - ObjectHasTagInCategory L149 (category nil/empty ID): the simulator
//     returns either a valid category or an error - never (nil, nil) or an
//     empty-ID category - so the defensive "no category" branch is not hit.
//   - ObjectHasTagInCategory L155 (ListTagsForCategory error) and L164
//     (ListAttachedTags error): these need GetCategory to succeed while the
//     list call fails (a partial failure). A logged-out REST client fails all
//     calls and a logged-in one succeeds all of them, so the partial failure is
//     not produced here. It IS reachable via a request-counting http.Transport
//     on s.restClient that fails only the list call (not set up in these tests).
//   - ensureTagCategory L222-226 (isAlreadyExists success tails) and EnsureTag
//     L310-315 (isAlreadyExists, get-failed and success): the CATEGORY-level
//     "already exists, get failed" branch (L220) IS covered via a URN-prefixed
//     name (a direct-ID get 404s while the name-uniqueness create collides).
//     The TAG-level branches are not covered: GetTagForCategory matches by name
//     within the category (no direct-ID 404 path), so the URN trick does not
//     apply, and the success tails need the resource to be created between two
//     identical lookups (a create/get race).
//   - AttachTag L346 ("tag already attached" log) and AttachFailureDomainTags
//     L412/L425 (attached=false logs): these fire only when AttachTag returns
//     (false, nil) for a non-empty tagID, which happens only on an
//     already-exists attach. The simulator makes a duplicate attach
//     idempotent (returns nil success, not already-exists), so these log
//     branches are not hit.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

// execFaultCtx returns a context whose execution.Check always fails, so the
// first execution gate in the target function faults before any mutation.
func execFaultCtx(ctx context.Context) context.Context {
	return execution.WithCheck(ctx, func(context.Context) error { return errors.New("execution fault") })
}

// logoutTagSession builds a session whose REST client is logged out, so every
// TagManager call returns a 401 (a non-NotFound error) instead of a real fault.
func logoutTagSession(ctx context.Context, t *testing.T, c *vim25.Client) *Session {
	t.Helper()

	s := newTestSession(ctx, t, c)
	if err := s.restClient.Logout(ctx); err != nil {
		t.Fatalf("REST logout: %v", err)
	}
	return s
}

// TestObjectHasTagInCategoryGaps covers ObjectHasTagInCategory's non-NotFound
// category lookup error and its empty-category (no tags) false case. The
// ListTagsForCategory / ListAttachedTags error branches need a partial REST
// failure (see the file header) and are not covered here.
func TestObjectHasTagInCategoryGaps(t *testing.T) {
	t.Run("get category non-notfound error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := logoutTagSession(ctx, t, c)
			dc, err := s.Finder.DefaultDatacenter(ctx)
			if err != nil {
				t.Fatalf("finding datacenter: %v", err)
			}
			got, err := ObjectHasTagInCategory(ctx, s, "no-such-category", dc)
			if err == nil || !strings.Contains(err.Error(), "getting tag category") {
				t.Fatalf("got=%v err=%v, want getting-tag-category error", got, err)
			}
		})
	})

	t.Run("no tags in category", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			createTestCategory(ctx, t, s, tags.Category{Name: "gap-empty-cat", Cardinality: "SINGLE", Description: "d", AssociableTypes: tagCategoryAssociableTypes})
			dc, err := s.Finder.DefaultDatacenter(ctx)
			if err != nil {
				t.Fatalf("finding datacenter: %v", err)
			}
			got, err := ObjectHasTagInCategory(ctx, s, "gap-empty-cat", dc)
			if err != nil {
				t.Fatalf("ObjectHasTagInCategory: %v", err)
			}
			if got {
				t.Fatal("got=true, want false for an empty category")
			}
		})
	})

}

// TestEnsureTagCategoryCreateGaps covers ensureTagCategory's pre-create
// execution-check fault and its create-error (non-already-exists) branch.
func TestEnsureTagCategoryCreateGaps(t *testing.T) {
	t.Run("execution check fault", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			// A brand-new name: GetCategory finds nothing, then the execution
			// check before CreateCategory faults.
			if _, err := EnsureTagCategory(execFaultCtx(ctx), s, "gap-exec-cat", "d", "SINGLE"); err == nil {
				t.Fatal("EnsureTagCategory = nil, want execution-check error")
			}
		})
	})

	t.Run("create category error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := logoutTagSession(ctx, t, c)
			// A brand-new name: GetCategory 401, CreateCategory 401 (not
			// already-exists) -> create-error branch.
			_, err := EnsureTagCategory(ctx, s, "gap-create-cat", "d", "SINGLE")
			if err == nil || !strings.Contains(err.Error(), "creating tag category") {
				t.Fatalf("err=%v, want creating-tag-category error", err)
			}
		})
	})

	t.Run("already exists get failed", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			// A URN-like name: GetCategory does a direct-ID lookup (404) so the
			// create path is taken, but CreateCategory's name-uniqueness check sees
			// the pre-created category -> already-exists. The recovery GetCategory
			// also 404s -> get-failed branch.
			name := "urn:fake-gap-category"
			createTestCategory(ctx, t, s, tags.Category{Name: name, Cardinality: "SINGLE", Description: "d", AssociableTypes: tagCategoryAssociableTypes})
			_, err := EnsureTagCategory(ctx, s, name, "d", "SINGLE")
			if err == nil || !strings.Contains(err.Error(), "already exists but get failed") {
				t.Fatalf("err=%v, want already-exists-but-get-failed error", err)
			}
		})
	})
}

// TestEnsureTagCreateGaps covers EnsureTag's pre-create execution-check fault
// and its create-error (non-already-exists) branch.
func TestEnsureTagCreateGaps(t *testing.T) {
	t.Run("execution check fault", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			catID := createTestCategory(ctx, t, s, tags.Category{Name: "gap-tagcat", Cardinality: "SINGLE", Description: "d", AssociableTypes: tagCategoryAssociableTypes})
			// A brand-new tag name: GetTagForCategory finds nothing, then the
			// execution check before CreateTag faults.
			if _, err := EnsureTag(execFaultCtx(ctx), s, catID, "gap-tag", "d"); err == nil {
				t.Fatal("EnsureTag = nil, want execution-check error")
			}
		})
	})

	t.Run("create tag error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := logoutTagSession(ctx, t, c)
			// GetTagForCategory 401, CreateTag 401 (not already-exists) ->
			// create-error branch.
			_, err := EnsureTag(ctx, s, "gap-cat-id", "gap-tag", "d")
			if err == nil || !strings.Contains(err.Error(), "creating tag") {
				t.Fatalf("err=%v, want creating-tag error", err)
			}
		})
	})
}

// TestAttachTagGaps covers AttachTag's execution-check fault and its
// attach-error (non-already-exists) branch.
func TestAttachTagGaps(t *testing.T) {
	t.Run("execution check fault", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			dc, err := s.Finder.DefaultDatacenter(ctx)
			if err != nil {
				t.Fatalf("finding datacenter: %v", err)
			}
			// Non-empty tagID passes the early return; the execution check
			// before AttachTag faults.
			if _, err := AttachTag(execFaultCtx(ctx), s, "gap-tag-id", dc); err == nil {
				t.Fatal("AttachTag = nil, want execution-check error")
			}
		})
	})

	t.Run("attach error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := logoutTagSession(ctx, t, c)
			dc, err := s.Finder.DefaultDatacenter(ctx)
			if err != nil {
				t.Fatalf("finding datacenter: %v", err)
			}
			// TagManager.AttachTag 401 (not already-exists) -> attach-error.
			_, err = AttachTag(ctx, s, "gap-tag-id", dc)
			if err == nil || !strings.Contains(err.Error(), "attaching tag") {
				t.Fatalf("err=%v, want attaching-tag error", err)
			}
		})
	})

	t.Run("nil session", func(t *testing.T) {
		// Non-nil obj and non-empty tagID, but a nil TagManager -> nil-session
		// error (no simulator needed).
		_, err := AttachTag(context.Background(), &Session{}, "tag-id", &object.Datacenter{})
		if err == nil || !strings.Contains(err.Error(), "session and TagManager must not be nil") {
			t.Fatalf("err=%v, want nil-session error", err)
		}
	})
}

// TestAttachClusterOwnershipTagGaps covers AttachClusterOwnershipTag's
// AttachTag error branch and its already-attached (not newly attached) log
// branch.
func TestAttachClusterOwnershipTagGaps(t *testing.T) {
	t.Run("attach tag error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			folder, err := vmFolder(ctx, t, s)
			if err != nil {
				t.Fatalf("finding folder: %v", err)
			}
			err = AttachClusterOwnershipTag(execFaultCtx(ctx), s, "gap-own-tag", folder)
			if err == nil || !strings.Contains(err.Error(), "attaching cluster ownership tag") {
				t.Fatalf("err=%v, want attaching-cluster-ownership-tag error", err)
			}
		})
	})

	t.Run("already attached log branch", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			folder, err := vmFolder(ctx, t, s)
			if err != nil {
				t.Fatalf("finding folder: %v", err)
			}
			// An empty tagID makes AttachTag return (false, nil), so the
			// already-attached (else) log branch runs.
			if err := AttachClusterOwnershipTag(ctx, s, "", folder); err != nil {
				t.Fatalf("AttachClusterOwnershipTag: %v", err)
			}
		})
	})
}

// TestCreateRegionAndZoneTagsGaps covers the three error branches of
// CreateRegionAndZoneTags by pre-creating a prefix of the resources and then
// faulting the next one via the execution check.
func TestCreateRegionAndZoneTagsGaps(t *testing.T) {
	t.Run("region category error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			// Nothing pre-created: the region category create faults.
			_, _, err := CreateRegionAndZoneTags(execFaultCtx(ctx), s, "us-east", "us-east-1a")
			if err == nil || !strings.Contains(err.Error(), "creating region tag category") {
				t.Fatalf("err=%v, want creating-region-tag-category error", err)
			}
		})
	})

	t.Run("zone category error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			// Pre-create the region category so it is reused; the zone category
			// create faults.
			if _, err := EnsureTagCategory(ctx, s, TagCategoryRegion, TagCategoryRegionDescription, "SINGLE"); err != nil {
				t.Fatalf("pre-creating region category: %v", err)
			}
			_, _, err := CreateRegionAndZoneTags(execFaultCtx(ctx), s, "us-east", "us-east-1a")
			if err == nil || !strings.Contains(err.Error(), "creating zone tag category") {
				t.Fatalf("err=%v, want creating-zone-tag-category error", err)
			}
		})
	})

	t.Run("region tag error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			// Pre-create both categories so they are reused; the region tag
			// create faults.
			if _, err := EnsureTagCategory(ctx, s, TagCategoryRegion, TagCategoryRegionDescription, "SINGLE"); err != nil {
				t.Fatalf("pre-creating region category: %v", err)
			}
			if _, err := EnsureTagCategory(ctx, s, TagCategoryZone, TagCategoryZoneDescription, "SINGLE"); err != nil {
				t.Fatalf("pre-creating zone category: %v", err)
			}
			_, _, err := CreateRegionAndZoneTags(execFaultCtx(ctx), s, "us-east", "us-east-1a")
			if err == nil || !strings.Contains(err.Error(), "creating region tag") {
				t.Fatalf("err=%v, want creating-region-tag error", err)
			}
		})
	})

	t.Run("zone tag error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			// Pre-create both categories and the region tag so they are reused;
			// only the zone tag create faults.
			regionCatID, err := EnsureTagCategory(ctx, s, TagCategoryRegion, TagCategoryRegionDescription, "SINGLE")
			if err != nil {
				t.Fatalf("pre-creating region category: %v", err)
			}
			if _, err := EnsureTagCategory(ctx, s, TagCategoryZone, TagCategoryZoneDescription, "SINGLE"); err != nil {
				t.Fatalf("pre-creating zone category: %v", err)
			}
			if _, err := EnsureTag(ctx, s, regionCatID, "us-east", "OpenShift region us-east"); err != nil {
				t.Fatalf("pre-creating region tag: %v", err)
			}
			_, _, err = CreateRegionAndZoneTags(execFaultCtx(ctx), s, "us-east", "us-east-1a")
			if err == nil || !strings.Contains(err.Error(), "creating zone tag") {
				t.Fatalf("err=%v, want creating-zone-tag error", err)
			}
		})
	})
}

// TestEnsureClusterOwnershipTagGaps covers EnsureClusterOwnershipTag's
// EnsureTag error branch: the ownership category is pre-created and reused,
// then the tag create faults via the execution check.
func TestEnsureClusterOwnershipTagGaps(t *testing.T) {
	t.Run("ensure tag error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			infraID := "gap-infra"
			// Pre-create the ownership category (with the cluster-ownership
			// associable types) so ensureTagCategory reuses it; the tag create
			// then faults.
			catName := ClusterOwnershipCategoryName(infraID)
			if _, err := ensureTagCategory(ctx, s, catName, ClusterOwnershipDescription, "SINGLE",
				clusterOwnershipAssociableTypes, requiredClusterOwnershipAssociableTypes); err != nil {
				t.Fatalf("pre-creating ownership category: %v", err)
			}
			_, err := EnsureClusterOwnershipTag(execFaultCtx(ctx), s, infraID)
			if err == nil || !strings.Contains(err.Error(), "creating cluster ownership tag") {
				t.Fatalf("err=%v, want creating-cluster-ownership-tag error", err)
			}
		})
	})
}

// vmFolder returns the datacenter's default VM folder.
func vmFolder(ctx context.Context, t *testing.T, s *Session) (*object.Folder, error) {
	t.Helper()
	dc, err := s.Finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, err
	}
	folders, err := dc.Folders(ctx)
	if err != nil {
		return nil, err
	}
	return folders.VmFolder, nil
}

// fdTargets returns a datacenter and a cluster for failure-domain attach tests.
func fdTargets(ctx context.Context, t *testing.T, s *Session) (*object.Datacenter, *object.ClusterComputeResource) {
	t.Helper()
	dc, err := s.Finder.DefaultDatacenter(ctx)
	if err != nil {
		t.Fatalf("finding datacenter: %v", err)
	}
	clusters, err := s.Finder.ClusterComputeResourceList(ctx, "*")
	if err != nil {
		t.Fatalf("listing clusters: %v", err)
	}
	if len(clusters) == 0 {
		t.Fatal("expected at least one cluster in simulator inventory")
	}
	return dc, clusters[0]
}

// TestAttachFailureDomainTagsGaps covers AttachFailureDomainTags' region and
// zone attach-error branches.
func TestAttachFailureDomainTagsGaps(t *testing.T) {
	t.Run("region attach tag error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			dc, cluster := fdTargets(ctx, t, s)
			// The execution check inside the region AttachTag faults.
			err := AttachFailureDomainTags(execFaultCtx(ctx), s, "region-tag", "zone-tag", dc, cluster)
			if err == nil || !strings.Contains(err.Error(), "attaching region tag") {
				t.Fatalf("err=%v, want attaching-region-tag error", err)
			}
		})
	})

	t.Run("zone attach tag error", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			dc, cluster := fdTargets(ctx, t, s)
			regionTagID, zoneTagID, err := CreateRegionAndZoneTags(ctx, s, "us-east", "us-east-1a")
			if err != nil {
				t.Fatalf("CreateRegionAndZoneTags: %v", err)
			}
			// A counter that lets the region attach pass and faults the zone
			// attach's execution check.
			var calls int
			ctx2 := execution.WithCheck(ctx, func(context.Context) error {
				calls++
				if calls >= 2 {
					return errors.New("execution fault")
				}
				return nil
			})
			err = AttachFailureDomainTags(ctx2, s, regionTagID, zoneTagID, dc, cluster)
			if err == nil || !strings.Contains(err.Error(), "attaching zone tag") {
				t.Fatalf("err=%v, want attaching-zone-tag error", err)
			}
		})
	})
}
