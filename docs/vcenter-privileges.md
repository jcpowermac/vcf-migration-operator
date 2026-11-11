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
| `internal/vsphere/session.go:108`, `internal/vsphere/list.go:46`, `internal/controller/preflight.go`, `internal/controller/vmwarecloudfoundationmigration_controller.go:378-383` — all `Finder.*` lookups (datacenter, cluster, datastore, network, resource pool, folder, template) | `PropertyCollector.RetrievePropertiesEx` (method: `System.Anonymous`; per-property access enforced) | **`System.View`** on traversed objects (`vim.ManagedEntity.name` / `parent` are documented as `System.View`) |
| `internal/vsphere/folder.go:31`, `internal/controller/preflight.go` — `dc.Folders()` reads `Datacenter.configInfo` | `RetrievePropertiesEx` property access | **`System.View`** |
| `internal/vsphere/folder.go:99` — `task.Wait` reads task `info` | `RetrievePropertiesEx` property access | **`System.View`** |
| `internal/controller/preflight.go` — `UserSession` reads `SessionManager.currentSession` | property access | `System.Anonymous` (none; `vim.SessionManager.html` property table) |
| `internal/controller/preflight.go` — privilege preflight | `AuthorizationManager.HasUserPrivilegeOnEntities` | method: `None`; `entities` param: **`System.View`** on the root folder, VM folder, datacenter, and cluster |
| `internal/vsphere/folder.go:47` — `CreateVMFolder` | `Folder.CreateFolder` | **`Folder.Create`** on the parent folder |
| `internal/vsphere/folder.go:94` — `DeleteVMFolder` | `ManagedEntity.Destroy_Task` | **`Folder.Delete`** when the object is a Folder |

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
| `System.View` | root folder | every inventory lookup (finder), `Datacenter.configInfo` read, task wait, `Logout`, `HasUserPrivilegeOnEntities` entities param |
| `Folder.Create` | datacenter's VM folder | `CreateVMFolder` (nested parts need it on each parent created) |
| `Folder.Delete` | VM folders the operator creates | `DeleteVMFolder` — **currently dead code in the controller path** (only exercised by tests), so optional until cleanup lands |
| `InventoryService.Tagging.Read` | root folder | category/tag/attachment reads happen on *every* reconcile (`ObjectHasTagInCategory`, `EnsureTagCategory`, `EnsureTag`) |
| `InventoryService.Tagging.CreateCategory` | root folder | `EnsureTagCategory` |
| `InventoryService.Tagging.CreateTag` | root folder | `EnsureTag` |
| `InventoryService.Tagging.AttachTag` | root folder | `AttachTag` |
| `InventoryService.Tagging.ObjectAttachable` | the specific datacenter + cluster (and the VM folder — attached at `controller.go:530`, but not preflight-checked there; see "Gaps and notes") | tag attachment to those objects on vSphere 7.0.3+ |

**Source vCenter:** read-only — `System.View` (datacenter existence check only,
`r.validatePreflightVSphere`; no mutations).

## Gaps and notes

1. **Preflight under-checks the real requirement set.** It checks tag privileges,
   `ObjectAttachable`, and `Folder.Create`, but does not check `System.View`
   (needed for inventory lookups) or `InventoryService.Tagging.Read` (used
   unconditionally). `ObjectAttachable` is checked on the datacenter and cluster,
   but not on the VM folder even though the operator attaches an ownership tag
   to that folder; insufficient folder privileges can fail destination initialization.
2. `Folder.Delete` is required only once folder cleanup is actually wired in; the
   controller never calls `DeleteVMFolder`.
3. Sourcing: the ReferenceGuide is SOAP-only — tag REST privileges are not
   documented there (only `InventoryService.Tagging.AttachTag` appears, in
   `vim.vslm.vcenter.VStorageObjectManager.html`). The tag privilege IDs above
   match the operator's own preflight constants (`preflight.go:46-57`) plus
   VMware's REST tag API privilege names; `AttachTag` on root folder is the one
   grounded in this doc set. SOAP rows above are grounded in the local SDK copy
   at `vsphere-ws/docs/ReferenceGuide`.
4. Out of scope for "running the operator": the vSphere creds secret the operator
   writes into the *destination* cluster is consumed by that cluster's
   machine-api/cloud-controller, which needs the full VM-lifecycle privilege set
   (`VirtualMachine.*`, `Host.*`, etc.) — a different account requirement than the
   operator's own.
