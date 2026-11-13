# Migrating an OpenShift Cluster from One vCenter to Another (Day 2)

This runbook moves an OpenShift cluster on vSphere from its current vCenter to a new one, after
installation. It is written for a cluster administrator working on the cluster directly — no migration
tooling involved.

The cluster is moved by adding the new vCenter, re-provisioning nodes onto it, and then removing the
old vCenter. Nodes are **replaced, not live-migrated**: new machines are cloned on the target vCenter,
join the cluster, and the old ones are drained and deleted.

> ## ⚠️ Before you start
>
> **The `VSphereMultiVCenterDay2` feature gate must be enabled on the cluster.** A cluster cannot run
> on two vCenters without it. Check with:
>
> ```bash
> oc get clusterversion -o jsonpath='{.items[0].status.desired.version}{"\n"}'
> oc get featuregate cluster -o json | jq -r --arg v "$(oc get clusterversion -o jsonpath='{.items[0].status.desired.version}')" \
>   '.status.featureGates[] | select(.version == $v) | .enabled[].name' | grep VSphereMultiVCenterDay2
> ```
>
> - **Enabled** — continue with this runbook.
> - **Not available in your release** — stop. There is no supported way to do this.
> - **Available but not enabled** (for example only in `TechPreviewNoUpgrade`) — enabling it is a
>   cluster-wide feature-set change that has to be planned with Red Hat Support. It is not a local
>   workaround.
>
> Do not try to get around this by editing the `Infrastructure` CRD validation. Cloud configuration
> generation, storage topology and Machine API are not reconciled for a second vCenter without the
> gate, and the cluster ends up in a state no operator manages.

## Terms used here

| Role | vCenter | Datacenter | Failure domain | Region / zone |
|------|---------|------------|----------------|---------------|
| source | `vcenter1.example.com` | `DC1` | `us-east-1a` | `us-east` / `us-east-1a` |
| target | `vcenter2.example.com` | `DC3` | `us-west-1a` | `us-west` / `us-west-1a` |

Anything in `<angle brackets>` below is a placeholder — substitute your value before running the
command.

`<infra-id>` is the cluster's infrastructure ID. Read it once and use it everywhere below:

```bash
oc get infrastructure cluster -o jsonpath='{.status.infrastructureName}{"\n"}'
```

The cluster will run with **both** vCenters for the whole migration. Up to three vCenters are allowed
by the API.

## You do not write the cloud configuration by hand

`openshift-cloud-controller-manager/cloud-conf` is generated on the cluster from two inputs:

- the vCenter and failure-domain information in the `Infrastructure` resource, and
- the existing `openshift-config/cloud-provider-config` ConfigMap.

CCCO (the cloud controller manager operator) merges the two and rewrites `cloud-conf` whenever either
changes. Adding the new vCenter to the `Infrastructure` resource in Step 3 is normally all you have to
do. On some releases the entry has to be added to `cloud-provider-config` by hand — Step 4 tells you
how to check, and what to add if it is missing.

**Never edit `cloud-conf`.** Anything you put there is reverted.

How the merge treats a vCenter that is no longer listed in `Infrastructure` depends on your release:
newer releases rebuild the vCenter list from `Infrastructure` alone, older ones keep entries already
present in `cloud-provider-config`. Either way, the check is the same — look at the generated
configuration after retiring the source vCenter (Step 9d) and remove its entry from
`cloud-provider-config` only if it is still listed there.

---

## Before you start: requirements

You need `oc`, `jq`, `yq` and `govc` on your workstation.

Every item below is a stop condition. Work through them in order.

### 1. The cluster is healthy and not upgrading

```bash
oc get clusterversion -o jsonpath='{.items[0].status.conditions[?(@.type=="Progressing")].status}{"\n"}'   # False
oc get co          # no Degraded / unavailable operators
```

An upgrade in progress and a degraded operator both make a control plane rollout unsafe. Finish or
postpone them first.

### 2. No storage volumes that live on the vSphere datastore

This is the most common reason a vCenter migration stalls. Volumes provided by the vSphere CSI driver
are bound to a datastore on a specific vCenter; a node created on the *other* vCenter cannot attach
them. The migration is not supported while such volumes exist.

```bash
# Must be 0
oc get pv -o json | jq '[.items[] | select(.spec.csi.driver=="csi.vsphere.vmware.com")] | length'

# The driver must be removed for the duration
oc get clustercsidriver csi.vsphere.vmware.com -o jsonpath='{.spec.managementState}{"\n"}'   # Removed

# Recommended: the storage operator out of the way too (warning, not a hard stop)
oc get storage cluster -o jsonpath='{.spec.managementState}{"\n"}'   # Unmanaged or Removed
```

This is a data operation, not a check to get past: the volumes must be moved to storage that does not
depend on either vCenter, and their data restored from that copy after the migration. Deleting the
`PersistentVolume` objects without that removes the data with them — check the reclaim policy before
deleting anything. Move the workloads, verify the data, then set the driver to `Removed`:

```bash
oc patch clustercsidriver csi.vsphere.vmware.com --type=merge -p '{"spec":{"managementState":"Removed"}}'
oc patch storage cluster --type=merge -p '{"spec":{"managementState":"Unmanaged"}}'
```

Re-enable them after the migration (Step 10).

### 3. Nothing will re-create the capacity you remove

Machine health checks and autoscalers fight the migration: they replace the nodes you drain and add
machines you did not ask for.

```bash
oc get machinehealthcheck -A     # only openshift-machine-api/machine-api-termination-handler may remain
oc get clusterautoscaler -A      # none
oc get machineautoscaler -A      # none
```

Delete user-defined health checks and autoscalers, and re-create them after Step 10.
`machine-api-termination-handler` can stay — it reacts to cloud preemption signals, not node health.

### 4. Credentials and permissions on the target vCenter

The cluster already has credentials for the source in `vsphere-creds`. For the target you need a
vCenter account with, at the target datacenter and compute cluster:

- tag create/attach (categories and tags)
- folder create, VM clone and VM reconfigure
- datastore and resource pool use, and port group use on the stretched network

A practical check: clone a throwaway template into `/DC3/vm/<infra-id>` with that account and delete it.

### 5. A failure domain shape that maps cleanly

- the cluster has **exactly one** vCenter today, with **exactly one** datacenter listed for it — this runbook
  is for that case;
- every new failure domain uses the **same** new vCenter (one target vCenter per migration);
- no failure domain may use `regionAffinity.type: ComputeCluster` or `zoneAffinity.type: HostGroup`;
  those topologies are not supported for this operation;
- if you will run more than one failure domain, each must be unambiguous: two failure domains sharing
  a datacenter need distinct `topology.resourcePool` values, so a MachineSet matches exactly one;
- decide now which source failure domain maps to which target failure domain. Each worker MachineSet
  is copied to the failure domain it maps to; one target failure domain may receive several source
  MachineSets.

### 6. The RHCOS template exists on the target, at a known path

The template path goes into the failure domain and is used when machines are cloned — worker machines
use the full path, the ControlPlaneMachineSet uses the template *name* (the last path element) and
looks it up within the datacenter. Decide the path now, for example
`/DC3/vm/<infra-id>-rhcos-us-west-us-west-1a`, confirm it resolves exactly (not with a wildcard), and
make sure the name is unique within the datacenter:

```bash
export GOVC_URL='https://vcenter2.example.com/sdk'
govc ls '/DC3/vm/<infra-id>-rhcos-us-west-us-west-1a'
```

### 7. The Layer 2 network really is stretched, and latency is low enough

This is not a formality; nodes on the two vCenters have to behave as if they were on one segment.

- all nodes, existing and new, on the same subnet — the network has to be stretched between vCenters;
- round-trip latency between the vCenters within the limits documented for the etcd version in your
  OpenShift release (see the etcd tuning guidance for your release — etcd's default heartbeat
  interval of 100 ms and election timeout of 1 s bound what is tolerable). Use your release's
  documented numbers rather than a generic threshold.

Verify it rather than assume it: clone the template once onto the target vCenter's failure-domain
network, boot it (live ISO or an image without ignition), confirm it reaches the cluster's API VIP and
an etcd peer on the same subnet, and measure round-trip latency. Delete it afterwards. A broken stretch
found here costs minutes; found during the control plane step, a rollout.

---

## Step 1: Prepare the new vCenter

Three things are needed on the target before it can host machines: the region/zone tags, the per-cluster
VM folder with its ownership tag, and the RHCOS template.

### 1a. Region and zone tags

The categories have fixed names and single-value cardinality, and the attachments are **asymmetric**:
the region tag goes on the **datacenter**, the zone tag on the **compute cluster**.

```bash
export GOVC_URL='https://vcenter2.example.com/sdk'

# Categories must be associable with datacenters and compute clusters
govc tags.category.create -t Datacenter -t ClusterComputeResource openshift-region
govc tags.category.create -t Datacenter -t ClusterComputeResource openshift-zone

govc tags.create -c openshift-region us-west
govc tags.create -c openshift-zone us-west-1a

govc tags.attach -c openshift-region us-west /DC3
govc tags.attach -c openshift-zone us-west-1a /DC3/host/Cluster1
```

Tag values must match the `region` and `zone` strings you will put in the failure domain
(case-sensitive). They become the node topology labels.

### 1b. VM folder and cluster ownership tag

Machines are cloned into `/<datacenter>/vm/<infra-id>`, and that folder carries the installer-style
ownership tag that tells anyone looking at vSphere which cluster owns it:

```bash
govc folder.create "/DC3/vm/<infra-id>"

govc tags.category.create -t urn:vim25:VirtualMachine -t urn:vim25:ResourcePool \
  -t urn:vim25:Folder -t urn:vim25:Datastore -t urn:vim25:StoragePod "openshift-<infra-id>"
govc tags.create -c "openshift-<infra-id>" -d "Added by openshift-install do not remove" "<infra-id>"
govc tags.attach -c "openshift-<infra-id>" "<infra-id>" "/DC3/vm/<infra-id>"
```

### 1c. RHCOS template

The template must exist on the target vCenter at exactly the path you will reference, in the datacenter
the failure domain points at, and be marked as a template. Clone it from the source vCenter or import
the matching RHCOS OVA. Keep template names in step with the OpenShift version — a stale template is
the most common cause of machines that fail to provision later.

---

## Step 2: Add credentials for the new vCenter

Do this **before** registering the vCenter, so the cloud controller manager and Machine API can
authenticate to it the first time they see it.

The cluster stores vCenter credentials as a Kubernetes Secret, and Secret `data` values are
base64-encoded. Keys are named `<vcenter-server>.username` and `<vcenter-server>.password`, where the
server string must match the `Infrastructure` resource exactly.

### 2a. Back up the secret

The backup contains the credentials in recoverable form, so create it with a restrictive umask:

```bash
umask 077
oc get secret vsphere-creds -n kube-system -o yaml > vsphere-creds-backup.yaml

# what is there today
oc get secret vsphere-creds -n kube-system -o json | jq '.data | keys'
```

The result looks like this — one pair per vCenter the cluster already knows about:

```json
[
  "vcenter1.example.com.password",
  "vcenter1.example.com.username"
]
```

### 2b. Produce the base64 values

Set the plain values in your shell (they never leave it), then encode them with `base64`:

```bash
VCENTER='vcenter2.example.com'
VC_USER='svc-openshift@vsphere.local'
read -rsp 'vCenter password: ' VC_PASS; echo

printf '%s' "$VC_USER" | base64 | tr -d '\n'; echo
printf '%s' "$VC_PASS" | base64 | tr -d '\n'; echo
```

The `printf '%s'` and `tr -d '\n'` matter: a trailing newline or line wrapping encoded into the value
is the most common way these keys end up wrong without any error.

Check the encoding before you write it to the cluster — this must print your original value:

```bash
printf '%s' "$VC_USER" | base64 | tr -d '\n' | base64 -d; echo
```

### 2c. Add the keys

The reliable way is a JSON merge patch from a protected file, which adds the two keys and leaves
everything else alone — the password never appears in your shell history or in the process list:

```bash
umask 077
patch_file=$(mktemp)
cat > "$patch_file" <<EOF
{
  "data": {
    "$VCENTER.username": "$(printf '%s' "$VC_USER" | base64 | tr -d '\n')",
    "$VCENTER.password": "$(printf '%s' "$VC_PASS" | base64 | tr -d '\n')"
  }
}
EOF

oc patch secret vsphere-creds -n kube-system --type=merge --patch-file "$patch_file"
rm -f "$patch_file"
```

Or edit the Secret and paste the two encoded values you produced in 2b:

```bash
oc edit secret vsphere-creds -n kube-system
```

```yaml
data:
  vcenter1.example.com.username: <unchanged>
  vcenter1.example.com.password: <unchanged>
  vcenter2.example.com.username: c3ZjLW9wZW5zaGlmdEB2c3BoZXJlLmxvY2Fs     # paste your base64
  vcenter2.example.com.password: dGhlLXBhc3N3b3Jk                          # paste your base64
```

Each vCenter needs its own key pair even when the username and password are identical across vCenters.

### 2d. Verify before moving on

```bash
# both new keys present
oc get secret vsphere-creds -n kube-system -o json | jq '.data | keys'

# and they decode to what you expect
oc get secret vsphere-creds -n kube-system -o jsonpath='{.data.vcenter2\.example\.com\.username}' | base64 -d; echo
```

If the decoded value has a trailing newline or stray quoting, fix it now — the cloud controller manager
will not tell you which field is wrong, it just fails to authenticate to that one vCenter.

---

## Step 3: Register the new vCenter on the cluster

```bash
oc get infrastructure cluster -o yaml > infrastructure-backup.yaml
oc edit infrastructure cluster
```

Add the vCenter and its failure domain, leaving the source entries untouched:

```yaml
spec:
  platformSpec:
    type: VSphere
    vsphere:
      vcenters:
      - server: vcenter1.example.com          # source, unchanged
        port: 443
        datacenters:
        - DC1
      - server: vcenter2.example.com          # target
        port: 443
        datacenters:
        - DC3

      failureDomains:
      - name: us-east-1a                      # source, unchanged
        region: us-east
        zone: us-east-1a
        server: vcenter1.example.com
        topology:
          datacenter: DC1
          computeCluster: /DC1/host/Cluster1
          networks:
          - VM Network
          datastore: /DC1/datastore/vsanDatastore

      - name: us-west-1a                      # target
        region: us-west
        zone: us-west-1a
        server: vcenter2.example.com
        topology:
          datacenter: DC3
          computeCluster: /DC3/host/Cluster1
          networks:
          - VM Network
          datastore: /DC3/datastore/vsanDatastore
          template: /DC3/vm/<infra-id>-rhcos-us-west-us-west-1a
```

What the API will reject, and why:

| Rejection | Cause |
|-----------|-------|
| `vcenters cannot be added or removed once set` | the feature gate is not enabled — go back to Before you start |
| `Cannot add and remove vCenters at the same time` | split the change into two updates |
| `all failure domains must have a corresponding vCenter entry` | a failure domain's `server` is missing from `vcenters` |
| `vcenters must have unique server values` | duplicate entry |

Rules for the values themselves: `computeCluster` is `/<dc>/host/<cluster>`, `datastore` is
`/<dc>/datastore/<datastore>`, and the `template` is `/<dc>/vm/<name>`. For `networks`, use the **bare
port group name** (for example `VM Network`), not the path: the API accepts both forms, but Machine API
and the ControlPlaneMachineSet resolve networks by object name — a value like `/DC3/network/VM Network`
is accepted and then fails when the machine provisions. Up to three vCenters, at least one datacenter
per vCenter.

`topology.template` is marked optional in the API but **set it explicitly**. If you leave it out, the
path is derived as `/<datacenter>/vm/<infra-id>-rhcos-<region>-<zone>` — a naming rule that is easy to
miss (note the region and zone are both appended, so `us-west` + `us-west-1a` gives
`...-rhcos-us-west-us-west-1a`). Set the field: worker MachineSets use the full path as written, and
the ControlPlaneMachineSet uses its last path element.

---

## Step 4: Wait for the cluster to pick up the new vCenter

Nothing has to be restarted by hand. The components that consume the vSphere configuration either
re-read it on every operation or roll themselves when it changes:

- the cloud controller manager is redeployed automatically when the generated `cloud-conf` changes;
- Machine API reads the cloud configuration each time it acts on a machine — there is no cache to
  clear;
- the machine config operator picks up the `Infrastructure` change on its own.

So this step is a wait-and-verify. Give it a couple of minutes, then check that both vCenters are in
the generated configuration and the involved operators are healthy:

```bash
oc get cm cloud-conf -n openshift-cloud-controller-manager -o jsonpath='{.data.cloud\.conf}' | yq '.vcenter | keys'
# expect both vcenter1.example.com and vcenter2.example.com

oc get co machine-config cloud-controller-manager machine-api
```

**If the new vCenter does not appear**, first check whether the generated configuration is built from
`Infrastructure` on your release. If not, add the entry to `cloud-provider-config` yourself:

```yaml
vcenter:
  vcenter2.example.com:
    server: vcenter2.example.com
    port: 443
    datacenters:
    - DC3
```

Then look at the operator that generates the configuration — not the pods consuming it:

```bash
oc get co cloud-controller-manager
oc logs -n openshift-cloud-controller-manager-operator deploy/cluster-cloud-controller-manager-operator --tail=50
```

As a last resort only, restarting the pods forces a re-read: the cloud controller manager
(`-l k8s-app=vsphere-cloud-controller-manager` in `openshift-cloud-controller-manager`) and Machine API
(`-l api=clusterapi` in `openshift-machine-api`). It should not be necessary on a healthy cluster.

---

## Step 5: Add worker capacity on the new vCenter

> This step only **adds** capacity. Nothing is removed from the source until Step 8.

Repeat Steps 5 and 8 for **every** non-master MachineSet — worker, infra, and any custom role. List
them all with `oc get machineset -n openshift-machine-api`; a MachineSet left on the source vCenter
blocks its removal in Step 9.

Machines are placed by the `providerSpec`, not by a failure-domain field — `VSphereMachineProviderSpec`
has none. Copy each source worker MachineSet to one target MachineSet and rewrite the placement from the
target failure domain.

```bash
oc get machineset -n openshift-machine-api -o custom-columns=NAME:.metadata.name,REPLICAS:.spec.replicas
oc get machineset -n openshift-machine-api <source-machineset> -o yaml > target-machineset.yaml

# the dump carries server-side state; strip it before editing
yq -i 'del(.status) | del(.metadata.resourceVersion) | del(.metadata.uid) | del(.metadata.creationTimestamp)' target-machineset.yaml
```

```yaml
apiVersion: machine.openshift.io/v1beta1
kind: MachineSet
metadata:
  name: <infra-id>-worker-us-west-1a          # <infra-id>-worker-<target failure domain name>
  namespace: openshift-machine-api
spec:
  replicas: <same as the source MachineSet>
  selector:
    matchLabels:
      machine.openshift.io/cluster-api-cluster: <infra-id>
      machine.openshift.io/cluster-api-machineset: <infra-id>-worker-us-west-1a
  template:
    metadata:
      labels:
        machine.openshift.io/cluster-api-cluster: <infra-id>
        machine.openshift.io/cluster-api-machine-role: worker
        machine.openshift.io/cluster-api-machine-type: worker
        machine.openshift.io/cluster-api-machineset: <infra-id>-worker-us-west-1a
    spec:
      providerSpec:
        value:
          apiVersion: machine.openshift.io/v1beta1
          kind: VSphereMachineProviderSpec
          template: /DC3/vm/<infra-id>-rhcos-us-west-us-west-1a
          workspace:
            server: vcenter2.example.com
            datacenter: DC3
            datastore: /DC3/datastore/vsanDatastore
            resourcePool: /DC3/host/Cluster1/Resources
            folder: /DC3/vm/<infra-id>
          network:
            devices:
            - networkName: "VM Network"
          numCPUs: <from the source>
          memoryMiB: <from the source>
          diskGiB: <from the source>
```

Points that cause most of the failures here:

- **Name it `<infra-id>-worker-<target failure domain>`.** If one target failure domain receives copies
  of several source MachineSets, name the extra ones `<base>-2`, `<base>-3`, … — two MachineSets may
  never share a name, and the name must not already exist (`oc get machineset -n openshift-machine-api`).
- **`networkName` is a port group name, not a path.** A path is accepted by the API
  and rejected when the machine provisions.
- **Use the target values for every `workspace.*` field** — copying them from the source is the second
  most common failure.
- **Replicas come from the source MachineSet**, so capacity is added, not shifted.
- **Do not add `machine.openshift.io/region` / `zone` labels.** Those are written from the vSphere tags
  when the VM is created.

Apply and wait for every machine and node to be Ready before continuing:

```bash
oc apply -f target-machineset.yaml
oc get machines -n openshift-machine-api -w
oc get nodes
```

---

## Step 6: Let the machine config pools settle

The credential and `Infrastructure` changes may trigger a machine config rollout that reboots control
plane nodes. A master rebooting while control plane machines are being replaced is how etcd quorum is
lost: a rebooting master looks unhealthy, the etcd operator releases its machine
deletion protection, and the replacement can delete a member before a new one has caught up.

Wait until every pool has finished and nothing is degraded:

```bash
oc get mcp      # every pool: UPDATED=True, UPDATING=False, DEGRADED=False
oc get co machine-config
```

Do not start Step 7 until this is stable.

---

## Step 7: Move the control plane

> ⚠️ **This step replaces all control plane machines.** Take a cluster backup (etcd snapshot) first,
> and store it somewhere this runbook's recovery steps can reach. The ControlPlaneMachineSet operator manages
> the rollout itself: it replaces machines one at a time, and stops if a replacement fails — a failed
> machine holds the rollout rather than letting it continue. Your part is to watch for that stop and fix
> the failed machine before the rollout resumes.

```bash
oc get controlplanemachineset cluster -n openshift-machine-api -o yaml > cpms-backup.yaml
oc get controlplanemachineset cluster -n openshift-machine-api -o jsonpath='{.spec.strategy.type}{"\n"}'
```

Two settings must be right:

- **`spec.strategy.type: RollingUpdate`.** With `OnDelete` nothing is replaced automatically and the
  migration stalls after the update, leaving you with a half-moved control plane.
- **`failureDomains` lists the target failure domain only.** Removing the source failure domain is what
  forces every control plane machine to be replaced onto the target.

```bash
oc edit controlplanemachineset cluster -n openshift-machine-api
```

```yaml
spec:
  replicas: 3
  state: Active
  strategy:
    type: RollingUpdate
  template:
    machineType: machines_v1beta1_machine_openshift_io
    machines_v1beta1_machine_openshift_io:
      failureDomains:
        platform: VSphere
        vsphere:
        - name: us-west-1a          # target only — the source failure domain is removed
      metadata:
        labels:
          machine.openshift.io/cluster-api-cluster: <infra-id>
          machine.openshift.io/cluster-api-machine-role: master
          machine.openshift.io/cluster-api-machine-type: master
      spec:
        providerSpec:
          value:
            apiVersion: machine.openshift.io/v1beta1
            kind: VSphereMachineProviderSpec
            template: <template name already present in your CPMS>
```

Only the failure domain **name** is listed; the placement is filled in from the `Infrastructure`
resource. In the same edit, **remove any `providerSpec.value.workspace` values and
`providerSpec.value.network.devices[].networkName` entries** that point at the source vCenter —
placement comes from the failure domain once it is set, and leftover source values can be rejected on
admission. Keep static-IP configuration (`addressesFromPools`) if the CPMS uses it. Two details about
how the injection works:

- The failure domain's `topology.networks` value is copied into the machine's network device
  **verbatim** — another reason Step 3 uses the bare port group name.
- The failure domain's `topology.template` is reduced to its **last path element** (the template
  name), and the machine looks it up within the failure domain's datacenter. If two templates share
  a name anywhere in that datacenter, the lookup is ambiguous — make sure the name is unique there,
  not just that the full path resolves.

Watch it, and stop if anything below breaks:

```bash
# rollout has started when observedGeneration catches up
oc get controlplanemachineset cluster -n openshift-machine-api -o jsonpath='{.metadata.generation} {.status.observedGeneration}{"\n"}'

oc get controlplanemachineset cluster -n openshift-machine-api -o json | jq '.status'
oc get machines -n openshift-machine-api -l machine.openshift.io/cluster-api-machine-role=master -w
oc get etcd cluster -o json | jq '.status.conditions'
oc get co
```

- The ControlPlaneMachineSet operator drives the rollout: each replacement is created, becomes Ready,
  then the old machine is removed — and a replacement that fails holds the rollout until it is fixed.
- A `Degraded` or `Error` condition on the ControlPlaneMachineSet names the problem: read its reason
  and message, fix what it points at, and the operator recomputes the condition on its next
  reconcile.
- If etcd degrades, stop and address it before letting the rollout continue.

---

## Step 8: Remove the old worker capacity

Only once the control plane is fully on the target and healthy.

```bash
# 1. Every source MachineSet must have a ready replacement on the target
oc get machineset -n openshift-machine-api -o custom-columns=NAME:.metadata.name,REPLICAS:.spec.replicas
oc get machines -n openshift-machine-api -o wide

# 2. Back up each source MachineSet while it still holds its replica count —
#    rollback needs the original object, and the dump must be taken before scaling
umask 077
mkdir -p source-machinesets
for ms in <source-machineset> <source-machineset-2>; do
  oc get machineset "$ms" -n openshift-machine-api -o yaml > "source-machinesets/$ms.yaml"
done

# 3. Scale each source MachineSet to 0 — Machine API drains and deletes the machines
oc scale machineset <source-machineset> -n openshift-machine-api --replicas=0

# 4. Wait until both the Machines and their Nodes are gone
oc get machines -n openshift-machine-api -w
oc get nodes

# 5. Only then delete the empty MachineSets
oc delete machineset -n openshift-machine-api <source-machineset>
```

Back up before scaling: the backup has to carry the original `spec.replicas`, because a backup taken
after the scale-down restores a MachineSet that provisions nothing. When re-applying a backup,
strip the server-side fields first (`status`, `metadata.resourceVersion`, `metadata.uid`,
`metadata.creationTimestamp`), as in Step 5.

Verify each step before starting the next.

---

## Step 9: Remove the old vCenter

Once nothing on the cluster references it. Order matters — infrastructure first, then credentials, then
wait for the change to propagate.

**a) Remove the source vCenter and its failure domain from the `Infrastructure` resource**, in one
update:

```yaml
spec:
  platformSpec:
    type: VSphere
    vsphere:
      vcenters:
      - server: vcenter2.example.com
        port: 443
        datacenters:
        - DC3
      failureDomains:
      - name: us-west-1a
        region: us-west
        zone: us-west-1a
        server: vcenter2.example.com
        topology:
          datacenter: DC3
          computeCluster: /DC3/host/Cluster1
          networks:
          - VM Network
          datastore: /DC3/datastore/vsanDatastore
          template: /DC3/vm/<infra-id>-rhcos-us-west-us-west-1a
```

The cluster will refuse this while any Machine, MachineSet or ControlPlaneMachineSet still references
the failure domain you are removing. That protection is why Steps 8 and 9 come first.

**b) Remove the source credentials** from `vsphere-creds` (`vcenter1.example.com.username` /
`.password`), and confirm the copies the cluster makes were refreshed:

```bash
oc get secret vsphere-cloud-credentials -n openshift-machine-api -o json | jq '.data | keys'
oc get secret vsphere-cloud-credentials -n openshift-cloud-controller-manager -o json | jq '.data | keys'
```

**c) Wait for the change to propagate** (as in Step 4), then look at the generated configuration:

```bash
oc get co machine-config cloud-controller-manager machine-api storage
oc get cm cloud-conf -n openshift-cloud-controller-manager -o jsonpath='{.data.cloud\.conf}' | yq
```

The `labels:` section is only written into the generated configuration while more than one failure
domain exists. After this step it may disappear — if it remains, it was inherited from the source
`cloud-provider-config` and is harmless. Either way it is not a fault.

**d) If the old vCenter is still listed in the generated configuration**, remove only its entry from the
source ConfigMap and let it regenerate:

```bash
oc get cm cloud-provider-config -n openshift-config -o yaml > cloud-provider-config-backup.yaml
oc get cm cloud-provider-config -n openshift-config -o json | jq '.data | keys'
```

The source ConfigMap keeps the vCenter entry the cluster was installed with; on releases that do not
rebuild the list from `Infrastructure`, it survives in the generated configuration. Delete that one
entry under `vcenter:` — leave `global:`
(the credentials secret lives there), `labels:` and `nodes:` untouched — then re-check the generated
configuration. If the old vCenter is already gone, nothing to do.

**e) Retire the source outside the cluster** — DNS entries, load balancer members, backup and
monitoring jobs that still point at `vcenter1.example.com`. Keep the source VMs and templates until
Step 10 has passed.

---

## Step 10: Verify, and turn storage back on

```bash
# Only the new vCenter remains
oc get infrastructure cluster -o json | jq '.spec.platformSpec.vsphere.vcenters'
oc get infrastructure cluster -o json | jq '.spec.platformSpec.vsphere.failureDomains'

# Every machine is in the target zone
oc get machines -n openshift-machine-api \
  -o custom-columns='NAME:.metadata.name,ZONE:.metadata.labels.machine\.openshift\.io/zone'

# Node topology labels match the tag values from Step 1
oc get nodes \
  -o custom-columns='NAME:.metadata.name,ZONE:.metadata.labels.topology\.kubernetes\.io/zone,REGION:.metadata.labels.topology\.kubernetes\.io/region'

# node providerID is vsphere://<bios-uuid> and carries no vCenter name — use the Machine labels above

# Healthy
oc get co
oc get nodes
oc get mcp       # every pool: UPDATED=True, UPDATING=False, DEGRADED=False
oc get clusteroperators -o json | jq -r '.items[] | select(.status.conditions[] | select(.type=="Degraded" and .status=="True")) | .metadata.name'
```

Prove the workload actually lands on the new vCenter — a plain deployment proves nothing:

```bash
oc create deployment test-multi-vcenter \
  --image=registry.access.redhat.com/ubi9/ubi-minimal:latest --replicas=4 -- sleep infinity

oc get pods -o wide -l app=test-multi-vcenter
oc get pods -l app=test-multi-vcenter -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' \
  | sort -u | xargs oc get nodes -o json | jq -r '.items[].metadata.labels["topology.kubernetes.io/zone"]' | sort -u

oc delete deployment test-multi-vcenter
```

Then re-enable what you disabled in Before you start:

```bash
oc patch clustercsidriver csi.vsphere.vmware.com --type=merge -p '{"spec":{"managementState":"Managed"}}'
oc patch storage cluster --type=merge -p '{"spec":{"managementState":"Managed"}}'
```

Re-create your MachineHealthChecks and autoscalers. Confirm storage reconciles (`oc get co storage`)
and that a test PVC binds and is writable from a node on the new vCenter.

Once you are past the rollback window, delete the backups that contain credentials:
`vsphere-creds-backup.yaml` and any `source-machinesets/` dumps you no longer need. They hold
recoverable credentials and do not belong on a shared workstation.

---

## Rollback

Steps 2–9 are reversible in reverse order. Each `apply` is declarative — wait for the resulting
rollout before starting the next one. Once Step 9 is done, "rollback" means running Steps 2–4 again to
re-add the source vCenter, then re-creating the source worker MachineSets.

1. **Control plane:** restore the failure domains from `cpms-backup.yaml` — but keep `spec.state:
   Active` and `spec.strategy.type: RollingUpdate`; the API forbids changing `state` once it has been
   `Active` ("state cannot be changed once Active"), so a backup taken while `Inactive` is not
   re-appliable as-is. Wait for the status to settle. With `OnDelete` nothing moves until you delete
   Machines yourself.
2. **Workers:** first bring the source workers back — if Step 8 deleted the source MachineSets,
   re-apply the saved YAMLs from `source-machinesets/` (strip `status`, `metadata.resourceVersion`,
   `metadata.uid`, `metadata.creationTimestamp` first, as in Step 5; the backups carry the original
   replica counts because they were taken before the scale-down), and wait for their machines and
   nodes to be Ready. Only then scale the target MachineSets to 0, wait for their machines **and
   nodes** to be gone, and delete them.
3. **Credentials:** restore `vsphere-creds-backup.yaml`. If Step 9d removed the old vCenter's entry
   from `cloud-provider-config`, restore `cloud-provider-config-backup.yaml` too. When re-applying any
   backup, strip `status`, `metadata.resourceVersion`, `metadata.uid`, `metadata.creationTimestamp`
   first, as in Step 5.
4. **Infrastructure:** `oc apply -f infrastructure-backup.yaml`. This is refused while any Machine,
   MachineSet or ControlPlaneMachineSet still references the failure domain being removed.
5. **Machine config pools:** wait for them to converge again before doing anything else with the
   control plane.

---

## Troubleshooting

**The `Infrastructure` update is rejected**

See the rejection table in Step 3. If the error is `vcenters cannot be added or removed once set`, the
feature gate is not enabled — return to Before you start; there is no supported bypass.

**The new vCenter never appears in the generated cloud config**

- Confirm the `Infrastructure` edit persisted, and that the cloud controller manager operator is healthy
  (`oc get co cloud-controller-manager`).
- Do not edit `cloud-conf`; it is regenerated from `Infrastructure` and `cloud-provider-config`.

**The cloud controller manager or Machine API cannot authenticate to the new vCenter**

- `vsphere-creds` keys are `<server>.username` / `<server>.password`, with `<server>` spelled exactly as
  in the `Infrastructure` resource.
- Check the copies: `vsphere-cloud-credentials` in `openshift-machine-api` and
  `openshift-cloud-controller-manager`.
- `insecureFlag` belongs in `cloud-provider-config`, not in `cloud-conf`.

**Nodes on the new vCenter have no zone or region label**

- The region tag must be attached to the **datacenter**, the zone tag to the **compute cluster**, under
  the exact category names `openshift-region` and `openshift-zone`.
- Tag values must equal the failure domain's `region` / `zone` strings.
- The labels only appear in the generated configuration while more than one failure domain exists.

**Machines on the new vCenter are stuck in `Provisioning`**

- `networkName` is a path instead of a port group name.
- Template path wrong, or the template is missing in that datacenter.
- `workspace.*` values were copied from the source MachineSet instead of the target failure domain.
- Credentials for the new vCenter missing from `vsphere-creds`.

**The control plane rollout does not start**

- `spec.strategy.type` is `OnDelete` — set `RollingUpdate`.
- `spec.state` is `Inactive`.
- Machine config pools are not settled (Step 6).

**The control plane rollout stalls part-way**

- A `Degraded`/`Error` condition on the ControlPlaneMachineSet does not clear by itself. Investigate the
  machine that is stuck, the Control Plane Machine Set operator, and etcd. Do not continue the migration
  until the rollout completes.

**`machine-config` is degraded after the `Infrastructure` change**

- The machine config operator picks up the new infrastructure on its own; nothing has to be deleted or
  restarted for that. Check the operator pods and log, and if it will not recover, back out the
  `Infrastructure` change rather than deleting anything.

---

## Appendix: if you use `vcf-migration-operator`

This runbook is the manual equivalent of what `vcf-migration-operator` automates. If the operator is
installed, it performs the same sequence — credentials, `Infrastructure` and cloud configuration, pod
restarts, worker MachineSets, control plane replacement, source removal — from a
`VmwareCloudFoundationMigration` resource and reports progress through conditions. Use the operator
instead of this document when it is available; this document is for clusters that do not run it.
