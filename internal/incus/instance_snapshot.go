package incus

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/meigma/codemode"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

// CreateSnapshot creates a stateless instance snapshot.
func (c *Client) CreateSnapshot(ctx context.Context, ref compute.Ref, snapshot string) error {
	if err := c.requireInstance(ctx, ref); err != nil {
		return err
	}
	op, err := c.Scoped(ctx, projectName(ref.Sandbox), "").CreateInstanceSnapshot(ref.Name, api.InstanceSnapshotsPost{
		Name:     snapshot,
		Stateful: false,
	})
	if err != nil {
		if isConflict(err) {
			return snapshotErrorf(
				"snapshot %q already exists on instance %q in sandbox %q",
				snapshot,
				ref.Name,
				ref.Sandbox,
			)
		}
		return mapError(err)
	}
	return waitOp(ctx, op)
}

// RestoreSnapshot recreates a guest from a snapshot without relaxing project restrictions.
func (c *Client) RestoreSnapshot(ctx context.Context, ref compute.Ref, snapshot string) error {
	if err := c.requireInstance(ctx, ref); err != nil {
		return err
	}
	srv := c.Scoped(ctx, projectName(ref.Sandbox), "")
	instance, _, err := srv.GetInstance(ref.Name)
	if err != nil {
		return mapError(err)
	}
	source, _, err := srv.GetInstanceSnapshot(ref.Name, snapshot)
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return snapshotErrorf("snapshot %q not found on instance %q in sandbox %q", snapshot, ref.Name, ref.Sandbox)
		}
		return mapError(err)
	}
	if _, stopErr := c.StopInstance(ctx, ref, true); stopErr != nil {
		return stopErr
	}

	// The source snapshot disappears with its parent. Stage the copy first.
	staged := "restore-" + strings.ToLower(rand.Text())
	config := make(map[string]string, len(source.Config))
	for key, value := range source.Config {
		if !strings.HasPrefix(key, "volatile.") {
			config[key] = value
		}
	}
	for key, value := range instance.Config {
		if strings.HasPrefix(key, metaPrefix) {
			config[key] = value
		}
	}
	op, err := c.Scoped(ctx, projectName(ref.Sandbox), instance.Location).CreateInstance(api.InstancesPost{
		Name: staged,
		Type: api.InstanceType(instance.Type),
		Source: api.InstanceSource{
			Type:   "copy",
			Source: ref.Name + "/" + snapshot,
		},
		InstancePut: api.InstancePut{
			Architecture: source.Architecture,
			Config:       config,
			Devices:      source.Devices,
			Profiles:     source.Profiles,
			Ephemeral:    source.Ephemeral,
		},
	})
	if err != nil {
		return mapError(err)
	}
	if err = waitOp(ctx, op); err != nil {
		return fmt.Errorf("copy snapshot to %q: %w", staged, err)
	}
	if err = c.forceDeleteInstance(ctx, ref.Sandbox, ref.Name); err != nil {
		return fmt.Errorf("remove original instance; snapshot copy retained as %q: %w", staged, err)
	}
	op, err = srv.RenameInstance(staged, api.InstancePost{Name: ref.Name})
	if err != nil {
		return fmt.Errorf("replace instance name; snapshot copy retained as %q: %w", staged, mapError(err))
	}
	if err = waitOp(ctx, op); err != nil {
		return fmt.Errorf("rename snapshot copy %q to %q: %w", staged, ref.Name, err)
	}
	_, err = c.StartInstance(ctx, ref, false)
	return err
}

// DeleteSnapshot deletes one instance snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, ref compute.Ref, snapshot string) error {
	if err := c.requireInstance(ctx, ref); err != nil {
		return err
	}
	op, err := c.Scoped(ctx, projectName(ref.Sandbox), "").DeleteInstanceSnapshot(ref.Name, snapshot)
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return snapshotErrorf(
				"snapshot %q not found on instance %q in sandbox %q",
				snapshot,
				ref.Name,
				ref.Sandbox,
			)
		}
		return mapError(err)
	}
	return waitOp(ctx, op)
}

// ListSnapshots returns snapshots for one guest.
func (c *Client) ListSnapshots(ctx context.Context, ref compute.Ref) ([]compute.Snapshot, error) {
	if err := c.requireInstance(ctx, ref); err != nil {
		return nil, err
	}
	items, err := c.Scoped(ctx, projectName(ref.Sandbox), "").GetInstanceSnapshots(ref.Name)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]compute.Snapshot, 0, len(items))
	for _, item := range items {
		out = append(out, compute.Snapshot{
			Name:      item.Name,
			CreatedAt: item.CreatedAt,
		})
	}
	return out, nil
}

func snapshotErrorf(format string, args ...any) error {
	return &codemode.AgentError{Message: fmt.Sprintf(format, args...)}
}
