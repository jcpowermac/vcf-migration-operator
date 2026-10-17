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
	"k8s.io/apimachinery/pkg/types"
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
// +kubebuilder:validation:XValidation:rule="(has(self.nodeMigration) && self.nodeMigration.workers.type == 'VMotion' && self.nodeMigration.controlPlane.type == 'VMotion') || self.failureDomains.all(fd, has(fd.topology.template) && size(fd.topology.template) > 0)",message="topology.template is required for legacy and Recreate configurations"
// +kubebuilder:validation:XValidation:rule="!has(self.nodeMigration) || (self.nodeMigration.workers.type != 'VMotion' && self.nodeMigration.controlPlane.type != 'VMotion') || size(self.failureDomains) == 1",message="VMotion requires exactly one failure domain"
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
	// dependency bumps. Legacy and Recreate configurations require topology.template;
	// VMotion configurations do not use a template and are limited to one failure domain.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	FailureDomains []configv1.VSpherePlatformFailureDomainSpec `json:"failureDomains"`

	// nodeMigration controls how nodes move to the target vCenter.
	// When omitted, the operator keeps the current MachineSet/CPMS replacement path.
	// When set, both roles select the same migration engine.
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

// VMotionMode selects live or cold relocation for VMotion.
type VMotionMode string

const (
	// VMotionModeHot performs live (hot) relocation.
	VMotionModeHot VMotionMode = "Hot"
	// VMotionModeCold performs cold relocation (powered off).
	VMotionModeCold VMotionMode = "Cold"
)

// NodeMigrationSpec controls how nodes move to the target vCenter.
// Each role selects its migration engine (VMotion or Recreate). VMotion roles
// must select the same explicit relocation mode (Hot or Cold).
// Both workers and controlPlane must be specified; omitting a role is not
// allowed because migration is a destructive operation and implicit defaults
// could cause unintended node replacement or relocation.
// +kubebuilder:validation:XValidation:rule="has(self.workers)",message="workers is required when nodeMigration is set"
// +kubebuilder:validation:XValidation:rule="has(self.controlPlane)",message="controlPlane is required when nodeMigration is set"
// +kubebuilder:validation:XValidation:rule="!has(self.controlPlane) || !has(self.controlPlane.maxUnavailable) || self.controlPlane.maxUnavailable == 1",message="controlPlane.maxUnavailable must be 1"
// +kubebuilder:validation:XValidation:rule="!has(self.workers) || !has(self.controlPlane) || self.workers.type == self.controlPlane.type",message="workers and controlPlane must use the same migration type"
// +kubebuilder:validation:XValidation:rule="!has(self.workers) || !has(self.controlPlane) || self.workers.type != 'VMotion' || self.controlPlane.type != 'VMotion' || !has(self.workers.vmotion) || !has(self.controlPlane.vmotion) || !has(self.workers.vmotion.mode) || !has(self.controlPlane.vmotion.mode) || self.workers.vmotion.mode == self.controlPlane.vmotion.mode",message="workers and controlPlane must use the same VMotion mode"
// +kubebuilder:validation:XValidation:rule="!has(self.workers) || self.workers.type != 'VMotion' || !has(self.workers.maxUnavailable) || self.workers.maxUnavailable == 1",message="VMotion workers.maxUnavailable must be 1"
type NodeMigrationSpec struct {
	// workers defines the migration engine and rolling limits for worker nodes.
	// +required
	Workers *RoleMigrationSpec `json:"workers"`

	// controlPlane defines the migration engine and rolling limits for
	// control-plane nodes. maxUnavailable is validated to 1 for this role.
	// +required
	ControlPlane *RoleMigrationSpec `json:"controlPlane"`

	// recoveryRequest asks the operator to recheck one blocked action. It is an
	// action-bound request, not evidence that the action is inactive or authority
	// to replay it. Runtime validation and authorization are not performed by schema.
	// +optional
	RecoveryRequest *NodeMigrationRecoveryRequest `json:"recoveryRequest,omitempty"`
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
	// mode explicitly selects live or cold relocation. There is no fallback mode.
	// +kubebuilder:validation:Enum=Hot;Cold
	// +required
	Mode VMotionMode `json:"mode"`
}

// NodeMigrationRecoveryRequest is an action-bound request for runtime rechecking.
// The identifiers describe the request target; they do not prove safe recovery.
type NodeMigrationRecoveryRequest struct {
	// requestID uniquely identifies this acknowledgement request.
	// +kubebuilder:validation:Format=uuid
	// +required
	RequestID string `json:"requestID"`
	// executionID identifies the frozen execution being rechecked.
	// +kubebuilder:validation:Format=uuid
	// +required
	ExecutionID string `json:"executionID"`
	// actionID identifies the specific blocked action being rechecked.
	// +kubebuilder:validation:Format=uuid
	// +required
	ActionID string `json:"actionID"`
	// reason is a sanitized, bounded administrator note; it must not contain secrets.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
}

// VmwareCloudFoundationMigrationStatus defines the observed state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationStatus struct {
	// progress surfaces real-time counts for worker and control-plane migration.
	// +optional
	Progress *MigrationProgress `json:"progress,omitempty"`

	// conditions represent the current state of the migration.
	// Known conditions are:
	// - Accepted: admission gate; True is normal for the single reconciled instance (cluster),
	//   while False indicates an unsupported object name.
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

	// startTime is when the migration started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// lastUpdateTime is when the migration status was last updated.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// completionTime is when the migration completed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

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
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Progress string `json:"progress,omitempty"`

	// controlPlane is control-plane progress: counts plus per-node entries.
	// +optional
	ControlPlane *RoleMigrationStatus `json:"controlPlane,omitempty"`

	// workers is worker progress: counts plus per-node entries.
	// +optional
	Workers *RoleMigrationStatus `json:"workers,omitempty"`

	// executionID is stable across pause-related generation changes; generation
	// and observedGeneration are not operation identifiers.
	// +kubebuilder:validation:Format=uuid
	// +optional
	ExecutionID string `json:"executionID,omitempty"`

	// migrationUID binds this execution record to the originating CR UID.
	// +optional
	MigrationUID types.UID `json:"migrationUID,omitempty"`

	// planFingerprint identifies the sanitized frozen plan, excluding credentials
	// and other secret material.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +optional
	PlanFingerprint string `json:"planFingerprint,omitempty"`

	// frozenPlan records bounded source, destination, mode and placement identity.
	// +optional
	FrozenPlan *NodeMigrationFrozenPlan `json:"frozenPlan,omitempty"`

	// activeAction is the single action claim currently being observed. A claim
	// without a task reference means submission may have begun and is not safe to retry.
	// +optional
	ActiveAction *NodeMigrationAction `json:"activeAction,omitempty"`

	// intervention records an unresolved condition requiring administrator review.
	// Its presence does not itself prove an action is inactive.
	// +optional
	Intervention *NodeMigrationIntervention `json:"intervention,omitempty"`

	// recoveryAcknowledgement records consumption of a recovery request. It is not
	// proof of inactivity, authorization, or replay safety.
	// +optional
	RecoveryAcknowledgement *NodeMigrationRecoveryAcknowledgement `json:"recoveryAcknowledgement,omitempty"`

	// cleanup is bounded progress for source-reference cleanup; it is observational
	// state and does not authorize cleanup.
	// +optional
	Cleanup *NodeMigrationCleanupProgress `json:"cleanup,omitempty"`
}

// NodeMigrationFrozenPlan is the bounded, sanitized identity of one frozen plan.
type NodeMigrationFrozenPlan struct {
	// sourceVCenter identifies the source vCenter endpoint.
	// +kubebuilder:validation:MaxLength=512
	// +required
	SourceVCenter string `json:"sourceVCenter"`
	// sourceDatacenter identifies the source datacenter.
	// +kubebuilder:validation:MaxLength=253
	// +required
	SourceDatacenter string `json:"sourceDatacenter"`
	// destinationVCenter identifies the destination vCenter endpoint.
	// +kubebuilder:validation:MaxLength=512
	// +required
	DestinationVCenter string `json:"destinationVCenter"`
	// mode is the explicitly selected mode frozen for this execution.
	// +kubebuilder:validation:Enum=Hot;Cold
	// +required
	Mode VMotionMode `json:"mode"`
	// targetFailureDomain is the selected target failure-domain name.
	// +kubebuilder:validation:MaxLength=253
	// +required
	TargetFailureDomain string `json:"targetFailureDomain"`
}

// NodeMigrationActionKind identifies one mutating VM operation.
type NodeMigrationActionKind string

const (
	// NodeMigrationActionShutdownGuest requests graceful guest shutdown.
	NodeMigrationActionShutdownGuest NodeMigrationActionKind = "ShutdownGuest"
	// NodeMigrationActionRelocateVM submits VM relocation.
	NodeMigrationActionRelocateVM NodeMigrationActionKind = "RelocateVM"
	// NodeMigrationActionPowerOnVM submits VM power-on.
	NodeMigrationActionPowerOnVM NodeMigrationActionKind = "PowerOnVM"
)

// NodeMigrationSubmissionState reports durable submission knowledge.
type NodeMigrationSubmissionState string

const (
	// NodeMigrationSubmissionClaimed means submission may have begun; it is not safe to retry.
	NodeMigrationSubmissionClaimed NodeMigrationSubmissionState = "Claimed"
	// NodeMigrationSubmissionSubmitted means the request submission was acknowledged.
	NodeMigrationSubmissionSubmitted NodeMigrationSubmissionState = "Submitted"
	// NodeMigrationSubmissionUnknown means submission outcome cannot be established.
	NodeMigrationSubmissionUnknown NodeMigrationSubmissionState = "Unknown"
)

// NodeMigrationObservationState reports the latest bounded action observation.
type NodeMigrationObservationState string

const (
	// NodeMigrationObservationNotObserved means no authoritative action observation is recorded.
	NodeMigrationObservationNotObserved NodeMigrationObservationState = "NotObserved"
	// NodeMigrationObservationRunning means the action is observed in progress.
	NodeMigrationObservationRunning NodeMigrationObservationState = "Running"
	// NodeMigrationObservationSucceeded means the action outcome was observed successful.
	NodeMigrationObservationSucceeded NodeMigrationObservationState = "Succeeded"
	// NodeMigrationObservationFailed means the action outcome was observed failed.
	NodeMigrationObservationFailed NodeMigrationObservationState = "Failed"
	// NodeMigrationObservationUnknown means the action outcome remains unknown.
	NodeMigrationObservationUnknown NodeMigrationObservationState = "Unknown"
)

// NodeMigrationOutcomeState is the bounded result of one completed or uncertain operation.
type NodeMigrationOutcomeState string

const (
	// NodeMigrationOutcomeSucceeded records observed success.
	NodeMigrationOutcomeSucceeded NodeMigrationOutcomeState = "Succeeded"
	// NodeMigrationOutcomeFailed records observed failure.
	NodeMigrationOutcomeFailed NodeMigrationOutcomeState = "Failed"
	// NodeMigrationOutcomeUnknown records an unresolved outcome.
	NodeMigrationOutcomeUnknown NodeMigrationOutcomeState = "Unknown"
)

// NodeMigrationTaskReference identifies a task within its owning vCenter.
type NodeMigrationTaskReference struct {
	// ownerVCenter scopes taskID to one vCenter.
	// +kubebuilder:validation:MaxLength=512
	// +required
	OwnerVCenter string `json:"ownerVCenter"`
	// taskID is the vCenter-local task identifier.
	// +kubebuilder:validation:MaxLength=512
	// +required
	TaskID string `json:"taskID"`
}

// NodeMigrationAction is the single durable mutating-action claim.
// +kubebuilder:validation:XValidation:rule="!has(self.taskReference) || self.taskReference.ownerVCenter == self.ownerVCenter",message="taskReference.ownerVCenter must match ownerVCenter"
// +kubebuilder:validation:XValidation:rule="self.kind != 'ShutdownGuest' || !has(self.taskReference)",message="ShutdownGuest does not have a task reference"
type NodeMigrationAction struct {
	// actionID is stable for this one mutating action.
	// +kubebuilder:validation:Format=uuid
	// +required
	ActionID string `json:"actionID"`
	// executionID binds this action to the frozen execution.
	// +kubebuilder:validation:Format=uuid
	// +required
	ExecutionID string `json:"executionID"`
	// kind is the mutating operation claimed.
	// +kubebuilder:validation:Enum=ShutdownGuest;RelocateVM;PowerOnVM
	// +required
	Kind NodeMigrationActionKind `json:"kind"`
	// nodeName identifies the Kubernetes Node.
	// +kubebuilder:validation:MaxLength=253
	// +required
	NodeName string `json:"nodeName"`
	// nodeUID binds the action to the selected Node object.
	// +required
	NodeUID types.UID `json:"nodeUID"`
	// vmBIOSUUID identifies the VM BIOS UUID, not vSphere instanceUUID.
	// +kubebuilder:validation:MaxLength=64
	// +required
	VMBIOSUUID string `json:"vmBIOSUUID"`
	// ownerVCenter is the vCenter in which this action/task is interpreted.
	// +kubebuilder:validation:MaxLength=512
	// +required
	OwnerVCenter string `json:"ownerVCenter"`
	// operationIdentity is a digest of sanitized, non-secret intended-operation inputs.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +required
	OperationIdentity string `json:"operationIdentity"`
	// submissionState distinguishes a persisted claim from acknowledged submission.
	// +kubebuilder:validation:Enum=Claimed;Submitted;Unknown
	// +required
	SubmissionState NodeMigrationSubmissionState `json:"submissionState"`
	// observationState records the latest action observation.
	// +kubebuilder:validation:Enum=NotObserved;Running;Succeeded;Failed;Unknown
	// +required
	ObservationState NodeMigrationObservationState `json:"observationState"`
	// taskReference is optional; ShutdownGuest has no task reference.
	// +optional
	TaskReference *NodeMigrationTaskReference `json:"taskReference,omitempty"`
	// claimedAt is when the durable claim was written.
	// +required
	ClaimedAt metav1.Time `json:"claimedAt"`
	// submittedAt is when submission was acknowledged, if known.
	// +optional
	SubmittedAt *metav1.Time `json:"submittedAt,omitempty"`
	// lastObservedAt is the latest observation time, if any.
	// +optional
	LastObservedAt *metav1.Time `json:"lastObservedAt,omitempty"`
	// deadline is the bounded action deadline, if set by the implementation.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
	// reason is a sanitized, bounded observation/outcome detail.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
}

// NodeMigrationOperationOutcome records one operation without overwriting another operation's result.
// +kubebuilder:validation:XValidation:rule="!has(self.taskReference) || self.taskReference.ownerVCenter == self.ownerVCenter",message="taskReference.ownerVCenter must match ownerVCenter"
// +kubebuilder:validation:XValidation:rule="self.kind != 'ShutdownGuest' || !has(self.taskReference)",message="ShutdownGuest does not have a task reference"
type NodeMigrationOperationOutcome struct {
	// actionID identifies the action that produced this outcome.
	// +kubebuilder:validation:Format=uuid
	// +required
	ActionID string `json:"actionID"`
	// kind identifies the operation represented by this outcome.
	// +kubebuilder:validation:Enum=ShutdownGuest;RelocateVM;PowerOnVM
	// +required
	Kind NodeMigrationActionKind `json:"kind"`
	// state is the bounded result; it is observational, not replay authority.
	// +kubebuilder:validation:Enum=Succeeded;Failed;Unknown
	// +required
	State NodeMigrationOutcomeState `json:"state"`
	// ownerVCenter scopes any task reference.
	// +kubebuilder:validation:MaxLength=512
	// +required
	OwnerVCenter string `json:"ownerVCenter"`
	// taskReference is optional and scoped to ownerVCenter.
	// +optional
	TaskReference *NodeMigrationTaskReference `json:"taskReference,omitempty"`
	// reason is a sanitized, bounded explanation.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
	// observedAt is when this outcome was last established.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// completedAt is when the operation outcome became terminal, if known.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// NodeMigrationIntervention records one unresolved intervention requirement.
type NodeMigrationIntervention struct {
	// executionID identifies the affected frozen execution.
	// +kubebuilder:validation:Format=uuid
	// +required
	ExecutionID string `json:"executionID"`
	// actionID identifies the affected action.
	// +kubebuilder:validation:Format=uuid
	// +required
	ActionID string `json:"actionID"`
	// nodeName identifies the affected Node.
	// +kubebuilder:validation:MaxLength=253
	// +required
	NodeName string `json:"nodeName"`
	// reason is a sanitized, bounded explanation.
	// +kubebuilder:validation:MaxLength=1024
	// +required
	Reason string `json:"reason"`
	// since is when intervention became necessary.
	// +required
	Since metav1.Time `json:"since"`
}

// NodeMigrationRecoveryAcknowledgement records that a request was consumed.
// Consumption alone is not runtime evidence or replay authorization.
type NodeMigrationRecoveryAcknowledgement struct {
	// requestID identifies the consumed request.
	// +kubebuilder:validation:Format=uuid
	// +required
	RequestID string `json:"requestID"`
	// executionID is the execution named by the request.
	// +kubebuilder:validation:Format=uuid
	// +required
	ExecutionID string `json:"executionID"`
	// actionID is the action named by the request.
	// +kubebuilder:validation:Format=uuid
	// +required
	ActionID string `json:"actionID"`
	// consumedAt records when the request was consumed.
	// +required
	ConsumedAt metav1.Time `json:"consumedAt"`
}

// NodeMigrationCleanupPhase is bounded progress through source cleanup.
type NodeMigrationCleanupPhase string

const (
	// NodeMigrationCleanupPending indicates cleanup has not started.
	NodeMigrationCleanupPending NodeMigrationCleanupPhase = "Pending"
	// NodeMigrationCleanupRemovingSourceReferences indicates source references are being removed.
	NodeMigrationCleanupRemovingSourceReferences NodeMigrationCleanupPhase = "RemovingSourceReferences"
	// NodeMigrationCleanupVerifyingReadiness indicates final readiness is being verified.
	NodeMigrationCleanupVerifyingReadiness NodeMigrationCleanupPhase = "VerifyingReadiness"
	// NodeMigrationCleanupCompleted indicates intended cleanup and verification completed.
	NodeMigrationCleanupCompleted NodeMigrationCleanupPhase = "Completed"
	// NodeMigrationCleanupBlocked indicates cleanup is blocked.
	NodeMigrationCleanupBlocked NodeMigrationCleanupPhase = "Blocked"
)

// NodeMigrationCleanupProgress is bounded source-cleanup progress, not an event journal.
type NodeMigrationCleanupProgress struct {
	// phase is the current cleanup observation.
	// +kubebuilder:validation:Enum=Pending;RemovingSourceReferences;VerifyingReadiness;Completed;Blocked
	// +required
	Phase NodeMigrationCleanupPhase `json:"phase"`
	// lastTransitionTime is when phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// reason is a sanitized, bounded explanation.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
}

// RoleMigrationStatus is progress for one role. Counts are derived from nodes.
type RoleMigrationStatus struct {
	// requestedMode is the explicit vMotion mode selected for this role.
	// +kubebuilder:validation:Enum=Hot;Cold
	// +optional
	RequestedMode VMotionMode `json:"requestedMode,omitempty"`
	// observedMode is the relocation mode observed for this role: Hot or Cold.
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
	// +kubebuilder:validation:MaxItems=500
	// +listType=map
	// +listMapKey=name
	Nodes []NodeMigrationProgress `json:"nodes,omitempty"`
}

// NodeMigrationPhase is the coarse node-local migration state.
type NodeMigrationPhase string

const (
	// NodeMigrationPhasePending is discovered, not started.
	NodeMigrationPhasePending NodeMigrationPhase = "Pending"
	// NodeMigrationPhasePreparing covers pre-action checks and scheduling preparation;
	// it does not imply that only Cold mode drains or cordons.
	NodeMigrationPhasePreparing NodeMigrationPhase = "Preparing"
	// NodeMigrationPhaseMigrating is RelocateVM_Task running.
	NodeMigrationPhaseMigrating NodeMigrationPhase = "Migrating"
	// NodeMigrationPhaseWaitingForNode is the VM on the target; kubelet not Ready yet.
	NodeMigrationPhaseWaitingForNode NodeMigrationPhase = "WaitingForNode"
	// NodeMigrationPhaseSucceeded means required action, identity, placement and
	// recovery verification completed; a stale Node Ready condition alone is insufficient.
	NodeMigrationPhaseSucceeded NodeMigrationPhase = "Succeeded"
	// NodeMigrationPhaseFailed is a terminal or currently-failed node.
	NodeMigrationPhaseFailed NodeMigrationPhase = "Failed"
)

// NodeMigrationProgress is observed state for a single node/VM.
// +kubebuilder:validation:XValidation:rule="!has(self.instanceUUID) || !has(self.vmBIOSUUID) || self.instanceUUID == self.vmBIOSUUID",message="instanceUUID and vmBIOSUUID must identify the same VM BIOS UUID"
// +kubebuilder:validation:XValidation:rule="!has(self.shutdownOutcome) || self.shutdownOutcome.kind == 'ShutdownGuest'",message="shutdownOutcome.kind must be ShutdownGuest"
// +kubebuilder:validation:XValidation:rule="!has(self.relocationOutcome) || self.relocationOutcome.kind == 'RelocateVM'",message="relocationOutcome.kind must be RelocateVM"
// +kubebuilder:validation:XValidation:rule="!has(self.powerOnOutcome) || self.powerOnOutcome.kind == 'PowerOnVM'",message="powerOnOutcome.kind must be PowerOnVM"
type NodeMigrationProgress struct {
	// name is the Kubernetes Node name. List merge key.
	// +required
	Name string `json:"name"`
	// phase is the node-local relocation state.
	// +kubebuilder:validation:Enum=Pending;Preparing;Migrating;WaitingForNode;Succeeded;Failed
	// +required
	Phase NodeMigrationPhase `json:"phase"`
	// requestedMode is the explicit mode in force when this node started.
	// +kubebuilder:validation:Enum=Hot;Cold
	// +optional
	RequestedMode VMotionMode `json:"requestedMode,omitempty"`
	// observedMode is the relocation mode observed for this node: Hot or Cold.
	// +kubebuilder:validation:Enum=Hot;Cold
	// +optional
	ObservedMode VMotionMode `json:"observedMode,omitempty"`
	// instanceUUID is the legacy wire name for the VM BIOS UUID. It does not mean
	// vSphere config.instanceUuid. Use vmBIOSUUID for new status; if both are set,
	// they must contain the same BIOS UUID.
	// Deprecated: retained for wire compatibility; use vmBIOSUUID.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	InstanceUUID string `json:"instanceUUID,omitempty"`
	// vmBIOSUUID is the VM BIOS UUID identity, distinct from vSphere instanceUUID.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	VMBIOSUUID string `json:"vmBIOSUUID,omitempty"`
	// nodeUID binds this progress record to the Kubernetes Node object.
	// +optional
	NodeUID types.UID `json:"nodeUID,omitempty"`
	// providerID is the Node spec.providerID captured for the frozen identity.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	ProviderID string `json:"providerID,omitempty"`
	// originalUnschedulable records the pre-migration scheduling state.
	// Pointer presence distinguishes an unrecorded state from false.
	// +optional
	OriginalUnschedulable *bool `json:"originalUnschedulable,omitempty"`
	// targetFailureDomain is the spec.failureDomains[].name chosen for placement.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	TargetFailureDomain string `json:"targetFailureDomain,omitempty"`
	// targetHost is the frozen destination host identity, if selected.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	TargetHost string `json:"targetHost,omitempty"`
	// targetResourcePool is the frozen destination resource-pool path.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	TargetResourcePool string `json:"targetResourcePool,omitempty"`
	// targetDatastore is the frozen destination datastore path.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	TargetDatastore string `json:"targetDatastore,omitempty"`
	// targetNetwork is the frozen destination network identity.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	TargetNetwork string `json:"targetNetwork,omitempty"`
	// sourceInventoryPath is the VM path before relocation.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	SourceInventoryPath string `json:"sourceInventoryPath,omitempty"`
	// targetInventoryPath is the VM path after relocation.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	TargetInventoryPath string `json:"targetInventoryPath,omitempty"`
	// shutdownOutcome records graceful shutdown separately from later operations.
	// +optional
	ShutdownOutcome *NodeMigrationOperationOutcome `json:"shutdownOutcome,omitempty"`
	// relocationOutcome remains intact when a later power-on is recorded.
	// +optional
	RelocationOutcome *NodeMigrationOperationOutcome `json:"relocationOutcome,omitempty"`
	// powerOnOutcome records destination power-on separately from relocation.
	// +optional
	PowerOnOutcome *NodeMigrationOperationOutcome `json:"powerOnOutcome,omitempty"`
	// message is a short human-readable explanation of phase or failure.
	// +kubebuilder:validation:MaxLength=1024
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
	// (named SingletonName) that the operator will act on.
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
