package incus

import (
	"context"
	"errors"
	"fmt"

	"github.com/lxc/incus/v7/shared/api"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func (c *Client) PeerNetworks(ctx context.Context, sandbox, network, peer string) error {
	ctx, cancel := c.ovnContext(ctx)
	defer cancel()
	left, err := c.ownedProjectNetwork(ctx, sandbox, network)
	if err != nil {
		return err
	}
	right, err := c.ownedProjectNetwork(ctx, sandbox, peer)
	if err != nil {
		return err
	}
	if left.Type != networkKindOVN || right.Type != networkKindOVN {
		return errors.New("network peering requires OVN networks")
	}
	project := projectName(sandbox)
	if err := c.ensurePeer(ctx, sandbox, network, peer, project); err != nil {
		return err
	}
	return c.ensurePeer(ctx, sandbox, peer, network, project)
}

func (c *Client) ensurePeer(ctx context.Context, sandbox, network, target, targetProject string) error {
	srv := c.Scoped(ctx, projectName(sandbox), "")
	existing, _, err := srv.GetNetworkPeer(network, target)
	if err == nil {
		if existing.TargetNetwork == target &&
			(existing.TargetProject == "" || existing.TargetProject == targetProject) {
			return nil
		}
		return fmt.Errorf("peer %q already exists on network %q", target, network)
	}
	if !errors.Is(mapError(err), compute.ErrNotFound) {
		return mapOVNError(err)
	}
	return mapOVNError(srv.CreateNetworkPeer(network, api.NetworkPeersPost{
		Name:          target,
		TargetProject: targetProject,
		TargetNetwork: target,
		Type:          "local",
		NetworkPeerPut: api.NetworkPeerPut{
			Config: map[string]string{
				metaSandbox: sandbox,
				metaVersion: versionValue,
			},
		},
	}))
}

func (c *Client) deletePeers(ctx context.Context, sandbox, network string) []error {
	ctx, cancel := c.ovnContext(ctx)
	defer cancel()
	srv := c.Scoped(ctx, projectName(sandbox), "")
	peers, err := srv.GetNetworkPeers(network)
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return nil
		}
		return []error{mapOVNError(err)}
	}
	var errs []error
	for _, peer := range peers {
		if err := srv.DeleteNetworkPeer(
			network,
			peer.Name,
		); err != nil &&
			!errors.Is(mapError(err), compute.ErrNotFound) {
			errs = append(errs, mapOVNError(err))
		}
	}
	return errs
}
