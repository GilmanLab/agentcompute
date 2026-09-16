package lume

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

// CreateSnapshot clones a stopped guest to a collision-safe VM and restores prior running state.
func (c *Client) CreateSnapshot(ctx context.Context, ref compute.Ref, snapshot string) (err error) {
	if snapshot == "" {
		return errors.New("snapshot name is required")
	}
	rec, err := c.readSandbox(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	mapped, err := rec.instance(ref.Name)
	if err != nil {
		return err
	}
	if mapped.Recovery != nil {
		return agentError("instance has an unfinished snapshot restore; delete the instance or sandbox to recover")
	}
	if existing := mapped.Snapshots[snapshot]; existing != nil {
		return agentErrorf("snapshot %q already exists on instance %q", snapshot, ref.Name)
	}
	vm, err := c.getVM(ctx, mapped.VM)
	if err != nil {
		return err
	}
	if vm.running() {
		if err = c.stopVM(ctx, mapped.VM); err != nil {
			return err
		}
		defer func() {
			if _, resumeErr := c.StartInstance(ctx, ref, false); resumeErr != nil {
				err = errors.Join(err, fmt.Errorf("restore running state after snapshot: %w", resumeErr))
			}
		}()
	}
	return c.cloneSnapshot(ctx, ref, snapshot, mapped.VM)
}

func (c *Client) cloneSnapshot(ctx context.Context, ref compute.Ref, snapshot, source string) error {
	name, err := c.uniqueVMName(ctx, snapshotVMName(ref.Sandbox, ref.Name, snapshot, ""))
	if err != nil {
		return err
	}
	err = c.updateInstance(ctx, ref, func(inst *instanceRecord) {
		if inst.Snapshots == nil {
			inst.Snapshots = map[string]*snapshotRecord{}
		}
		inst.Snapshots[snapshot] = &snapshotRecord{VM: name, CreatedAt: time.Now().UTC()}
	})
	if err != nil {
		return err
	}
	if err = c.cloneAndPin(ctx, source, name); err != nil {
		if cleanupErr := c.deleteExactVM(ctx, name); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("snapshot copy retained for cleanup: %w", cleanupErr))
		}
		cleanupErr := c.updateInstance(ctx, ref, func(inst *instanceRecord) {
			delete(inst.Snapshots, snapshot)
		})
		return errors.Join(err, cleanupErr)
	}
	return nil
}

// RestoreSnapshot clones a snapshot VM over the instance mapping without deleting the seed.
func (c *Client) RestoreSnapshot(ctx context.Context, ref compute.Ref, snapshot string) error {
	rec, err := c.readSandbox(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	mapped, err := rec.instance(ref.Name)
	if err != nil {
		return err
	}
	if mapped.Recovery != nil {
		return agentError("instance has an unfinished snapshot restore; delete the instance or sandbox to recover")
	}
	snap := mapped.Snapshots[snapshot]
	if snap == nil || snap.VM == "" {
		return agentErrorf("snapshot %q not found on instance %q", snapshot, ref.Name)
	}
	if rec.protected(mapped.VM) || rec.protected(snap.VM) {
		return agentErrorf("refusing to restore snapshot %q: protected seed mapping", snapshot)
	}
	pending, err := c.stageSnapshotRestore(ctx, ref, mapped.VM, snap.VM, snapshot)
	if err != nil {
		return err
	}
	wasRunning, err := c.activateSnapshotRestore(ctx, ref, mapped.VM, snap.VM, pending)
	if err != nil || !wasRunning {
		return err
	}
	_, err = c.StartInstance(ctx, ref, false)
	return err
}

func (c *Client) stageSnapshotRestore(
	ctx context.Context, ref compute.Ref, previous, source, snapshot string,
) (string, error) {
	pending, err := c.uniqueVMName(ctx, snapshotVMName(ref.Sandbox, ref.Name, snapshot, "r"))
	if err != nil {
		return "", err
	}
	err = c.updateInstance(ctx, ref, func(inst *instanceRecord) {
		inst.Recovery = &recoveryRecord{PreviousVM: previous, PendingVM: pending}
	})
	if err != nil {
		return "", err
	}
	if err = c.cloneAndPin(ctx, source, pending); err != nil {
		if cleanupErr := c.deleteExactVM(ctx, pending); cleanupErr != nil {
			return "", errors.Join(err, fmt.Errorf("restore copy retained for cleanup: %w", cleanupErr))
		}
		cleanupErr := c.updateInstance(ctx, ref, func(inst *instanceRecord) {
			inst.Recovery = nil
		})
		return "", errors.Join(err, cleanupErr)
	}
	return pending, nil
}

func (c *Client) activateSnapshotRestore(
	ctx context.Context, ref compute.Ref, previous, source, pending string,
) (bool, error) {
	vm, err := c.getVM(ctx, previous)
	if err != nil && !errors.Is(err, compute.ErrNotFound) {
		return false, err
	}
	wasRunning := err == nil && vm.running()
	if wasRunning {
		if err = c.stopVM(ctx, previous); err != nil {
			return false, err
		}
	}
	err = c.updateInstance(ctx, ref, func(inst *instanceRecord) {
		inst.VM = pending
	})
	if err != nil {
		return false, err
	}
	// The caller has rejected seed mappings. Retain Recovery until deletion succeeds.
	if previous != source {
		if err = c.deleteExactVM(ctx, previous); err != nil {
			return false, fmt.Errorf("remove original instance; snapshot copy retained as %q: %w", pending, err)
		}
	}
	err = c.updateInstance(ctx, ref, func(inst *instanceRecord) {
		inst.Recovery = nil
	})
	return wasRunning, err
}

func (c *Client) updateInstance(ctx context.Context, ref compute.Ref, update func(*instanceRecord)) error {
	return c.updateSandbox(ctx, ref.Sandbox, func(rec *sandboxRecord) error {
		inst, err := rec.instance(ref.Name)
		if err != nil {
			return err
		}
		update(inst)
		return nil
	})
}

// DeleteSnapshot deletes one snapshot VM by its exact mapped name.
func (c *Client) DeleteSnapshot(ctx context.Context, ref compute.Ref, snapshot string) error {
	rec, err := c.readSandbox(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	mapped, err := rec.instance(ref.Name)
	if err != nil {
		return err
	}
	snap, ok := mapped.Snapshots[snapshot]
	if !ok || snap == nil {
		return agentErrorf("snapshot %q not found on instance %q", snapshot, ref.Name)
	}
	if rec.protected(snap.VM) {
		return agentErrorf("refusing to delete protected vm %q", snap.VM)
	}
	actual, err := c.listActualVMs(ctx)
	if err != nil {
		return err
	}
	if actual[snap.VM] {
		if err := c.deleteExactVM(ctx, snap.VM); err != nil {
			return err
		}
	}
	return c.updateSandbox(ctx, ref.Sandbox, func(rec *sandboxRecord) error {
		inst, err := rec.instance(ref.Name)
		if err != nil {
			return err
		}
		delete(inst.Snapshots, snapshot)
		return nil
	})
}

// ListSnapshots returns snapshots recorded for one guest.
func (c *Client) ListSnapshots(ctx context.Context, ref compute.Ref) ([]compute.Snapshot, error) {
	rec, err := c.readSandbox(ctx, ref.Sandbox)
	if err != nil {
		return nil, err
	}
	mapped, err := rec.instance(ref.Name)
	if err != nil {
		return nil, err
	}
	out := make([]compute.Snapshot, 0, len(mapped.Snapshots))
	for name, snap := range mapped.Snapshots {
		if snap == nil {
			continue
		}
		out = append(out, compute.Snapshot{Name: name, CreatedAt: snap.CreatedAt})
	}
	slices.SortFunc(out, func(a, b compute.Snapshot) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}
