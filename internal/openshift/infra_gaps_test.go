package openshift

import (
	"context"
	"errors"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

// Read methods must propagate a Get error from the missing Infrastructure object.
func TestInfrastructureGetErrors(t *testing.T) {
	m := NewInfrastructureManager(configfake.NewClientset())
	ctx := context.Background()

	if _, err := m.Get(ctx); err == nil {
		t.Error("Get: expected error when infrastructure is missing")
	}
	if _, err := m.GetSourceVCenter(ctx); err == nil {
		t.Error("GetSourceVCenter: expected error when infrastructure is missing")
	}
	if _, err := m.GetSourceFailureDomain(ctx); err == nil {
		t.Error("GetSourceFailureDomain: expected error when infrastructure is missing")
	}
	if _, err := m.GetVCenterByServer(ctx, "any.example.com"); err == nil {
		t.Error("GetVCenterByServer: expected error when infrastructure is missing")
	}
}

func TestAddTargetVCenterUpdateError(t *testing.T) {
	infra := newTestInfrastructure(
		[]configv1.VSpherePlatformVCenterSpec{{Server: "source.example.com", Port: 443, Datacenters: []string{"DC0"}}},
		[]configv1.VSpherePlatformFailureDomainSpec{{Name: "fd-0", Server: "source.example.com"}},
	)
	fds := []configv1.VSpherePlatformFailureDomainSpec{
		{Name: testFailureDomain, Server: "target.example.com", Topology: configv1.VSpherePlatformTopology{Datacenter: "DC1"}},
	}

	t.Run("execution check denies", func(t *testing.T) {
		client := configfake.NewClientset(infra.DeepCopy())
		m := NewInfrastructureManager(client)
		sentinel := errors.New("denied")
		ctx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		if _, err := m.AddTargetVCenter(ctx, infra.DeepCopy(), fds); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("update error", func(t *testing.T) {
		client := configfake.NewClientset(infra.DeepCopy())
		sentinel := errors.New("infra update failed")
		client.PrependReactor("update", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewInfrastructureManager(client)
		if _, err := m.AddTargetVCenter(context.Background(), infra.DeepCopy(), fds); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped update sentinel", err)
		}
	})
}

func TestRemoveSourceVCenterUpdateError(t *testing.T) {
	// Two vCenters so the "refuse to remove the last one" guard is not triggered.
	infra := newTestInfrastructure(
		[]configv1.VSpherePlatformVCenterSpec{
			{Server: "source.example.com", Port: 443, Datacenters: []string{"DC0"}},
			{Server: "target.example.com", Port: 443, Datacenters: []string{"DC1"}},
		},
		[]configv1.VSpherePlatformFailureDomainSpec{
			{Name: "fd-0", Server: "source.example.com"},
			{Name: testFailureDomain, Server: "target.example.com"},
		},
	)

	t.Run("execution check denies", func(t *testing.T) {
		client := configfake.NewClientset(infra.DeepCopy())
		m := NewInfrastructureManager(client)
		sentinel := errors.New("denied")
		ctx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		if _, err := m.RemoveSourceVCenter(ctx, infra.DeepCopy(), "source.example.com"); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("update error", func(t *testing.T) {
		client := configfake.NewClientset(infra.DeepCopy())
		sentinel := errors.New("infra update failed")
		client.PrependReactor("update", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewInfrastructureManager(client)
		if _, err := m.RemoveSourceVCenter(context.Background(), infra.DeepCopy(), "source.example.com"); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped update sentinel", err)
		}
	})
}

func TestGetVCenterByServerNilVSphere(t *testing.T) {
	// Infrastructure exists but has no vSphere platform spec.
	infra := &configv1.Infrastructure{ObjectMeta: metav1.ObjectMeta{Name: InfrastructureName}}
	m := NewInfrastructureManager(configfake.NewClientset(infra))
	if _, err := m.GetVCenterByServer(context.Background(), "any.example.com"); err == nil {
		t.Error("expected error when VSphere platform spec is nil")
	}
}

func TestGetInfrastructureID(t *testing.T) {
	ctx := context.Background()

	t.Run("get error", func(t *testing.T) {
		m := NewInfrastructureManager(configfake.NewClientset())
		if _, err := m.GetInfrastructureID(ctx); err == nil {
			t.Fatal("expected error when infrastructure is missing")
		}
	})

	t.Run("empty infrastructure name", func(t *testing.T) {
		infra := newTestInfrastructure(nil, nil)
		infra.Status.InfrastructureName = ""
		m := NewInfrastructureManager(configfake.NewClientset(infra))
		if _, err := m.GetInfrastructureID(ctx); err == nil {
			t.Fatal("expected error for empty infrastructure name")
		}
	})

	t.Run("returns infrastructure name", func(t *testing.T) {
		infra := newTestInfrastructure(nil, nil)
		infra.Status.InfrastructureName = "my-infra"
		m := NewInfrastructureManager(configfake.NewClientset(infra))
		got, err := m.GetInfrastructureID(ctx)
		if err != nil {
			t.Fatalf("GetInfrastructureID: %v", err)
		}
		if got != "my-infra" {
			t.Errorf("GetInfrastructureID = %q, want %q", got, "my-infra")
		}
	})
}
