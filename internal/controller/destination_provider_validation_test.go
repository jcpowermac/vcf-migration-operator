package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

func TestDestinationProviderValidationWithoutSources(t *testing.T) {
	for _, sourceFDPresent := range []bool{true, false} {
		for _, layout := range []string{"base", "suffix", "legacy"} {
			for _, provider := range []struct {
				name    string
				raw     string
				wantErr string
			}{
				{name: "wrong server", raw: `{"workspace":{"server":"other.example.com"}}`, wantErr: "collides"},
				{name: "missing workspace", raw: `{}`, wantErr: "collides"},
				{name: "malformed provider", raw: `{`, wantErr: "extracting provider spec"},
				{name: "valid", raw: `{"workspace":{"server":"target.example.com"}}`},
			} {
				for _, gate := range []string{"existence", "recovery"} {
					t.Run(fmt.Sprintf("sourceFD=%t/%s/%s/%s", sourceFDPresent, layout, provider.name, gate), func(t *testing.T) {
						ctx := context.Background()
						migration := newRolloutMigration()
						base := migration.Status.WorkerMachineSetNames[0]
						name := base
						objects := []runtime.Object{}
						if layout == "suffix" {
							name += "-2"
							migration.Status.WorkerMachineSetNames = append(migration.Status.WorkerMachineSetNames, name)
							objects = append(objects, newTargetMachineSetForRollout(base, "target.example.com", 1))
						}
						dest := newTargetMachineSetForRollout(name, "target.example.com", 1)
						dest.Spec.Template.Spec.ProviderSpec.Value.Raw = []byte(provider.raw)
						if layout == "legacy" {
							dest.Annotations = map[string]string{openshift.LegacyAggregateAnnotation: "true"}
						}
						objects = append(objects, dest)
						r := newRolloutReconciler(t, objects)
						mgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, nil)
						var sourceFDs []configv1.VSpherePlatformFailureDomainSpec
						if sourceFDPresent {
							sourceFDs = newInfrastructureForRollout().Spec.PlatformSpec.VSphere.FailureDomains
						}
						before := slices.Clone(migration.Status.WorkerMachineSetNames)
						var err error
						if gate == "existence" {
							var exists bool
							exists, err = r.targetMachineSetsExist(ctx, migration, mgr, "test-infra", migration.Status.FailureDomainMappings, sourceFDs)
							if provider.wantErr == "" && !exists {
								t.Error("valid replacement not accepted")
							}
						} else {
							_, err = recoverUnrecordedDestinationNames(ctx, mgr, migration, nil, "test-infra", migration.Status.FailureDomainMappings, sourceFDs)
						}
						if provider.wantErr == "" {
							if err != nil {
								t.Fatal(err)
							}
						} else if err == nil || !strings.Contains(err.Error(), provider.wantErr) {
							t.Errorf("%s error = %v, want %q", gate, err, provider.wantErr)
						}
						if !slices.Equal(before, migration.Status.WorkerMachineSetNames) {
							t.Error("recorded inventory changed")
						}
					})
				}
			}
		}
	}
}

func TestEnsureWorkloadMigratedRejectsWrongServerAfterSourceDeletion(t *testing.T) {
	for _, wrongName := range []string{"base", "suffix"} {
		t.Run(wrongName, func(t *testing.T) {
			ctx := context.Background()
			migration := newRolloutMigration()
			migration.Spec.FailureDomains[0] = newTargetFailureDomain("target-fd-1")
			base := migration.Status.WorkerMachineSetNames[0]
			suffix := base + "-2"
			migration.Status.WorkerMachineSetNames = append(migration.Status.WorkerMachineSetNames, suffix)
			baseMS := newTargetMachineSetForRollout(base, "target.example.com", 1)
			suffixMS := newTargetMachineSetForRollout(suffix, "target.example.com", 1)
			if wrongName == "base" {
				baseMS = newTargetMachineSetForRollout(base, "other.example.com", 1)
			} else {
				suffixMS = newTargetMachineSetForRollout(suffix, "other.example.com", 1)
			}
			r := newRolloutReconciler(t, []runtime.Object{
				newInfrastructureForRollout(), newCPMSUpdatedForRollout([]string{"target-fd-1"}, true),
				baseMS, suffixMS, newReadyWorkerMachineForRollout(base), newReadyWorkerMachineForRollout(suffix),
				newReadyNodeForRollout(rolloutReadyNodeName),
			})
			r.Client, r.Scheme = newDestinationRecoveryClient(t, migration)
			_, err := r.ensureWorkloadMigrated(ctx, migration)
			if err == nil || !strings.Contains(err.Error(), "collides") {
				t.Errorf("ensureWorkloadMigrated error = %v, want collision", err)
			}
			if apimeta.IsStatusConditionTrue(migration.Status.Conditions, migrationv1alpha1.ConditionWorkloadMigrated) {
				t.Error("migration completed with wrong-server replacement")
			}
			infra, err := r.ConfigClient.ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if findFailureDomainByName(infra.Spec.PlatformSpec.VSphere.FailureDomains, "source-fd-1") == nil {
				t.Error("source failure domain cleaned up")
			}
			for _, action := range r.MachineClient.(*fakemachineclient.Clientset).Actions() {
				if action.GetVerb() == "delete" {
					t.Errorf("unexpected cleanup: %v", action)
				}
			}
			for _, action := range r.ConfigClient.(*configfake.Clientset).Actions() {
				if action.GetVerb() == "update" || action.GetVerb() == "patch" {
					t.Errorf("unexpected infrastructure mutation: %v", action)
				}
			}
		})
	}
}
