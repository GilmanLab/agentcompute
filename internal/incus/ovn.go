package incus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/meigma/codemode"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	networkKindOVN        = "ovn"
	defaultOVNUplink      = "fast40-uplink"
	defaultOVNRanges      = "10.10.40.64/26"
	ovnMutationTimeout    = 20 * time.Second
	securityACLKey        = "security.acls"
	securityACLIngressKey = "security.acls.default.ingress.action"
	securityACLEgressKey  = "security.acls.default.egress.action"
	aclActionAllow        = "allow"
	dnsModeManaged        = "managed"
)

func (c *Client) ovnUplinkName() string {
	if c.ovnUplink != "" {
		return c.ovnUplink
	}
	return defaultOVNUplink
}

func (c *Client) ovnRangeSpec() string {
	if c.ovnRanges != "" {
		return c.ovnRanges
	}
	return defaultOVNRanges
}

func (c *Client) ovnContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, ovnMutationTimeout)
}

func ovnUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, compute.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "northbound") ||
		strings.Contains(msg, "ovn") && (strings.Contains(msg, "connect") ||
			strings.Contains(msg, "unavailable") ||
			strings.Contains(msg, "timed out") ||
			strings.Contains(msg, "timeout") ||
			strings.Contains(msg, "database"))
}

func mapOVNError(err error) error {
	if err == nil {
		return nil
	}
	mapped := mapError(err)
	if ovnUnavailable(mapped) {
		return fmt.Errorf("%w", compute.ErrUnavailable)
	}
	return mapped
}

func (c *Client) createOVNNetwork(
	ctx context.Context,
	sandbox string,
	network compute.Network,
) (compute.Network, error) {
	logical := network.Name
	if logical == "" {
		return compute.Network{}, errors.New("network name is required")
	}
	ctx, cancel := c.ovnContext(ctx)
	defer cancel()
	config, err := ovnConfig(sandbox, logical, network, c.ovnUplinkName())
	if err != nil {
		return compute.Network{}, err
	}

	existing, err := c.projectNetwork(ctx, sandbox, logical)
	if err != nil && !errors.Is(err, compute.ErrNotFound) {
		return compute.Network{}, mapOVNError(err)
	}
	if err == nil {
		return matchingOVNNetwork(existing, sandbox, logical, config)
	}

	if err = c.ensureNetworkACLs(ctx, sandbox, logical); err != nil {
		return compute.Network{}, err
	}

	err = c.Scoped(ctx, projectName(sandbox), "").CreateNetwork(api.NetworksPost{
		Name:       logical,
		Type:       networkKindOVN,
		NetworkPut: api.NetworkPut{Config: config},
	})
	if err != nil {
		if isConflict(err) {
			created, getErr := c.projectNetwork(ctx, sandbox, logical)
			if getErr == nil {
				return matchingOVNNetwork(created, sandbox, logical, config)
			}
		}
		return compute.Network{}, mapOVNError(err)
	}
	return c.observedProjectNetwork(ctx, sandbox, logical)
}

func matchingOVNNetwork(
	existing *api.Network,
	sandbox, logical string,
	desired map[string]string,
) (compute.Network, error) {
	if existing.Config[metaSandbox] != sandbox || existing.Config[metaVersion] != versionValue {
		return compute.Network{}, &codemode.AgentError{
			Message: fmt.Sprintf("network %q is not owned by sandbox %q", logical, sandbox),
		}
	}
	if existing.Status != api.NetworkStatusCreated {
		return compute.Network{}, fmt.Errorf("%w", compute.ErrUnavailable)
	}
	if existing.Type != networkKindOVN {
		return compute.Network{}, &codemode.AgentError{
			Message: fmt.Sprintf("network %q already exists with a different kind", logical),
		}
	}
	for key, value := range desired {
		if key == ipv4AddressKey && value == addressAuto {
			continue
		}
		if existing.Config[key] != value {
			return compute.Network{}, &codemode.AgentError{
				Message: fmt.Sprintf(
					"network %q already exists with a different %s setting; delete it before recreating it",
					logical,
					key,
				),
			}
		}
	}
	return networkFromAPI(*existing, logical, "", projectName(sandbox)), nil
}

func (c *Client) projectNetwork(ctx context.Context, sandbox, name string) (*api.Network, error) {
	network, _, err := c.Scoped(ctx, projectName(sandbox), "").GetNetwork(name)
	if err != nil {
		return nil, mapError(err)
	}
	return network, nil
}

func (c *Client) observedProjectNetwork(ctx context.Context, sandbox, logical string) (compute.Network, error) {
	network, err := c.projectNetwork(ctx, sandbox, logical)
	if err != nil {
		return compute.Network{}, err
	}
	return networkFromAPI(*network, logical, "", projectName(sandbox)), nil
}

func (c *Client) ownedOVNNetworks(ctx context.Context, sandbox string) ([]compute.Network, error) {
	if _, _, err := c.getProject(ctx, sandbox); err != nil {
		if errors.Is(err, compute.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	networks, err := c.Scoped(ctx, projectName(sandbox), "").GetNetworks()
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return nil, nil
		}
		return nil, mapError(err)
	}
	out := make([]compute.Network, 0)
	for _, network := range networks {
		if network.Type != networkKindOVN {
			continue
		}
		logical := network.Config[metaName]
		if logical == "" {
			logical = network.Name
		}
		if network.Config[metaSandbox] != sandbox || network.Config[metaVersion] != versionValue {
			continue
		}
		out = append(out, networkFromAPI(network, logical, "", projectName(sandbox)))
	}
	return out, nil
}

func (c *Client) deleteOVNNetwork(ctx context.Context, sandbox, name string) error {
	ctx, cancel := c.ovnContext(ctx)
	defer cancel()
	if err := c.deleteForwardsInProject(ctx, projectName(sandbox), name); err != nil {
		joined := errors.Join(err...)
		if joined != nil {
			return joined
		}
	}
	if err := c.deletePeers(ctx, sandbox, name); err != nil {
		joined := errors.Join(err...)
		if joined != nil {
			return joined
		}
	}
	err := c.Scoped(ctx, projectName(sandbox), "").DeleteNetwork(name)
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		return mapOVNError(err)
	}
	srv := c.Scoped(ctx, projectName(sandbox), "")
	aclName := agentACLName(name)
	acl, _, err := srv.GetNetworkACL(aclName)
	if errors.Is(mapError(err), compute.ErrNotFound) {
		return nil
	}
	if err != nil {
		return mapOVNError(err)
	}
	if acl.Config[metaSandbox] != sandbox || acl.Config[metaVersion] != versionValue ||
		acl.Config[metaName] != name {
		return fmt.Errorf("acl %q is not owned by network %q", aclName, name)
	}
	return mapOVNError(srv.DeleteNetworkACL(aclName))
}

func ovnConfig(sandbox, logical string, network compute.Network, uplink string) (map[string]string, error) {
	address := addressAuto
	if network.Gateway != "" && strings.Contains(network.Gateway, "/") {
		address = network.Gateway
	} else if network.CIDR != "" {
		converted, err := routerCIDR(network.CIDR)
		if err != nil {
			return nil, err
		}
		address = converted
	}
	dnsMode := configNone
	if network.DNS {
		dnsMode = dnsModeManaged
	}
	if !network.NAT {
		uplink = configNone
	}
	return map[string]string{
		deviceNetworkKey:      uplink,
		ipv4AddressKey:        address,
		ipv4NATKey:            strconv.FormatBool(network.NAT),
		ipv4DHCPKey:           strconv.FormatBool(network.DHCP),
		"ipv6.address":        configNone,
		dnsModeKey:            dnsMode,
		securityACLKey:        aclNameBaseline + "," + agentACLName(logical),
		securityACLIngressKey: aclActionAllow,
		securityACLEgressKey:  aclActionAllow,
		metaSandbox:           sandbox,
		metaName:              logical,
		metaVersion:           versionValue,
	}, nil
}

func routerCIDR(cidr string) (string, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("invalid cidr %q", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	if ones == 0 || ones >= bits {
		return "", fmt.Errorf("invalid cidr %q", cidr)
	}
	if !ip.Equal(ipnet.IP) {
		return ip.String() + "/" + strconv.Itoa(ones), nil
	}
	next := make(net.IP, len(ipnet.IP))
	copy(next, ipnet.IP)
	for i := range slices.Backward(next) {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	if !ipnet.Contains(next) {
		return "", fmt.Errorf("invalid cidr %q", cidr)
	}
	return next.String() + "/" + strconv.Itoa(ones), nil
}

func featuresNetworks(project *api.Project) bool {
	if project == nil {
		return false
	}
	return isTrue(project.Config[featuresNetworksKey])
}
