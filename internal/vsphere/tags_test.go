package vsphere

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25"

	_ "github.com/vmware/govmomi/vapi/simulator"
)

func newTestSession(ctx context.Context, t *testing.T, c *vim25.Client) *Session {
	t.Helper()

	gc := &govmomi.Client{
		Client: c,
	}

	finder := find.NewFinder(c, true)
	dc, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		t.Fatalf("finding default datacenter: %v", err)
	}
	finder.SetDatacenter(dc)

	rc := rest.NewClient(c)
	if err := rc.Login(ctx, simulator.DefaultLogin); err != nil {
		t.Fatalf("REST login: %v", err)
	}

	tagMgr := tags.NewManager(rc)

	return &Session{
		Client:     gc,
		Finder:     finder,
		TagManager: tagMgr,
		Datacenter: dc.Name(),
		restClient: rc,
	}
}

func TestEnsureTagCategory(t *testing.T) {
	t.Run("rejects nil session", func(t *testing.T) {
		_, err := EnsureTagCategory(context.Background(), nil, "test-category", "a test category", "SINGLE")
		if err == nil {
			t.Fatal("EnsureTagCategory succeeded, want nil session error")
		}
		if !strings.Contains(err.Error(), "session and TagManager must not be nil") {
			t.Fatalf("EnsureTagCategory error = %q, want nil session detail", err.Error())
		}
	})

	t.Run("creates and reuses category", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)

			id, err := EnsureTagCategory(ctx, s, "test-category", "a test category", "SINGLE")
			if err != nil {
				t.Fatalf("EnsureTagCategory: %v", err)
			}
			if id == "" {
				t.Fatal("expected non-empty category ID")
			}

			id2, err := EnsureTagCategory(ctx, s, "test-category", "a test category", "SINGLE")
			if err != nil {
				t.Fatalf("EnsureTagCategory (second call): %v", err)
			}
			if id2 != id {
				t.Fatalf("expected same category ID, got %q and %q", id, id2)
			}
		})
	})

	t.Run("rejects mismatched cardinality on reuse", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			createTestCategory(ctx, t, s, tags.Category{
				Name:        "test-category",
				Description: "created externally",
				Cardinality: "MULTIPLE",
				AssociableTypes: []string{
					datacenterType,
					clusterComputeResourceType,
					datastoreType,
					folderType,
				},
			})

			_, err := EnsureTagCategory(ctx, s, "test-category", "a test category", "SINGLE")
			if err == nil {
				t.Fatal("EnsureTagCategory succeeded, want cardinality mismatch error")
			}
			if !strings.Contains(err.Error(), `cardinality "MULTIPLE"`) {
				t.Fatalf("EnsureTagCategory error = %q, want cardinality mismatch detail", err.Error())
			}
			if !strings.Contains(err.Error(), "delete it and let the operator recreate it") {
				t.Fatalf("EnsureTagCategory error = %q, want resolution guidance", err.Error())
			}
		})
	})

	t.Run("rejects missing required associable types on reuse", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			createTestCategory(ctx, t, s, tags.Category{
				Name:            "test-category",
				Description:     "created externally",
				Cardinality:     "SINGLE",
				AssociableTypes: []string{datastoreType, folderType},
			})

			_, err := EnsureTagCategory(ctx, s, "test-category", "a test category", "SINGLE")
			if err == nil {
				t.Fatal("EnsureTagCategory succeeded, want associable type mismatch error")
			}
			if !strings.Contains(err.Error(), "missing required associable types") {
				t.Fatalf("EnsureTagCategory error = %q, want missing associable types detail", err.Error())
			}
			if !strings.Contains(err.Error(), datacenterType) || !strings.Contains(err.Error(), clusterComputeResourceType) {
				t.Fatalf("EnsureTagCategory error = %q, want missing type names", err.Error())
			}
		})
	})

	t.Run("allows compatible existing category with extra associable types", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			id := createTestCategory(ctx, t, s, tags.Category{
				Name:        "test-category",
				Description: "custom description",
				Cardinality: "SINGLE",
				AssociableTypes: []string{
					datacenterType,
					clusterComputeResourceType,
					datastoreType,
					folderType,
					"VirtualMachine",
				},
			})

			gotID, err := EnsureTagCategory(ctx, s, "test-category", "a test category", "SINGLE")
			if err != nil {
				t.Fatalf("EnsureTagCategory: %v", err)
			}
			if gotID != id {
				t.Fatalf("EnsureTagCategory ID = %q, want %q", gotID, id)
			}
		})
	})
}

func TestEnsureTag(t *testing.T) {
	t.Run("rejects nil session", func(t *testing.T) {
		_, err := EnsureTag(context.Background(), nil, "category-id", "test-tag", "a test tag")
		if err == nil {
			t.Fatal("EnsureTag succeeded, want nil session error")
		}
		if !strings.Contains(err.Error(), "session and TagManager must not be nil") {
			t.Fatalf("EnsureTag error = %q, want nil session detail", err.Error())
		}
	})

	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		s := newTestSession(ctx, t, c)

		catID, err := EnsureTagCategory(ctx, s, "test-category", "a test category", "SINGLE")
		if err != nil {
			t.Fatalf("EnsureTagCategory: %v", err)
		}

		tagID, err := EnsureTag(ctx, s, catID, "test-tag", "a test tag")
		if err != nil {
			t.Fatalf("EnsureTag: %v", err)
		}
		if tagID == "" {
			t.Fatal("expected non-empty tag ID")
		}
		// Idempotent: second call returns same tag
		tagID2, err := EnsureTag(ctx, s, catID, "test-tag", "a test tag")
		if err != nil {
			t.Fatalf("EnsureTag (second call): %v", err)
		}
		if tagID2 != tagID {
			t.Fatalf("expected same tag ID, got %q and %q", tagID, tagID2)
		}
	})
}

func TestCreateRegionAndZoneTags(t *testing.T) {
	t.Run("creates and reuses tags", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)

			regionTagID, zoneTagID, err := CreateRegionAndZoneTags(ctx, s, "us-east", "us-east-1a")
			if err != nil {
				t.Fatalf("CreateRegionAndZoneTags: %v", err)
			}
			if regionTagID == "" {
				t.Fatal("expected non-empty region tag ID")
			}
			if zoneTagID == "" {
				t.Fatal("expected non-empty zone tag ID")
			}

			regionTagID2, zoneTagID2, err := CreateRegionAndZoneTags(ctx, s, "us-east", "us-east-1a")
			if err != nil {
				t.Fatalf("CreateRegionAndZoneTags (second call): %v", err)
			}
			if regionTagID2 != regionTagID {
				t.Fatalf("expected same region tag ID, got %q and %q", regionTagID, regionTagID2)
			}
			if zoneTagID2 != zoneTagID {
				t.Fatalf("expected same zone tag ID, got %q and %q", zoneTagID, zoneTagID2)
			}
		})
	})

	t.Run("surfaces region category compatibility errors", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			createTestCategory(ctx, t, s, tags.Category{
				Name:            TagCategoryRegion,
				Description:     "created externally",
				Cardinality:     "MULTIPLE",
				AssociableTypes: append([]string(nil), tagCategoryAssociableTypes...),
			})

			_, _, err := CreateRegionAndZoneTags(ctx, s, "us-east", "us-east-1a")
			if err == nil {
				t.Fatal("CreateRegionAndZoneTags succeeded, want region category compatibility error")
			}
			if !strings.Contains(err.Error(), "creating region tag category") {
				t.Fatalf("CreateRegionAndZoneTags error = %q, want wrapped region category detail", err.Error())
			}
		})
	})
}

func TestAttachTag(t *testing.T) {
	t.Run("rejects nil object", func(t *testing.T) {
		_, err := AttachTag(context.Background(), &Session{}, "tag-id", nil)
		if err == nil {
			t.Fatal("AttachTag succeeded, want nil object error")
		}
		if !strings.Contains(err.Error(), "object reference is nil") {
			t.Fatalf("AttachTag error = %q, want nil object detail", err.Error())
		}
	})

	t.Run("skips empty tag ID", func(t *testing.T) {
		attached, err := AttachTag(context.Background(), &Session{}, "", &object.Datacenter{})
		if err != nil {
			t.Fatalf("AttachTag: %v", err)
		}
		if attached {
			t.Fatal("AttachTag reported attachment for empty tag ID")
		}
	})

	t.Run("matches already exists errors", func(t *testing.T) {
		if !isAlreadyExists(fmt.Errorf("server returned already_exists")) {
			t.Fatal("isAlreadyExists returned false, want true")
		}
		if isAlreadyExists(fmt.Errorf("not found")) {
			t.Fatal("isAlreadyExists returned true for unrelated error")
		}
		if isAlreadyExists(nil) {
			t.Fatal("isAlreadyExists returned true for nil error")
		}
	})
}

func TestAttachFailureDomainTags(t *testing.T) {
	t.Run("attaches expected tags and is idempotent", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)

			regionTagID, zoneTagID, err := CreateRegionAndZoneTags(ctx, s, "us-east", "us-east-1a")
			if err != nil {
				t.Fatalf("CreateRegionAndZoneTags: %v", err)
			}

			dc, err := s.Finder.DefaultDatacenter(ctx)
			if err != nil {
				t.Fatalf("finding default datacenter: %v", err)
			}

			clusters, err := s.Finder.ClusterComputeResourceList(ctx, "*")
			if err != nil {
				t.Fatalf("listing clusters: %v", err)
			}
			if len(clusters) == 0 {
				t.Fatal("expected at least one cluster in simulator inventory")
			}
			cluster := clusters[0]

			if err := AttachFailureDomainTags(ctx, s, regionTagID, zoneTagID, dc, cluster); err != nil {
				t.Fatalf("AttachFailureDomainTags: %v", err)
			}

			dcTags, err := s.TagManager.ListAttachedTags(ctx, dc.Reference())
			if err != nil {
				t.Fatalf("listing attached datacenter tags: %v", err)
			}
			assertTagAttached(t, dcTags, regionTagID)

			clusterTags, err := s.TagManager.ListAttachedTags(ctx, cluster.Reference())
			if err != nil {
				t.Fatalf("listing attached cluster tags: %v", err)
			}
			assertTagAttached(t, clusterTags, zoneTagID)

			if err := AttachFailureDomainTags(ctx, s, regionTagID, zoneTagID, dc, cluster); err != nil {
				t.Fatalf("AttachFailureDomainTags (second call): %v", err)
			}
		})
	})

	t.Run("rejects nil datacenter", func(t *testing.T) {
		err := AttachFailureDomainTags(context.Background(), &Session{}, "region-tag", "zone-tag", nil, &object.ClusterComputeResource{})
		if err == nil {
			t.Fatal("AttachFailureDomainTags succeeded, want nil datacenter error")
		}
		if !strings.Contains(err.Error(), "datacenter is nil") {
			t.Fatalf("AttachFailureDomainTags error = %q, want nil datacenter detail", err.Error())
		}
	})

	t.Run("rejects nil cluster", func(t *testing.T) {
		err := AttachFailureDomainTags(context.Background(), &Session{}, "region-tag", "zone-tag", &object.Datacenter{}, nil)
		if err == nil {
			t.Fatal("AttachFailureDomainTags succeeded, want nil cluster error")
		}
		if !strings.Contains(err.Error(), "cluster is nil") {
			t.Fatalf("AttachFailureDomainTags error = %q, want nil cluster detail", err.Error())
		}
	})
}

func TestObjectHasTagInCategory(t *testing.T) {
	t.Run("rejects nil session", func(t *testing.T) {
		_, err := ObjectHasTagInCategory(context.Background(), nil, "test-category", &object.Datacenter{})
		if err == nil {
			t.Fatal("ObjectHasTagInCategory succeeded, want nil session error")
		}
	})

	t.Run("returns false when category does not exist", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			dc, err := s.Finder.DefaultDatacenter(ctx)
			if err != nil {
				t.Fatalf("finding default datacenter: %v", err)
			}

			has, err := ObjectHasTagInCategory(ctx, s, "nonexistent-category", dc)
			if err != nil {
				t.Fatalf("ObjectHasTagInCategory: %v", err)
			}
			if has {
				t.Fatal("expected false for nonexistent category")
			}
		})
	})

	t.Run("returns false when category exists but no tags attached", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			createTestCategory(ctx, t, s, tags.Category{
				Name:            "test-category",
				Description:     "test",
				Cardinality:     "SINGLE",
				AssociableTypes: []string{datacenterType},
			})
			if _, err := s.TagManager.CreateTag(ctx, &tags.Tag{
				Name:       "test-tag",
				CategoryID: "test-category",
			}); err != nil {
				t.Fatalf("creating test tag: %v", err)
			}

			dc, err := s.Finder.DefaultDatacenter(ctx)
			if err != nil {
				t.Fatalf("finding default datacenter: %v", err)
			}

			has, err := ObjectHasTagInCategory(ctx, s, "test-category", dc)
			if err != nil {
				t.Fatalf("ObjectHasTagInCategory: %v", err)
			}
			if has {
				t.Fatal("expected false when no tags attached")
			}
		})
	})

	t.Run("returns true when tag from category is attached", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			regionTagID, _, err := CreateRegionAndZoneTags(ctx, s, "us-east", "us-east-1a")
			if err != nil {
				t.Fatalf("CreateRegionAndZoneTags: %v", err)
			}

			dc, err := s.Finder.DefaultDatacenter(ctx)
			if err != nil {
				t.Fatalf("finding default datacenter: %v", err)
			}

			if err := s.TagManager.AttachTag(ctx, regionTagID, dc.Reference()); err != nil {
				t.Fatalf("AttachTag: %v", err)
			}

			has, err := ObjectHasTagInCategory(ctx, s, TagCategoryRegion, dc)
			if err != nil {
				t.Fatalf("ObjectHasTagInCategory: %v", err)
			}
			if !has {
				t.Fatal("expected true when tag from category is attached")
			}
		})
	})

	t.Run("returns false for different category tag", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			regionTagID, _, err := CreateRegionAndZoneTags(ctx, s, "us-east", "us-east-1a")
			if err != nil {
				t.Fatalf("CreateRegionAndZoneTags: %v", err)
			}

			dc, err := s.Finder.DefaultDatacenter(ctx)
			if err != nil {
				t.Fatalf("finding default datacenter: %v", err)
			}

			if err := s.TagManager.AttachTag(ctx, regionTagID, dc.Reference()); err != nil {
				t.Fatalf("AttachTag: %v", err)
			}

			has, err := ObjectHasTagInCategory(ctx, s, TagCategoryZone, dc)
			if err != nil {
				t.Fatalf("ObjectHasTagInCategory: %v", err)
			}
			if has {
				t.Fatal("expected false when only a different category's tag is attached")
			}
		})
	})
}

func TestEnsureClusterOwnershipTag(t *testing.T) {
	t.Run("rejects nil session", func(t *testing.T) {
		_, err := EnsureClusterOwnershipTag(context.Background(), nil, "test-infra-abc")
		if err == nil {
			t.Fatal("EnsureClusterOwnershipTag succeeded, want nil session error")
		}
		if !strings.Contains(err.Error(), "session and TagManager must not be nil") {
			t.Fatalf("EnsureClusterOwnershipTag error = %q, want nil session detail", err.Error())
		}
	})

	t.Run("rejects empty infraID", func(t *testing.T) {
		_, err := EnsureClusterOwnershipTag(context.Background(), &Session{}, "")
		if err == nil {
			t.Fatal("EnsureClusterOwnershipTag succeeded, want empty infraID error")
		}
		if !strings.Contains(err.Error(), "infraID") {
			t.Fatalf("EnsureClusterOwnershipTag error = %q, want infraID detail", err.Error())
		}
	})

	t.Run("creates and reuses ownership category and tag", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			infraID := "cluster-abc123"

			tagID, err := EnsureClusterOwnershipTag(ctx, s, infraID)
			if err != nil {
				t.Fatalf("EnsureClusterOwnershipTag: %v", err)
			}
			if tagID == "" {
				t.Fatal("expected non-empty tag ID")
			}

			categoryName := ClusterOwnershipCategoryName(infraID)
			cat, err := s.TagManager.GetCategory(ctx, categoryName)
			if err != nil {
				t.Fatalf("GetCategory %q: %v", categoryName, err)
			}
			if cat.Description != ClusterOwnershipDescription {
				t.Fatalf("category description = %q, want %q", cat.Description, ClusterOwnershipDescription)
			}
			if cat.Cardinality != "SINGLE" {
				t.Fatalf("category cardinality = %q, want SINGLE", cat.Cardinality)
			}
			for _, wantType := range clusterOwnershipAssociableTypes {
				found := false
				for _, gotType := range cat.AssociableTypes {
					if gotType == wantType {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("category AssociableTypes = %v, want to include %q", cat.AssociableTypes, wantType)
				}
			}

			tag, err := s.TagManager.GetTag(ctx, tagID)
			if err != nil {
				t.Fatalf("GetTag: %v", err)
			}
			if tag.Name != infraID {
				t.Fatalf("tag name = %q, want %q", tag.Name, infraID)
			}
			if tag.Description != ClusterOwnershipDescription {
				t.Fatalf("tag description = %q, want %q", tag.Description, ClusterOwnershipDescription)
			}

			tagID2, err := EnsureClusterOwnershipTag(ctx, s, infraID)
			if err != nil {
				t.Fatalf("EnsureClusterOwnershipTag (second call): %v", err)
			}
			if tagID2 != tagID {
				t.Fatalf("expected same tag ID, got %q and %q", tagID, tagID2)
			}
		})
	})

	t.Run("surfaces incompatible existing category", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			infraID := "cluster-bad"
			createTestCategory(ctx, t, s, tags.Category{
				Name:            ClusterOwnershipCategoryName(infraID),
				Description:     ClusterOwnershipDescription,
				Cardinality:     "MULTIPLE",
				AssociableTypes: append([]string(nil), clusterOwnershipAssociableTypes...),
			})

			_, err := EnsureClusterOwnershipTag(ctx, s, infraID)
			if err == nil {
				t.Fatal("EnsureClusterOwnershipTag succeeded, want cardinality mismatch error")
			}
			if !strings.Contains(err.Error(), "incompatible") {
				t.Fatalf("EnsureClusterOwnershipTag error = %q, want incompatible detail", err.Error())
			}
		})
	})

	t.Run("accepts existing category whose associable types lack the urn:vim25 prefix", func(t *testing.T) {
		// Real vCenter's tagging service is suspected to return associable types
		// without the "urn:vim25:" prefix that the installer uses on create, even
		// though the govmomi simulator round-trips them verbatim. Validation must
		// tolerate this so a second reconcile doesn't treat the installer-style
		// category it just created as incompatible.
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			infraID := "cluster-unprefixed"
			createTestCategory(ctx, t, s, tags.Category{
				Name:            ClusterOwnershipCategoryName(infraID),
				Description:     ClusterOwnershipDescription,
				Cardinality:     "SINGLE",
				AssociableTypes: []string{"VirtualMachine", "ResourcePool", "Folder", "Datastore", "StoragePod"},
			})

			if _, err := EnsureClusterOwnershipTag(ctx, s, infraID); err != nil {
				t.Fatalf("EnsureClusterOwnershipTag: %v", err)
			}
		})
	})
}

func TestAttachClusterOwnershipTag(t *testing.T) {
	t.Run("rejects nil folder", func(t *testing.T) {
		err := AttachClusterOwnershipTag(context.Background(), &Session{}, "tag-id", nil)
		if err == nil {
			t.Fatal("AttachClusterOwnershipTag succeeded, want nil folder error")
		}
		if !strings.Contains(err.Error(), "folder is nil") {
			t.Fatalf("AttachClusterOwnershipTag error = %q, want nil folder detail", err.Error())
		}
	})

	t.Run("attaches ownership tag to folder and is idempotent", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)
			infraID := "cluster-folder-tag"

			tagID, err := EnsureClusterOwnershipTag(ctx, s, infraID)
			if err != nil {
				t.Fatalf("EnsureClusterOwnershipTag: %v", err)
			}

			folder, err := CreateVMFolder(ctx, s, infraID)
			if err != nil {
				t.Fatalf("CreateVMFolder: %v", err)
			}

			if err := AttachClusterOwnershipTag(ctx, s, tagID, folder); err != nil {
				t.Fatalf("AttachClusterOwnershipTag: %v", err)
			}

			attached, err := s.TagManager.ListAttachedTags(ctx, folder.Reference())
			if err != nil {
				t.Fatalf("listing attached folder tags: %v", err)
			}
			assertTagAttached(t, attached, tagID)

			if err := AttachClusterOwnershipTag(ctx, s, tagID, folder); err != nil {
				t.Fatalf("AttachClusterOwnershipTag (second call): %v", err)
			}
		})
	})

}

func TestEnsureClusterTag(t *testing.T) {
	t.Run("rejects incompatible existing category", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)

			createTestCategory(ctx, t, s, tags.Category{
				Name:            "openshift-bad-infra",
				Description:     "incompatible category",
				Cardinality:     "SINGLE",
				AssociableTypes: []string{datastoreType, folderType},
			})

			_, err := EnsureClusterTag(ctx, s, "bad-infra")
			if err == nil {
				t.Fatal("EnsureClusterTag succeeded, want incompatible category error")
			}
			if !strings.Contains(err.Error(), "missing required associable types") {
				t.Fatalf("EnsureClusterTag error = %q, want missing associable types detail", err.Error())
			}
			for _, missingType := range []string{"urn:vim25:VirtualMachine", "urn:vim25:ResourcePool", "urn:vim25:StoragePod"} {
				if !strings.Contains(err.Error(), missingType) {
					t.Fatalf("EnsureClusterTag error = %q, want %q in missing types", err.Error(), missingType)
				}
			}
		})
	})

	t.Run("creates category with urn-prefixed associable types", func(t *testing.T) {
		simulator.Test(func(ctx context.Context, c *vim25.Client) {
			s := newTestSession(ctx, t, c)

			tagID, err := EnsureClusterTag(ctx, s, "urn-infra")
			if err != nil {
				t.Fatalf("EnsureClusterTag: %v", err)
			}
			if tagID == "" {
				t.Fatal("EnsureClusterTag returned empty tag ID")
			}

			cat, err := s.TagManager.GetCategory(ctx, "openshift-urn-infra")
			if err != nil {
				t.Fatalf("GetCategory: %v", err)
			}
			got := make(map[string]bool, len(cat.AssociableTypes))
			for _, at := range cat.AssociableTypes {
				got[normalizeAssociableType(at)] = true
			}
			for _, want := range clusterOwnershipAssociableTypes {
				if !got[normalizeAssociableType(want)] {
					t.Fatalf("category associable types %v missing %q", cat.AssociableTypes, want)
				}
			}
		})
	})
}

func createTestCategory(ctx context.Context, t *testing.T, s *Session, category tags.Category) string {
	t.Helper()

	id, err := s.TagManager.CreateCategory(ctx, &category)
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	return id
}

func assertTagAttached(t *testing.T, attachedTags []string, wantTagID string) {
	t.Helper()

	for _, tagID := range attachedTags {
		if tagID == wantTagID {
			return
		}
	}

	t.Fatalf("attached tags = %v, want %q", attachedTags, wantTagID)
}
