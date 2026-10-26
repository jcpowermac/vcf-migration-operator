package vsphere

import (
	"context"
	"crypto/tls"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"

	_ "github.com/vmware/govmomi/vapi/simulator"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

// startSimVC boots a fresh govmomi VPX simulator server and returns its host,
// the default login username, and password. Cleanup is registered on t.
func startSimVC(t *testing.T) (host, user, pass string) {
	t.Helper()

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("creating simulator model: %v", err)
	}
	t.Cleanup(model.Remove)

	model.Service.TLS = new(tls.Config)
	model.Service.RegisterEndpoints = true
	server := model.Service.NewServer()
	t.Cleanup(server.Close)

	user = simulator.DefaultLogin.Username()
	pass, ok := simulator.DefaultLogin.Password()
	if !ok {
		t.Fatal("simulator default login missing password")
	}
	return server.URL.Host, user, pass
}

// startSimVCRejectLogin boots a VPX simulator whose SOAP login always fails
// (via the SessionManager's ValidLogin hook), letting tests hit the
// client.Login error path without relying on the simulator's lenient default.
func startSimVCRejectLogin(t *testing.T) string {
	t.Helper()

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("creating simulator model: %v", err)
	}
	t.Cleanup(model.Remove)
	model.Service.TLS = new(tls.Config)
	model.Service.RegisterEndpoints = true

	// Pre-set a non-default user on the listen URL so the simulator's
	// Authenticate takes the ValidLogin branch (otherwise it always accepts
	// any non-empty credential). ValidLogin returns false to force a login fault.
	model.Service.Listen = &url.URL{User: url.UserPassword("svc", "svcpass")}
	model.Map().SessionManager().ValidLogin = func(*types.Login) bool { return false }

	server := model.Service.NewServer()
	t.Cleanup(server.Close)
	return server.URL.Host
}

func TestListDatacenters(t *testing.T) {
	ctx := context.Background()

	t.Run("missing params", func(t *testing.T) {
		if _, err := ListDatacenters(ctx, Params{}); err == nil || !strings.Contains(err.Error(), "server, username, and password are required") {
			t.Fatalf("error = %v, want missing-params error", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		host, user, pass := startSimVC(t)
		got, err := ListDatacenters(ctx, Params{Server: host, Username: user, Password: pass, Insecure: true})
		if err != nil {
			t.Fatalf("ListDatacenters: %v", err)
		}
		if len(got) == 0 {
			t.Fatal("expected at least one datacenter")
		}
	})

	t.Run("unreachable host", func(t *testing.T) {
		_, err := ListDatacenters(ctx, Params{Server: "127.0.0.1:9", Username: "u", Password: "p", Insecure: true})
		if err == nil || !strings.Contains(err.Error(), "creating vim25 client") {
			t.Fatalf("error = %v, want vim25 client error", err)
		}
	})

	t.Run("login error", func(t *testing.T) {
		host := startSimVCRejectLogin(t)
		_, err := ListDatacenters(ctx, Params{Server: host, Username: "u", Password: "p", Insecure: true})
		if err == nil || !strings.Contains(err.Error(), "logging in") {
			t.Fatalf("error = %v, want login error", err)
		}
	})

	t.Run("datacenter list error", func(t *testing.T) {
		// Remove the root folder from the registry so the finder's traversal
		// faults, exercising the DatacenterList error branch.
		model := simulator.VPX()
		if err := model.Create(); err != nil {
			t.Fatalf("creating simulator model: %v", err)
		}
		t.Cleanup(model.Remove)
		model.Service.TLS = new(tls.Config)
		model.Service.RegisterEndpoints = true
		model.Map().Remove(model.Service.Context, model.ServiceContent.RootFolder)
		server := model.Service.NewServer()
		t.Cleanup(server.Close)
		_, err := ListDatacenters(ctx, Params{Server: server.URL.Host, Username: "u", Password: "p", Insecure: true})
		if err == nil || !strings.Contains(err.Error(), "listing datacenters") {
			t.Fatalf("error = %v, want listing datacenters error", err)
		}
	})
}

func TestCacheKey(t *testing.T) {
	tests := []struct {
		name string
		p    Params
		want string
	}{
		{name: "all fields", p: Params{Server: "h", Datacenter: "dc", Username: "u"}, want: "h#dc#u"},
		{name: "empty fields", p: Params{}, want: "##"},
		{name: "hash in value", p: Params{Server: "a#b", Datacenter: "dc", Username: "u"}, want: "a#b#dc#u"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cacheKey(tt.p); got != tt.want {
				t.Fatalf("cacheKey(%+v) = %q, want %q", tt.p, got, tt.want)
			}
		})
	}
}

func TestNewSessionErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("unreachable host", func(t *testing.T) {
		_, err := NewSession(ctx, Params{Server: "127.0.0.1:9", Datacenter: "DC0", Username: "user", Password: "pass", Insecure: true})
		if err == nil || !strings.Contains(err.Error(), "creating vim25 client") {
			t.Fatalf("error = %v, want vim25 client error", err)
		}
	})

	t.Run("bad credentials", func(t *testing.T) {
		host, _, _ := startSimVC(t)
		// The simulator accepts any non-empty credentials, so empty credentials
		// are what trigger the InvalidLogin fault.
		_, err := NewSession(ctx, Params{Server: host, Datacenter: "DC0", Username: "", Password: "", Insecure: true})
		if err == nil || !strings.Contains(err.Error(), "logging in to") {
			t.Fatalf("error = %v, want login error", err)
		}
	})

	t.Run("missing datacenter", func(t *testing.T) {
		host, user, pass := startSimVC(t)
		_, err := NewSession(ctx, Params{Server: host, Datacenter: missingDatacenter, Username: user, Password: pass, Insecure: true})
		if err == nil || !strings.Contains(err.Error(), "finding datacenter") {
			t.Fatalf("error = %v, want datacenter error", err)
		}
	})

	t.Run("REST login error", func(t *testing.T) {
		// A SOAP-only server (RegisterEndpoints left false) serves no /rest
		// handler, so the SOAP login succeeds but the REST session login 404s,
		// exercising the REST-login failure branch.
		model := simulator.VPX()
		if err := model.Create(); err != nil {
			t.Fatalf("creating simulator model: %v", err)
		}
		t.Cleanup(model.Remove)
		model.Service.TLS = new(tls.Config)
		server := model.Service.NewServer()
		t.Cleanup(server.Close)
		_, err := NewSession(ctx, Params{Server: server.URL.Host, Datacenter: "DC0", Username: "u", Password: "p", Insecure: true})
		if err == nil || !strings.Contains(err.Error(), "REST login to") {
			t.Fatalf("error = %v, want REST login error", err)
		}
	})
}

func TestClose(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		s := newTestSession(ctx, t, c)
		// newTestSession leaves the govmomi Client's SessionManager nil; wire it
		// up so the SOAP logout path in Close is exercised without a nil deref.
		s.Client.SessionManager = session.NewManager(c)

		// Success path: both REST and SOAP clients are logged in.
		s.Close(ctx)
		// Second close: both logouts have already completed, exercising the
		// error-log branches.
		s.Close(ctx)
	})

	t.Run("nil fields skipped", func(t *testing.T) {
		s := &Session{}
		s.Close(context.Background())
	})
}

func TestGetOrCreate(t *testing.T) {
	ctx := context.Background()
	host, user, pass := startSimVC(t)
	t.Cleanup(func() { ClearSessions(ctx) })

	p := Params{Server: host, Datacenter: "DC0", Username: user, Password: pass, Insecure: true}

	t.Run("creates and returns the cached session", func(t *testing.T) {
		ClearSessions(ctx)
		s1, err := GetOrCreate(ctx, p)
		if err != nil {
			t.Fatalf("GetOrCreate: %v", err)
		}
		s2, err := GetOrCreate(ctx, p)
		if err != nil {
			t.Fatalf("GetOrCreate (cached): %v", err)
		}
		if s1 != s2 {
			t.Fatal("expected the same cached session")
		}
	})

	t.Run("creation error is not cached", func(t *testing.T) {
		ClearSessions(ctx)
		// Empty credentials make the simulated SOAP login fail.
		bad := Params{Server: host, Datacenter: "DC0", Username: "", Password: "", Insecure: true}
		if _, err := GetOrCreate(ctx, bad); err == nil {
			t.Fatal("expected creation error")
		}
		// "not cached": a second call must re-attempt (and error again) rather
		// than return a cached failure.
		if _, err := GetOrCreate(ctx, bad); err == nil {
			t.Fatal("expected creation error on retry (failure must not be cached)")
		}
	})

	t.Run("ClearSessions forces a new session", func(t *testing.T) {
		ClearSessions(ctx)
		s1, err := GetOrCreate(ctx, p)
		if err != nil {
			t.Fatalf("GetOrCreate: %v", err)
		}
		ClearSessions(ctx)
		s2, err := GetOrCreate(ctx, p)
		if err != nil {
			t.Fatalf("GetOrCreate after ClearSessions: %v", err)
		}
		if s1 == s2 {
			t.Fatal("expected a new session after ClearSessions")
		}
	})
}

// TestExecutionCheckFolder guards the execution.Check error path for folder ops.
func TestExecutionCheckFolderErrors(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		s := newTestSession(ctx, t, c)

		sentinel := errors.New("exec")
		execCtx := execution.WithCheck(ctx, func(context.Context) error { return sentinel })

		if _, err := CreateVMFolder(execCtx, s, "exec-folder"); !errors.Is(err, sentinel) {
			t.Fatalf("CreateVMFolder: error = %v, want execution sentinel", err)
		}

		folder, err := CreateVMFolder(ctx, s, "exec-delete-folder")
		if err != nil {
			t.Fatalf("CreateVMFolder: %v", err)
		}
		if err := DeleteVMFolder(execCtx, s, folder); !errors.Is(err, sentinel) {
			t.Fatalf("DeleteVMFolder: error = %v, want execution sentinel", err)
		}
	})
}
