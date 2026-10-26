package vsphere

import (
	"context"
	"testing"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/pbm"
	pbmmethods "github.com/vmware/govmomi/pbm/methods"
	pbmsimulator "github.com/vmware/govmomi/pbm/simulator"
	pbmtypes "github.com/vmware/govmomi/pbm/types"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	vimtypes "github.com/vmware/govmomi/vim25/types"
)

func TestStoragePolicyPlanning(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		s := newTestSession(ctx, t, c)
		id, err := GetProfileIDByName(ctx, s, "vSAN Default Storage Policy")
		if err != nil || id == "" {
			t.Fatalf("GetProfileIDByName: %q %v", id, err)
		}
		missing, err := GetProfileIDByName(ctx, s, "missing-policy")
		if err != nil || missing != "" {
			t.Fatalf("GetProfileIDByName missing: %q %v", missing, err)
		}
		datastores, err := s.Finder.DatastoreList(ctx, "*")
		if err != nil || len(datastores) == 0 {
			t.Fatalf("DatastoreList: %v", err)
		}
		compatible, err := IsDatastoreCompatible(ctx, s, id, datastores[0].InventoryPath)
		if err != nil || !compatible {
			t.Fatalf("IsDatastoreCompatible: %v %v", compatible, err)
		}
		if _, err := IsDatastoreCompatible(ctx, s, id, "missing-datastore"); err == nil {
			t.Fatal("IsDatastoreCompatible: missing datastore accepted")
		}
	})
}

// incompatiblePlacementSolver exercises the PBM response that the stock simulator
// does not produce: a policy incompatible with the requested datastore.
type incompatiblePlacementSolver struct {
	vimtypes.ManagedObjectReference
	requests chan *pbmtypes.PbmCheckRequirements
}

func (s *incompatiblePlacementSolver) PbmCheckRequirements(_ *simulator.Context, req *pbmtypes.PbmCheckRequirements) soap.HasFault {
	s.requests <- req
	return &pbmmethods.PbmCheckRequirementsBody{Res: &pbmtypes.PbmCheckRequirementsResponse{Returnval: []pbmtypes.PbmPlacementCompatibilityResult{{Hub: req.HubsToSearch[0], Error: []vimtypes.LocalizedMethodFault{{LocalizedMessage: "policy incompatible"}}}}}}
}

func TestIncompatibleStoragePolicy(t *testing.T) {
	ctx := context.Background()
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	defer model.Remove()
	model.Service.RegisterEndpoints = true
	server := model.Service.NewServer()
	defer server.Close()
	registry := pbmsimulator.New()
	model.Service.RegisterSDK(registry)
	c, err := govmomi.NewClient(ctx, server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Logout(ctx); err != nil {
			t.Error(err)
		}
	}()
	pbmClient, err := pbm.NewClient(ctx, c.Client)
	if err != nil {
		t.Fatal(err)
	}
	solver := &incompatiblePlacementSolver{ManagedObjectReference: pbmClient.ServiceContent.PlacementSolver, requests: make(chan *pbmtypes.PbmCheckRequirements, 1)}
	registry.Put(solver)
	model.Service.RegisterSDK(registry)
	s := newTestSession(ctx, t, c.Client)
	datastore, err := s.Finder.DefaultDatastore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	compatible, err := IsDatastoreCompatible(ctx, s, "profile-id", datastore.InventoryPath)
	if err != nil || compatible {
		t.Fatalf("IsDatastoreCompatible: %v %v", compatible, err)
	}
	request := <-solver.requests
	if len(request.HubsToSearch) != 1 || request.HubsToSearch[0].HubId != datastore.Reference().Value {
		t.Fatalf("CheckRequirements target: %+v", request)
	}
	requirements := request.PlacementSubjectRequirement
	if len(requirements) != 1 || requirements[0].(*pbmtypes.PbmPlacementCapabilityProfileRequirement).ProfileId.UniqueId != "profile-id" {
		t.Fatalf("CheckRequirements policy: %+v", requirements)
	}
}
