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
- Tag ID injected into machine provider `TagIDs` (and template VM)

## Current Baseline

- `ensureDestinationInitialized` creates the infraID VM folder and topology tags only
- `updateMachineSetProviderSpec` never sets `TagIDs`
- Machine API expects `tagIDs` as vSphere tag IDs (URN-notation), max 10

## Design Decisions

1. Create ownership category/tag, attach to destination VM folder, and set MachineSet `tagIDs`
2. Match installer naming, description, and associable types exactly
3. Scope MachineSet wiring to destination worker MachineSets created by this operator (not CPMS)
4. Reuse `EnsureTag` / `AttachTag`; add `EnsureClusterOwnershipTag` orchestrator
5. Validate existing categories for `SINGLE` cardinality and required associable types (at least Folder + VirtualMachine)

## Implementation Plan

### 1. vSphere helpers (`internal/vsphere/tags.go`)

- `EnsureClusterOwnershipTag(ctx, session, infraID) (tagID string, err error)`
- `AttachClusterOwnershipTag(ctx, session, tagID, folder)` wrapping `AttachTag`
- Unit tests with govmomi simulator

### 2. DestinationInitialized

After folder create/verify per server/datacenter:

1. Ensure ownership tag
2. Attach to infraID VM folder
3. Dedup per vCenter within reconcile

### 3. Worker MachineSets

Extend provider-spec update to append ownership tag ID to `TagIDs` (preserve existing, max 10). Caller ensures/looks up tag ID before create.

### 4. Docs

README subsection distinguishing topology vs ownership tags.

## Out of Scope

- Attaching ownership tag to already-running / live-migrated VMs
- CPMS / control-plane providerSpec `tagIDs`
- Deleting ownership tags
- Changing topology tag behavior

## Test Plan

- `go test ./internal/vsphere/ -run 'TestEnsureClusterOwnership|TestAttachCluster'`
- `go test ./internal/openshift/` (TagIDs on provider spec)
- Manual: `govc tags.attached.ls` on destination folder and new worker VMs
