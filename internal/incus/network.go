package incus

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"

	"github.com/lxc/incus/v7/shared/api"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	bridgeMTUKey       = "bridge.mtu"
	defaultEthernetMTU = 1500
)

var errBridgeCollision = errors.New("bridge name is already owned")

// ListNetworks returns agent-facing networks owned by the sandbox.
//
// Ownership is resolved from network metadata and project reservations, never
// by parsing physical names.
func (c *Client) ListNetworks(ctx context.Context, sandbox string) ([]compute.Network, error) {
	project, _, err := c.getProject(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	return c.listNetworks(ctx, sandbox, project)
}

// CreateNetwork defines a bridge in the default project or an OVN network in the sandbox project.
func (c *Client) CreateNetwork(ctx context.Context, sandbox string, network compute.Network) (compute.Network, error) {
	project, _, err := c.getProject(ctx, sandbox)
	if err != nil {
		return compute.Network{}, err
	}
	fabric := networkKindBridge
	if featuresNetworks(project) {
		fabric = networkKindOVN
	}
	kind := network.Kind
	if kind == "" {
		kind = fabric
	}
	if kind != fabric {
		return compute.Network{}, fmt.Errorf("cannot create a %s network in a %s sandbox", kind, fabric)
	}
	if kind == networkKindOVN {
		return c.createOVNNetwork(ctx, sandbox, network)
	}
	if kind != networkKindBridge {
		return compute.Network{}, fmt.Errorf("network kind %q is not available", kind)
	}
	logical := network.Name
	if logical == "" {
		return compute.Network{}, errors.New("network name is required")
	}

	host := project.Config[metaHost]
	if host == "" {
		host = c.host
	}

	physical := reservedNetworks(project.Config)[logical]
	if physical == "" {
		physical, err = c.allocatePhysicalName(ctx)
		if err != nil {
			return compute.Network{}, err
		}
		if reserveErr := c.reserveNetwork(ctx, sandbox, logical, physical); reserveErr != nil {
			return compute.Network{}, reserveErr
		}
	}

	network.Host = host
	network.Kind = networkKindBridge
	physical, err = c.createReservedBridge(
		ctx,
		physical,
		sandbox,
		logical,
		network,
		project.Config[networkKey(logical)] != "",
	)
	if err != nil {
		return compute.Network{}, err
	}
	return c.observedNetwork(ctx, logical, physical, host)
}

// AttachNIC adds a nic device on a metadata-resolved network.
func (c *Client) AttachNIC(ctx context.Context, ref compute.Ref, network, nic, ip, mac string) (compute.NIC, error) {
	if ref.Sandbox == "" || ref.Name == "" {
		return compute.NIC{}, errors.New("instance reference is required")
	}
	physical, err := c.resolvePhysical(ctx, ref.Sandbox, network)
	if err != nil {
		return compute.NIC{}, err
	}

	srv := c.Scoped(ctx, projectName(ref.Sandbox), "")
	instance, etag, err := srv.GetInstance(ref.Name)
	if err != nil {
		return compute.NIC{}, mapError(err)
	}

	devices := copyDevices(instance.Devices)
	nicName := nic
	if nicName == "" {
		nicName = nextNICName(devices)
	}
	if _, exists := devices[nicName]; exists {
		return compute.NIC{}, fmt.Errorf("device %q already exists", nicName)
	}
	device := map[string]string{
		deviceTypeKey:    deviceTypeNIC,
		deviceNetworkKey: physical,
		"name":           nicName,
	}
	if ip != "" {
		device[ipv4AddressKey] = ip
	}
	if mac != "" {
		device["hwaddr"] = mac
	}
	devices[nicName] = device
	instance.Devices = devices

	op, err := srv.UpdateInstance(ref.Name, instance.Writable(), etag)
	if err != nil {
		return compute.NIC{}, mapError(err)
	}
	if waitErr := waitOp(ctx, op); waitErr != nil {
		return compute.NIC{}, waitErr
	}

	observed, err := c.GetInstance(ctx, ref)
	if err != nil {
		return compute.NIC{}, err
	}
	attached, ok := nicByName(observed.NICs, nicName)
	if !ok {
		return compute.NIC{}, fmt.Errorf("attached NIC %q was not observed", nicName)
	}
	if configureErr := c.configureWindowsNICs(ctx, observed); configureErr != nil {
		return compute.NIC{}, configureErr
	}
	if !windowsGuest(observed.OS) || !instanceRunningStatus(observed.Status) {
		return attached, nil
	}
	observed, err = c.GetInstance(ctx, ref)
	if err != nil {
		return compute.NIC{}, err
	}
	attached, ok = nicByName(observed.NICs, nicName)
	if !ok {
		return compute.NIC{}, fmt.Errorf("attached NIC %q was not observed", nicName)
	}
	return attached, nil
}

// GetNetwork returns one owned agent-facing network.
func (c *Client) GetNetwork(ctx context.Context, sandbox, name string) (compute.Network, error) {
	networks, err := c.ListNetworks(ctx, sandbox)
	if err != nil {
		return compute.Network{}, err
	}
	for _, network := range networks {
		if network.Name == name {
			return network, nil
		}
	}
	return compute.Network{}, compute.ErrNotFound
}

// DeleteNetwork removes an owned network after dependents are gone.
func (c *Client) DeleteNetwork(ctx context.Context, sandbox, name string) error {
	network, err := c.GetNetwork(ctx, sandbox, name)
	if err != nil {
		return err
	}
	if network.Kind == networkKindOVN {
		return c.deleteOVNNetwork(ctx, sandbox, network.PhysicalName)
	}
	if err := c.checkBridgeOwnership(ctx, sandbox, network.PhysicalName); err != nil {
		return err
	}
	if errs := c.deleteForwards(ctx, network.PhysicalName); len(errs) > 0 {
		return errors.Join(errs...)
	}
	return c.deleteBridge(ctx, network.PhysicalName)
}

// DetachNIC removes a NIC device from an instance.
func (c *Client) DetachNIC(ctx context.Context, ref compute.Ref, nic string) error {
	if ref.Sandbox == "" || ref.Name == "" || nic == "" {
		return errors.New("instance reference and nic are required")
	}
	srv := c.Scoped(ctx, projectName(ref.Sandbox), "")
	instance, etag, err := srv.GetInstance(ref.Name)
	if err != nil {
		return mapError(err)
	}
	devices := copyDevices(instance.Devices)
	if _, exists := devices[nic]; !exists {
		found := false
		for name, device := range devices {
			if device[deviceTypeKey] == deviceTypeNIC && device["name"] == nic {
				delete(devices, name)
				found = true
				break
			}
		}
		if !found {
			return compute.ErrNotFound
		}
	} else {
		delete(devices, nic)
	}
	instance.Devices = devices
	op, err := srv.UpdateInstance(ref.Name, instance.Writable(), etag)
	if err != nil {
		return mapError(err)
	}
	return waitOp(ctx, op)
}

func (c *Client) listNetworks(ctx context.Context, sandbox string, project *api.Project) ([]compute.Network, error) {
	host := c.host
	if project != nil && project.Config[metaHost] != "" {
		host = project.Config[metaHost]
	}

	out := make([]compute.Network, 0)
	seen := map[string]struct{}{}
	if featuresNetworks(project) {
		ovn, err := c.ownedOVNNetworks(ctx, sandbox)
		if err != nil {
			return nil, err
		}
		for _, network := range ovn {
			seen[network.Name] = struct{}{}
			out = append(out, network)
		}
	}

	networks, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetNetworks()
	if err != nil {
		return nil, mapError(err)
	}
	for _, network := range networks {
		logical := network.Config[metaName]
		if network.Config[metaSandbox] != sandbox || network.Config[metaVersion] != versionValue || logical == "" {
			continue
		}
		if _, ok := seen[logical]; ok {
			continue
		}
		out = append(out, networkFromAPI(network, logical, host, api.ProjectDefaultName))
	}
	return out, nil
}

func (c *Client) ownedBridges(ctx context.Context, sandbox string) ([]compute.Network, error) {
	project, _, err := c.getProject(ctx, sandbox)
	if err != nil && !errors.Is(err, compute.ErrNotFound) {
		return nil, err
	}
	return c.listDefaultBridges(ctx, sandbox, project)
}

func (c *Client) listDefaultBridges(
	ctx context.Context,
	sandbox string,
	project *api.Project,
) ([]compute.Network, error) {
	host := c.host
	if project != nil && project.Config[metaHost] != "" {
		host = project.Config[metaHost]
	}
	networks, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetNetworks()
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]compute.Network, 0)
	for _, network := range networks {
		logical := network.Config[metaName]
		if network.Config[metaSandbox] != sandbox || network.Config[metaVersion] != versionValue || logical == "" {
			continue
		}
		out = append(out, networkFromAPI(network, logical, host, api.ProjectDefaultName))
	}
	return out, nil
}

func (c *Client) resolvePhysical(ctx context.Context, sandbox, logical string) (string, error) {
	if logical == "" {
		logical = defaultLogicalNetwork
	}
	project, _, err := c.getProject(ctx, sandbox)
	if err != nil {
		return "", err
	}
	if featuresNetworks(project) {
		ovns, ovnErr := c.ownedOVNNetworks(ctx, sandbox)
		if ovnErr != nil {
			return "", ovnErr
		}
		for _, network := range ovns {
			if network.Name == logical {
				return network.PhysicalName, nil
			}
		}
		return "", compute.ErrNotFound
	}
	networks, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetNetworks()
	if err != nil {
		return "", mapError(err)
	}
	for _, network := range networks {
		if network.Config[metaVersion] != versionValue {
			continue
		}
		if network.Config[metaSandbox] == sandbox && network.Config[metaName] == logical {
			return network.Name, nil
		}
	}
	return "", compute.ErrNotFound
}

func (c *Client) reserveNetwork(ctx context.Context, sandbox, logical, physical string) error {
	return c.patchProject(ctx, sandbox, func(project *api.Project) {
		if project.Config == nil {
			project.Config = map[string]string{}
		}
		previous := project.Config[networkKey(logical)]
		project.Config[networkKey(logical)] = physical
		access := splitCSV(project.Config["restricted.networks.access"])
		kept := access[:0]
		for _, name := range access {
			if name != previous {
				kept = append(kept, name)
			}
		}
		project.Config["restricted.networks.access"] = joinCSV(append(kept, physical))
	})
}

func (c *Client) allocatePhysicalName(ctx context.Context) (string, error) {
	srv := c.Scoped(ctx, api.ProjectDefaultName, "")
	for range physicalNameTries {
		name, err := randomPhysicalName()
		if err != nil {
			return "", err
		}
		_, _, err = srv.GetNetwork(name)
		if err != nil {
			if errors.Is(mapError(err), compute.ErrNotFound) {
				return name, nil
			}
			return "", mapError(err)
		}
	}
	return "", errors.New("allocate bridge name")
}

func (c *Client) createReservedBridge(
	ctx context.Context,
	physical, sandbox, logical string,
	network compute.Network,
	resume bool,
) (string, error) {
	for range physicalNameTries {
		err := c.ensureBridge(ctx, physical, sandbox, logical, network, resume)
		if !errors.Is(err, errBridgeCollision) {
			return physical, err
		}
		physical, err = c.allocatePhysicalName(ctx)
		if err != nil {
			return "", err
		}
		if err := c.reserveNetwork(ctx, sandbox, logical, physical); err != nil {
			return "", err
		}
		resume = false
	}
	return "", errBridgeCollision
}

func (c *Client) ensureBridge(
	ctx context.Context,
	physical, sandbox, logical string,
	network compute.Network,
	resume bool,
) error {
	server := c.Scoped(ctx, api.ProjectDefaultName, "")
	if resume {
		ready, err := c.existingBridge(ctx, physical, sandbox, logical)
		if err != nil || ready {
			return err
		}
	}
	members, err := c.members(ctx)
	if err != nil {
		return err
	}
	definition := api.NetworksPost{Name: physical, Type: networkKindBridge}
	for index, member := range members {
		err := c.Scoped(ctx, api.ProjectDefaultName, member).CreateNetwork(definition)
		if err == nil {
			continue
		}
		if index == 0 && !resume && isConflict(err) {
			return errBridgeCollision
		}
		if resume && isConflict(err) {
			continue
		}
		return mapError(err)
	}
	return mapError(server.CreateNetwork(api.NetworksPost{
		Name:       physical,
		Type:       networkKindBridge,
		NetworkPut: api.NetworkPut{Config: bridgeConfig(sandbox, logical, network)},
	}))
}

func (c *Client) existingBridge(ctx context.Context, physical, sandbox, logical string) (bool, error) {
	existing, _, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetNetwork(physical)
	if errors.Is(mapError(err), compute.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, mapError(err)
	}
	if existing.Status == api.NetworkStatusCreated && existing.Config[metaSandbox] == sandbox &&
		existing.Config[metaName] == logical && existing.Config[metaVersion] == versionValue {
		return true, nil
	}
	if existing.Status != api.NetworkStatusPending || existing.Config[metaSandbox] != "" {
		return false, errBridgeCollision
	}
	return false, c.checkBridgeOwnership(ctx, sandbox, physical)
}

func (c *Client) checkBridgeOwnership(ctx context.Context, sandbox, physical string) error {
	server := c.Scoped(ctx, api.ProjectDefaultName, "")
	network, _, err := server.GetNetwork(physical)
	if errors.Is(mapError(err), compute.ErrNotFound) {
		return nil
	}
	if err != nil {
		return mapError(err)
	}
	if network.Type == networkKindBridge && network.Config[metaSandbox] == sandbox &&
		network.Config[metaVersion] == versionValue {
		return nil
	}
	if network.Type == networkKindBridge && network.Status == api.NetworkStatusPending &&
		network.Config[metaSandbox] == "" {
		owner, err := c.pendingBridgeOwner(ctx, physical)
		if err != nil {
			return err
		}
		if owner == projectName(sandbox) {
			return nil
		}
	}
	return fmt.Errorf("%w: refusing to modify bridge %q not owned by sandbox %q", errBridgeCollision, physical, sandbox)
}

func (c *Client) pendingBridgeOwner(ctx context.Context, physical string) (string, error) {
	projects, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetProjects()
	if err != nil {
		return "", mapError(err)
	}
	owner := ""
	for _, project := range projects {
		for _, reserved := range reservedNetworks(project.Config) {
			if reserved != physical {
				continue
			}
			if owner != "" && owner != project.Name {
				return "", fmt.Errorf(
					"%w: pending bridge %q has conflicting project reservations",
					errBridgeCollision,
					physical,
				)
			}
			owner = project.Name
		}
	}
	return owner, nil
}

func (c *Client) deleteForwards(ctx context.Context, physical string) []error {
	srv := c.Scoped(ctx, api.ProjectDefaultName, "")
	forwards, err := srv.GetNetworkForwards(physical)
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return nil
		}
		return []error{mapError(err)}
	}
	var errs []error
	for _, forward := range forwards {
		if err := srv.DeleteNetworkForward(
			physical,
			forward.ListenAddress,
		); err != nil &&
			!errors.Is(mapError(err), compute.ErrNotFound) {
			errs = append(errs, mapError(err))
		}
	}
	return errs
}

func (c *Client) deleteBridge(ctx context.Context, physical string) error {
	err := c.Scoped(ctx, api.ProjectDefaultName, "").DeleteNetwork(physical)
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		return mapError(err)
	}
	return nil
}

func (c *Client) observedNetwork(ctx context.Context, logical, physical, host string) (compute.Network, error) {
	network, _, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetNetwork(physical)
	if err != nil {
		return compute.Network{}, mapError(err)
	}
	return networkFromAPI(*network, logical, host, api.ProjectDefaultName), nil
}

func bridgeConfig(sandbox, logical string, network compute.Network) map[string]string {
	address := configNone
	switch {
	case network.Gateway != "" && strings.Contains(network.Gateway, "/"):
		address = network.Gateway
	case network.CIDR != "":
		address = network.CIDR
	case network.DHCP || network.NAT || network.DNS:
		address = addressAuto
	}
	dnsMode := configNone
	if network.DNS {
		dnsMode = configManaged
	}
	return map[string]string{
		ipv4AddressKey: address,
		ipv4NATKey:     strconv.FormatBool(network.NAT),
		ipv4DHCPKey:    strconv.FormatBool(network.DHCP),
		"ipv6.address": configNone,
		dnsModeKey:     dnsMode,
		metaSandbox:    sandbox,
		metaName:       logical,
		metaVersion:    versionValue,
	}
}

func networkFromAPI(network api.Network, logical, host, project string) compute.Network {
	cidr := network.Config[ipv4AddressKey]
	kind := network.Type
	if kind == "" {
		kind = networkKindBridge
	}
	if project == "" {
		project = api.ProjectDefaultName
	}
	return compute.Network{
		Name:         logical,
		PhysicalName: network.Name,
		Project:      project,
		Kind:         kind,
		CIDR:         cidr,
		Gateway:      gatewayIP(cidr),
		Host:         host,
		DHCP:         isTrue(network.Config[ipv4DHCPKey]),
		NAT:          isTrue(network.Config[ipv4NATKey]),
		DNS:          network.Config[dnsModeKey] != configNone,
	}
}

func gatewayIP(cidr string) string {
	if cidr == "" || cidr == addressAuto || cidr == configNone {
		return ""
	}
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return cidr
	}
	return ip.String()
}

func copyDevices(in map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(in))
	for name, device := range in {
		copied := make(map[string]string, len(device))
		maps.Copy(copied, device)
		out[name] = copied
	}
	return out
}

func nextNICName(devices map[string]map[string]string) string {
	used := map[string]struct{}{}
	for name, device := range devices {
		used[name] = struct{}{}
		if iface := device["name"]; iface != "" {
			used[iface] = struct{}{}
		}
	}
	if _, ok := used[defaultNICName]; !ok {
		return defaultNICName
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("eth%d", i)
		if _, ok := used[candidate]; !ok {
			return candidate
		}
	}
}

func nicByName(nics []compute.NIC, name string) (compute.NIC, bool) {
	for _, nic := range nics {
		if nic.Name == name {
			return nic, true
		}
	}
	return compute.NIC{}, false
}

func (c *Client) ownedNetwork(ctx context.Context, sandbox, logical string) (*api.Network, error) {
	if logical == "" {
		return nil, compute.ErrNotFound
	}
	project, _, err := c.getProject(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	if featuresNetworks(project) {
		network, networkErr := c.projectNetwork(ctx, sandbox, logical)
		if networkErr != nil {
			return nil, networkErr
		}
		name := network.Config[metaName]
		if name == "" {
			name = network.Name
		}
		if network.Config[metaSandbox] != sandbox || network.Config[metaVersion] != versionValue || name != logical {
			return nil, compute.ErrNotFound
		}
		return network, nil
	}
	networks, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetNetworks()
	if err != nil {
		return nil, mapError(err)
	}
	for i := range networks {
		network := &networks[i]
		if network.Config[metaVersion] != versionValue {
			continue
		}
		if network.Config[metaSandbox] == sandbox && network.Config[metaName] == logical {
			return network, nil
		}
	}
	return nil, compute.ErrNotFound
}

func parseNetworkMTU(config map[string]string) (int, error) {
	value := strings.TrimSpace(config[bridgeMTUKey])
	if value == "" {
		// Incus bridgeMTUDefault when bridge.mtu is unset on an ordinary bridge.
		return defaultEthernetMTU, nil
	}
	mtu, err := strconv.Atoi(value)
	if err != nil || mtu <= 0 {
		return 0, fmt.Errorf("invalid %s %q", bridgeMTUKey, value)
	}
	return mtu, nil
}
