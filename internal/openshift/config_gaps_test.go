package openshift

import (
	"context"
	"errors"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

// version.go: empty desired version and feature-gate list error.
func TestGetVSphereMultiVCenterSupportEmptyVersionAndGateErrors(t *testing.T) {
	t.Run("empty desired version", func(t *testing.T) {
		client := configfake.NewClientset(&configv1.ClusterVersion{
			ObjectMeta: metav1.ObjectMeta{Name: clusterVersionName},
		})
		_, err := GetVSphereMultiVCenterSupport(context.Background(), client)
		if err == nil {
			t.Fatal("expected error for empty desired version")
		}
		if !strings.Contains(err.Error(), "empty status.desired.version") {
			t.Errorf("error = %q, want empty-version message", err.Error())
		}
	})

	t.Run("feature gate get error", func(t *testing.T) {
		client := configfake.NewClientset(&configv1.ClusterVersion{
			ObjectMeta: metav1.ObjectMeta{Name: clusterVersionName},
			Status:     configv1.ClusterVersionStatus{Desired: configv1.Release{Version: "5.0.0"}},
		})
		sentinel := errors.New("feature gate get failed")
		client.PrependReactor("get", "featuregates", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		_, err := GetVSphereMultiVCenterSupport(context.Background(), client)
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped feature gate sentinel", err)
		}
	})

	t.Run("feature gate status missing for version", func(t *testing.T) {
		client := configfake.NewClientset(
			&configv1.ClusterVersion{
				ObjectMeta: metav1.ObjectMeta{Name: clusterVersionName},
				Status:     configv1.ClusterVersionStatus{Desired: configv1.Release{Version: "5.0.0"}},
			},
			// FeatureGate whose status only covers a different version.
			&configv1.FeatureGate{
				ObjectMeta: metav1.ObjectMeta{Name: featureGateName},
				Status: configv1.FeatureGateStatus{
					FeatureGates: []configv1.FeatureGateDetails{{Version: "4.19.0"}},
				},
			},
		)
		_, err := GetVSphereMultiVCenterSupport(context.Background(), client)
		if err == nil {
			t.Fatal("expected error when feature gate has no status for the version")
		}
		if !strings.Contains(err.Error(), "checking feature gate") {
			t.Errorf("error = %q, want feature-gate-check message", err.Error())
		}
	})
}

// secrets.go: execution-check and update error branches.
func TestAddTargetVCenterCredsErrors(t *testing.T) {
	mkSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: VSphereCredsSecretNamespace, Name: VSphereCredsSecretName},
			Data:       map[string][]byte{},
		}
	}

	t.Run("execution check denies", func(t *testing.T) {
		client := fakekube.NewClientset(mkSecret())
		m := NewSecretManager(client)
		sentinel := errors.New("denied")
		ctx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		_, err := m.AddTargetVCenterCreds(ctx, mkSecret(), "target.example.com", "user", "pass")
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("update error", func(t *testing.T) {
		client := fakekube.NewClientset(mkSecret())
		sentinel := errors.New("secret update failed")
		client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewSecretManager(client)
		_, err := m.AddTargetVCenterCreds(context.Background(), mkSecret(), "target.example.com", "user", "pass")
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped update sentinel", err)
		}
	})
}

func TestRemoveSourceVCenterCredsErrors(t *testing.T) {
	mkSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: VSphereCredsSecretNamespace, Name: VSphereCredsSecretName},
			Data:       map[string][]byte{"source.example.com.username": []byte("u")},
		}
	}

	t.Run("nil secret", func(t *testing.T) {
		m := NewSecretManager(fakekube.NewClientset())
		if _, err := m.RemoveSourceVCenterCreds(context.Background(), nil, "source.example.com"); err == nil {
			t.Fatal("expected error for nil secret")
		}
	})

	t.Run("execution check denies", func(t *testing.T) {
		client := fakekube.NewClientset(mkSecret())
		m := NewSecretManager(client)
		sentinel := errors.New("denied")
		ctx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		_, err := m.RemoveSourceVCenterCreds(ctx, mkSecret(), "source.example.com")
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("update error", func(t *testing.T) {
		client := fakekube.NewClientset(mkSecret())
		sentinel := errors.New("secret update failed")
		client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewSecretManager(client)
		_, err := m.RemoveSourceVCenterCreds(context.Background(), mkSecret(), "source.example.com")
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped update sentinel", err)
		}
	})
}

// configmaps.go: the marshalAndUpdateConfig error return inside add/remove.
func TestCloudProviderConfigUpdateErrors(t *testing.T) {
	cm := newCloudProviderConfigMap(map[string][]string{
		"source.example.com": {"DC0"},
	})

	t.Run("add target vcenter update error", func(t *testing.T) {
		client := fakekube.NewClientset(cm.DeepCopy())
		sentinel := errors.New("configmap update failed")
		client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewConfigMapManager(client)
		_, err := m.AddTargetVCenterToConfig(context.Background(), cm.DeepCopy(), []configv1.VSpherePlatformFailureDomainSpec{
			{Server: "target.example.com", Topology: configv1.VSpherePlatformTopology{Datacenter: "DC1"}},
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped update sentinel", err)
		}
	})

	t.Run("remove source vcenter update error", func(t *testing.T) {
		client := fakekube.NewClientset(cm.DeepCopy())
		sentinel := errors.New("configmap update failed")
		client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewConfigMapManager(client)
		_, err := m.RemoveSourceVCenterFromConfig(context.Background(), cm.DeepCopy(), "source.example.com")
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped update sentinel", err)
		}
	})
}

func TestParseCloudProviderConfigNilVCenter(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: CloudProviderConfigMapName},
		Data: map[string]string{
			cloudProviderConfigKey: "global: {}\nvcenter: null",
		},
	}
	cfg, err := parseCloudProviderConfig(cm)
	if err != nil {
		t.Fatalf("parseCloudProviderConfig: %v", err)
	}
	if cfg.Vcenter == nil {
		t.Error("expected Vcenter map to be re-initialized from an explicit null")
	}
}

// TestMarshalAndUpdateConfigErrors covers the execution-check, update, and
// nil-Data branches. The yaml.Marshal error return (configmaps.go:176-177) is
// left uncovered: all three cloudProviderConfigYAML fields marshal to plain
// YAML, so a marshal failure is not reachable with these types.
func TestMarshalAndUpdateConfigErrors(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: CloudProviderConfigMapNamespace, Name: CloudProviderConfigMapName}, Data: map[string]string{}}
	cfg := &cloudProviderConfigYAML{}

	t.Run("execution check denies", func(t *testing.T) {
		client := fakekube.NewClientset()
		sentinel := errors.New("denied")
		ctx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		if _, err := marshalAndUpdateConfig(ctx, client, cm.DeepCopy(), cfg); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("update error", func(t *testing.T) {
		client := fakekube.NewClientset(cm.DeepCopy())
		sentinel := errors.New("configmap update failed")
		client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		if _, err := marshalAndUpdateConfig(context.Background(), client, cm.DeepCopy(), cfg); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped update sentinel", err)
		}
	})

	t.Run("nil data map is initialized", func(t *testing.T) {
		client := fakekube.NewClientset(cm.DeepCopy())
		empty := cm.DeepCopy()
		empty.Data = nil
		if _, err := marshalAndUpdateConfig(context.Background(), client, empty, cfg); err != nil {
			t.Fatalf("marshalAndUpdateConfig: %v", err)
		}
		if empty.Data == nil || empty.Data[cloudProviderConfigKey] == "" {
			t.Error("expected config key to be written into the previously-nil Data map")
		}
	})
}

// operators.go: list/get error branches.
func TestOperatorManagerErrors(t *testing.T) {
	t.Run("check all operators healthy list error", func(t *testing.T) {
		client := configfake.NewClientset()
		sentinel := errors.New("list failed")
		client.PrependReactor("list", "clusteroperators", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewOperatorManager(client)
		_, _, err := m.CheckAllOperatorsHealthy(context.Background())
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped list sentinel", err)
		}
	})

	t.Run("check all operators stable list error", func(t *testing.T) {
		client := configfake.NewClientset()
		sentinel := errors.New("list failed")
		client.PrependReactor("list", "clusteroperators", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewOperatorManager(client)
		_, _, err := m.CheckAllOperatorsStable(context.Background())
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped list sentinel", err)
		}
	})

	t.Run("get operator not found", func(t *testing.T) {
		m := NewOperatorManager(configfake.NewClientset())
		if _, err := m.GetOperator(context.Background(), "missing-op"); err == nil {
			t.Fatal("expected error for missing operator")
		}
	})

	t.Run("is operator healthy get error", func(t *testing.T) {
		m := NewOperatorManager(configfake.NewClientset())
		if _, _, err := m.IsOperatorHealthy(context.Background(), "missing-op"); err == nil {
			t.Fatal("expected error propagating from GetOperator")
		}
	})

	t.Run("check all operators stable skips excluded operator", func(t *testing.T) {
		// machine-config is excluded even though it is unstable; the healthy
		// operator must be the only one that counts.
		client := configfake.NewClientset(
			newTestClusterOperator("config-operator", true, false),
			newTestClusterOperator("machine-config", false, true),
		)
		m := NewOperatorManager(client)
		stable, summary, err := m.CheckAllOperatorsStable(context.Background())
		if err != nil {
			t.Fatalf("CheckAllOperatorsStable: %v", err)
		}
		if !stable {
			t.Errorf("expected stable (excluded operator skipped), got %+v", summary)
		}
	})
}
