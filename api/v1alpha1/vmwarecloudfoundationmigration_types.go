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
	"k8s.io/apimachinery/pkg/util/intstr"
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
	// +listType=atomic
	FailureDomains []configv1.VSpherePlatformFailureDomainSpec `json:"failureDomains"`

	// image controls RHCOS OVA resolution and import into destination vCenter.
	// When set, the operator downloads and imports the OVA as a VM template
	// for each failure domain and populates topology.template automatically.
	// When omitted, topology.template must be set manually in each failure domain.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// NodeMigration controls how nodes move to the target vCenter.
	// When omitted, the operator keeps the current MachineSet/CPMS replacement path.
	// +optional
	NodeMigration *NodeMigrationSpec `json:"nodeMigration,omitempty"`
}

// DiskProvisioningMode defines the disk provisioning type for imported VM templates.
type DiskProvisioningMode string

const (
	// DiskProvisioningModeThin allocates storage on demand as blocks are written.
	DiskProvisioningModeThin DiskProvisioningMode = "thin"
	// DiskProvisioningModeThick pre-allocates all storage at creation time.
	DiskProvisioningModeThick DiskProvisioningMode = "thick"
	// DiskProvisioningModeEagerZeroedThick pre-allocates all storage and zeroes blocks at creation time.
	DiskProvisioningModeEagerZeroedThick DiskProvisioningMode = "eagerZeroedThick"
)

// ImageSpec controls RHCOS OVA import behavior.
type ImageSpec struct {
	// ovaUrl is a direct URL to the RHCOS OVA file. The URL must use https:// and
	// end in .ova (an optional query string is allowed for proxy tokens or
	// integrity digests appended by stream metadata tooling). When set, the operator
	// downloads from this URL instead of resolving via the coreos-bootimages
	// ConfigMap delivered by CVO.
	// Required for air-gapped environments: point to an internal HTTPS mirror.
	// +optional
	// +kubebuilder:validation:Pattern=`^https://.*\.ova(\?.*)?$`
	OVAUrl string `json:"ovaUrl,omitempty"`

	// diskProvisioning controls the VMDK disk provisioning type when importing
	// the OVA (thin, thick, eagerZeroedThick). Matches the installer's behavior.
	// When omitted, vSphere defaults to the provisioning type specified in the
	// OVF descriptor.
	// +optional
	// +kubebuilder:validation:Enum=thin;thick;eagerZeroedThick
	DiskProvisioning DiskProvisioningMode `json:"diskProvisioning,omitempty"`
}

// NodeMigrationStrategy defines the engine type used to move nodes to the
// target vCenter.
type NodeMigrationStrategy string

const (
	// NodeMigrationStrategyVMotion uses vMotion (Hot or Cold) to relocate VMs.
	NodeMigrationStrategyVMotion NodeMigrationStrategy = "VMotion"
	// NodeMigrationStrategyRecreate uses a Day-2 recreate (MachineSet) path.
	NodeMigrationStrategyRecreate NodeMigrationStrategy = "Recreate"
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

// NodeMigrationRole identifies the node role a migration setting applies to.
type NodeMigrationRole string

const (
	// NodeMigrationRoleControlPlane is the control-plane role.
	NodeMigrationRoleControlPlane NodeMigrationRole = "ControlPlane"
	// NodeMigrationRoleWorker is the worker role.
	NodeMigrationRoleWorker NodeMigrationRole = "Worker"
)

// NodeMigrationSpec controls how nodes move to the target vCenter.
type NodeMigrationSpec struct {
	// Strategy defines the engine type: VMotion (Hot/Cold) or Recreate (Day-2).
	// The vmotion field applies only when strategy is VMotion.
	// +kubebuilder:validation:Enum=VMotion;Recreate
	// +kubebuilder:default=VMotion
	// +optional
	Strategy NodeMigrationStrategy `json:"strategy,omitempty"`

	// VMotion is vMotion-specific behavior. Ignored unless strategy is VMotion.
	// +optional
	VMotion *VMotionSpec `json:"vmotion,omitempty"`

	// Workers are worker-node rolling limits.
	// +optional
	Workers *RoleMigrationSpec `json:"workers,omitempty"`

	// ControlPlane is control-plane rolling limits.
	// maxUnavailable is validated to 1 for this role.
	// +optional
	ControlPlane *RoleMigrationSpec `json:"controlPlane,omitempty"`
}

// VMotionSpec is vMotion-specific migration behavior.
type VMotionSpec struct {
	// Mode selects live vs cold relocation.
	// Auto tries Hot and falls back to Cold.
	// +kubebuilder:validation:Enum=Auto;Hot;Cold
	// +kubebuilder:default=Auto
	// +optional
	Mode VMotionMode `json:"mode,omitempty"`
}

// RoleMigrationSpec holds rolling limits for one node role.
type RoleMigrationSpec struct {
	// Mode overrides spec.nodeMigration.vmotion.mode for this role.
	// +kubebuilder:validation:Enum=Auto;Hot;Cold
	// +optional
	Mode *VMotionMode `json:"mode,omitempty"`
	// MaxUnavailable is a rolling window (count or percent of this role).
	// Control plane is validated to 1.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
}

// VmwareCloudFoundationMigrationStatus defines the observed state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationStatus struct {
	// conditions represent the current state of the migration.
	// Known conditions are:
	// - Accepted: admission gate; True is normal for the single reconciled instance (cluster),
	//   while False indicates an unsupported object name.
	// - InfrastructurePrepared: preflight validation and migration path selection.
	// - DestinationInitialized: destination vCenter folders and tags created.
	// - DestinationImageImported: RHCOS OVA imported as a VM template on destination vCenter.
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

	// completionTime is when the migration completed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// image reports the RHCOS OVA import state.
	// +optional
	Image *ImageStatus `json:"image,omitempty"`

	// NodeMigration reports node-migration progress.
	// +optional
	NodeMigration *NodeMigrationStatus `json:"nodeMigration,omitempty"`
}

// ImageURLSource describes how the resolved OVA URL was obtained.
type ImageURLSource string

const (
	// ImageURLSourceUser indicates the OVA URL was user-specified in spec.image.ovaUrl.
	ImageURLSourceUser ImageURLSource = "user"
	// ImageURLSourceAuto indicates the OVA URL was auto-resolved from stream metadata.
	ImageURLSourceAuto ImageURLSource = "auto"
)

// ImageStatus reports the RHCOS OVA import progress and results.
type ImageStatus struct {
	// resolvedOVAUrl is the URL from which the OVA was (or will be) downloaded.
	// +optional
	ResolvedOVAUrl string `json:"resolvedOVAUrl,omitempty"`

	// resolvedSHA256 is the expected sha256 digest of the OVA file, when
	// resolved from stream metadata. Empty for user-provided URLs.
	// +optional
	ResolvedSHA256 string `json:"resolvedSHA256,omitempty"`

	// importedTemplates maps failure domain names to the inventory paths
	// of imported VM templates.
	// +optional
	ImportedTemplates map[string]string `json:"importedTemplates,omitempty"`

	// operatorImportedTemplates records the OVA URL each failure domain's
	// template was imported from by the operator. It is populated only for
	// operator imports; user-pre-configured templates are not recorded here.
	// Used to detect a changed OVA URL and re-import only operator-managed
	// templates without touching user-provided ones.
	// +optional
	OperatorImportedTemplates map[string]string `json:"operatorImportedTemplates,omitempty"`

	// urlSource records how resolvedOVAUrl was populated: "" (unresolved / no opinion),
	// "user" (user-specified), or "auto" (auto-resolved). The zero value ("")
	// indicates no opinion or unresolved. Used to tell a deliberate user-clear
	// of spec.image.ovaUrl apart from an empty auto-resolution when deciding
	// whether to clear resolvedOVAUrl.
	// +optional
	// +kubebuilder:validation:Enum="";user;auto
	URLSource ImageURLSource `json:"urlSource,omitempty"`
}

// NodeMigrationStatus reports node-migration progress.
type NodeMigrationStatus struct {
	// Strategy is the engine selected when WorkloadMigrated started.
	// +kubebuilder:validation:Enum=VMotion;Recreate
	// +optional
	Strategy NodeMigrationStrategy `json:"strategy,omitempty"`

	// Progress is a human-readable rollup for kubectl get,
	// for example "control plane 2/3, workers 4/10".
	// +optional
	Progress string `json:"progress,omitempty"`

	// ControlPlane is the rollup for control-plane nodes.
	// +optional
	ControlPlane *RoleMigrationStatus `json:"controlPlane,omitempty"`

	// Workers is the rollup for worker nodes.
	// +optional
	Workers *RoleMigrationStatus `json:"workers,omitempty"`

	// Nodes is per-node progress keyed by Kubernetes node name.
	// +optional
	// +listType=map
	// +listMapKey=name
	Nodes []NodeMigrationProgress `json:"nodes,omitempty"`
}

// RoleMigrationStatus rolls up one role (control plane or workers).
type RoleMigrationStatus struct {
	// RequestedMode is the vMotion mode from spec for this role
	// (inherited from spec.nodeMigration.vmotion.mode when the role omits mode).
	// +kubebuilder:validation:Enum=Auto;Hot;Cold
	// +optional
	RequestedMode VMotionMode `json:"requestedMode,omitempty"`
	// Total is the number of discovered nodes in this role.
	// +optional
	Total int32 `json:"total,omitempty"`
	// Pending is nodes not yet started.
	// +optional
	Pending int32 `json:"pending,omitempty"`
	// InProgress is nodes in Preparing, Migrating, or WaitingForNode.
	// +optional
	InProgress int32 `json:"inProgress,omitempty"`
	// Succeeded is nodes in Succeeded.
	// +optional
	Succeeded int32 `json:"succeeded,omitempty"`
	// Failed is nodes in Failed.
	// +optional
	Failed int32 `json:"failed,omitempty"`
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
	// Name is the Kubernetes Node name. List merge key.
	// +required
	Name string `json:"name"`
	// Role is ControlPlane or Worker.
	// +kubebuilder:validation:Enum=ControlPlane;Worker
	// +required
	Role NodeMigrationRole `json:"role"`
	// Phase is the node-local relocation state.
	// +kubebuilder:validation:Enum=Pending;Preparing;Migrating;WaitingForNode;Succeeded;Failed
	// +required
	Phase NodeMigrationPhase `json:"phase"`
	// RequestedMode is the mode in force when this node started.
	// Frozen for in-flight nodes if spec changes later.
	// +kubebuilder:validation:Enum=Auto;Hot;Cold
	// +optional
	RequestedMode VMotionMode `json:"requestedMode,omitempty"`
	// ObservedMode is the relocation that actually ran: Hot or Cold.
	// Empty until RelocateVM starts. Auto resolves here, never in spec.
	// +kubebuilder:validation:Enum=Hot;Cold
	// +optional
	ObservedMode VMotionMode `json:"observedMode,omitempty"`
	// InstanceUUID is the VM BIOS UUID (node spec.providerID, without vsphere://).
	// Stable identity across vCenters; MoRef is not.
	// +optional
	InstanceUUID string `json:"instanceUUID,omitempty"`
	// TargetFailureDomain is the spec.failureDomains[].name chosen for placement.
	// +optional
	TargetFailureDomain string `json:"targetFailureDomain,omitempty"`
	// SourceInventoryPath is the VM path before relocation.
	// +optional
	SourceInventoryPath string `json:"sourceInventoryPath,omitempty"`
	// TargetInventoryPath is the VM path after relocation.
	// +optional
	TargetInventoryPath string `json:"targetInventoryPath,omitempty"`
	// Message is a short human-readable explanation of phase or failure.
	// +optional
	Message string `json:"message,omitempty"`
	// LastTransitionTime is when phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// StartedAt is when Preparing began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when phase became Succeeded or Failed.
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

	// ConditionDestinationImageImported indicates the RHCOS OVA has been
	// downloaded and imported as a VM template on all target vCenters.
	// When spec.image is nil, this condition is immediately set to True.
	ConditionDestinationImageImported = "DestinationImageImported"

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

func init() {
	SchemeBuilder.Register(&VmwareCloudFoundationMigration{}, &VmwareCloudFoundationMigrationList{})
}
