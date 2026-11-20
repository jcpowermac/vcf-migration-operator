package vsphere

import (
	"context"
	"fmt"

	"github.com/vmware/govmomi/pbm"
	pbmtypes "github.com/vmware/govmomi/pbm/types"
)

// GetProfileIDByName returns the target policy ID, or an empty ID when the policy does not exist.
// API and permission failures remain errors rather than being mistaken for missing policies.
func GetProfileIDByName(ctx context.Context, s *Session, name string) (string, error) {
	c, err := pbm.NewClient(ctx, s.Client.Client)
	if err != nil {
		return "", fmt.Errorf("creating PBM client: %w", err)
	}
	ids, err := c.QueryProfile(ctx, pbmtypes.PbmProfileResourceType{ResourceType: string(pbmtypes.PbmProfileResourceTypeEnumSTORAGE)}, string(pbmtypes.PbmProfileCategoryEnumREQUIREMENT))
	if err != nil {
		return "", fmt.Errorf("listing storage policies: %w", err)
	}
	if len(ids) == 0 {
		return "", nil
	}
	profiles, err := c.RetrieveContent(ctx, ids)
	if err != nil {
		return "", fmt.Errorf("reading storage policies: %w", err)
	}
	for _, p := range profiles {
		profile := p.GetPbmProfile()
		if profile.Name == name {
			return profile.ProfileId.UniqueId, nil
		}
	}
	return "", nil
}

// IsDatastoreCompatible checks a policy against exactly the destination datastore.
func IsDatastoreCompatible(ctx context.Context, s *Session, profileID, datastore string) (bool, error) {
	ds, err := s.Finder.Datastore(ctx, datastore)
	if err != nil {
		return false, fmt.Errorf("finding datastore %s: %w", datastore, err)
	}
	c, err := pbm.NewClient(ctx, s.Client.Client)
	if err != nil {
		return false, fmt.Errorf("creating PBM client: %w", err)
	}
	ref := ds.Reference()
	result, err := c.CheckRequirements(ctx, []pbmtypes.PbmPlacementHub{{HubType: ref.Type, HubId: ref.Value}}, nil, []pbmtypes.BasePbmPlacementRequirement{&pbmtypes.PbmPlacementCapabilityProfileRequirement{ProfileId: pbmtypes.PbmProfileId{UniqueId: profileID}}})
	if err != nil {
		return false, fmt.Errorf("checking policy %s on datastore %s: %w", profileID, datastore, err)
	}
	for _, hub := range result.CompatibleDatastores() {
		if hub.HubId == ref.Value && hub.HubType == ref.Type {
			return true, nil
		}
	}
	return false, nil
}
