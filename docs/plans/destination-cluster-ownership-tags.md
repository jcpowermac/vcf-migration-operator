# Destination Cluster Ownership Tags

## Overview

Mirror the OpenShift installer’s cluster ownership tagging on the destination vCenter so destroy/cleanup and inventory ownership work after migration. This is separate from topology tags (`openshift-region` / `openshift-zone`).

## Installer Reference

Source: `openshift/installer` `pkg/infrastructure/vsphere/clusterapi/tags.go` (`createClusterTagID`) and `clusterapi.go` (PreProvision).

| Field | Value |
|---|---|
| Category name | `openshift-<infraID>` |
| Tag name | `<infraID>` |
| Description (both) | `Added by openshift-install do not remove` |
| Cardinality | `SINGLE` |
| Associable types | `urn:vim25:VirtualMachine`, `urn:vim25:ResourcePool`, `urn:vim25:Folder`, `urn:vim25:Datastore`, `urn:vim25:StoragePod` |

Installer attachments:

- Tag attached to the VM folder when the installer creates it
- Tag attached to template VMs during OVA import

## Machine API attachment (not this operator)

`openshift/machine-api-operator` `pkg/controller/vsphere/reconciler.go` `reconcileTags` attaches the cluster ID tag to VMs using the machine label `machine.openshift.io/cluster-api-cluster` (the infraID) as the tag **name**. It expects the tag/category to already exist (created by installer or administrator). Optional extra tags come from `providerSpec.tagIDs`; the cluster ownership tag itself is **not** driven by `tagIDs`.

Therefore this operator must create the category and tag on the destination, but must **not** set MachineSet `providerSpec.tagIDs` for ownership.

## Design Decisions

1. Create ownership category/tag and attach to destination VM folder
2. Match installer naming, description, and associable types exactly
3. Do not set ownership tag via MachineSet `tagIDs` — MAO `reconcileTags` handles VM attachment
4. Reuse `EnsureTag` / `AttachTag`; add `EnsureClusterOwnershipTag` orchestrator
5. Validate existing categories for `SINGLE` cardinality and required associable types (at least Folder + VirtualMachine)

## Implementation

### 1. vSphere helpers (`internal/vsphere/tags.go`)

- `EnsureClusterOwnershipTag(ctx, session, infraID) (tagID string, err error)`
- `AttachClusterOwnershipTag(ctx, session, tagID, folder)` wrapping `AttachTag`
- Unit tests with govmomi simulator

### 2. DestinationInitialized

After folder create/verify per server/datacenter:

1. Ensure ownership tag
2. Attach to infraID VM folder
3. Dedup per vCenter within reconcile

### 3. Docs

README subsection distinguishing topology vs ownership tags, and clarifying MAO owns VM attachment.

## Out of Scope

- Setting `providerSpec.tagIDs` on MachineSets / CPMS
- Attaching ownership tag to already-running / live-migrated VMs (MAO attaches on reconcile for machines it manages)
- Deleting ownership tags
- Changing topology tag behavior

## Test Plan

- `go test ./internal/vsphere/ -run 'TestEnsureClusterOwnership|TestAttachCluster'`
- Manual: after DestinationInitialized, `govc tags.ls` shows `openshift-<infraID>` / `<infraID>`; folder has the tag attached; new machines get the tag via MAO
