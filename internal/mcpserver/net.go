package mcpserver

import (
	"context"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

type netCreateIn struct {
	Sandbox string  `json:"sandbox"`
	Name    string  `json:"name"`
	Kind    *string `json:"kind,omitempty"`
	CIDR    *string `json:"cidr,omitempty"`
	DHCP    *bool   `json:"dhcp,omitempty"`
	NAT     *bool   `json:"nat,omitempty"`
	DNS     *bool   `json:"dns,omitempty"`
}

type networkOut struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	CIDR    string `json:"cidr"`
	Gateway string `json:"gateway"`
}

type netAttachIn struct {
	Sandbox  string  `json:"sandbox"`
	Instance string  `json:"instance"`
	Network  string  `json:"network"`
	NIC      *string `json:"nic,omitempty"`
	IP       *string `json:"ip,omitempty"`
	MAC      *string `json:"mac,omitempty"`
}

type netAttachOut struct {
	NIC string `json:"nic"`
	MAC string `json:"mac"`
}

type netAPI struct {
	networks networkService
}

func registerNet(builder *codemode.Builder, deps Dependencies) {
	api := netAPI{networks: deps.Network}
	codemode.Register(builder, codemode.Capability[netCreateIn, networkOut]{
		ID:      capabilityNetCreate,
		Name:    capabilityNetCreate,
		Summary: "Create a sandbox bridge network.",
		Handler: api.create,
	})
	codemode.Register(builder, codemode.Capability[netAttachIn, netAttachOut]{
		ID:      capabilityNetAttach,
		Name:    capabilityNetAttach,
		Summary: "Attach a NIC to an instance.",
		Handler: api.attach,
	})
}

func (api netAPI) create(
	ctx context.Context,
	_ authz.Subject,
	in netCreateIn,
) (networkOut, error) {
	kind, err := resolveNetworkKind(in.Kind)
	if err != nil {
		return networkOut{}, err
	}
	network, err := api.networks.CreateNetwork(ctx, in.Sandbox, compute.Network{
		Name: in.Name,
		Kind: kind,
		CIDR: deref(in.CIDR, ""),
		DHCP: deref(in.DHCP, false),
		NAT:  deref(in.NAT, false),
		DNS:  deref(in.DNS, false),
	})
	if err != nil {
		return networkOut{}, err
	}
	return networkDTO(network), nil
}

func (api netAPI) attach(
	ctx context.Context,
	_ authz.Subject,
	in netAttachIn,
) (netAttachOut, error) {
	nic, err := api.networks.AttachNIC(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Instance},
		in.Network,
		deref(in.NIC, ""),
		deref(in.IP, ""),
		deref(in.MAC, ""),
	)
	if err != nil {
		return netAttachOut{}, err
	}
	return netAttachOut{NIC: nic.Name, MAC: nic.MAC}, nil
}

func resolveNetworkKind(kind *string) (string, error) {
	value := deref(kind, kindBridge)
	if value == "" {
		value = kindBridge
	}
	switch value {
	case kindBridge:
		return kindBridge, nil
	case kindOVN:
		return "", agentError("OVN networks are not available yet")
	default:
		return "", agentErrorf("unsupported network kind %q", value)
	}
}
