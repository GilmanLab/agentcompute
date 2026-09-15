package lume

import (
	"context"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

// ListNetworks returns no agent-facing networks; GetSandbox still succeeds.
func (c *Client) ListNetworks(ctx context.Context, sandbox string) ([]compute.Network, error) {
	if _, err := c.GetSandbox(ctx, sandbox); err != nil {
		return nil, err
	}
	return []compute.Network{}, nil
}

// CreateNetwork is unsupported on the Mac backend.
func (c *Client) CreateNetwork(context.Context, string, compute.Network) (compute.Network, error) {
	return compute.Network{}, unsupportedMac()
}

// AttachNIC is unsupported on the Mac backend.
func (c *Client) AttachNIC(context.Context, compute.Ref, string, string, string, string) (compute.NIC, error) {
	return compute.NIC{}, unsupportedMac()
}

// GetNetwork reports that Mac sandboxes have no managed networks.
func (c *Client) GetNetwork(ctx context.Context, sandbox, _ string) (compute.Network, error) {
	if _, err := c.GetSandbox(ctx, sandbox); err != nil {
		return compute.Network{}, err
	}
	return compute.Network{}, compute.ErrNotFound
}

// DeleteNetwork is unsupported on the Mac backend.
func (c *Client) DeleteNetwork(context.Context, string, string) error {
	return unsupportedMac()
}

// DetachNIC is unsupported on the Mac backend.
func (c *Client) DetachNIC(context.Context, compute.Ref, string) error {
	return unsupportedMac()
}

// PeerNetworks is unsupported on the Mac backend.
func (c *Client) PeerNetworks(context.Context, string, string, string) error {
	return unsupportedMac()
}

// AddACLRule is unsupported on the Mac backend.
func (c *Client) AddACLRule(context.Context, string, string, compute.ACLRule) (compute.ACLRule, error) {
	return compute.ACLRule{}, unsupportedMac()
}

// RemoveACLRule is unsupported on the Mac backend.
func (c *Client) RemoveACLRule(context.Context, string, string, string) error {
	return unsupportedMac()
}

// CreateForward is unsupported on the Mac backend.
func (c *Client) CreateForward(
	context.Context,
	string,
	string,
	compute.Ref,
	int64,
	int64,
	string,
) (compute.Forward, error) {
	return compute.Forward{}, unsupportedMac()
}

// InstanceForward is unsupported on the Mac backend.
func (c *Client) InstanceForward(context.Context, compute.Ref, int64, string) (compute.Forward, error) {
	return compute.Forward{}, unsupportedMac()
}
