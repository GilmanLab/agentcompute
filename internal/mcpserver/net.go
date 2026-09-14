package mcpserver

import (
	"context"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	capabilityNetList      = "net.list"
	capabilityNetGet       = "net.get"
	capabilityNetDelete    = "net.delete"
	capabilityNetDetach    = "net.detach"
	capabilityNetPeer      = "net.peer"
	capabilityNetACLAdd    = "net.acl.add"
	capabilityNetACLRemove = "net.acl.remove"
	capabilityNetForward   = "net.forward"
	capabilityNetImpair    = "net.impair"
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

type netListIn struct {
	Sandbox string `json:"sandbox"`
}

type netListOut struct {
	Items []networkOut `json:"items"`
}

type netGetIn struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
}

type netDeleteIn struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
}

type netDeleteOut struct{}

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

type netDetachIn struct {
	Sandbox  string `json:"sandbox"`
	Instance string `json:"instance"`
	NIC      string `json:"nic"`
}

type netDetachOut struct{}

type netPeerIn struct {
	Sandbox string `json:"sandbox"`
	Network string `json:"network"`
	Peer    string `json:"peer"`
}

type netPeerOut struct{}

type netACLAddIn struct {
	Sandbox   string  `json:"sandbox"`
	Network   string  `json:"network"`
	Direction string  `json:"direction"`
	Action    string  `json:"action"`
	Protocol  *string `json:"protocol,omitempty"`
	Src       *string `json:"src,omitempty"`
	Dst       *string `json:"dst,omitempty"`
	Port      *string `json:"port,omitempty"`
}

type netACLAddOut struct {
	Rule string `json:"rule"`
}

type netACLRemoveIn struct {
	Sandbox string `json:"sandbox"`
	Network string `json:"network"`
	Rule    string `json:"rule"`
}

type netACLRemoveOut struct{}

type netForwardIn struct {
	Sandbox    string  `json:"sandbox"`
	Network    string  `json:"network"`
	Instance   string  `json:"instance"`
	Port       int64   `json:"port"`
	ListenPort *int64  `json:"listen_port,omitempty"`
	Protocol   *string `json:"protocol,omitempty"`
}

type netForwardOut struct {
	Address string `json:"address"`
	Port    int64  `json:"port"`
}

type netImpairIn struct {
	Sandbox     string   `json:"sandbox"`
	Instance    string   `json:"instance"`
	NIC         string   `json:"nic"`
	LatencyMS   *int64   `json:"latency_ms,omitempty"`
	JitterMS    *int64   `json:"jitter_ms,omitempty"`
	LossPercent *float64 `json:"loss_percent,omitempty"`
	RateMbit    *int64   `json:"rate_mbit,omitempty"`
	Clear       *bool    `json:"clear,omitempty"`
}

type netImpairOut struct{}

type netAPI struct {
	networks networkService
}

func registerNet(builder *codemode.Builder, deps Dependencies) {
	api := netAPI{networks: deps.Network}
	codemode.Register(builder, codemode.Capability[netCreateIn, networkOut]{
		ID:      capabilityNetCreate,
		Name:    capabilityNetCreate,
		Summary: "Create a sandbox network. nat=false networks are unreachable from outside the sandbox; attach a router instance or use net.peer. No uplink gateway is accepted.",
		Handler: api.create,
	})
	codemode.Register(builder, codemode.Capability[netListIn, netListOut]{
		ID:      capabilityNetList,
		Name:    capabilityNetList,
		Summary: "List sandbox networks.",
		Handler: api.list,
	})
	codemode.Register(builder, codemode.Capability[netGetIn, networkOut]{
		ID:      capabilityNetGet,
		Name:    capabilityNetGet,
		Summary: "Get one sandbox network.",
		Handler: api.get,
	})
	codemode.Register(builder, codemode.Capability[netDeleteIn, netDeleteOut]{
		ID:      capabilityNetDelete,
		Name:    capabilityNetDelete,
		Summary: "Delete a sandbox network.",
		Handler: api.delete,
	})
	codemode.Register(builder, codemode.Capability[netAttachIn, netAttachOut]{
		ID:      capabilityNetAttach,
		Name:    capabilityNetAttach,
		Summary: "Attach a NIC to an instance.",
		Handler: api.attach,
	})
	codemode.Register(builder, codemode.Capability[netDetachIn, netDetachOut]{
		ID:      capabilityNetDetach,
		Name:    capabilityNetDetach,
		Summary: "Detach a NIC from an instance.",
		Handler: api.detach,
	})
	codemode.Register(builder, codemode.Capability[netPeerIn, netPeerOut]{
		ID:      capabilityNetPeer,
		Name:    capabilityNetPeer,
		Summary: "Peer two OVN networks in a sandbox.",
		Handler: api.peer,
	})
	codemode.Register(builder, codemode.Capability[netACLAddIn, netACLAddOut]{
		ID:      capabilityNetACLAdd,
		Name:    capabilityNetACLAdd,
		Summary: "Add an ACL rule. Allow rules require an explicit destination IP/CIDR outside management and OOB.",
		Handler: api.addACL,
	})
	codemode.Register(builder, codemode.Capability[netACLRemoveIn, netACLRemoveOut]{
		ID:      capabilityNetACLRemove,
		Name:    capabilityNetACLRemove,
		Summary: "Remove a network ACL rule.",
		Handler: api.removeACL,
	})
	codemode.Register(builder, codemode.Capability[netForwardIn, netForwardOut]{
		ID:      capabilityNetForward,
		Name:    capabilityNetForward,
		Summary: "Forward a port from the OVN uplink range on a NAT-enabled network. Isolated nat=false networks cannot have forwards.",
		Handler: api.forward,
	})
	codemode.Register(builder, codemode.Capability[netImpairIn, netImpairOut]{
		ID:      capabilityNetImpair,
		Name:    capabilityNetImpair,
		Summary: "Apply Linux tc netem impairment on a NIC.",
		Handler: api.impair,
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
	ovn := kind == kindOVN
	network, err := api.networks.CreateNetwork(ctx, in.Sandbox, compute.Network{
		Name: in.Name,
		Kind: kind,
		CIDR: deref(in.CIDR, ""),
		DHCP: deref(in.DHCP, ovn),
		NAT:  deref(in.NAT, ovn),
		DNS:  deref(in.DNS, ovn),
	})
	if err != nil {
		return networkOut{}, err
	}
	return networkDTO(network), nil
}

func (api netAPI) list(
	ctx context.Context,
	_ authz.Subject,
	in netListIn,
) (netListOut, error) {
	networks, err := api.networks.ListNetworks(ctx, in.Sandbox)
	if err != nil {
		return netListOut{}, err
	}
	return netListOut{Items: networkDTOs(networks)}, nil
}

func (api netAPI) get(
	ctx context.Context,
	_ authz.Subject,
	in netGetIn,
) (networkOut, error) {
	network, err := api.networks.GetNetwork(ctx, in.Sandbox, in.Name)
	if err != nil {
		return networkOut{}, err
	}
	return networkDTO(network), nil
}

func (api netAPI) delete(
	ctx context.Context,
	_ authz.Subject,
	in netDeleteIn,
) (netDeleteOut, error) {
	return netDeleteOut{}, api.networks.DeleteNetwork(ctx, in.Sandbox, in.Name)
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

func (api netAPI) detach(
	ctx context.Context,
	_ authz.Subject,
	in netDetachIn,
) (netDetachOut, error) {
	return netDetachOut{}, api.networks.DetachNIC(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Instance},
		in.NIC,
	)
}

func (api netAPI) peer(
	ctx context.Context,
	_ authz.Subject,
	in netPeerIn,
) (netPeerOut, error) {
	return netPeerOut{}, api.networks.PeerNetworks(ctx, in.Sandbox, in.Network, in.Peer)
}

func (api netAPI) addACL(
	ctx context.Context,
	_ authz.Subject,
	in netACLAddIn,
) (netACLAddOut, error) {
	rule, err := api.networks.AddACLRule(ctx, in.Sandbox, in.Network, compute.ACLRule{
		Direction: in.Direction,
		Action:    in.Action,
		Protocol:  deref(in.Protocol, ""),
		Src:       deref(in.Src, ""),
		Dst:       deref(in.Dst, ""),
		Port:      deref(in.Port, ""),
	})
	if err != nil {
		return netACLAddOut{}, err
	}
	return netACLAddOut{Rule: rule.ID}, nil
}

func (api netAPI) removeACL(
	ctx context.Context,
	_ authz.Subject,
	in netACLRemoveIn,
) (netACLRemoveOut, error) {
	return netACLRemoveOut{}, api.networks.RemoveACLRule(ctx, in.Sandbox, in.Network, in.Rule)
}

func (api netAPI) forward(
	ctx context.Context,
	_ authz.Subject,
	in netForwardIn,
) (netForwardOut, error) {
	created, err := api.networks.CreateForward(
		ctx,
		in.Sandbox,
		in.Network,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Instance},
		in.Port,
		deref(in.ListenPort, 0),
		deref(in.Protocol, ""),
	)
	if err != nil {
		return netForwardOut{}, err
	}
	return netForwardOut{Address: created.Address, Port: created.Port}, nil
}

func (api netAPI) impair(
	ctx context.Context,
	_ authz.Subject,
	in netImpairIn,
) (netImpairOut, error) {
	return netImpairOut{}, api.networks.ImpairNIC(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Instance},
		in.NIC,
		compute.Impairment{
			LatencyMS:   deref(in.LatencyMS, 0),
			JitterMS:    deref(in.JitterMS, 0),
			LossPercent: deref(in.LossPercent, 0),
			RateMbit:    deref(in.RateMbit, 0),
			Clear:       deref(in.Clear, false),
		},
	)
}

func resolveNetworkKind(kind *string) (string, error) {
	value := deref(kind, kindOVN)
	if value == "" {
		value = kindOVN
	}
	switch value {
	case kindBridge, kindOVN:
		return value, nil
	default:
		return "", agentErrorf("unsupported network kind %q", value)
	}
}
