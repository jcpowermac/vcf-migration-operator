package vsphere

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/tags"
	"k8s.io/klog/v2"
)

const (
	// TagCategoryRegion is the vSphere tag category name used for OpenShift
	// region failure domains.
	TagCategoryRegion = "openshift-region"

	// TagCategoryZone is the vSphere tag category name used for OpenShift
	// zone failure domains.
	TagCategoryZone = "openshift-zone"

	// TagCategoryRegionDescription describes the region tag category.
	TagCategoryRegionDescription = "OpenShift region for failure domain topology"

	// TagCategoryZoneDescription describes the zone tag category.
	TagCategoryZoneDescription = "OpenShift zone for failure domain topology"

	// ClusterOwnershipDescription matches the OpenShift installer cluster
	// ownership tag/category description.
	ClusterOwnershipDescription = "Added by openshift-install do not remove"

	// datacenterType is the vSphere associable type name for datacenters.
	datacenterType = "Datacenter"
	// clusterComputeResourceType is the vSphere associable type name for clusters.
	clusterComputeResourceType = "ClusterComputeResource"
	// datastoreType is the vSphere associable type name for datastores.
	datastoreType = "Datastore"
	// folderType is the vSphere associable type name for folders.
	folderType = "Folder"
)

var (
	tagCategoryAssociableTypes = []string{
		datacenterType,
		clusterComputeResourceType,
		datastoreType,
		folderType,
	}
	requiredTagCategoryAssociableTypes = []string{
		datacenterType,
		clusterComputeResourceType,
	}

	// clusterOwnershipAssociableTypes matches the installer createClusterTagID
	// category associable types (URN-prefixed inventory types).
	clusterOwnershipAssociableTypes = []string{
		"urn:vim25:VirtualMachine",
		"urn:vim25:ResourcePool",
		"urn:vim25:Folder",
		"urn:vim25:Datastore",
		"urn:vim25:StoragePod",
	}
	// requiredClusterOwnershipAssociableTypes are the minimum types needed to
	// attach ownership tags to folders and VMs.
	requiredClusterOwnershipAssociableTypes = []string{
		"urn:vim25:VirtualMachine",
		"urn:vim25:Folder",
	}
)

// ClusterOwnershipCategoryName returns the installer-style cluster ownership
// tag category name for the given infrastructure ID.
func ClusterOwnershipCategoryName(infraID string) string {
	return fmt.Sprintf("openshift-%s", infraID)
}

// isAlreadyExists returns true if the error indicates the resource already exists
// (e.g. vSphere API returns already_exists).
func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already_exists")
}

func validateExistingCategory(category *tags.Category, name, cardinality string, requiredTypes []string) error {
	var incompatibilities []string

	if category.Cardinality != cardinality {
		incompatibilities = append(incompatibilities, fmt.Sprintf("cardinality %q does not match required %q", category.Cardinality, cardinality))
	}

	missingTypes := missingAssociableTypes(category.AssociableTypes, requiredTypes)
	if len(missingTypes) > 0 {
		incompatibilities = append(incompatibilities, fmt.Sprintf("missing required associable types %s", strings.Join(missingTypes, ", ")))
	}

	if len(incompatibilities) == 0 {
		return nil
	}

	return fmt.Errorf("existing tag category %q is incompatible: %s; update the category in the vSphere UI or delete it and let the operator recreate it", name, strings.Join(incompatibilities, "; "))
}

func missingAssociableTypes(existingTypes, requiredTypes []string) []string {
	existing := make(map[string]struct{}, len(existingTypes))
	for _, associableType := range existingTypes {
		existing[associableType] = struct{}{}
	}

	missing := make([]string, 0, len(requiredTypes))
	for _, requiredType := range requiredTypes {
		if _, ok := existing[requiredType]; !ok {
			missing = append(missing, requiredType)
		}
	}

	return missing
}

// ObjectHasTagInCategory reports whether the given vSphere managed object
// already has a tag from the named category attached. It is used to skip
// tag creation and attachment when the customer (or a previous run) has
// already set up the failure domain tags.
func ObjectHasTagInCategory(ctx context.Context, s *Session, categoryName string, obj object.Reference) (bool, error) {
	if s == nil || s.TagManager == nil {
		return false, fmt.Errorf("session and TagManager must not be nil")
	}
	log := klog.FromContext(ctx)

	category, err := s.TagManager.GetCategory(ctx, categoryName)
	if err != nil {
		if rest.IsStatusError(err, http.StatusNotFound) {
			log.V(2).Info("tag category not found, no tags to check", "category", categoryName)
			return false, nil
		}
		return false, fmt.Errorf("getting tag category %q: %w", categoryName, err)
	}
	if category == nil || category.ID == "" {
		log.V(2).Info("tag category not found, no tags to check", "category", categoryName)
		return false, nil
	}

	categoryTagIDs, err := s.TagManager.ListTagsForCategory(ctx, category.ID)
	if err != nil {
		return false, fmt.Errorf("listing tags for category %q: %w", categoryName, err)
	}
	if len(categoryTagIDs) == 0 {
		return false, nil
	}

	ref := obj.Reference()
	attachedTagIDs, err := s.TagManager.ListAttachedTags(ctx, ref)
	if err != nil {
		return false, fmt.Errorf("listing attached tags on %s: %w", ref, err)
	}

	categoryTagSet := make(map[string]struct{}, len(categoryTagIDs))
	for _, id := range categoryTagIDs {
		categoryTagSet[id] = struct{}{}
	}
	for _, id := range attachedTagIDs {
		if _, ok := categoryTagSet[id]; ok {
			log.V(2).Info("object already has tag from category", "category", categoryName, "tagID", id, "object", ref)
			return true, nil
		}
	}
	return false, nil
}

// EnsureTagCategory returns the vSphere tag category ID for the given name,
// creating the category if it does not exist. If the category already exists
// (e.g. from a previous run or another cluster), it is looked up and its ID is
// returned so creation is idempotent. Categories are created with the OpenShift
// topology associable types (Datacenter, ClusterComputeResource, Datastore, Folder).
func EnsureTagCategory(ctx context.Context, s *Session, name, description, cardinality string) (string, error) {
	return ensureTagCategory(ctx, s, name, description, cardinality, tagCategoryAssociableTypes, requiredTagCategoryAssociableTypes)
}

func ensureTagCategory(ctx context.Context, s *Session, name, description, cardinality string, associableTypes, requiredTypes []string) (string, error) {
	if s == nil || s.TagManager == nil {
		return "", fmt.Errorf("session and TagManager must not be nil")
	}
	log := klog.FromContext(ctx)
	log.V(2).Info("ensuring tag category", "name", name, "cardinality", cardinality)

	existing, err := s.TagManager.GetCategory(ctx, name)
	if err == nil && existing != nil && existing.ID != "" {
		if err := validateExistingCategory(existing, name, cardinality, requiredTypes); err != nil {
			return "", err
		}
		log.V(2).Info("using existing tag category", "name", name, "id", existing.ID)
		return existing.ID, nil
	}

	cat := tags.Category{
		Name:            name,
		Description:     description,
		Cardinality:     cardinality,
		AssociableTypes: associableTypes,
	}

	id, err := s.TagManager.CreateCategory(ctx, &cat)
	if err != nil {
		if isAlreadyExists(err) {
			existing, getErr := s.TagManager.GetCategory(ctx, name)
			if getErr != nil {
				return "", fmt.Errorf("creating tag category %q (already exists but get failed): %w", name, getErr)
			}
			if err := validateExistingCategory(existing, name, cardinality, requiredTypes); err != nil {
				return "", err
			}
			log.V(2).Info("tag category already existed, using existing", "name", name, "id", existing.ID)
			return existing.ID, nil
		}
		return "", fmt.Errorf("creating tag category %q: %w", name, err)
	}

	log.V(2).Info("created tag category", "name", name, "id", id)
	return id, nil
}

// EnsureClusterOwnershipTag ensures the installer-style cluster ownership tag
// category (openshift-<infraID>) and tag (<infraID>) exist on the vCenter,
// creating them if necessary. It returns the tag ID for use in MachineSet
// providerSpec.tagIDs. Idempotent when the category or tag already exists.
func EnsureClusterOwnershipTag(ctx context.Context, s *Session, infraID string) (string, error) {
	if infraID == "" {
		return "", fmt.Errorf("infraID must not be empty")
	}
	log := klog.FromContext(ctx)
	categoryName := ClusterOwnershipCategoryName(infraID)
	log.V(1).Info("ensuring cluster ownership tag", "infraID", infraID, "category", categoryName)

	catID, err := ensureTagCategory(ctx, s, categoryName, ClusterOwnershipDescription, "SINGLE",
		clusterOwnershipAssociableTypes, requiredClusterOwnershipAssociableTypes)
	if err != nil {
		return "", fmt.Errorf("creating cluster ownership tag category: %w", err)
	}

	tagID, err := EnsureTag(ctx, s, catID, infraID, ClusterOwnershipDescription)
	if err != nil {
		return "", fmt.Errorf("creating cluster ownership tag: %w", err)
	}

	log.V(1).Info("ensured cluster ownership tag", "infraID", infraID, "tagID", tagID)
	return tagID, nil
}

// AttachClusterOwnershipTag attaches the cluster ownership tag to the given
// VM folder. Idempotent when the tag is already attached.
func AttachClusterOwnershipTag(ctx context.Context, s *Session, tagID string, folder *object.Folder) error {
	if folder == nil {
		return fmt.Errorf("cannot attach cluster ownership tag: folder is nil")
	}
	log := klog.FromContext(ctx)
	ref := folder.Reference()

	attached, err := AttachTag(ctx, s, tagID, folder)
	if err != nil {
		return fmt.Errorf("attaching cluster ownership tag to folder %s: %w", ref, err)
	}
	if attached {
		log.V(1).Info("ensured cluster ownership tag attached to folder", "tagID", tagID, "folder", ref)
	} else {
		log.V(1).Info("cluster ownership tag already attached to folder", "tagID", tagID, "folder", ref)
	}
	return nil
}

// EnsureTag returns the vSphere tag ID for the given name in the category,
// creating the tag if it does not exist. Idempotent when the tag already exists.
func EnsureTag(ctx context.Context, s *Session, categoryID, name, description string) (string, error) {
	if s == nil || s.TagManager == nil {
		return "", fmt.Errorf("session and TagManager must not be nil")
	}
	log := klog.FromContext(ctx)
	log.V(2).Info("ensuring tag", "name", name, "categoryID", categoryID)

	existing, err := s.TagManager.GetTagForCategory(ctx, name, categoryID)
	if err == nil && existing != nil && existing.ID != "" {
		log.V(2).Info("using existing tag", "name", name, "id", existing.ID)
		return existing.ID, nil
	}

	tag := tags.Tag{
		Name:        name,
		Description: description,
		CategoryID:  categoryID,
	}

	id, err := s.TagManager.CreateTag(ctx, &tag)
	if err != nil {
		if isAlreadyExists(err) {
			existing, getErr := s.TagManager.GetTagForCategory(ctx, name, categoryID)
			if getErr != nil {
				return "", fmt.Errorf("creating tag %q (already exists but get failed): %w", name, getErr)
			}
			log.V(2).Info("tag already existed, using existing", "name", name, "id", existing.ID)
			return existing.ID, nil
		}
		return "", fmt.Errorf("creating tag %q in category %s: %w", name, categoryID, err)
	}

	log.V(2).Info("created tag", "name", name, "id", id)
	return id, nil
}

// AttachTag attaches a vSphere tag to the given managed object reference. It
// returns true when the tag was newly attached, and false when the attachment
// already existed or no tag ID was provided.
func AttachTag(ctx context.Context, s *Session, tagID string, obj object.Reference) (bool, error) {
	if obj == nil {
		return false, fmt.Errorf("cannot attach tag: object reference is nil")
	}
	if tagID == "" {
		return false, nil
	}
	if s == nil || s.TagManager == nil {
		return false, fmt.Errorf("session and TagManager must not be nil")
	}
	log := klog.FromContext(ctx)
	ref := obj.Reference()
	log.V(2).Info("attaching tag", "tagID", tagID, "object", ref)

	if err := s.TagManager.AttachTag(ctx, tagID, ref); err != nil {
		if isAlreadyExists(err) {
			log.V(2).Info("tag already attached", "tagID", tagID, "object", ref)
			return false, nil
		}
		return false, fmt.Errorf("attaching tag %s to %s: %w", tagID, ref, err)
	}
	return true, nil
}

// CreateRegionAndZoneTags ensures the OpenShift region and zone tag categories
// and their corresponding tags exist, creating them if necessary. It returns the
// region and zone tag IDs. Idempotent when categories or tags already exist.
func CreateRegionAndZoneTags(ctx context.Context, s *Session, region, zone string) (regionTagID, zoneTagID string, err error) {
	log := klog.FromContext(ctx)
	log.V(1).Info("creating region and zone tags", "region", region, "zone", zone)

	regionCatID, err := EnsureTagCategory(ctx, s, TagCategoryRegion, TagCategoryRegionDescription, "SINGLE")
	if err != nil {
		return "", "", fmt.Errorf("creating region tag category: %w", err)
	}

	zoneCatID, err := EnsureTagCategory(ctx, s, TagCategoryZone, TagCategoryZoneDescription, "SINGLE")
	if err != nil {
		return "", "", fmt.Errorf("creating zone tag category: %w", err)
	}

	regionTagID, err = EnsureTag(ctx, s, regionCatID, region, fmt.Sprintf("OpenShift region %s", region))
	if err != nil {
		return "", "", fmt.Errorf("creating region tag: %w", err)
	}

	zoneTagID, err = EnsureTag(ctx, s, zoneCatID, zone, fmt.Sprintf("OpenShift zone %s", zone))
	if err != nil {
		return "", "", fmt.Errorf("creating zone tag: %w", err)
	}

	log.V(1).Info("created region and zone tags", "regionTagID", regionTagID, "zoneTagID", zoneTagID)
	return regionTagID, zoneTagID, nil
}

// AttachFailureDomainTags attaches the region tag to the datacenter and the zone
// tag to the cluster compute resource. This establishes the vSphere failure
// domain topology required by OpenShift.
func AttachFailureDomainTags(ctx context.Context, s *Session, regionTagID, zoneTagID string, datacenter *object.Datacenter, cluster *object.ClusterComputeResource) error {
	if datacenter == nil {
		return fmt.Errorf("cannot attach failure domain tags: datacenter is nil")
	}
	if cluster == nil {
		return fmt.Errorf("cannot attach failure domain tags: cluster is nil")
	}
	log := klog.FromContext(ctx)
	log.V(1).Info("attaching failure domain tags",
		"regionTagID", regionTagID,
		"zoneTagID", zoneTagID,
		"datacenter", datacenter.Reference(),
		"cluster", cluster.Reference(),
	)

	ref := datacenter.Reference()
	if regionTagID != "" {
		attached, err := AttachTag(ctx, s, regionTagID, datacenter)
		if err != nil {
			return fmt.Errorf("attaching region tag to datacenter %s: %w", ref, err)
		}
		if attached {
			log.V(1).Info("ensured region tag attached to datacenter", "regionTagID", regionTagID, "datacenter", ref)
		} else {
			log.V(1).Info("region tag already attached to datacenter", "regionTagID", regionTagID, "datacenter", ref)
		}
	}

	clusterRef := cluster.Reference()
	if zoneTagID != "" {
		attached, err := AttachTag(ctx, s, zoneTagID, cluster)
		if err != nil {
			return fmt.Errorf("attaching zone tag to cluster %s: %w", clusterRef, err)
		}
		if attached {
			log.V(1).Info("ensured zone tag attached to cluster", "zoneTagID", zoneTagID, "cluster", clusterRef)
		} else {
			log.V(1).Info("zone tag already attached to cluster", "zoneTagID", zoneTagID, "cluster", clusterRef)
		}
	}

	return nil
}
