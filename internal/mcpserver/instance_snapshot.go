package mcpserver

import (
	"context"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	capabilityInstanceSnapshotCreate  = "instance.snapshot.create"
	capabilityInstanceSnapshotRestore = "instance.snapshot.restore"
	capabilityInstanceSnapshotDelete  = "instance.snapshot.delete"
	capabilityInstanceSnapshotList    = "instance.snapshot.list"
)

type instanceSnapshotIn struct {
	Sandbox  string `json:"sandbox"`
	Name     string `json:"name"`
	Snapshot string `json:"snapshot"`
}

type instanceSnapshotOut struct{}

type instanceSnapshotListIn struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
}

type instanceSnapshotItem struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type instanceSnapshotListOut struct {
	Items []instanceSnapshotItem `json:"items"`
}

func registerInstanceSnapshots(builder *codemode.Builder, api instanceAPI) {
	codemode.Register(builder, codemode.Capability[instanceSnapshotIn, instanceSnapshotOut]{
		ID:      capabilityInstanceSnapshotCreate,
		Name:    capabilityInstanceSnapshotCreate,
		Summary: "Create an instance snapshot.",
		Handler: api.createSnapshot,
	})
	codemode.Register(builder, codemode.Capability[instanceSnapshotIn, instanceSnapshotOut]{
		ID:      capabilityInstanceSnapshotRestore,
		Name:    capabilityInstanceSnapshotRestore,
		Summary: "Recreate and start the same instance name from a snapshot. Deletes its old snapshots; identity changes and MAC/DHCP lease may change.",
		Handler: api.restoreSnapshot,
	})
	codemode.Register(builder, codemode.Capability[instanceSnapshotIn, instanceSnapshotOut]{
		ID:      capabilityInstanceSnapshotDelete,
		Name:    capabilityInstanceSnapshotDelete,
		Summary: "Delete an instance snapshot.",
		Handler: api.deleteSnapshot,
	})
	codemode.Register(builder, codemode.Capability[instanceSnapshotListIn, instanceSnapshotListOut]{
		ID:      capabilityInstanceSnapshotList,
		Name:    capabilityInstanceSnapshotList,
		Summary: "List instance snapshots.",
		Handler: api.listSnapshots,
	})
}

func (api instanceAPI) createSnapshot(
	ctx context.Context,
	_ authz.Subject,
	in instanceSnapshotIn,
) (instanceSnapshotOut, error) {
	if err := api.instances.CreateSnapshot(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		in.Snapshot,
	); err != nil {
		return instanceSnapshotOut{}, err
	}
	return instanceSnapshotOut{}, nil
}

func (api instanceAPI) restoreSnapshot(
	ctx context.Context,
	_ authz.Subject,
	in instanceSnapshotIn,
) (instanceSnapshotOut, error) {
	if err := api.instances.RestoreSnapshot(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		in.Snapshot,
	); err != nil {
		return instanceSnapshotOut{}, err
	}
	return instanceSnapshotOut{}, nil
}

func (api instanceAPI) deleteSnapshot(
	ctx context.Context,
	_ authz.Subject,
	in instanceSnapshotIn,
) (instanceSnapshotOut, error) {
	if err := api.instances.DeleteSnapshot(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		in.Snapshot,
	); err != nil {
		return instanceSnapshotOut{}, err
	}
	return instanceSnapshotOut{}, nil
}

func (api instanceAPI) listSnapshots(
	ctx context.Context,
	_ authz.Subject,
	in instanceSnapshotListIn,
) (instanceSnapshotListOut, error) {
	snapshots, err := api.instances.ListSnapshots(ctx, compute.Ref{Sandbox: in.Sandbox, Name: in.Name})
	if err != nil {
		return instanceSnapshotListOut{}, err
	}
	items := make([]instanceSnapshotItem, 0, len(snapshots))
	for _, snapshot := range snapshots {
		items = append(items, instanceSnapshotItem{
			Name:      snapshot.Name,
			CreatedAt: formatTime(snapshot.CreatedAt),
		})
	}
	return instanceSnapshotListOut{Items: items}, nil
}
