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

	// MigrationStatePlanning indicates the cluster administrator has requested migration planning.
	MigrationStatePlanning MigrationState = "Planning"

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

// FailureDomainMapping maps one source failure domain to one destination failure domain.
type FailureDomainMapping struct {
	// source is the name of the failure domain in the cluster's Infrastructure
	// resource that this entry migrates workloads from.
	// This field is required and must be a non-empty string.
	// Must be between 1 and 256 characters in length.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Source string `json:"source"`

	// destination is the name of the entry in spec.failureDomains that receives
	// the workloads migrated from source.
	// This field is required and must be a non-empty string.
	// Must be between 1 and 256 characters in length.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Destination string `json:"destination"`
}

// VmwareCloudFoundationMigrationSpec defines the desired state of VmwareCloudFoundationMigration.
// +kubebuilder:validation:XValidation:rule="!has(self.failureDomainMappings) || self.failureDomainMappings.all(m, m.destination in self.failureDomains.map(fd, fd.name))",message="every failureDomainMappings.destination must match a failureDomains[].name"
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || (has(self.migrateStorage) && self.migrateStorage)",message="storage can only be set when migrateStorage is true"
type VmwareCloudFoundationMigrationSpec struct {
	// state controls the workflow: Pending, Planning, Running, Paused.
	// Planning runs non-disruptive checks; Running proceeds when the plan has no blockers.
	// When omitted, defaults to Pending.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Planning;Running;Paused
	// +kubebuilder:default=Pending
	State MigrationState `json:"state,omitempty"`

	// migrateStorage indicates whether vSphere CSI volumes are migrated along
	// with compute nodes. When false, vSphere CSI persistent volumes block the
	// migration during preflight.
	// When omitted, defaults to false.
	// +optional
	// +kubebuilder:default=false
	MigrateStorage bool `json:"migrateStorage,omitempty"`

	// targetVCenterCredentialsSecret references the secret containing target vCenter credentials.
	// The secret must contain keys: {target-vcenter-fqdn}.username and {target-vcenter-fqdn}.password.
	// +required
	TargetVCenterCredentialsSecret SecretReference `json:"targetVCenterCredentialsSecret"`

	// failureDomains defines failure domains for the target vCenter. The embedded
	// element type intentionally tracks config.openshift.io/v1 VSpherePlatformFailureDomainSpec
	// (including Name, Region, Zone, Server, and Topology) to align with OpenShift API
	// failure domain semantics; the schema is re-inlined on "make manifests" after
	// dependency bumps.  At most 64 failure domains are allowed.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:XValidation:rule="self.all(fd, has(fd.topology.template) && fd.topology.template != '')",message="topology.template is required for every failure domain"
	// +listType=atomic
	FailureDomains []configv1.VSpherePlatformFailureDomainSpec `json:"failureDomains"`

	// failureDomainMappings maps source failure domains to destination failure domains by name.
	// When omitted, exactly one source failure domain and one destination failure domain must exist, and they are mapped to each other implicitly.
	// When used, must contain between 1 and 64 items.
	// Each source and each destination must be unique; duplicate sources are also
	// rejected by the list's map semantics.
	// +optional
	// +listType=map
	// +listMapKey=source
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:XValidation:rule="self.all(m, self.exists_one(n, n.destination == m.destination))",message="destination must be unique across mappings"
	FailureDomainMappings []FailureDomainMapping `json:"failureDomainMappings,omitempty"`

	// nodeMigration controls how nodes move to the target vCenter.
	// When omitted, the operator keeps the current MachineSet/CPMS replacement path.
	// When set, both roles select the same migration engine.
	// +optional
	NodeMigration *NodeMigrationSpec `json:"nodeMigration,omitempty"`

	// storage configures CNS volume data migration. It can only be set when
	// migrateStorage is true.
	// +optional
	Storage *StorageMigrationSpec `json:"storage,omitempty"`
}

// NodeMigrationType is the cluster-wide engine used to move nodes to the
// target vCenter. Allowed values are `VMotion` (relocate existing VMs) and
// `Recreate` (replace VMs through MachineSets/CPMS). It is the tagged-union
// discriminator for NodeMigrationSpec.
// +kubebuilder:validation:Enum=VMotion;Recreate
type NodeMigrationType string

const (
	// NodeMigrationTypeVMotion relocates existing VMs (Hot or Cold).
	NodeMigrationTypeVMotion NodeMigrationType = "VMotion"
	// NodeMigrationTypeRecreate replaces VMs via MachineSets/CPMS.
	NodeMigrationTypeRecreate NodeMigrationType = "Recreate"
)

// VMotionMode selects the vSphere relocation mode. Allowed values are `Hot`
// (live relocation) and `Cold` (relocation after graceful shutdown).
// +kubebuilder:validation:Enum=Hot;Cold
type VMotionMode string

const (
	// VMotionModeHot performs live (hot) relocation.
	VMotionModeHot VMotionMode = "Hot"
	// VMotionModeCold performs cold relocation (powered off).
	VMotionModeCold VMotionMode = "Cold"
)

// NodeMigrationSpec controls how nodes move to the target vCenter. Both roles
// are required and must select the same engine: `VMotion` or `Recreate`. For
// VMotion, both roles must explicitly select the same mode, `Hot` or `Cold`.
// Control-plane maxUnavailable must always be 1; VMotion worker maxUnavailable
// must also be 1.
// +kubebuilder:validation:XValidation:rule="self.controlPlane.maxUnavailable == 1",message="controlPlane.maxUnavailable must be 1"
// +kubebuilder:validation:XValidation:rule="self.workers.type == self.controlPlane.type",message="workers and controlPlane must use the same migration type"
// +kubebuilder:validation:XValidation:rule="self.workers.type != 'VMotion' || self.controlPlane.type != 'VMotion' || !has(self.workers.vmotion) || !has(self.controlPlane.vmotion) || !has(self.workers.vmotion.mode) || !has(self.controlPlane.vmotion.mode) || self.workers.vmotion.mode == self.controlPlane.vmotion.mode",message="workers and controlPlane must use the same VMotion mode"
// +kubebuilder:validation:XValidation:rule="self.workers.type != 'VMotion' || !has(self.workers.maxUnavailable) || self.workers.maxUnavailable == 1",message="VMotion workers.maxUnavailable must be 1"
type NodeMigrationSpec struct {
	// workers defines the migration engine and rolling limit for worker nodes.
	// This field is required.
	// +required
	Workers RoleMigrationSpec `json:"workers"`

	// controlPlane defines the migration engine and rolling limit for
	// control-plane nodes. This field is required and maxUnavailable must be 1.
	// +required
	ControlPlane RoleMigrationSpec `json:"controlPlane"`

	// recoveryRequest asks the operator to recheck one blocked action. When omitted,
	// no recovery recheck is requested. It is an action-bound request, not evidence
	// that the action is inactive or authority to replay it. Runtime validation and
	// authorization are not performed by schema.
	// +optional
	RecoveryRequest *NodeMigrationRecoveryRequest `json:"recoveryRequest,omitempty"`
}

// RoleMigrationSpec holds the migration engine selection and rolling limit for
// one node role. `type` is required and accepts `VMotion` or `Recreate`. It is
// a tagged-union discriminator: `vmotion` is required for `VMotion` and
// forbidden for `Recreate`.
// +union
// +kubebuilder:validation:XValidation:rule="self.type == 'VMotion' ? has(self.vmotion) : !has(self.vmotion)",message="vmotion is required when type is VMotion, and forbidden otherwise"
type RoleMigrationSpec struct {
	// type is the migration engine for this role: `VMotion` relocates existing
	// VMs, while `Recreate` replaces VMs through MachineSets/CPMS. Required.
	// +unionDiscriminator
	// +required
	Type NodeMigrationType `json:"type"`

	// vmotion contains VMotion-specific behavior. Required when type is
	// `VMotion` and forbidden when type is `Recreate`.
	// +unionMember
	// +optional
	VMotion *VMotionSpec `json:"vmotion,omitempty"`

	// maxUnavailable is the maximum number of nodes in this role that can migrate
	// simultaneously. It must be an integer from 1 through 32767 and defaults to
	// 1 when omitted; percentage strings are not accepted. The control-plane value
	// must be 1, and VMotion worker values must be 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32767
	// +kubebuilder:default=1
	// +optional
	MaxUnavailable int32 `json:"maxUnavailable,omitempty"`
}

// VMotionSpec is VMotion-specific migration behavior. `mode` is required and
// accepts `Hot` or `Cold`; the operator does not fall back to another mode.
type VMotionSpec struct {
	// mode explicitly selects `Hot` (live relocation) or `Cold` (graceful
	// shutdown before relocation). There is no default or fallback mode.
	// +required
	Mode VMotionMode `json:"mode"`
}

// NodeMigrationRecoveryRequest is an action-bound request for runtime rechecking.
// requestID, executionID, and actionID must each use the canonical UUID form
// (8-4-4-4-12 hexadecimal digits), for example
// `11111111-1111-4111-8111-111111111111`. They identify the request target but
// do not prove safe recovery.
type NodeMigrationRecoveryRequest struct {
	// requestID uniquely identifies this acknowledgement request. Required UUID.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:example="11111111-1111-4111-8111-111111111111"
	// +required
	RequestID string `json:"requestID"`
	// executionID identifies the frozen execution being rechecked. Required UUID.
	// +kubebuilder:validation:Format=uuid
	// +required
	ExecutionID string `json:"executionID"`
	// actionID identifies the specific blocked action being rechecked. Required UUID.
	// +kubebuilder:validation:Format=uuid
	// +required
	ActionID string `json:"actionID"`

	// reason is an optional administrator note of at most 1024 characters.
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
	// - Accepted: admission gate; True is normal for the single reconciled instance
	//   (openshift-vcf-migration/cluster), while False indicates an unsupported
	//   object name or namespace.
	// - InfrastructurePrepared: preflight validation and migration path selection.
	// - StoragePlanned: storage inventory and target resolution have no blockers.
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

	// failureDomainMappings is the effective source-to-destination failure domain
	// mapping resolved at preflight. When spec.failureDomainMappings is omitted it
	// holds the single inferred pair. Until the migration starts, each preflight
	// run replaces it with the mapping resolved for the current spec. Once the
	// migration has started the controller treats this field as write-once.
	// +optional
	// +listType=atomic
	FailureDomainMappings []FailureDomainMapping `json:"failureDomainMappings,omitempty"`

	// workerMachineSetNames records the destination worker MachineSet names
	// generated for the recorded mappings. Persisted before source cleanup so
	// retries that re-enter workload migration after the source MachineSets
	// were deleted still require these MachineSets to exist and be ready;
	// mappings whose source failure domain had no MachineSets contribute no
	// names and intentionally stay empty.
	//
	// Maximum items: 256.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=256
	WorkerMachineSetNames []string `json:"workerMachineSetNames,omitempty"`

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

// NodeMigrationStatus reports node-migration progress. UUID-formatted IDs use
// canonical 8-4-4-4-12 hexadecimal form (for example,
// `11111111-1111-4111-8111-111111111111`).
type NodeMigrationStatus struct {
	// type is the engine selected when WorkloadMigrated started. When set to
	// `VMotion`, existing VMs are relocated; when set to `Recreate`, VMs are
	// replaced through MachineSets/CPMS. Required.
	// +required
	Type NodeMigrationType `json:"type"`

	// progress is an optional human-readable rollup for kubectl get, for example
	// "control plane 2/3, workers 4/10". It is at most 256 characters.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Progress string `json:"progress,omitempty"`

	// controlPlane is control-plane progress: counts plus per-node entries.
	// +optional
	ControlPlane *RoleMigrationStatus `json:"controlPlane,omitempty"`

	// workers is worker progress: counts plus per-node entries.
	// +optional
	Workers *RoleMigrationStatus `json:"workers,omitempty"`

	// executionID is the UUID stable across pause-related generation changes;
	// generation and observedGeneration are not operation identifiers.
	// +kubebuilder:validation:Format=uuid
	// +optional
	ExecutionID string `json:"executionID,omitempty"`

	// migrationUID binds this execution record to the originating CR UID. When
	// set, it must not be empty.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +optional
	MigrationUID types.UID `json:"migrationUID,omitempty"`

	// planFingerprint identifies the sanitized frozen plan, excluding credentials
	// and other secret material. It must be exactly 64 lowercase hexadecimal
	// characters (a SHA-256 digest).
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
	// sourceVCenter identifies the source vCenter endpoint. Required, non-empty,
	// and at most 512 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +required
	SourceVCenter string `json:"sourceVCenter"`
	// sourceDatacenter identifies the source datacenter. Required, non-empty,
	// and at most 253 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	SourceDatacenter string `json:"sourceDatacenter"`
	// destinationVCenter identifies the destination vCenter endpoint. Required,
	// non-empty, and at most 512 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +required
	DestinationVCenter string `json:"destinationVCenter"`
	// mode is the explicitly selected mode frozen for this execution. When set to
	// `Hot`, VMs are live-relocated; when set to `Cold`, they are shut down
	// gracefully before relocation. Required.
	// +required
	Mode VMotionMode `json:"mode"`
}

// NodeMigrationActionKind identifies one mutating VM operation. Allowed values
// are `ShutdownGuest`, `RelocateVM`, and `PowerOnVM`.
// +kubebuilder:validation:Enum=ShutdownGuest;RelocateVM;PowerOnVM
type NodeMigrationActionKind string

const (
	// NodeMigrationActionShutdownGuest requests graceful guest shutdown.
	NodeMigrationActionShutdownGuest NodeMigrationActionKind = "ShutdownGuest"
	// NodeMigrationActionRelocateVM submits VM relocation.
	NodeMigrationActionRelocateVM NodeMigrationActionKind = "RelocateVM"
	// NodeMigrationActionPowerOnVM submits VM power-on.
	NodeMigrationActionPowerOnVM NodeMigrationActionKind = "PowerOnVM"
)

// NodeMigrationSubmissionState reports durable submission knowledge. Allowed
// values are `Claimed` (submission may have begun), `Submitted` (submission was
// acknowledged), and `Unknown` (the outcome of submission cannot be established).
// +kubebuilder:validation:Enum=Claimed;Submitted;Unknown
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
// Allowed values are `NotObserved`, `Running`, `Succeeded`, `Failed`, and
// `Unknown`.
// +kubebuilder:validation:Enum=NotObserved;Running;Succeeded;Failed;Unknown
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

// NodeMigrationOutcomeState is the bounded result of one completed or uncertain
// operation. Allowed values are `Succeeded`, `Failed`, and `Unknown`.
// +kubebuilder:validation:Enum=Succeeded;Failed;Unknown
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
	// ownerVCenter scopes taskID to one vCenter. Required, non-empty, and at most
	// 512 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +required
	OwnerVCenter string `json:"ownerVCenter"`
	// taskID is the vCenter-local task identifier. Required, non-empty, and at
	// most 512 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +required
	TaskID string `json:"taskID"`
}

// NodeMigrationAction is the single durable mutating-action claim. actionID and
// executionID are UUIDs in canonical 8-4-4-4-12 hexadecimal form. A task
// reference must use the same ownerVCenter, and `ShutdownGuest` cannot have a
// task reference.
// +kubebuilder:validation:XValidation:rule="!has(self.taskReference) || self.taskReference.ownerVCenter == self.ownerVCenter",message="taskReference.ownerVCenter must match ownerVCenter"
// +kubebuilder:validation:XValidation:rule="self.kind != 'ShutdownGuest' || !has(self.taskReference)",message="ShutdownGuest does not have a task reference"
type NodeMigrationAction struct {
	// actionID is the UUID stable for this one mutating action.
	// +kubebuilder:validation:Format=uuid
	// +required
	ActionID string `json:"actionID"`

	// executionID binds this action to the frozen execution; it is a UUID.
	// +kubebuilder:validation:Format=uuid
	// +required
	ExecutionID string `json:"executionID"`
	// kind is the mutating operation claimed. When set to `ShutdownGuest`, the
	// guest is asked to shut down; when set to `RelocateVM`, the VM is relocated;
	// when set to `PowerOnVM`, the VM is powered on.
	// +required
	Kind NodeMigrationActionKind `json:"kind"`
	// nodeName identifies the Kubernetes Node. Required, non-empty, and at most
	// 253 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	NodeName string `json:"nodeName"`
	// nodeUID is the non-empty Kubernetes UID of the selected Node object.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +required
	NodeUID types.UID `json:"nodeUID"`
	// vmBIOSUUID is the non-empty VM BIOS UUID, not the vSphere instanceUUID. It
	// is at most 64 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +required
	VMBIOSUUID string `json:"vmBIOSUUID"`
	// ownerVCenter is the non-empty vCenter in which this action/task is
	// interpreted; it is at most 512 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +required
	OwnerVCenter string `json:"ownerVCenter"`
	// operationIdentity is a digest of sanitized, non-secret intended-operation
	// inputs. It must be exactly 64 lowercase hexadecimal characters.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +required
	OperationIdentity string `json:"operationIdentity"`
	// submissionState reports submission status. When set to `Claimed`, submission
	// may have begun; `Submitted` means it was acknowledged; `Unknown` means its
	// outcome cannot be established.
	// +required
	SubmissionState NodeMigrationSubmissionState `json:"submissionState"`
	// observationState records the latest action observation. `NotObserved` means
	// no authoritative observation is recorded; `Running` means the action is in
	// progress; `Succeeded` means the action was observed successful; `Failed`
	// means it was observed unsuccessful; `Unknown` means the outcome cannot be
	// established.
	// +required
	ObservationState NodeMigrationObservationState `json:"observationState"`
	// taskReference is optional; `ShutdownGuest` has no task reference. When
	// present, its ownerVCenter must match this action's ownerVCenter.
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
	// reason is an optional observation/outcome detail of at most 1024 characters.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
}

// NodeMigrationOperationOutcome records one operation without overwriting
// another operation's result. actionID is a UUID; kind is `ShutdownGuest`,
// `RelocateVM`, or `PowerOnVM`; state is `Succeeded`, `Failed`, or `Unknown`.
// A task reference, when present, must use the same ownerVCenter; `ShutdownGuest`
// cannot have a task reference.
// +kubebuilder:validation:XValidation:rule="!has(self.taskReference) || self.taskReference.ownerVCenter == self.ownerVCenter",message="taskReference.ownerVCenter must match ownerVCenter"
// +kubebuilder:validation:XValidation:rule="self.kind != 'ShutdownGuest' || !has(self.taskReference)",message="ShutdownGuest does not have a task reference"
type NodeMigrationOperationOutcome struct {
	// actionID identifies the action that produced this outcome; it is a UUID.
	// +kubebuilder:validation:Format=uuid
	// +required
	ActionID string `json:"actionID"`

	// kind identifies the operation represented by this outcome. When set to
	// `ShutdownGuest`, the guest was asked to shut down; when set to `RelocateVM`,
	// the VM was relocated; when set to `PowerOnVM`, the VM was powered on.
	// +required
	Kind NodeMigrationActionKind `json:"kind"`
	// state is the bounded result and is observational, not replay authority. When
	// set to `Succeeded`, the operation was observed successful; `Failed` means it
	// was observed unsuccessful; `Unknown` means its outcome is unresolved.
	// +required
	State NodeMigrationOutcomeState `json:"state"`
	// ownerVCenter scopes any task reference. Required, non-empty, and at most
	// 512 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +required
	OwnerVCenter string `json:"ownerVCenter"`
	// taskReference is optional and scoped to ownerVCenter. It is forbidden for
	// `ShutdownGuest`.
	// +optional
	TaskReference *NodeMigrationTaskReference `json:"taskReference,omitempty"`
	// reason is an optional explanation of at most 1024 characters.
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
// executionID and actionID use canonical UUID format.
type NodeMigrationIntervention struct {
	// executionID identifies the affected frozen execution; it is a UUID.
	// +kubebuilder:validation:Format=uuid
	// +required
	ExecutionID string `json:"executionID"`
	// actionID identifies the affected action; it is a UUID.
	// https://www.rfc-editor.org/rfc/rfc9562.txt
	// +kubebuilder:validation:Format=uuid
	// +required
	ActionID string `json:"actionID"`
	// nodeName identifies the affected Node. Required, non-empty, and at most
	// 253 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	NodeName string `json:"nodeName"`
	// reason is a required, non-empty explanation of at most 1024
	// characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +required
	Reason string `json:"reason"`
	// since is when intervention became necessary.
	// +required
	Since metav1.Time `json:"since"`
}

// NodeMigrationRecoveryAcknowledgement records that a request was consumed.
// requestID, executionID, and actionID are UUIDs in canonical format.
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

// NodeMigrationCleanupPhase is bounded progress through source cleanup. Allowed
// values are `Pending`, `RemovingSourceReferences`, `VerifyingReadiness`,
// `Completed`, and `Blocked`.
// +kubebuilder:validation:Enum=Pending;RemovingSourceReferences;VerifyingReadiness;Completed;Blocked
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
	// phase is the current cleanup observation. `Pending` means cleanup has not
	// started; `RemovingSourceReferences` means source references are being
	// removed; `VerifyingReadiness` means readiness is being checked; `Completed`
	// means cleanup finished; `Blocked` means it cannot proceed.
	// +required
	Phase NodeMigrationCleanupPhase `json:"phase"`
	// lastTransitionTime is when phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// reason is an optional explanation of at most 1024 characters.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
}

// RoleMigrationStatus is progress for one role. Counts are derived from nodes;
// the nodes list is limited to 500 entries.
type RoleMigrationStatus struct {
	// requestedMode is the explicit vMotion mode selected for this role. When set
	// to `Hot`, VMs are live-relocated; when set to `Cold`, they are shut down
	// gracefully before relocation.
	// +optional
	RequestedMode VMotionMode `json:"requestedMode,omitempty"`
	// observedMode is the relocation mode observed for this role. When set to
	// `Hot`, live relocation was observed; when set to `Cold`, relocation after
	// graceful shutdown was observed.
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
	// At most 500 entries are allowed.
	// +optional
	// +kubebuilder:validation:MaxItems=500
	// +listType=map
	// +listMapKey=name
	Nodes []NodeMigrationProgress `json:"nodes,omitempty"`
}

// NodeMigrationState is the coarse node-local migration state. Allowed values
// are `Pending`, `Preparing`, `Migrating`, `WaitingForNode`, `Succeeded`, and
// `Failed`.
// +kubebuilder:validation:Enum=Pending;Preparing;Migrating;WaitingForNode;Succeeded;Failed
type NodeMigrationState string

// NodeMigrationSchedulingState is the recorded pre-migration scheduling state.
// Allowed values are `Schedulable` and `Unschedulable`.
// +kubebuilder:validation:Enum=Schedulable;Unschedulable
type NodeMigrationSchedulingState string

const (
	// NodeMigrationSchedulingStateSchedulable means the node was schedulable before migration.
	NodeMigrationSchedulingStateSchedulable NodeMigrationSchedulingState = "Schedulable"
	// NodeMigrationSchedulingStateUnschedulable means the node was unschedulable before migration.
	NodeMigrationSchedulingStateUnschedulable NodeMigrationSchedulingState = "Unschedulable"
)

const (
	// NodeMigrationStatePending is discovered, not started.
	NodeMigrationStatePending NodeMigrationState = "Pending"
	// NodeMigrationStatePreparing covers pre-action checks and scheduling preparation;
	// it does not imply that only Cold mode drains or cordons.
	NodeMigrationStatePreparing NodeMigrationState = "Preparing"
	// NodeMigrationStateMigrating is RelocateVM_Task running.
	NodeMigrationStateMigrating NodeMigrationState = "Migrating"
	// NodeMigrationStateWaitingForNode is the VM on the target; kubelet not Ready yet.
	NodeMigrationStateWaitingForNode NodeMigrationState = "WaitingForNode"
	// NodeMigrationStateSucceeded means required action, identity, placement and
	// recovery verification completed; a stale Node Ready condition alone is insufficient.
	NodeMigrationStateSucceeded NodeMigrationState = "Succeeded"
	// NodeMigrationStateFailed is a terminal or currently-failed node.
	NodeMigrationStateFailed NodeMigrationState = "Failed"
)

// NodeMigrationProgress is observed state for a single node/VM. When any outcome
// is set, its kind must match the corresponding field: shutdownOutcome uses
// `ShutdownGuest`, relocationOutcome uses `RelocateVM`, and powerOnOutcome uses
// `PowerOnVM`.
// +kubebuilder:validation:XValidation:rule="!has(self.shutdownOutcome) || self.shutdownOutcome.kind == 'ShutdownGuest'",message="shutdownOutcome.kind must be ShutdownGuest"
// +kubebuilder:validation:XValidation:rule="!has(self.relocationOutcome) || self.relocationOutcome.kind == 'RelocateVM'",message="relocationOutcome.kind must be RelocateVM"
// +kubebuilder:validation:XValidation:rule="!has(self.powerOnOutcome) || self.powerOnOutcome.kind == 'PowerOnVM'",message="powerOnOutcome.kind must be PowerOnVM"
type NodeMigrationProgress struct {
	// name is the required, non-empty Kubernetes Node name and list merge key; it
	// is at most 253 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`
	// state is the node-local relocation state. `Pending` means not started;
	// `Preparing` means pre-action checks are running; `Migrating` means relocation
	// is running; `WaitingForNode` means the VM is on target but the node is not
	// Ready; `Succeeded` means node migration completed successfully; `Failed`
	// means node migration did not complete successfully.
	// +required
	State NodeMigrationState `json:"state"`
	// requestedMode is the explicit mode in force when this node started. When set
	// to `Hot`, the node was live-relocated; when set to `Cold`, it was shut down
	// gracefully before relocation.
	// +optional
	RequestedMode VMotionMode `json:"requestedMode,omitempty"`
	// observedMode is the relocation mode observed for this node. When set to
	// `Hot`, live relocation was observed; when set to `Cold`, relocation after
	// graceful shutdown was observed.
	// +optional
	ObservedMode VMotionMode `json:"observedMode,omitempty"`
	// vmBIOSUUID is the VM BIOS UUID identity, distinct from vSphere instanceUUID.
	// When set, it must be non-empty and at most 64 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +optional
	VMBIOSUUID string `json:"vmBIOSUUID,omitempty"`
	// nodeUID is the Kubernetes Node object UID, when recorded; it must not be
	// empty when set.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +optional
	NodeUID types.UID `json:"nodeUID,omitempty"`
	// providerID is the Node spec.providerID captured for the frozen identity.
	// When set, it must be non-empty and at most 512 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +optional
	ProviderID string `json:"providerID,omitempty"`
	// originalSchedulingState records whether the node was schedulable before
	// migration: `Schedulable` or `Unschedulable`.
	// +optional
	OriginalSchedulingState NodeMigrationSchedulingState `json:"originalSchedulingState,omitempty"`
	// targetFailureDomain is the spec.failureDomains[].name chosen for placement.
	// When set, it must be non-empty and at most 253 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	TargetFailureDomain string `json:"targetFailureDomain,omitempty"`
	// targetHost is the frozen destination host identity, if selected. When set,
	// it must be non-empty and at most 1024 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	TargetHost string `json:"targetHost,omitempty"`
	// targetResourcePool is the frozen destination resource-pool path. When set,
	// it must be non-empty and at most 1024 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	TargetResourcePool string `json:"targetResourcePool,omitempty"`
	// targetDatastore is the frozen destination datastore path. When set, it must
	// be non-empty and at most 1024 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	TargetDatastore string `json:"targetDatastore,omitempty"`
	// targetNetwork is the frozen destination network identity. When set, it must
	// be non-empty and at most 1024 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	TargetNetwork string `json:"targetNetwork,omitempty"`
	// sourceInventoryPath is the VM path before relocation. When set, it must be
	// non-empty and at most 1024 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	SourceInventoryPath string `json:"sourceInventoryPath,omitempty"`
	// targetInventoryPath is the VM path after relocation. When set, it must be
	// non-empty and at most 1024 characters.
	// +kubebuilder:validation:MinLength=1
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
	// message is an optional human-readable explanation of state or failure, at
	// most 1024 characters.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`
	// lastTransitionTime is when state last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// startedAt is when Preparing began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// completedAt is when state became Succeeded or Failed.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// VolumeDataMigrationStrategy selects how CNS volume data is migrated.
// CnsNative is reserved for future support and cannot currently be selected.
// +kubebuilder:validation:Enum=Auto;Rsync
type VolumeDataMigrationStrategy string

const (
	// VolumeDataMigrationStrategyAuto lets VMO choose the migration strategy.
	VolumeDataMigrationStrategyAuto VolumeDataMigrationStrategy = "Auto"

	// VolumeDataMigrationStrategyRsync copies volume data with rsync.
	VolumeDataMigrationStrategyRsync VolumeDataMigrationStrategy = "Rsync"

	// VolumeDataMigrationStrategyCnsNative reserves CNS native migration, which is currently unsupported.
	VolumeDataMigrationStrategyCnsNative VolumeDataMigrationStrategy = "CnsNative"
)

// StorageMigrationSpec configures storage for CNS volume migration.
type StorageMigrationSpec struct {
	// DataMigrationStrategy defines how VMO will move the data.
	// When omitted, defaults to Auto. CnsNative is not currently supported.
	// +optional
	// +kubebuilder:default=Auto
	DataMigrationStrategy VolumeDataMigrationStrategy `json:"dataMigrationStrategy,omitempty"`

	// StorageClassMapping maps source StorageClasses to target StorageClasses for
	// provisioning destination PVCs. An unmapped source uses the same StorageClass.
	// This allows in-tree or datastore-pinned source classes to use a suitable
	// vSphere CSI target class. Each source may appear only once; multiple sources
	// may share a target. Existing source PVCs and StorageClasses are not modified.
	// +optional
	// +listType=map
	// +listMapKey=source
	StorageClassMapping []StorageClassMapping `json:"storageClassMapping,omitempty"`
}

// StorageClassMapping maps a source StorageClass to a target StorageClass.
type StorageClassMapping struct {
	// Source is the name of the source PVC's StorageClass.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Source string `json:"source"`

	// Target is the name of the vSphere CSI StorageClass for destination PVCs.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Target string `json:"target"`
}

// Condition type constants for the migration workflow.
// The reconciler checks conditions in this order; if a condition is not True,
// it executes the work for that condition and returns with RequeueAfter.
const (
	// ConditionInfrastructurePrepared indicates preflight checks passed and the
	// migration path has been selected.
	ConditionInfrastructurePrepared = "InfrastructurePrepared"

	// ConditionStoragePlanned indicates storage inventory has no migration blockers.
	ConditionStoragePlanned = "StoragePlanned"

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
	// ReasonPlanning indicates non-disruptive checks are running.
	ReasonPlanning = "Planning"
	// ReasonPlanComplete indicates the plan is ready for customer review.
	ReasonPlanComplete = "PlanComplete"
	// ReasonPlanBlocked indicates storage blockers prevent migration.
	ReasonPlanBlocked = "PlanBlocked"
	// ReasonStorageExecutionUnsupported indicates storage planning is available but execution is not implemented.
	ReasonStorageExecutionUnsupported = "StorageExecutionUnsupported"
	// ReasonPlanningAfterStart indicates planning cannot restart a migration.
	ReasonPlanningAfterStart = "PlanningAfterStart"
	// ReasonNotConfigured indicates storage migration was not requested.
	ReasonNotConfigured = "NotConfigured"
	ReasonProgressing   = "Progressing"
	ReasonCompleted     = "Completed"
	ReasonFailed        = "Failed"
	// ReasonMappingRejected indicates spec.failureDomainMappings was changed
	// after the migration started with the recorded mapping. It is distinct from
	// ReasonFailed so condition handling does not have to guess which entries a
	// generic failure left behind.
	ReasonMappingRejected = "MappingRejected"
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
// +kubebuilder:validation:XValidation:rule="has(self.spec.failureDomainMappings) || size(self.spec.failureDomains) == 1 || (has(self.status) && has(self.status.failureDomainMappings) && size(self.status.failureDomainMappings) > 0)",message="failureDomainMappings is required when more than one failureDomain is defined until effective mappings are recorded"
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
