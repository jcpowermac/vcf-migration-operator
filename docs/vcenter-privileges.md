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
| `internal/vsphere/session.go:108`, `internal/vsphere/list.go:46`, `internal/controller/preflight.go:189-218`, `internal/controller/vmwarecloudfoundationmigration_controller.go:305-311` — all `Finder.*` lookups (datacenter, cluster, datastore, network, resource pool, folder, template) | `PropertyCollector.RetrievePropertiesEx` (method: `System.Anonymous`; per-property access enforced) | **`System.View`** on traversed objects (`vim.ManagedEntity.name` / `parent` are documented as `System.View`) |
| `internal/vsphere/folder.go:31`, `internal/controller/preflight.go:378` — `dc.Folders()` reads `Datacenter.configInfo` | `RetrievePropertiesEx` property access | **`System.View`** |
| `internal/vsphere/folder.go:99` — `task.Wait` reads task `info` | `RetrievePropertiesEx` property access | **`System.View`** |
| `internal/controller/preflight.go:317` — `UserSession` reads `SessionManager.currentSession` | property access | `System.Anonymous` (none; `vim.SessionManager.html` property table) |
| `internal/controller/preflight.go:411` — privilege preflight | `AuthorizationManager.HasUserPrivilegeOnEntities` | method: `None`; `entities` param: **`System.View`** on root folder, vm folder, datacenter, cluster |
| `internal/vsphere/folder.go:47` — `CreateVMFolder` | `Folder.CreateFolder` | **`Folder.Create`** on the parent folder |
| `internal/vsphere/folder.go:94` — `DeleteVMFolder` | `ManagedEntity.Destroy_Task` | **`Folder.Delete`** when the object is a Folder |

### REST (vapi tag API) calls

| Code path | HTTP endpoint | Privilege |
|---|---|---|
| `internal/vsphere/session.go:115` — REST login | SAML exchange | none (auth) |
| `internal/vsphere/tags.go:138,194,287` — `GetCategory` / `GetTagForCategory` | `GET /api/v2/category[/{id}]`, `GET /api/v2/tag?category-id=` | **`InventoryService.Tagging.Read`** |
| `internal/vsphere/tags.go:151` — `ListTagsForCategory` | `GET /api/v2/tag` | **`InventoryService.Tagging.Read`** |
| `internal/vsphere/tags.go:160` — `ListAttachedTags` | `POST /api/v2/category/{id}/action/list-attached-tags` | **`InventoryService.Tagging.Read`** |
| `internal/vsphere/tags.go:210` — `CreateCategory` | `POST /api/v2/category` | **`InventoryService.Tagging.CreateCategory`** (root folder) |
| `internal/vsphere/tags.go:299` — `CreateTag` | `POST /api/v2/tag` | **`InventoryService.Tagging.CreateTag`** (root folder) |
| `internal/vsphere/tags.go:333` — `AttachTag` | `POST /api/v2/category/{id}/action/attach` | **`InventoryService.Tagging.AttachTag`** (root folder) + **`InventoryService.Tagging.ObjectAttachable`** on the target object (vSphere ≥ 7.0.3) |

## Required privilege set (target vCenter)

| Privilege | Scope | Why |
|---|---|---|
| `System.View` | root folder | every inventory lookup (finder), `Datacenter.configInfo` read, task wait, `Logout`, `HasUserPrivilegeOnEntities` entities param |
| `Folder.Create` | datacenter's VM folder | `CreateVMFolder` (nested parts need it on each parent created) |
| `Folder.Delete` | VM folders the operator creates | `DeleteVMFolder` — **currently dead code in the controller path** (only exercised by tests), so optional until cleanup lands |
| `InventoryService.Tagging.Read` | root folder | category/tag/attachment reads happen on *every* reconcile (`ObjectHasTagInCategory`, `EnsureTagCategory`, `EnsureTag`) |
| `InventoryService.Tagging.CreateCategory` | root folder | `EnsureTagCategory` |
| `InventoryService.Tagging.CreateTag` | root folder | `EnsureTag` |
| `InventoryService.Tagging.AttachTag` | root folder | `AttachTag` |
| `InventoryService.Tagging.ObjectAttachable` | the specific datacenter + cluster (+ folder) | tag attachment to those objects on vSphere 7.0.3+ |

**Source vCenter:** read-only — `System.View` (datacenter existence check only,
`preflight.go:149`; no mutations).

## Gaps and notes

1. **Preflight under-checks the real requirement set** (`preflight.go:45-57`). It
   verifies the tag privileges + `ObjectAttachable` + `Folder.Create`, but never
   checks `System.View` (needed for every finder call and even `Logout`) nor
   `InventoryService.Tagging.Read` (used unconditionally). A user with only the
   preflight-checked set would pass preflight, then fail on first reconcile.
2. `Folder.Delete` is required only once folder cleanup is actually wired in; the
   controller never calls `DeleteVMFolder`.
3. Sourcing: the ReferenceGuide is SOAP-only — tag REST privileges are not
   documented there (only `InventoryService.Tagging.AttachTag` appears, in
   `vim.vslm.vcenter.VStorageObjectManager.html`). The tag privilege IDs above
   match the operator's own preflight constants (`preflight.go:46-56`) plus
   VMware's REST tag API privilege names; `AttachTag` on root folder is the one
   grounded in this doc set.
4. Out of scope for "running the operator": the vSphere creds secret the operator
   writes into the *destination* cluster is consumed by that cluster's
   machine-api/cloud-controller, which needs the full VM-lifecycle privilege set
   (`VirtualMachine.*`, `Host.*`, etc.) — a different account requirement than the
   operator's own.
