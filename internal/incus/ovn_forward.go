package incus

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/lxc/incus/v7/shared/api"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func (c *Client) CreateForward(
	ctx context.Context,
	sandbox, network string,
	ref compute.Ref,
	port, listenPort int64,
	protocol string,
) (compute.Forward, error) {
	ctx, cancel := c.ovnContext(ctx)
	defer cancel()
	if _, err := c.ownedProjectNetwork(ctx, sandbox, network); err != nil {
		return compute.Forward{}, err
	}
	instance, err := c.GetInstance(ctx, ref)
	if err != nil {
		return compute.Forward{}, err
	}
	target, err := c.forwardTargetAddress(ctx, sandbox, network, instance)
	if err != nil {
		return compute.Forward{}, err
	}
	listen := strconv.FormatInt(listenPort, 10)
	targetPort := strconv.FormatInt(port, 10)
	portSpec := api.NetworkForwardPort{
		Protocol:      protocol,
		ListenPort:    listen,
		TargetPort:    targetPort,
		TargetAddress: target,
	}

	srv := c.Scoped(ctx, projectName(sandbox), "")
	forwards, err := srv.GetNetworkForwards(network)
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		return compute.Forward{}, mapOVNError(err)
	}
	if len(forwards) > 0 {
		existing := forwards[0]
		if forwardHasPort(existing, protocol, listen) {
			return compute.Forward{}, fmt.Errorf(
				"listen port %s/%s is already forwarded on %s",
				listen,
				protocol,
				existing.ListenAddress,
			)
		}
		writable := existing.Writable()
		writable.Ports = append(writable.Ports, portSpec)
		if err := srv.UpdateNetworkForward(network, existing.ListenAddress, writable, ""); err != nil {
			return compute.Forward{}, mapOVNError(err)
		}
		return compute.Forward{
			Address:  existing.ListenAddress,
			Port:     listenPort,
			Protocol: protocol,
			Network:  network,
			Instance: ref.Name,
		}, nil
	}

	for {
		address, err := c.allocateForwardAddress(ctx)
		if err != nil {
			return compute.Forward{}, err
		}
		err = srv.CreateNetworkForward(network, api.NetworkForwardsPost{
			ListenAddress: address,
			NetworkForwardPut: api.NetworkForwardPut{
				Ports: []api.NetworkForwardPort{portSpec},
				Config: map[string]string{
					metaSandbox: sandbox,
					metaVersion: versionValue,
					metaName:    network,
				},
			},
		})
		if err == nil {
			return compute.Forward{
				Address: address, Port: listenPort, Protocol: protocol,
				Network: network, Instance: ref.Name,
			}, nil
		}
		// Retry only a confirmed allocation race. A forward already on this
		// network may be an uncertain commit; never create a second one.
		current, inspectErr := srv.GetNetworkForwards(network)
		if inspectErr != nil || len(current) != 0 {
			return compute.Forward{}, mapOVNError(err)
		}
		used, inspectErr := c.usedOVNAddresses(ctx)
		if inspectErr != nil || !used[address] {
			return compute.Forward{}, mapOVNError(err)
		}
	}
}

func (c *Client) forwardTargetAddress(ctx context.Context, sandbox, network string, instance compute.Instance) (string, error) {
	for _, nic := range instance.NICs {
		if nic.Network != network {
			continue
		}
		for _, address := range nic.Addresses {
			ip := net.ParseIP(address)
			if ip != nil && ip.To4() != nil {
				return ip.String(), nil
			}
		}
	}
	leases, err := c.Scoped(ctx, projectName(sandbox), "").GetNetworkLeases(network)
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		return "", mapError(err)
	}
	for _, lease := range leases {
		if lease.Hostname != instance.Ref.Name {
			continue
		}
		ip := net.ParseIP(lease.Address)
		if ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("instance %q has no address on network %q", instance.Ref.Name, network)
}

func (c *Client) allocateForwardAddress(ctx context.Context) (string, error) {
	used, err := c.usedOVNAddresses(ctx)
	if err != nil {
		return "", err
	}
	start, end, err := parseIPv4Range(c.ovnRangeSpec())
	if err != nil {
		return "", err
	}
	for ip := start; ip.IsValid() && !end.Less(ip); ip = ip.Next() {
		addr := ip.String()
		if used[addr] {
			continue
		}
		return addr, nil
	}
	return "", errors.New("no free address remains in the OVN range")
}

func (c *Client) usedOVNAddresses(ctx context.Context) (map[string]bool, error) {
	used := map[string]bool{}
	allocations, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetNetworkAllocationsAllProjects()
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		return nil, mapError(err)
	}
	for _, allocation := range allocations {
		ip, _, parseErr := net.ParseCIDR(allocation.Address)
		if parseErr != nil {
			ip = net.ParseIP(strings.TrimSpace(allocation.Address))
		}
		if ip != nil && ip.To4() != nil {
			used[ip.String()] = true
		}
	}
	networks, err := c.Scoped(ctx, "", "").GetNetworksAllProjects()
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		return nil, mapError(err)
	}
	for _, network := range networks {
		if network.Type != networkKindOVN {
			continue
		}
		if addr := net.ParseIP(network.Config["volatile.network.ipv4.address"]); addr != nil {
			used[addr.String()] = true
		}
	}
	return used, nil
}

// InstanceForward observes the scalar forward shape created by net.forward.
func (c *Client) InstanceForward(ctx context.Context, ref compute.Ref, targetPort int64, protocol string) (compute.Forward, error) {
	inst, err := c.GetInstance(ctx, ref)
	if err != nil {
		return compute.Forward{}, err
	}
	networks, err := c.ListNetworks(ctx, ref.Sandbox)
	if err != nil {
		return compute.Forward{}, err
	}
	srv := c.Scoped(ctx, projectName(ref.Sandbox), "")
	target := strconv.FormatInt(targetPort, 10)
	for _, network := range networks {
		if network.Kind != networkKindOVN {
			continue
		}
		for _, nic := range inst.NICs {
			if nic.Network != network.Name {
				continue
			}
			forwards, err := srv.GetNetworkForwards(network.Name)
			if err != nil {
				return compute.Forward{}, mapOVNError(err)
			}
			if len(forwards) == 0 {
				continue
			}
			targetAddress, err := c.forwardTargetAddress(ctx, ref.Sandbox, network.Name, inst)
			if err != nil {
				return compute.Forward{}, err
			}
			for _, forward := range forwards {
				for _, port := range forward.Ports {
					address := port.TargetAddress
					if address == "" {
						address = forward.Config["target_address"]
					}
					mapped := port.TargetPort
					if mapped == "" {
						mapped = port.ListenPort
					}
					if port.Protocol != protocol || mapped != target || address != targetAddress {
						continue
					}
					listen, err := strconv.ParseInt(port.ListenPort, 10, 64)
					if err != nil {
						continue
					}
					return compute.Forward{Address: forward.ListenAddress, Port: listen, Protocol: protocol, Network: network.Name, Instance: ref.Name}, nil
				}
			}
		}
	}
	return compute.Forward{}, nil
}

func (c *Client) deleteForwardsInProject(ctx context.Context, project, network string) []error {
	ctx, cancel := c.ovnContext(ctx)
	defer cancel()
	srv := c.Scoped(ctx, project, "")
	forwards, err := srv.GetNetworkForwards(network)
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return nil
		}
		return []error{mapOVNError(err)}
	}
	var errs []error
	for _, forward := range forwards {
		if err := srv.DeleteNetworkForward(network, forward.ListenAddress); err != nil &&
			!errors.Is(mapError(err), compute.ErrNotFound) {
			errs = append(errs, mapOVNError(err))
		}
	}
	return errs
}

func forwardHasPort(forward api.NetworkForward, protocol, listen string) bool {
	for _, port := range forward.Ports {
		if port.Protocol == protocol && port.ListenPort == listen {
			return true
		}
	}
	return false
}

func parseIPv4Range(spec string) (netip.Addr, netip.Addr, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return netip.Addr{}, netip.Addr{}, errors.New("OVN range is required")
	}
	if strings.Contains(spec, "/") {
		prefix, err := netip.ParsePrefix(spec)
		if err != nil || !prefix.Addr().Is4() {
			return netip.Addr{}, netip.Addr{}, fmt.Errorf("invalid OVN range %q", spec)
		}
		prefix = prefix.Masked()
		start := prefix.Addr()
		last := start.As4()
		hostMask := ^uint32(0) >> prefix.Bits()
		binary.BigEndian.PutUint32(last[:], binary.BigEndian.Uint32(last[:])|hostMask)
		return start, netip.AddrFrom4(last), nil
	}
	startText, endText, ok := strings.Cut(spec, "-")
	if !ok {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("invalid OVN range %q", spec)
	}
	start, err := netip.ParseAddr(strings.TrimSpace(startText))
	if err != nil || !start.Is4() {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("invalid OVN range %q", spec)
	}
	end, err := netip.ParseAddr(strings.TrimSpace(endText))
	if err != nil || !end.Is4() || end.Less(start) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("invalid OVN range %q", spec)
	}
	return start, end, nil
}
