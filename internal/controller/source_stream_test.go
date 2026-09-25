package controller

import (
	"strings"
	"testing"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRhcosStreamFromMachineSets(t *testing.T) {
	tests := []struct {
		name        string
		machineSets []*machinev1beta1.MachineSet
		want        string
		wantErr     string
	}{
		{name: "legacy MachineSet", machineSets: []*machinev1beta1.MachineSet{{}}, want: ""},
		{name: "MachineSet label", machineSets: []*machinev1beta1.MachineSet{{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{machineSetOSStreamLabelKey: "rhel-10"}}}}, want: "rhel-10"},
		{name: "template label", machineSets: []*machinev1beta1.MachineSet{{Spec: machinev1beta1.MachineSetSpec{Template: machinev1beta1.MachineTemplateSpec{ObjectMeta: machinev1beta1.ObjectMeta{Labels: map[string]string{machineSetOSStreamLabelKey: "rhel-9"}}}}}}, want: "rhel-9"},
		{name: "mixed streams", machineSets: []*machinev1beta1.MachineSet{
			{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{machineSetOSStreamLabelKey: "rhel-9"}}},
			{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{machineSetOSStreamLabelKey: "rhel-10"}}},
		}, wantErr: "multiple RHCOS streams"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rhcosStreamFromMachineSets(tt.machineSets)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("rhcosStreamFromMachineSets: %v", err)
			}
			if got != tt.want {
				t.Errorf("stream = %q, want %q", got, tt.want)
			}
		})
	}
}
