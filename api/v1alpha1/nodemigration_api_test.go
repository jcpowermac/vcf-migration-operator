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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("NodeMigration API validation", func() {
	var serial int

	baseResource := func(name string) *VmwareCloudFoundationMigration {
		return &VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: VmwareCloudFoundationMigrationSpec{
				State:                          MigrationStatePending,
				TargetVCenterCredentialsSecret: SecretReference{Name: "target-vcenter-creds", Namespace: "default"},
				FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
					Name: "target-fd-1", Region: "target-region", Zone: "target-zone-1", Server: "vcenter-target.example.com",
					Topology: configv1.VSpherePlatformTopology{
						Datacenter: "TargetDC", ComputeCluster: "/TargetDC/host/TargetCluster",
						Datastore: "/TargetDC/datastore/TargetDS", Networks: []string{"VM Network"},
						ResourcePool: "/TargetDC/host/TargetCluster/Resources", Template: "/TargetDC/vm/rhcos-template",
						Folder: "/TargetDC/vm/my-cluster",
					},
				}},
			},
		}
	}

	recreateRole := func() *RoleMigrationSpec {
		return &RoleMigrationSpec{Type: NodeMigrationTypeRecreate}
	}

	vmotionRole := func(mode VMotionMode) *RoleMigrationSpec {
		return &RoleMigrationSpec{Type: NodeMigrationTypeVMotion, VMotion: &VMotionSpec{Mode: mode}}
	}

	vmotionConfig := func(mode VMotionMode) *NodeMigrationSpec {
		return &NodeMigrationSpec{Workers: vmotionRole(mode), ControlPlane: vmotionRole(mode)}
	}

	withoutTemplate := func(resource *VmwareCloudFoundationMigration) {
		resource.Spec.FailureDomains[0].Topology.Template = ""
	}

	addFailureDomain := func(resource *VmwareCloudFoundationMigration) {
		fd := resource.Spec.FailureDomains[0]
		fd.Name = "target-fd-2"
		fd.Zone = "target-zone-2"
		resource.Spec.FailureDomains = append(resource.Spec.FailureDomains, fd)
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

	It("preserves legacy omission and accepts multiple templated failure domains", func() {
		resource := baseResource(uniqueName())
		addFailureDomain(resource)
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	It("requires a template for legacy and Recreate configurations", func() {
		resource := baseResource(uniqueName())
		withoutTemplate(resource)
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())

		resource = baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{Workers: recreateRole(), ControlPlane: recreateRole()}
		withoutTemplate(resource)
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())
	})

	It("requires both roles when nodeMigration is set", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{Workers: recreateRole()}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.nodeMigration.controlPlane"))

		resource = baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{ControlPlane: recreateRole()}
		err = k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.nodeMigration.workers"))
	})

	It("defaults omitted role type and maxUnavailable for the legacy Recreate engine", func() {
		maxWorkers := int32(3)
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      &RoleMigrationSpec{MaxUnavailable: &maxWorkers},
			ControlPlane: &RoleMigrationSpec{},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		created := &VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, clientKey(resource), created)).To(Succeed())
		Expect(created.Spec.NodeMigration.Workers.Type).To(Equal(NodeMigrationTypeRecreate))
		Expect(created.Spec.NodeMigration.ControlPlane.Type).To(Equal(NodeMigrationTypeRecreate))
		Expect(*created.Spec.NodeMigration.Workers.MaxUnavailable).To(Equal(int32(3)))
		Expect(*created.Spec.NodeMigration.ControlPlane.MaxUnavailable).To(Equal(int32(1)))
	})

	It("accepts both-role Recreate with its template requirement", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{Workers: recreateRole(), ControlPlane: recreateRole()}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	It("rejects a vmotion block on a Recreate role", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      &RoleMigrationSpec{Type: NodeMigrationTypeRecreate, VMotion: &VMotionSpec{Mode: VMotionModeCold}},
			ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())
	})

	It("requires vmotion settings and explicit mode for each VMotion role", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      &RoleMigrationSpec{Type: NodeMigrationTypeVMotion},
			ControlPlane: vmotionRole(VMotionModeHot),
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("vmotion is required when type is VMotion"))

		resource = baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers:      &RoleMigrationSpec{Type: NodeMigrationTypeVMotion, VMotion: &VMotionSpec{}},
			ControlPlane: &RoleMigrationSpec{Type: NodeMigrationTypeVMotion, VMotion: &VMotionSpec{}},
		}
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())
	})

	DescribeTable("accepts same-mode VMotion without a template",
		func(mode VMotionMode) {
			resource := baseResource(uniqueName())
			resource.Spec.NodeMigration = vmotionConfig(mode)
			withoutTemplate(resource)
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		},
		Entry("Hot", VMotionModeHot),
		Entry("Cold", VMotionModeCold),
	)

	It("rejects Auto rather than defaulting or falling back", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = vmotionConfig(VMotionMode("Auto"))
		object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(resource)
		Expect(err).NotTo(HaveOccurred())
		unstructuredResource := &unstructured.Unstructured{Object: object}
		unstructuredResource.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "migration.openshift.io", Version: "v1alpha1", Kind: "VmwareCloudFoundationMigration",
		})
		Expect(k8sClient.Create(ctx, unstructuredResource)).To(HaveOccurred())
	})

	It("rejects malformed recovery request identifiers", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = vmotionConfig(VMotionModeCold)
		resource.Spec.NodeMigration.RecoveryRequest = &NodeMigrationRecoveryRequest{
			RequestID: "not-a-uuid", ExecutionID: "22222222-2222-4222-8222-222222222222",
			ActionID: "33333333-3333-4333-8333-333333333333",
		}
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())
	})

	It("rejects mixed VMotion and Recreate role selections", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{Workers: vmotionRole(VMotionModeCold), ControlPlane: recreateRole()}
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())

		resource = baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{Workers: recreateRole(), ControlPlane: vmotionRole(VMotionModeHot)}
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())
	})

	It("rejects different VMotion modes across roles", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{Workers: vmotionRole(VMotionModeCold), ControlPlane: vmotionRole(VMotionModeHot)}
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())
	})

	It("rejects multiple failure domains and worker concurrency for VMotion", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = vmotionConfig(VMotionModeHot)
		addFailureDomain(resource)
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())

		maxWorkers := int32(2)
		resource = baseResource(uniqueName())
		resource.Spec.NodeMigration = vmotionConfig(VMotionModeCold)
		resource.Spec.NodeMigration.Workers.MaxUnavailable = &maxWorkers
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())
	})

	It("rejects percentage and zero maxUnavailable while preserving the integer schema", func() {
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{Workers: recreateRole(), ControlPlane: recreateRole()}
		object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(resource)
		Expect(err).NotTo(HaveOccurred())
		unstructuredResource := &unstructured.Unstructured{Object: object}
		unstructuredResource.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "migration.openshift.io", Version: "v1alpha1", Kind: "VmwareCloudFoundationMigration",
		})
		Expect(unstructured.SetNestedField(unstructuredResource.Object, "25%", "spec", "nodeMigration", "workers", "maxUnavailable")).To(Succeed())
		Expect(k8sClient.Create(ctx, unstructuredResource)).To(HaveOccurred())

		zero := int32(0)
		resource = baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: &RoleMigrationSpec{Type: NodeMigrationTypeRecreate, MaxUnavailable: &zero}, ControlPlane: recreateRole(),
		}
		Expect(k8sClient.Create(ctx, resource)).To(HaveOccurred())
	})

	It("continues to enforce control-plane maxUnavailable of one", func() {
		two := int32(2)
		resource := baseResource(uniqueName())
		resource.Spec.NodeMigration = &NodeMigrationSpec{
			Workers: recreateRole(), ControlPlane: &RoleMigrationSpec{Type: NodeMigrationTypeRecreate, MaxUnavailable: &two},
		}
		err := k8sClient.Create(ctx, resource)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("controlPlane.maxUnavailable must be 1"))
	})

	It("validates bounded action status without binding it to current generation or recovery request", func() {
		resource := baseResource(uniqueName())
		resource.Spec.State = MigrationStatePaused
		resource.Spec.NodeMigration = vmotionConfig(VMotionModeCold)
		resource.Spec.NodeMigration.RecoveryRequest = &NodeMigrationRecoveryRequest{
			RequestID:   "11111111-1111-4111-8111-111111111111",
			ExecutionID: "22222222-2222-4222-8222-222222222222",
			ActionID:    "33333333-3333-4333-8333-333333333333",
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		created := &VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, clientKey(resource), created)).To(Succeed())
		migrationUID := created.UID
		now := metav1.Now()
		wasUnschedulable := false
		created.Status.NodeMigration = &NodeMigrationStatus{
			Type: NodeMigrationTypeVMotion, Progress: "workers 0/1, control plane 0/3",
			ExecutionID: "22222222-2222-4222-8222-222222222222", MigrationUID: migrationUID,
			PlanFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			FrozenPlan: &NodeMigrationFrozenPlan{
				SourceVCenter: "vc-source.example.com", SourceDatacenter: "SourceDC",
				DestinationVCenter: "vc-target.example.com", Mode: VMotionModeCold, TargetFailureDomain: "target-fd-1",
			},
			ActiveAction: &NodeMigrationAction{
				ActionID:    "44444444-4444-4444-8444-444444444444",
				ExecutionID: "22222222-2222-4222-8222-222222222222",
				Kind:        NodeMigrationActionRelocateVM, NodeName: "worker-1", NodeUID: types.UID("node-uid-1"),
				VMBIOSUUID: "420e5e2e-c9eb-4a76-9623-725885785001", OwnerVCenter: "vc-source.example.com",
				OperationIdentity: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				SubmissionState:   NodeMigrationSubmissionClaimed, ObservationState: NodeMigrationObservationNotObserved,
				ClaimedAt: now,
			},
			Intervention: &NodeMigrationIntervention{
				ExecutionID: "22222222-2222-4222-8222-222222222222", ActionID: "33333333-3333-4333-8333-333333333333",
				NodeName: "worker-1", Reason: "action outcome requires recheck", Since: now,
			},
			RecoveryAcknowledgement: &NodeMigrationRecoveryAcknowledgement{
				RequestID:   "11111111-1111-4111-8111-111111111111",
				ExecutionID: "22222222-2222-4222-8222-222222222222",
				ActionID:    "33333333-3333-4333-8333-333333333333", ConsumedAt: now,
			},
			Cleanup: &NodeMigrationCleanupProgress{Phase: NodeMigrationCleanupPending},
			Workers: &RoleMigrationStatus{RequestedMode: VMotionModeCold, Nodes: []NodeMigrationProgress{{
				Name: "worker-1", Phase: NodeMigrationPhaseMigrating, RequestedMode: VMotionModeCold,
				NodeUID: types.UID("node-uid-1"), ProviderID: "vsphere://420e5e2e-c9eb-4a76-9623-725885785001",
				VMBIOSUUID: "420e5e2e-c9eb-4a76-9623-725885785001", OriginalUnschedulable: &wasUnschedulable,
				TargetFailureDomain: "target-fd-1", TargetHost: "/TargetDC/host/esx-1",
				TargetResourcePool: "/TargetDC/host/esx-1/Resources", TargetDatastore: "/TargetDC/datastore/TargetDS",
				TargetNetwork: "VM Network",
				ShutdownOutcome: &NodeMigrationOperationOutcome{
					ActionID: "55555555-5555-4555-8555-555555555555", Kind: NodeMigrationActionShutdownGuest,
					State: NodeMigrationOutcomeSucceeded, OwnerVCenter: "vc-source.example.com", ObservedAt: &now,
				},
				RelocationOutcome: &NodeMigrationOperationOutcome{
					ActionID: "66666666-6666-4666-8666-666666666666", Kind: NodeMigrationActionRelocateVM,
					State: NodeMigrationOutcomeSucceeded, OwnerVCenter: "vc-source.example.com",
					TaskReference: &NodeMigrationTaskReference{OwnerVCenter: "vc-source.example.com", TaskID: "task-101"},
				},
				PowerOnOutcome: &NodeMigrationOperationOutcome{
					ActionID: "77777777-7777-4777-8777-777777777777", Kind: NodeMigrationActionPowerOnVM,
					State: NodeMigrationOutcomeFailed, OwnerVCenter: "vc-target.example.com",
					TaskReference: &NodeMigrationTaskReference{OwnerVCenter: "vc-target.example.com", TaskID: "task-202"},
					Reason:        "power-on observation failed", CompletedAt: &now,
				},
			}}},
		}
		Expect(k8sClient.Status().Update(ctx, created)).To(Succeed())

		// The old request remains while status advances to another action. Schema
		// validation checks request shape, not live ownership or action authorization.
		Expect(k8sClient.Get(ctx, clientKey(resource), created)).To(Succeed())
		created.Status.NodeMigration.ActiveAction.ActionID = "88888888-8888-4888-8888-888888888888"
		created.Status.NodeMigration.ActiveAction.Kind = NodeMigrationActionPowerOnVM
		created.Status.NodeMigration.ActiveAction.OwnerVCenter = "vc-target.example.com"
		Expect(k8sClient.Status().Update(ctx, created)).To(Succeed())

		got := &VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, clientKey(resource), got)).To(Succeed())
		progress := got.Status.NodeMigration.Workers.Nodes[0]
		Expect(progress.ShutdownOutcome.State).To(Equal(NodeMigrationOutcomeSucceeded))
		Expect(progress.RelocationOutcome.State).To(Equal(NodeMigrationOutcomeSucceeded))
		Expect(progress.PowerOnOutcome.State).To(Equal(NodeMigrationOutcomeFailed))
		Expect(progress.RelocationOutcome.TaskReference.OwnerVCenter).To(Equal("vc-source.example.com"))
		Expect(progress.PowerOnOutcome.TaskReference.OwnerVCenter).To(Equal("vc-target.example.com"))
		Expect(got.Status.NodeMigration.ActiveAction.TaskReference).To(BeNil(), "a durable claim without a task reference is valid")

		deepCopy := got.DeepCopy()
		deepCopy.Status.NodeMigration.Workers.Nodes[0].ProviderID = "mutated"
		Expect(got.Status.NodeMigration.Workers.Nodes[0].ProviderID).NotTo(Equal("mutated"))
		*deepCopy.Status.NodeMigration.Workers.Nodes[0].OriginalUnschedulable = true
		Expect(*got.Status.NodeMigration.Workers.Nodes[0].OriginalUnschedulable).To(BeFalse())
		deepCopy.Status.NodeMigration.Workers.Nodes[0].RelocationOutcome.TaskReference.TaskID = "mutated-task"
		Expect(got.Status.NodeMigration.Workers.Nodes[0].RelocationOutcome.TaskReference.TaskID).To(Equal("task-101"))

		invalid := got.DeepCopy()
		invalid.Status.NodeMigration.ActiveAction.Kind = NodeMigrationActionShutdownGuest
		invalid.Status.NodeMigration.ActiveAction.TaskReference = &NodeMigrationTaskReference{
			OwnerVCenter: "vc-target.example.com", TaskID: "task-shutdown",
		}
		Expect(k8sClient.Status().Update(ctx, invalid)).To(HaveOccurred(), "ShutdownGuest has no task reference")

		invalid.Status.NodeMigration.ActiveAction.Kind = NodeMigrationActionRelocateVM
		invalid.Status.NodeMigration.ActiveAction.TaskReference.OwnerVCenter = "vc-other.example.com"
		Expect(k8sClient.Status().Update(ctx, invalid)).To(HaveOccurred(), "task reference must be vCenter-scoped")

		invalid = got.DeepCopy()
		invalid.Status.NodeMigration.Workers.Nodes[0].InstanceUUID = "420e5e2e-c9eb-4a76-9623-725885785099"
		Expect(k8sClient.Status().Update(ctx, invalid)).To(HaveOccurred(), "legacy instanceUUID cannot conflict with vmBIOSUUID")
	})
})
