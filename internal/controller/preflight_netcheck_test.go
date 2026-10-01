package controller

import (
	"strings"
	"testing"

	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

func TestProbeName(t *testing.T) {
	tests := []struct {
		infraID string
		fdName  string
		want    string
	}{
		{infraID: "abc123", fdName: "fd-a", want: "netcheck-abc123-fd-a"},
		{infraID: "abc123", fdName: "FD-A", want: "netcheck-abc123-fd-a"},
		{infraID: "abc123", fdName: "fd/a.b c", want: "netcheck-abc123-fd-a-b-c"},
	}
	for _, tt := range tests {
		t.Run(tt.infraID+"-"+tt.fdName, func(t *testing.T) {
			if got := probeName(tt.infraID, tt.fdName); got != tt.want {
				t.Errorf("probeName(%q, %q) = %q, want %q", tt.infraID, tt.fdName, got, tt.want)
			}
		})
	}

	long := probeName("abc123", strings.Repeat("x", 100))
	if len(long) > probeNameMaxLen {
		t.Errorf("probeName length = %d, want <= %d", len(long), probeNameMaxLen)
	}
}

func TestDescribeFDMismatch(t *testing.T) {
	probe := []vsphere.NetworkInfo{{IP: "10.0.1.5", Prefix: 24, Gateway: "10.0.1.1"}}
	sources := []vsphere.NetworkInfo{
		{IP: "192.168.5.10", Prefix: 24, Gateway: "192.168.5.1"},
		{IP: "192.168.5.11", Prefix: 24, Gateway: "192.168.5.1"},
	}
	got := describeFDMismatch("fd-a", probe, sources)
	want := `failure domain "fd-a": probe network 10.0.1.0/24 (gateway 10.0.1.1) does not match any source node network (source networks: 192.168.5.0/24 (gateway 192.168.5.1)); IP connectivity would not be preserved when recreating source machines on the destination vCenter`
	if got != want {
		t.Errorf("describeFDMismatch:\n got: %s\nwant: %s", got, want)
	}
}
