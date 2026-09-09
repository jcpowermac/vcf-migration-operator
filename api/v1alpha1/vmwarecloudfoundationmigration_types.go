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
	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MigrationState represents the overall state of the migration workflow.
type MigrationState string

const (
	// MigrationStatePending indicates the migration has not started.
	MigrationStatePending MigrationState = "Pending"
	// MigrationStateRunning indicates the migration is actively progressing.
	MigrationStateRunning MigrationState = "Running"
	// MigrationStatePaused indicates the migration is paused by the user.
	MigrationStatePaused MigrationState = "Paused"
)

// MigrationProgress contains real-time counters and details of resource migration.
type MigrationProgress struct {
	// Workers tracks worker machine and node migration progress.
	// +optional
	Workers *WorkerMigrationProgress `json:"workers,omitempty"`

	// ControlPlane tracks ControlPlaneMachineSet rollout progress.
	// +optional
	ControlPlane *ControlPlaneProgress `json:"controlPlane,omitempty"`
}

// WorkerMigrationProgress surfaces worker machine and node counts across source and target.
type WorkerMigrationProgress struct {
	// targetMachinesTotal is the desired number of worker machines in target failure domains.
	// +optional
	TargetMachinesTotal int32 `json:"targetMachinesTotal,omitempty"`
	// targetMachinesReady is the number of target worker machines in Running phase with a NodeRef.
	// +optional
	TargetMachinesReady int32 `json:"targetMachinesReady,omitempty"`
	// targetNodesReady is the number of target worker nodes reporting NodeReady=True.
	// +optional
	TargetNodesReady int32 `json:"targetNodesReady,omitempty"`
	// sourceMachinesRemaining is the number of source worker machines still existing.
	// +optional
	SourceMachinesRemaining int32 `json:"sourceMachinesRemaining,omitempty"`
}

// ControlPlaneProgress surfaces CPMS rollout counts.
type ControlPlaneProgress struct {
	// replicas is the total desired control plane replicas.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// updatedReplicas is the number of control plane replicas matching the target spec.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`
	// readyReplicas is the number of control plane replicas running and ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
}

// SingletonName is the only object name the operator will reconcile. Since a
// single OpenShift cluster can only ever have one active vCenter migration,
// this follows OpenShift's singleton resource pattern (e.g.
// infrastructures.config.openshift.io/cluster), but as a namespaced resource
// whose singleton behavior is enforced at reconcile time via the Accepted
// condition rather than cluster-scoped singleton=true.
const SingletonName = "cluster"

// SingletonNamespace is the only namespace the operator will reconcile the
// singleton (SingletonName) from. It matches the namespace of the operator
// deployment (config/default/kustomization.yaml), so the authoritative
// migration record is openshift-vcf-migration/cluster.
const SingletonNamespace = "openshift-vcf-migration"

// FinalizerName is the finalizer that retains the VmwareCloudFoundationMigration
// record once deletion is requested. Deletion stops new migration actions but
// never cancels, powers off, deletes VMs, or rolls back; the record is held
// until the migration is complete or an administrator releases it via
// AnnotationAllowDeletion.
const FinalizerName = "migration.openshift.io/record-retention"

// AnnotationAllowDeletion is the administrator escape hatch that releases
// FinalizerName on a migration whose work is not yet complete. The only
// accepted value is "true".
const AnnotationAllowDeletion = "migration.openshift.io/allow-deletion"

// SecretReference references a secret by name and namespace.
type SecretReference struct {
	// name is the secret name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// namespace is the secret namespace. When omitted, defaults to the namespace
	// of the VmwareCloudFoundationMigration object.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// VmwareCloudFoundationMigrationSpec defines the desired state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationSpec struct {
	// state controls the workflow: Pending, Running, Paused.
	// The migration workflow only executes when state is Running.
	// When omitted, defaults to Pending.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Running;Paused
	// +kubebuilder:default=Pending
	State MigrationState `json:"state,omitempty"`

	// targetVCenterCredentialsSecret references the secret containing target vCenter credentials.
	// The secret must contain keys: {target-vcenter-fqdn}.username and {target-vcenter-fqdn}.password.
	// +required
	TargetVCenterCredentialsSecret SecretReference `json:"targetVCenterCredentialsSecret"`

	// failureDomains defines failure domains for the target vCenter. The embedded
	// element type intentionally tracks config.openshift.io/v1 VSpherePlatformFailureDomainSpec
	// (including Name, Region, Zone, Server, and Topology) to align with OpenShift API
	// failure domain semantics; the schema is re-inlined on "make manifests" after
	// dependency bumps.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:XValidation:rule="self.all(fd, has(fd.topology.template) && fd.topology.template != '')",message="topology.template is required for every failure domain"
	// +listType=atomic
	FailureDomains []configv1.VSpherePlatformFailureDomainSpec `json:"failureDomains"`

	// nodeMigration controls how nodes move to the target vCenter.
	// When omitted, the operator keeps the current MachineSet/CPMS replacement path.
	// When set, type is the cluster-wide engine; roles cannot override it.
	// +optional
	NodeMigration *NodeMigrationSpec `json:"nodeMigration,omitempty"`
}

// NodeMigrationType is the cluster-wide engine used to move nodes to the
// target vCenter. It is the tagged-union discriminator for NodeMigrationSpec.
type NodeMigrationType string

const (
	// NodeMigrationTypeVMotion relocates existing VMs (Hot or Cold).
	NodeMigrationTypeVMotion NodeMigrationType = "VMotion"
	// NodeMigrationTypeRecreate replaces VMs via MachineSets/CPMS.
	NodeMigrationTypeRecreate NodeMigrationType = "Recreate"
)

// VMotionMode selects live vs cold relocation for vMotion.
type VMotionMode string

const (
	// VMotionModeAuto tries Hot and falls back to Cold.
	VMotionModeAuto VMotionMode = "Auto"
	// VMotionModeHot performs live (hot) relocation.
	VMotionModeHot VMotionMode = "Hot"
	// VMotionModeCold performs cold relocation (powered off).
	VMotionModeCold VMotionMode = "Cold"
)

// NodeMigrationSpec controls how nodes move to the target vCenter.
// Each role independently selects its migration engine (VMotion or Recreate)
// and, when VMotion, the relocation mode (Auto, Hot, Cold).
// Both workers and controlPlane must be specified; omitting a role is not
// allowed because migration is a destructive operation and implicit defaults
// could cause unintended node replacement or relocation.
// +kubebuilder:validation:XValidation:rule="has(self.workers)",message="workers is required when nodeMigration is set"
// +kubebuilder:validation:XValidation:rule="has(self.controlPlane)",message="controlPlane is required when nodeMigration is set"
// +kubebuilder:validation:XValidation:rule="!has(self.controlPlane) || !has(self.controlPlane.maxUnavailable) || self.controlPlane.maxUnavailable == 1",message="controlPlane.maxUnavailable must be 1"
type NodeMigrationSpec struct {
	// workers defines the migration engine and rolling limits for worker nodes.
	// +required
	Workers *RoleMigrationSpec `json:"workers"`

	// controlPlane defines the migration engine and rolling limits for
	// control-plane nodes. maxUnavailable is validated to 1 for this role.
	// +required
	ControlPlane *RoleMigrationSpec `json:"controlPlane"`
}

// RoleMigrationSpec holds the migration engine selection and rolling limits
// for one node role. type is a tagged-union discriminator: vmotion is
// required when type is VMotion and forbidden when type is Recreate.
// +union
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type == 'VMotion' || !has(self.vmotion)",message="vmotion is only valid when type is VMotion"
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type != 'VMotion' || has(self.vmotion)",message="vmotion is required when type is VMotion"
type RoleMigrationSpec struct {
	// type is the migration engine for this role.
	// VMotion relocates existing VMs. Recreate replaces VMs via MachineSets/CPMS.
	// +kubebuilder:validation:Enum=VMotion;Recreate
	// +kubebuilder:default=Recreate
	// +unionDiscriminator
	// +optional
	Type NodeMigrationType `json:"type,omitempty"`

	// vmotion is VMotion-specific behavior. Required when type is VMotion.
	// Forbidden when type is Recreate.
	// +unionMember
	// +optional
	VMotion *VMotionSpec `json:"vmotion,omitempty"`

	// maxUnavailable is the maximum number of nodes in this role that can be
	// migrating simultaneously. It must be a positive integer and defaults to 1.
	// The control-plane value is restricted to 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MaxUnavailable *int32 `json:"maxUnavailable,omitempty"`
}

// VMotionSpec is VMotion-specific migration behavior.
type VMotionSpec struct {
	// mode selects live vs cold relocation.
	// Auto tries Hot and falls back to Cold.
	// +kubebuilder:validation:Enum=Auto;Hot;Cold
	// +kubebuilder:default=Auto
	// +optional
	Mode VMotionMode `json:"mode,omitempty"`
}

// VmwareCloudFoundationMigrationStatus defines the observed state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationStatus struct {
	// progress surfaces real-time counts for worker and control-plane migration.
	// +optional
	Progress *MigrationProgress `json:"progress,omitempty"`

	// conditions represent the current state of the migration.
	// Known conditions are:
	// - Accepted: admission gate; True is normal for the single reconciled instance
	//   (openshift-vcf-migration/cluster), while False indicates an unsupported
	//   object name or namespace.
	// - InfrastructurePrepared: preflight validation and migration path selection.
	// - DestinationInitialized: destination vCenter folders and tags created.
	// - MultiSiteConfigured: cluster configured for both source and target vCenters.
	// - WorkloadMigrated: workloads migrated to destination vCenter machine sets.
	// - SourceCleaned: source vCenter references removed and cleaned up.
	// - Ready: aggregate condition indicating migration is complete and cluster is healthy.
	// For stage conditions (InfrastructurePrepared through Ready), False is normal while
	// progressing; True indicates the stage has completed. Stages execute in order.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// sourceVCenter records the vCenter server removed during source cleanup.
	// It is captured before any cleanup side effects so retries stay bound to
	// the original source after it is removed from the Infrastructure
	// vCenters list, where VCenters[0] would then be the destination.
	// +optional
	SourceVCenter string `json:"sourceVCenter,omitempty"`

	// startTime is when the migration started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// lastUpdateTime is when the migration status was last updated.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// completionTime is when the migration completed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// completionSpecHash is the lowercase hex SHA-256 of encoding/json.Marshal
	// of the typed spec when Ready=True is established. With Ready=True and
	// completionTime, a matching hash authorizes automatic record release even
	// when deletion increments generation. When omitted, administrator handoff
	// is required to release the record.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	CompletionSpecHash string `json:"completionSpecHash,omitempty"`

	// nodeMigration reports per-node move progress.
	// Nil when the MachineSet/CPMS replacement path is used.
	// +optional
	NodeMigration *NodeMigrationStatus `json:"nodeMigration,omitempty"`
}

// NodeMigrationStatus reports node-migration progress.
type NodeMigrationStatus struct {
	// type is the engine selected when WorkloadMigrated started.
	// +kubebuilder:validation:Enum=VMotion;Recreate
	// +optional
	Type NodeMigrationType `json:"type,omitempty"`

	// progress is a human-readable rollup for kubectl get,
	// for example "control plane 2/3, workers 4/10".
	// +optional
	Progress string `json:"progress,omitempty"`

	// controlPlane is control-plane progress: counts plus per-node entries.
	// +optional
	ControlPlane *RoleMigrationStatus `json:"controlPlane,omitempty"`

	// workers is worker progress: counts plus per-node entries.
	// +optional
	Workers *RoleMigrationStatus `json:"workers,omitempty"`
}

// RoleMigrationStatus is progress for one role. Counts are derived from nodes.
type RoleMigrationStatus struct {
	// requestedMode is the vMotion mode from spec for this role
	// (inherited from spec.nodeMigration.vmotion.mode when the role omits mode).
	// Auto is spec policy, not what vCenter ran; see observedMode.
	// +kubebuilder:validation:Enum=Auto;Hot;Cold
	// +optional
	RequestedMode VMotionMode `json:"requestedMode,omitempty"`
	// observedMode is the relocation that actually ran for this role: Hot or Cold.
	// Never Auto. Empty until at least one node has started relocating, and empty
	// if Auto produced a mix of Hot and Cold (inspect nodes[].observedMode).
	// +kubebuilder:validation:Enum=Hot;Cold
	// +optional
	ObservedMode VMotionMode `json:"observedMode,omitempty"`
	// total is the number of discovered nodes in this role.
	// +optional
	Total int32 `json:"total,omitempty"`
	// pending is nodes not yet started.
	// +optional
	Pending int32 `json:"pending,omitempty"`
	// inProgress is nodes in Preparing, Migrating, or WaitingForNode.
	// +optional
	InProgress int32 `json:"inProgress,omitempty"`
	// succeeded is nodes in Succeeded.
	// +optional
	Succeeded int32 `json:"succeeded,omitempty"`
	// failed is nodes in Failed.
	// +optional
	Failed int32 `json:"failed,omitempty"`
	// nodes is per-node progress for this role, keyed by Kubernetes node name.
	// +optional
	// +listType=map
	// +listMapKey=name
	Nodes []NodeMigrationProgress `json:"nodes,omitempty"`
}

// NodeMigrationPhase is the node-local relocation state.
type NodeMigrationPhase string

const (
	// NodeMigrationPhasePending is discovered, not started.
	NodeMigrationPhasePending NodeMigrationPhase = "Pending"
	// NodeMigrationPhasePreparing is drain/cordon (Cold) or live-compat checks (Hot).
	NodeMigrationPhasePreparing NodeMigrationPhase = "Preparing"
	// NodeMigrationPhaseMigrating is RelocateVM_Task running.
	NodeMigrationPhaseMigrating NodeMigrationPhase = "Migrating"
	// NodeMigrationPhaseWaitingForNode is the VM on the target; kubelet not Ready yet.
	NodeMigrationPhaseWaitingForNode NodeMigrationPhase = "WaitingForNode"
	// NodeMigrationPhaseSucceeded is node Ready on the target.
	NodeMigrationPhaseSucceeded NodeMigrationPhase = "Succeeded"
	// NodeMigrationPhaseFailed is a terminal or currently-failed node.
	NodeMigrationPhaseFailed NodeMigrationPhase = "Failed"
)

// NodeMigrationProgress is observed state for a single node/VM.
type NodeMigrationProgress struct {
	// name is the Kubernetes Node name. List merge key.
	// +required
	Name string `json:"name"`
	// phase is the node-local relocation state.
	// +kubebuilder:validation:Enum=Pending;Preparing;Migrating;WaitingForNode;Succeeded;Failed
	// +required
	Phase NodeMigrationPhase `json:"phase"`
	// requestedMode is the mode in force when this node started.
	// Frozen for in-flight nodes if spec changes later.
	// +kubebuilder:validation:Enum=Auto;Hot;Cold
	// +optional
	RequestedMode VMotionMode `json:"requestedMode,omitempty"`
	// observedMode is the relocation that actually ran: Hot or Cold.
	// Empty until RelocateVM starts. Auto resolves here, never in spec.
	// +kubebuilder:validation:Enum=Hot;Cold
	// +optional
	ObservedMode VMotionMode `json:"observedMode,omitempty"`
	// instanceUUID is the VM BIOS UUID (node spec.providerID, without vsphere://).
	// Stable identity across vCenters; MoRef is not.
	// +optional
	InstanceUUID string `json:"instanceUUID,omitempty"`
	// targetFailureDomain is the spec.failureDomains[].name chosen for placement.
	// +optional
	TargetFailureDomain string `json:"targetFailureDomain,omitempty"`
	// sourceInventoryPath is the VM path before relocation.
	// +optional
	SourceInventoryPath string `json:"sourceInventoryPath,omitempty"`
	// targetInventoryPath is the VM path after relocation.
	// +optional
	TargetInventoryPath string `json:"targetInventoryPath,omitempty"`
	// message is a short human-readable explanation of phase or failure.
	// +optional
	Message string `json:"message,omitempty"`
	// lastTransitionTime is when phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// startedAt is when Preparing began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// completedAt is when phase became Succeeded or Failed.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// Condition type constants for the migration workflow.
// The reconciler checks conditions in this order; if a condition is not True,
// it executes the work for that condition and returns with RequeueAfter.
const (
	// ConditionInfrastructurePrepared indicates preflight checks passed and the
	// migration path has been selected.
	ConditionInfrastructurePrepared = "InfrastructurePrepared"

	// ConditionDestinationInitialized indicates the target vCenter has all required assets
	// (VM folders, region/zone tags).
	ConditionDestinationInitialized = "DestinationInitialized"

	// ConditionMultiSiteConfigured indicates the cluster recognizes both vCenters
	// (secrets, Infrastructure CRD, cloud-provider-config updated, pods restarted).
	ConditionMultiSiteConfigured = "MultiSiteConfigured"

	// ConditionWorkloadMigrated indicates compute is running in the new location
	// (new workers created, control plane rolled out, old MachineSets scaled to 0).
	ConditionWorkloadMigrated = "WorkloadMigrated"

	// ConditionSourceCleaned indicates the old vCenter is fully detached
	// (removed from Infrastructure, config, and secrets; CVO is re-enabled when
	// required by the legacy path).
	ConditionSourceCleaned = "SourceCleaned"

	// ConditionReady indicates migration is 100% complete.
	// This is an aggregate condition: all operators healthy, only target vCenters in Infrastructure.
	ConditionReady = "Ready"

	// ConditionAccepted indicates whether this object is the singleton instance
	// (named SingletonName in SingletonNamespace) that the operator will act on.
	ConditionAccepted = "Accepted"
)

// Condition reason constants.
const (
	ReasonProgressing = "Progressing"
	ReasonCompleted   = "Completed"
	ReasonFailed      = "Failed"
	// ReasonPaused indicates migration has been paused because spec.state is set to Paused.
	ReasonPaused = "Paused"

	// ReasonRetrying indicates the operator hit a transient condition (e.g. an
	// external resource is temporarily unhealthy) and will automatically retry.
	// Unlike ReasonFailed, this is not a terminal state.
	ReasonRetrying = "Retrying"

	// ReasonUnsupportedName indicates the object's name is not SingletonName,
	// so the operator is ignoring it.
	ReasonUnsupportedName = "UnsupportedName"

	// ReasonUnsupportedNamespace indicates the object's namespace is not
	// SingletonNamespace, so the operator is ignoring it.
	ReasonUnsupportedNamespace = "UnsupportedNamespace"

	// ReasonDeletionBlocked indicates deletion was requested while migration
	// work is outstanding; the record is retained behind FinalizerName.
	ReasonDeletionBlocked = "DeletionBlocked"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=vmwarecloudfoundationmigrations,scope=Namespaced,shortName=vcfm,categories=migration
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completionTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VmwareCloudFoundationMigration is the Schema for the vmwarecloudfoundationmigrations API.
// It orchestrates migration of an OpenShift cluster from one vCenter to another.
type VmwareCloudFoundationMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of VmwareCloudFoundationMigration.
	// +optional
	Spec VmwareCloudFoundationMigrationSpec `json:"spec,omitempty"`

	// status defines the observed state of VmwareCloudFoundationMigration.
	// +optional
	Status VmwareCloudFoundationMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VmwareCloudFoundationMigrationList contains a list of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// items is the list of VmwareCloudFoundationMigration objects.
	// +required
	Items []VmwareCloudFoundationMigration `json:"items"`
}
