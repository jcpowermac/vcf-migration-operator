/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("NodeMigration API validation", func() {
	var serial int

	baseResource := func(name string) *VmwareCloudFoundationMigration {
		return &VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Spec: VmwareCloudFoundationMigrationSpec{
				State: MigrationStatePending,
				TargetVCenterCredentialsSecret: SecretReference{
					Name:      "target-vcenter-creds",
					Namespace: "default",
				},
				FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
					{
						Name:   "target-fd-1",
						Region: "target-region",
						Zone:   "target-zone-1",
						Server: "vcenter-target.example.com",
						Topology: configv1.VSpherePlatformTopology{
							Datacenter:     "TargetDC",
							ComputeCluster: "/TargetDC/host/TargetCluster",
							Datastore:      "/TargetDC/datastore/TargetDS",
							Networks:       []string{"VM Network"},
							ResourcePool:   "/TargetDC/host/TargetCluster/Resources",
							Template:       "/TargetDC/vm/rhcos-template",
							Folder:         "/TargetDC/vm/my-cluster",
						},
					},
				},
			},
		}
	}

	recreateRole := func() *RoleMigrationSpec {
		return &RoleMigrationSpec{Type: NodeMigrationTypeRecreate}
	}

	vmotionRole := func(mode VMotionMode) *RoleMigrationSpec {
		return &RoleMigrationSpec{
			Type:    NodeMigrationTypeVMotion,
			VMotion: &VMotionSpec{Mode: mode},
		}
	}

	uniqueName := func() string {
		serial++
		return fmt.Sprintf("nm-test-%d", serial)
	}

	clientKey := func(obj *VmwareCloudFoundationMigration) client.ObjectKey {
		return client.ObjectKey{Name: obj.Name, Namespace: obj.Namespace}
	}

	AfterEach(func() {
		all := &VmwareCloudFoundationMigrationList{}
		if err := k8sClient.List(ctx, all); err == nil {
			for i := range all.Items {
				_ = k8sClient.Delete(ctx, &all.Items[i])
			}
		}
	})

	// ── nodeMigration omitted ───────────────────────────────────────────

	It("accepts spec without nodeMigration (defaults to MachineSet/CPMS replacement)", func() {
		resource := baseResource(uniqueName())
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	// ── Both roles required ─────────────────────────────────────────────

	It("rejects nodeMigration with only workers", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: recreateRole(),
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.nodeMigration.controlPlane"))
	})

	It("rejects nodeMigration with only controlPlane", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			ControlPlane: recreateRole(),
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.nodeMigration.workers"))
	})

	// ── RoleMigrationSpec type defaults ─────────────────────────────────

	It("defaults type to Recreate when set without explicit type", func() {
		resource := baseResource(uniqueName())
		maxU := intstr.FromInt32(2)
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{
				MaxUnavailable: &maxU,
			},
			ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		created := &VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, clientKey(resource), created)).To(Succeed())
		Expect(created.Spec.NodeMigration.Workers.Type).To(Equal(NodeMigrationTypeRecreate))
	})

	// ── VMotion requires vmotion field ──────────────────────────────────

	It("rejects workers type VMotion without vmotion field", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{
				Type: NodeMigrationTypeVMotion,
			},
			ControlPlane: recreateRole(),
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("vmotion is required when type is VMotion"))
	})

	It("rejects controlPlane type VMotion without vmotion field", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: recreateRole(),
			ControlPlane: &RoleMigrationSpec{
				Type: NodeMigrationTypeVMotion,
			},
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("vmotion is required when type is VMotion"))
	})

	It("accepts type VMotion with vmotion field", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      vmotionRole(VMotionModeAuto),
			ControlPlane: vmotionRole(VMotionModeAuto),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	// ── Recreate forbids vmotion field ──────────────────────────────────

	It("rejects workers type Recreate with vmotion field", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{
				Type:    NodeMigrationTypeRecreate,
				VMotion: &VMotionSpec{Mode: VMotionModeCold},
			},
			ControlPlane: recreateRole(),
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("vmotion is only valid when type is VMotion"))
	})

	It("rejects controlPlane type Recreate with vmotion field", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: recreateRole(),
			ControlPlane: &RoleMigrationSpec{
				Type:    NodeMigrationTypeRecreate,
				VMotion: &VMotionSpec{Mode: VMotionModeHot},
			},
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("vmotion is only valid when type is VMotion"))
	})

	It("accepts type Recreate without vmotion field", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      recreateRole(),
			ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	// ── VMotion modes ───────────────────────────────────────────────────

	DescribeTable("accepts valid VMotion modes",
		func(mode VMotionMode) {
			resource := baseResource(uniqueName())
			resource.Spec.NodeMigration = &NodeMigrationSpec{
				Workers:      vmotionRole(mode),
				ControlPlane: recreateRole(),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		},
		Entry("Auto", VMotionModeAuto),
		Entry("Hot", VMotionModeHot),
		Entry("Cold", VMotionModeCold),
	)

	// ── Per-role independence ───────────────────────────────────────────

	It("accepts workers VMotion and controlPlane Recreate", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      vmotionRole(VMotionModeCold),
			ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	It("accepts workers Recreate and controlPlane VMotion", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      recreateRole(),
			ControlPlane: vmotionRole(VMotionModeHot),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	It("accepts both roles as VMotion with different modes", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      vmotionRole(VMotionModeCold),
			ControlPlane: vmotionRole(VMotionModeHot),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	// ── maxUnavailable validation ───────────────────────────────────────

	It("accepts maxUnavailable as integer", func() {
		resource := baseResource(uniqueName())
		maxU := intstr.FromInt32(3)
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{
				Type:           NodeMigrationTypeRecreate,
				MaxUnavailable: &maxU,
			},
			ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	It("accepts maxUnavailable as percentage", func() {
		resource := baseResource(uniqueName())
		maxU := intstr.FromString("25%")
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{
				Type:           NodeMigrationTypeRecreate,
				MaxUnavailable: &maxU,
			},
			ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	It("rejects maxUnavailable of 0", func() {
		resource := baseResource(uniqueName())
		maxU := intstr.FromInt32(0)
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{
				Type:           NodeMigrationTypeRecreate,
				MaxUnavailable: &maxU,
			},
			ControlPlane: recreateRole(),
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("maxUnavailable must be a positive integer"))
	})

	It("rejects maxUnavailable with invalid percentage", func() {
		resource := baseResource(uniqueName())
		maxU := intstr.FromString("101%")
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{
				Type:           NodeMigrationTypeRecreate,
				MaxUnavailable: &maxU,
			},
			ControlPlane: recreateRole(),
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("maxUnavailable must be a positive integer"))
	})

	// ── maxUnavailable default ──────────────────────────────────────────

	It("defaults maxUnavailable to 1 when omitted", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      recreateRole(),
			ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		created := &VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, clientKey(resource), created)).To(Succeed())
		Expect(created.Spec.NodeMigration.Workers.MaxUnavailable).NotTo(BeNil())
		Expect(created.Spec.NodeMigration.Workers.MaxUnavailable.IntValue()).To(Equal(1))
		Expect(created.Spec.NodeMigration.ControlPlane.MaxUnavailable).NotTo(BeNil())
		Expect(created.Spec.NodeMigration.ControlPlane.MaxUnavailable.IntValue()).To(Equal(1))
	})

	// ── VMotion mode default ────────────────────────────────────────────

	It("defaults vmotion.mode to Auto when omitted", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{
				Type:    NodeMigrationTypeVMotion,
				VMotion: &VMotionSpec{},
			},
			ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		created := &VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, clientKey(resource), created)).To(Succeed())
		Expect(created.Spec.NodeMigration.Workers.VMotion.Mode).To(Equal(VMotionModeAuto))
	})

	// ── UPI typical config (both roles VMotion) ─────────────────────────

	It("accepts a full UPI configuration", func() {
		resource := baseResource(uniqueName())
		maxWorkers := intstr.FromString("25%")
		maxCP := intstr.FromInt32(1)
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{
				Type:           NodeMigrationTypeVMotion,
				VMotion:        &VMotionSpec{Mode: VMotionModeCold},
				MaxUnavailable: &maxWorkers,
			},
			ControlPlane: &RoleMigrationSpec{
				Type:           NodeMigrationTypeVMotion,
				VMotion:        &VMotionSpec{Mode: VMotionModeHot},
				MaxUnavailable: &maxCP,
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	// ── IPI typical config (both roles Recreate) ────────────────────────

	It("accepts a full IPI configuration", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      recreateRole(),
			ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})
})
