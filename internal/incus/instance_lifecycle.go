package incus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lxc/incus/v7/shared/api"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

// StartInstance starts a guest and waits until it is running.
func (c *Client) StartInstance(ctx context.Context, ref compute.Ref, force bool) (compute.Instance, error) {
	if err := c.requireInstance(ctx, ref); err != nil {
		return compute.Instance{}, err
	}
	if err := c.changeState(ctx, ref, startAction, force); err != nil {
		return compute.Instance{}, err
	}
	if err := c.waitRunning(ctx, projectName(ref.Sandbox), ref.Name); err != nil {
		return compute.Instance{}, err
	}
	return c.GetInstance(ctx, ref)
}

// StopInstance stops a guest and waits until it is stopped.
func (c *Client) StopInstance(ctx context.Context, ref compute.Ref, force bool) (compute.Instance, error) {
	if err := c.requireInstance(ctx, ref); err != nil {
		return compute.Instance{}, err
	}
	if err := c.changeState(ctx, ref, stopAction, force); err != nil {
		return compute.Instance{}, err
	}
	if err := c.waitStopped(ctx, projectName(ref.Sandbox), ref.Name); err != nil {
		return compute.Instance{}, err
	}
	return c.GetInstance(ctx, ref)
}

// RestartInstance restarts a guest and waits until it is running.
func (c *Client) RestartInstance(ctx context.Context, ref compute.Ref, force bool) (compute.Instance, error) {
	if err := c.requireInstance(ctx, ref); err != nil {
		return compute.Instance{}, err
	}
	action := "restart"
	state, err := c.instanceState(ctx, ref)
	if err != nil {
		return compute.Instance{}, err
	}
	if state.StatusCode == api.Stopped {
		action = startAction
	}
	if err := c.changeState(ctx, ref, action, force); err != nil {
		return compute.Instance{}, err
	}
	if err := c.waitRunning(ctx, projectName(ref.Sandbox), ref.Name); err != nil {
		return compute.Instance{}, err
	}
	return c.GetInstance(ctx, ref)
}

// WaitInstance polls guest state until the requested stage is reached.
func (c *Client) WaitInstance(ctx context.Context, req compute.WaitRequest) (compute.WaitResult, error) {
	if err := c.requireInstance(ctx, req.Ref); err != nil {
		return compute.WaitResult{}, err
	}
	lastStatus := ""
	for {
		if err := ctx.Err(); err != nil {
			return compute.WaitResult{Status: lastStatus}, err
		}
		ok, status, err := c.waitSatisfied(ctx, req)
		lastStatus = status
		if err != nil {
			return compute.WaitResult{Status: status}, err
		}
		if ok {
			return compute.WaitResult{Status: status}, nil
		}
		timer := time.NewTimer(runningPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return compute.WaitResult{Status: lastStatus}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) changeState(ctx context.Context, ref compute.Ref, action string, force bool) error {
	srv := c.Scoped(ctx, projectName(ref.Sandbox), "")
	state, _, err := srv.GetInstanceState(ref.Name)
	if err != nil {
		return mapError(err)
	}
	if stateSatisfiesAction(action, state.StatusCode) {
		return nil
	}
	op, err := srv.UpdateInstanceState(ref.Name, api.InstanceStatePut{
		Action:  action,
		Timeout: -1,
		Force:   force,
	}, "")
	if err != nil {
		if state, _, stateErr := srv.GetInstanceState(ref.Name); stateErr == nil &&
			stateSatisfiesAction(action, state.StatusCode) {
			return nil
		}
		return mapError(err)
	}
	return waitOp(ctx, op)
}

func (c *Client) waitStopped(ctx context.Context, project, name string) error {
	srv := c.Scoped(ctx, project, "")
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, _, err := srv.GetInstanceState(name)
		if err != nil {
			return mapError(err)
		}
		if state.StatusCode == api.Stopped {
			return nil
		}
		if state.StatusCode == api.Error {
			return errors.New("instance entered error state")
		}
		timer := time.NewTimer(runningPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) waitSatisfied(ctx context.Context, req compute.WaitRequest) (bool, string, error) {
	full, _, err := c.Scoped(ctx, projectName(req.Ref.Sandbox), "").GetInstanceFull(req.Ref.Name)
	if err != nil {
		return false, "", mapError(err)
	}
	status := full.Status
	if full.StatusCode == api.Error {
		return false, status, errors.New("instance entered error state")
	}
	switch req.Until {
	case compute.WaitUntilRunning:
		return instanceRunningOrReady(full.StatusCode), status, nil
	case compute.WaitUntilStopped:
		return full.StatusCode == api.Stopped, status, nil
	case compute.WaitUntilAgent:
		return agentWaitReady(full), status, nil
	case compute.WaitUntilNetwork:
		ready, waitErr := c.networkWaitReady(ctx, req.Ref.Sandbox, full)
		return ready, status, waitErr
	default:
		return false, status, fmt.Errorf("unsupported wait stage %q", req.Until)
	}
}

func instanceRunningOrReady(code api.StatusCode) bool {
	return code == api.Running || code == api.Ready
}

func stateSatisfiesAction(action string, code api.StatusCode) bool {
	switch action {
	case startAction:
		return instanceRunningOrReady(code)
	case stopAction:
		return code == api.Stopped
	default:
		return false
	}
}

func agentWaitReady(full *api.InstanceFull) bool {
	if !instanceRunningOrReady(full.StatusCode) {
		return false
	}
	if full.Type != string(api.InstanceTypeVM) {
		return true
	}
	return full.State != nil && (full.State.Processes >= 0 || full.State.OSInfo != nil)
}

func (c *Client) networkWaitReady(ctx context.Context, sandbox string, full *api.InstanceFull) (bool, error) {
	if !instanceRunningOrReady(full.StatusCode) {
		return false, nil
	}
	nics, err := c.mapNICs(ctx, sandbox, full)
	if err != nil {
		return false, err
	}
	for _, nic := range nics {
		if len(nic.Addresses) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) instanceState(ctx context.Context, ref compute.Ref) (*api.InstanceState, error) {
	state, _, err := c.Scoped(ctx, projectName(ref.Sandbox), "").GetInstanceState(ref.Name)
	if err != nil {
		return nil, mapError(err)
	}
	return state, nil
}

func (c *Client) requireInstance(ctx context.Context, ref compute.Ref) error {
	if ref.Sandbox == "" || ref.Name == "" {
		return errors.New("instance reference is required")
	}
	if _, _, err := c.getProject(ctx, ref.Sandbox); err != nil {
		return err
	}
	if _, _, err := c.Scoped(ctx, projectName(ref.Sandbox), "").GetInstance(ref.Name); err != nil {
		return mapError(err)
	}
	return nil
}
