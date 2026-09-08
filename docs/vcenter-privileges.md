# vCenter Privilege Requirements

Privilege set required to run this operator against a vCenter, derived from the
operator's actual vSphere API calls and the VMware vSphere SDK ReferenceGuide
(`vsphere-ws/docs/ReferenceGuide`, vim25 SOAP API).

## How it was derived

Each govmomi call in the operator was traced to the underlying SDK API method,
then mapped to the "Required Privileges" section of that method's ReferenceGuide
page (or, for property access, the per-property privilege on the object page).

### SOAP (vim25) calls

| Code path | SDK API call | Privilege per ReferenceGuide |
|---|---|---|
| `internal/vsphere/session.go:92`, `internal/vsphere/list.go:31` — client init | `ServiceInstance.RetrieveServiceContent` | `System.Anonymous` (none) |
| `internal/vsphere/session.go:102`, `internal/vsphere/list.go:40` — login | `SessionManager.Login` | `System.Anonymous` (none) |
| `internal/vsphere/session.go:131-143`, `internal/vsphere/list.go:43` — logout | `SessionManager.Logout` | **`System.View`** |
| `internal/vsphere/session.go:108`, `internal/vsphere/list.go:46`, `internal/controller/preflight.go:187,223-256,302,318,329`, `internal/vsphere/image.go:265,298,464`, `internal/controller/vmwarecloudfoundationmigration_controller.go:378-383` — all `Finder.*` lookups (datacenter, cluster, datastore, network, resource pool, folder, template) | `PropertyCollector.RetrievePropertiesEx` (method: `System.Anonymous`; per-property access enforced) | **`System.View`** on traversed objects (`vim.ManagedEntity.name` / `parent` are documented as `System.View`) |
| `internal/vsphere/folder.go:31`, `internal/controller/preflight.go:334,520`, `internal/vsphere/image.go:392` — `dc.Folders()` reads `Datacenter.configInfo` | `RetrievePropertiesEx` property access | **`System.View`** |
| `internal/vsphere/folder.go:99` — `task.Wait` reads task `info` | `RetrievePropertiesEx` property access | **`System.View`** |
| `internal/controller/preflight.go:291,511` — `UserSession` reads `SessionManager.currentSession` | property access | `System.Anonymous` (none; `vim.SessionManager.html` property table) |
| `internal/controller/preflight.go:351,553` — privilege preflight | `AuthorizationManager.HasUserPrivilegeOnEntities` | method: `None`; `entities` param: **`System.View`** on root folder, vm folder, datacenter, cluster, resource pool, and datastore |
| `internal/vsphere/folder.go:47` — `CreateVMFolder` | `Folder.CreateFolder` | **`Folder.Create`** on the parent folder |
| `internal/vsphere/folder.go:94` — `DeleteVMFolder` | `ManagedEntity.Destroy_Task` | **`Folder.Delete`** when the object is a Folder |
| `internal/vsphere/image.go:584-624` — OVA import spec (`OvfManager.CreateImportSpec`) | `OvfManager.CreateImportSpec` | **`System.View`**, plus **`Datastore.AllocateSpace`** on the target datastore (required by the `datastore` parameter; `vim.OvfManager.html`) |
| `internal/vsphere/image.go:432` — OVA import, incl. NFC lease upload and `task.Wait` | `ResourcePool.ImportVApp` | **`VApp.Import`** on the resource pool (`vim.ResourcePool.html`) |
| `internal/vsphere/image.go:475` — mark the imported VM as a template | `VirtualMachine.MarkAsTemplate_Task` | **`VirtualMachine.Provisioning.MarkAsTemplate`** on the imported VM (`vim.VirtualMachine.html`) |
| `internal/vsphere/image.go:517-582` — `findAvailableHost` collects `ComputeResource.host` and `HostSystem` `name`/`runtime`/`datastore`/`network` | `RetrievePropertiesEx` property access | **`System.View`** (`vim.ComputeResource.html`, `vim.HostSystem.html` property tables) |
| `internal/vsphere/image.go:665-692` — `ReconfigVM_Task` disables secure boot on imported template (boot options) | `VirtualMachine.ReconfigVM_Task` | **`VirtualMachine.Config.Settings`** on the imported VM, only when the OVF references secure boot (RHCOS OVAs do) (`vim.VirtualMachine.html`) |
| `internal/vsphere/image.go:293-315` — `DeleteTemplate` on OVA URL change | `ManagedEntity.Destroy_Task` | **`VirtualMachine.Inventory.Delete`** on the template VM *and its parent folder* (`vim.ManagedEntity.html` per-type table) |

### REST (vapi tag API) calls

| Code path | HTTP endpoint | Privilege |
|---|---|---|
| `internal/vsphere/session.go:115` — REST login | SAML exchange | none (auth) |
| `internal/vsphere/tags.go:138,194,287` — `GetCategory` / `GetTagForCategory` | `GET /rest/com/vmware/cis/tagging/category/id:{category-id}`, `POST .../tag/id:{category-id}?~action=list-tags-for-category`, `GET .../tag/id:{tag-id}` (a name is resolved client-side: list, then match) | **`InventoryService.Tagging.Read`** |
| `internal/vsphere/tags.go:151` — `ListTagsForCategory` | `POST /rest/com/vmware/cis/tagging/tag/id:{category-id}?~action=list-tags-for-category` | **`InventoryService.Tagging.Read`** |
| `internal/vsphere/tags.go:160` — `ListAttachedTags` | `POST /rest/com/vmware/cis/tagging/tag-association?~action=list-attached-tags` | **`InventoryService.Tagging.Read`** |
| `internal/vsphere/tags.go:210` — `CreateCategory` | `POST /rest/com/vmware/cis/tagging/category` | **`InventoryService.Tagging.CreateCategory`** (root folder) |
| `internal/vsphere/tags.go:299` — `CreateTag` | `POST /rest/com/vmware/cis/tagging/tag` | **`InventoryService.Tagging.CreateTag`** (root folder) |
| `internal/vsphere/tags.go:333` — `AttachTag` | `POST /rest/com/vmware/cis/tagging/tag-association/id:{tag-id}?~action=attach` | **`InventoryService.Tagging.AttachTag`** (root folder) + **`InventoryService.Tagging.ObjectAttachable`** on the target object (vSphere ≥ 7.0.3) |

> Endpoints are the actual HTTP calls as made by the vendored govmomi client
> (`vendor/github.com/vmware/govmomi/vapi/tags/`; base = `rest.Path` `/rest` +
> `internal.CategoryPath`/`TagPath`/`AssociationPath` =
> `/com/vmware/cis/tagging/{category|tag|tag-association}`). IDs are path
> segments of the form `id:{id}` (`Resource.WithID`) and actions are the
> `~action=` query parameter (`Resource.WithAction`); the `/action/...` path
> form seen in some vSphere docs is not what this client sends. The privileges
> protecting these vAPI calls are the `InventoryService.Tagging.*` privilege IDs
> (see "Gaps and notes" below).

## Required privilege set (target vCenter)

| Privilege | Scope | Why |
|---|---|---|
| `System.View` | root folder | every inventory lookup (finder), `Datacenter.configInfo` read, task wait, `Logout`, `HasUserPrivilegeOnEntities` entities param, OVA-import property collection (`findAvailableHost`), `OvfManager.CreateImportSpec` |
| `Folder.Create` | datacenter's VM folder | `CreateVMFolder` (nested parts need it on each parent created) |
| `Folder.Delete` | VM folders the operator creates | `DeleteVMFolder` — **currently dead code in the controller path** (only exercised by tests), so optional until cleanup lands |
| `VApp.Import` | the failure domain's resource pool | `ImportVApp` during OVA template import (`spec.image` set) |
| `Datastore.AllocateSpace` | the failure domain's datastore | `OvfManager.CreateImportSpec` `datastore` parameter during OVA template import |
| `VirtualMachine.Provisioning.MarkAsTemplate` | the VM folder the imported template lands in (preflight checks the FD folder, or the datacenter VM folder when `topology.folder` is empty) | `vm.MarkAsTemplate` after import (`image.go:475`) |
| `VirtualMachine.Config.Settings` | the imported template VM | secure-boot disable after import; conditional on the OVF referencing secure boot (RHCOS OVAs do) |
| `VirtualMachine.Inventory.Delete` | imported template VM + its folder | `DeleteTemplate` when the resolved OVA URL changes and the old template is re-imported |
| `InventoryService.Tagging.Read` | root folder | category/tag/attachment reads happen on *every* reconcile (`ObjectHasTagInCategory`, `EnsureTagCategory`, `EnsureTag`) |
| `InventoryService.Tagging.CreateCategory` | root folder | `EnsureTagCategory` |
| `InventoryService.Tagging.CreateTag` | root folder | `EnsureTag` |
| `InventoryService.Tagging.AttachTag` | root folder | `AttachTag` |
| `InventoryService.Tagging.ObjectAttachable` | the specific datacenter + cluster (and the VM folder — attached at `controller.go:530`, but not preflight-checked there; see "Gaps and notes") | tag attachment to those objects on vSphere 7.0.3+ |

**Source vCenter:** read-only — `System.View` (datacenter existence check only,
`r.validatePreflightVSphere`; no mutations).

## Gaps and notes

1. **Preflight under-checks the real requirement set** (`preflight.go:46-57`). It
   verifies the tag privileges + `ObjectAttachable` + `Folder.Create`, and — when
   `spec.image` is set and `topology.template` is empty — the OVA import set:
   `VApp.Import`, `VirtualMachine.Config.AddNewDisk`,
   `VirtualMachine.Inventory.CreateFromExisting` on the resource pool, plus
   `Datastore.AllocateSpace` on the datastore and
   `VirtualMachine.Provisioning.MarkAsTemplate` on the VM folder
   (`preflight.go:273-348`), but never checks `System.View` (needed for every
   finder call and even `Logout`) nor `InventoryService.Tagging.Read` (used
   unconditionally) nor `VirtualMachine.Config.Settings` / `VirtualMachine.Inventory.Delete`
   (used on the imported VM). A user with only the preflight-checked set would
   pass preflight, then fail on first reconcile. Note the import preflight also
   *over*-checks: per the ReferenceGuide, `CreateImportSpec` needs `System.View`
   plus `Datastore.AllocateSpace` on the datastore, and `ImportVApp` needs
   `VApp.Import`, so `Config.AddNewDisk` and `Inventory.CreateFromExisting` are
   defensive extras. The `ObjectAttachable`
   check is also asymmetric: `validateTargetPrivileges` verifies it on the
   datacenter and the cluster but not on the VM folder itself, even though
   `AttachClusterOwnershipTag` attaches the tag to the folder
   (`controller.go:530`); a role scoped to the datacenter/cluster would pass
   preflight and then fail at destination init with a 403 from the folder attach.
2. `Folder.Delete` is required only once folder cleanup is actually wired in; the
   controller never calls `DeleteVMFolder`.
3. Sourcing: the ReferenceGuide is SOAP-only — tag REST privileges are not
   documented there (only `InventoryService.Tagging.AttachTag` appears, in
   `vim.vslm.vcenter.VStorageObjectManager.html`). The tag privilege IDs above
   match the operator's own preflight constants (`preflight.go:46-57`) plus
   VMware's REST tag API privilege names; `AttachTag` on root folder is the one
   grounded in this doc set. SOAP rows above are grounded in the local SDK copy
   at `vsphere-ws/docs/ReferenceGuide`; OVA-import rows were added for the RHCOS
   OVA import work (merged 2026-09-03, after this doc's branch cut).
4. Out of scope for "running the operator": the vSphere creds secret the operator
   writes into the *destination* cluster is consumed by that cluster's
   machine-api/cloud-controller, which needs the full VM-lifecycle privilege set
   (`VirtualMachine.*`, `Host.*`, etc.) — a different account requirement than the
   operator's own.
