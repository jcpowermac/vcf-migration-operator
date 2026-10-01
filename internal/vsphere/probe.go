package vsphere

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	// defaultProbeIgnition is the minimal ignition document that lets an RHCOS
	// guest boot without a real cluster config. The guest only needs to bring
	// up its network and run open-vm-tools so the probe can report its IP.
	defaultProbeIgnition = `{"ignition":{"version":"3.4.0"},"passwd":{},"storage":{"files":[]},"systemd":{"units":[]},"networkd":{"units":[]}}`

	// probeGuestPollInterval is how often WaitForGuestNetworks polls guest info.
	probeGuestPollInterval = 5 * time.Second

	// guestToolsRunning is the GuestInfo.toolsRunningStatus value that
	// indicates guest tools are running.
	guestToolsRunning = "guestToolsRunning"
)

// ProbeSpec describes how to create a probe VM on a destination failure domain.
type ProbeSpec struct {
	// Name is the deterministic probe VM name.
	Name string
	// Datacenter is the destination datacenter (FD topology).
	Datacenter string
	// Cluster is the compute cluster the probe is placed in.
	Cluster string
	// Datastore is the datastore to clone onto.
	Datastore string
	// ResourcePool is the resource pool; empty selects the cluster root pool.
	ResourcePool string
	// Folder is the VM folder; empty selects the datacenter VM folder.
	Folder string
	// Template is the template VM to clone.
	Template string
	// Network is the single network to attach the probe NIC to.
	Network string
	// Ignition is the ignition document; empty selects defaultProbeIgnition.
	Ignition string
	// NetworkKargs, when set, is passed to the guest as network kargs.
	NetworkKargs string
	// ExtraConfig carries additional guestinfo keys from the migration spec.
	ExtraConfig map[string]string
}

// NetworkInfo is one observed IPv4 address with its network context.
type NetworkInfo struct {
	// IP is the IPv4 address.
	IP string
	// Prefix is the CIDR prefix length.
	Prefix int
	// Gateway is the default gateway.
	Gateway string
}

// CreateProbeVM clones spec.Template into the failure domain described by spec,
// powered on, with the probe extra config and a single NIC on spec.Network.
func (s *Session) CreateProbeVM(ctx context.Context, spec ProbeSpec) (*object.VirtualMachine, error) {
	template, err := s.Finder.VirtualMachine(ctx, spec.Template)
	if err != nil {
		return nil, fmt.Errorf("looking up template %s: %w", spec.Template, err)
	}
	folder, err := s.probeFolder(ctx, spec)
	if err != nil {
		return nil, err
	}
	datastore, err := s.Finder.Datastore(ctx, spec.Datastore)
	if err != nil {
		return nil, fmt.Errorf("looking up datastore %s: %w", spec.Datastore, err)
	}
	pool, err := s.probeResourcePool(ctx, spec)
	if err != nil {
		return nil, err
	}
	network, err := s.Finder.Network(ctx, spec.Network)
	if err != nil {
		return nil, fmt.Errorf("looking up network %s: %w", spec.Network, err)
	}
	nicChanges, err := singleNICDeviceChange(ctx, template, network)
	if err != nil {
		return nil, err
	}

	folderRef := folder.Reference()
	datastoreRef := datastore.Reference()
	cloneSpec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Folder:       &folderRef,
			Datastore:    &datastoreRef,
			Pool:         pool,
			DiskMoveType: string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndDisallowSharing),
		},
		PowerOn: true,
		Config: &types.VirtualMachineConfigSpec{
			Name:         spec.Name,
			ExtraConfig:  probeExtraConfigValues(spec),
			DeviceChange: nicChanges,
		},
	}

	task, err := template.Clone(ctx, folder, spec.Name, cloneSpec)
	if err != nil {
		return nil, fmt.Errorf("cloning template %s as %s: %w", spec.Template, spec.Name, err)
	}
	taskInfo, err := task.WaitForResult(ctx)
	if err != nil {
		return nil, fmt.Errorf("cloning template %s as %s: %w", spec.Template, spec.Name, err)
	}
	result, ok := taskInfo.Result.(types.ManagedObjectReference)
	if !ok {
		return nil, fmt.Errorf("cloning template %s: unexpected clone result", spec.Template)
	}
	return object.NewVirtualMachine(s.Client.Client, result), nil
}

// probeFolder resolves spec.Folder or the datacenter default VM folder.
func (s *Session) probeFolder(ctx context.Context, spec ProbeSpec) (*object.Folder, error) {
	if spec.Folder != "" {
		folder, err := s.Finder.Folder(ctx, spec.Folder)
		if err != nil {
			return nil, fmt.Errorf("looking up folder %s: %w", spec.Folder, err)
		}
		return folder, nil
	}
	dc, err := s.Finder.Datacenter(ctx, spec.Datacenter)
	if err != nil {
		return nil, fmt.Errorf("looking up datacenter %s: %w", spec.Datacenter, err)
	}
	folders, err := dc.Folders(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading folders for datacenter %s: %w", spec.Datacenter, err)
	}
	return folders.VmFolder, nil
}

// probeResourcePool resolves spec.ResourcePool or the cluster root resource
// pool, which vSphere names "<cluster>/Resources".
func (s *Session) probeResourcePool(ctx context.Context, spec ProbeSpec) (*types.ManagedObjectReference, error) {
	name := spec.ResourcePool
	if name == "" {
		name = spec.Cluster + "/Resources"
	}
	pool, err := s.Finder.ResourcePool(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("looking up resource pool %s: %w", name, err)
	}
	ref := pool.Reference()
	return &ref, nil
}

// probeExtraConfigValues builds the probe extra config entries: base64 ignition,
// hostname, stealclock, optional network kargs, and any caller extra config.
func probeExtraConfigValues(spec ProbeSpec) []types.BaseOptionValue {
	ignition := spec.Ignition
	if ignition == "" {
		ignition = defaultProbeIgnition
	}
	values := map[string]string{
		"guestinfo.ignition.config.data":          base64.StdEncoding.EncodeToString([]byte(ignition)),
		"guestinfo.ignition.config.data.encoding": "base64",
		"guestinfo.hostname":                      spec.Name,
		"stealclock.enable":                       "TRUE",
	}
	if spec.NetworkKargs != "" {
		values["guestinfo.afterburn.initrd.network-kargs"] = spec.NetworkKargs
	}
	for key, value := range spec.ExtraConfig {
		values[key] = value
	}
	options := make([]types.BaseOptionValue, 0, len(values))
	for key, value := range values {
		options = append(options, &types.OptionValue{Key: key, Value: value})
	}
	return options
}

// singleNICDeviceChange returns the device changes that leave the clone with a
// single network adapter backed by network: the template's first ethernet card
// (any subtype) is rewired to network and any additional ethernet cards are
// removed. When the template has no ethernet card, one is added.
func singleNICDeviceChange(ctx context.Context, template *object.VirtualMachine, network object.NetworkReference) ([]types.BaseVirtualDeviceConfigSpec, error) {
	devices, err := template.Device(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading template devices: %w", err)
	}
	backing, err := network.EthernetCardBackingInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading backing for network %s: %w", network.Reference().Value, err)
	}
	// EthernetCardBackingInfo leaves the Network reference unset for standard
	// networks; set it so real vCenters do not rely on the deprecated
	// deviceName field alone.
	if std, ok := backing.(*types.VirtualEthernetCardNetworkBackingInfo); ok && std.Network == nil {
		ref := network.Reference()
		std.Network = &ref
	}
	var changes []types.BaseVirtualDeviceConfigSpec
	found := false
	for _, base := range devices {
		nic, ok := base.(types.BaseVirtualEthernetCard)
		if !ok {
			continue
		}
		card := nic.GetVirtualEthernetCard()
		if !found {
			// A partial edit is applied by the server against the existing
			// device of the same key, preserving the concrete adapter type.
			changes = append(changes, &types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationEdit,
				Device: &types.VirtualEthernetCard{
					VirtualDevice: types.VirtualDevice{
						Key:     card.Key,
						Backing: backing,
					},
				},
			})
			found = true
			continue
		}
		changes = append(changes, &types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationRemove,
			Device:    base,
		})
	}
	if !found {
		changes = append(changes, &types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationAdd,
			Device: &types.VirtualEthernetCard{
				VirtualDevice: types.VirtualDevice{
					Key:     nextDeviceKey(devices),
					Backing: backing,
				},
			},
		})
	}
	return changes, nil
}

// nextDeviceKey returns a device key that does not collide with existing keys.
func nextDeviceKey(devices object.VirtualDeviceList) int32 {
	var maxKey int32
	for _, base := range devices {
		if key := base.GetVirtualDevice().Key; key > maxKey {
			maxKey = key
		}
	}
	return maxKey + 1000
}

// guestInfo reads one guest info snapshot of vm.
func guestInfo(ctx context.Context, vm *object.VirtualMachine) (*types.GuestInfo, error) {
	var moVM mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"guest"}, &moVM); err != nil {
		return nil, err
	}
	return moVM.Guest, nil
}

// WaitForGuestNetworks polls vm guest info until guest tools report running and
// at least one IPv4 address with a default gateway is present, then returns all
// observed IPv4 networks.
func WaitForGuestNetworks(ctx context.Context, vm *object.VirtualMachine) ([]NetworkInfo, error) {
	ticker := time.NewTicker(probeGuestPollInterval)
	defer ticker.Stop()
	for {
		networks, ready, err := readGuestNetworks(ctx, vm)
		if err != nil {
			return nil, fmt.Errorf("reading guest info for %s: %w", vm.Name(), err)
		}
		if ready {
			return networks, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for guest networks on %s: %w (the template must run open-vm-tools and the network must provide DHCP or static network kargs)", vm.Name(), ctx.Err())
		case <-ticker.C:
		}
	}
}

// readGuestNetworks reads one guest info snapshot and reports whether the guest
// is network-ready (tools running with at least one IPv4 default gateway).
func readGuestNetworks(ctx context.Context, vm *object.VirtualMachine) ([]NetworkInfo, bool, error) {
	guest, err := guestInfo(ctx, vm)
	if err != nil {
		return nil, false, err
	}
	if guest == nil {
		return nil, false, nil
	}
	if guest.ToolsRunningStatus != guestToolsRunning {
		return nil, false, nil
	}
	networks := networksFromGuest(guest)
	for i := range networks {
		if networks[i].Gateway != "" {
			return networks, true, nil
		}
	}
	return networks, false, nil
}

// networksFromGuest extracts the observed IPv4 networks from a guest info
// snapshot. Addresses come from the NIC ipConfig (with their prefix lengths)
// plus the deprecated flat address list (prefix unknown, zero); the default
// gateway comes from the guest route table.
func networksFromGuest(guest *types.GuestInfo) []NetworkInfo {
	gateway := defaultGateway(guest.IpStack)
	var networks []NetworkInfo
	for _, nic := range guest.Net {
		type addrInfo struct {
			ip     string
			prefix int
		}
		var addrs []addrInfo
		seen := make(map[string]bool)
		if nic.IpConfig != nil {
			for _, ip := range nic.IpConfig.IpAddress {
				addrs = append(addrs, addrInfo{ip: ip.IpAddress, prefix: int(ip.PrefixLength)})
				seen[ip.IpAddress] = true
			}
		}
		for _, ip := range nic.IpAddress {
			if !seen[ip] {
				addrs = append(addrs, addrInfo{ip: ip})
			}
		}
		for _, a := range addrs {
			addr, err := netip.ParseAddr(a.ip)
			if err != nil || !addr.Is4() {
				continue
			}
			networks = append(networks, NetworkInfo{
				IP:      addr.String(),
				Prefix:  a.prefix,
				Gateway: gateway,
			})
		}
	}
	return networks
}

// defaultGateway returns the IPv4 default route gateway from the guest IP
// stacks, or the empty string when no default route is reported.
func defaultGateway(stacks []types.GuestStackInfo) string {
	for _, stack := range stacks {
		if stack.IpRouteConfig == nil {
			continue
		}
		for _, route := range stack.IpRouteConfig.IpRoute {
			if route.Network == "0.0.0.0" && route.PrefixLength == 0 {
				if gw := route.Gateway.IpAddress; gw != "" {
					return gw
				}
			}
		}
	}
	return ""
}

// MatchesAny reports whether n is on the same masked IPv4 subnet with the same
// default gateway as at least one of sources. The returned string describes n.
func (n NetworkInfo) MatchesAny(sources []NetworkInfo) (bool, string) {
	desc := fmt.Sprintf("%s (gateway %s)", n.IP, n.Gateway)
	if n.Prefix == 0 {
		return false, desc
	}
	ip, err := netip.ParseAddr(n.IP)
	if err != nil {
		return false, desc
	}
	probePfx, err := ip.Prefix(n.Prefix)
	if err != nil {
		return false, desc
	}
	probeNet := probePfx.Masked().String()
	desc = fmt.Sprintf("%s (gateway %s)", probeNet, n.Gateway)
	for _, src := range sources {
		if src.Prefix == 0 {
			continue
		}
		srcIP, err := netip.ParseAddr(src.IP)
		if err != nil {
			continue
		}
		srcPfx, err := srcIP.Prefix(src.Prefix)
		if err != nil {
			continue
		}
		if srcPfx.Masked().String() == probeNet && src.Gateway == n.Gateway {
			return true, desc
		}
	}
	return false, desc
}

// GetVMNetworks reads a single snapshot of the named VM's observed IPv4 networks.
func (s *Session) GetVMNetworks(ctx context.Context, vmName string) ([]NetworkInfo, error) {
	vm, err := s.Finder.VirtualMachine(ctx, vmName)
	if err != nil {
		return nil, fmt.Errorf("looking up VM %s: %w", vmName, err)
	}
	guest, err := guestInfo(ctx, vm)
	if err != nil {
		return nil, fmt.Errorf("reading guest info for VM %s: %w", vmName, err)
	}
	if guest == nil {
		return nil, fmt.Errorf("VM %s has no guest info; open-vm-tools may not be installed", vmName)
	}
	if guest.ToolsRunningStatus != guestToolsRunning {
		return nil, fmt.Errorf("guest tools are not running on VM %s", vmName)
	}
	return networksFromGuest(guest), nil
}

// DestroyProbeVM destroys the probe VM, powering it off first when running
// (DestroyTask rejects powered-on VMs) and waiting for the tasks.
func DestroyProbeVM(ctx context.Context, vm *object.VirtualMachine) error {
	state, err := vm.PowerState(ctx)
	if err != nil {
		return fmt.Errorf("reading power state for probe VM %s: %w", vm.Name(), err)
	}
	if state == types.VirtualMachinePowerStatePoweredOn {
		offTask, err := vm.PowerOff(ctx)
		if err != nil {
			return fmt.Errorf("powering off probe VM %s: %w", vm.Name(), err)
		}
		if err := offTask.Wait(ctx); err != nil {
			return fmt.Errorf("powering off probe VM %s: %w", vm.Name(), err)
		}
	}
	task, err := vm.Destroy(ctx)
	if err != nil {
		return fmt.Errorf("destroying probe VM %s: %w", vm.Name(), err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("destroying probe VM %s: %w", vm.Name(), err)
	}
	return nil
}

// ReapProbeVMs destroys every VM whose name starts with namePrefix and returns
// the names destroyed. When no VM matches the prefix, it returns an empty list.
func (s *Session) ReapProbeVMs(ctx context.Context, namePrefix string) ([]string, error) {
	vms, err := s.Finder.VirtualMachineList(ctx, namePrefix+"*")
	if err != nil {
		var notFound *find.NotFoundError
		if !errors.As(err, &notFound) {
			return nil, fmt.Errorf("listing VMs with prefix %q: %w", namePrefix, err)
		}
		return nil, nil
	}
	destroyed := make([]string, 0, len(vms))
	for _, vm := range vms {
		if err := DestroyProbeVM(ctx, vm); err != nil {
			return destroyed, err
		}
		destroyed = append(destroyed, vm.Name())
	}
	return destroyed, nil
}
