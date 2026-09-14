package mcpserver

import (
	"context"
	"time"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	capabilityInstanceStart   = "instance.start"
	capabilityInstanceStop    = "instance.stop"
	capabilityInstanceRestart = "instance.restart"
	capabilityInstanceWait    = "instance.wait"
)

type instanceStateIn struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
	Force   *bool  `json:"force,omitempty"`
}

type instanceStateOut struct {
	Status string `json:"status"`
}

type instanceWaitIn struct {
	Sandbox        string `json:"sandbox"`
	Name           string `json:"name"`
	Until          string `json:"until"`
	TimeoutSeconds *int64 `json:"timeout_seconds,omitempty"`
}

type instanceWaitOut struct {
	Status         string `json:"status"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
}

func registerInstanceLifecycle(builder *codemode.Builder, api instanceAPI) {
	codemode.Register(builder, codemode.Capability[instanceStateIn, instanceStateOut]{
		ID:      capabilityInstanceStart,
		Name:    capabilityInstanceStart,
		Summary: "Start an instance and wait until it is running.",
		Handler: api.start,
	})
	codemode.Register(builder, codemode.Capability[instanceStateIn, instanceStateOut]{
		ID:      capabilityInstanceStop,
		Name:    capabilityInstanceStop,
		Summary: "Stop an instance and wait until it is stopped.",
		Handler: api.stop,
	})
	codemode.Register(builder, codemode.Capability[instanceStateIn, instanceStateOut]{
		ID:      capabilityInstanceRestart,
		Name:    capabilityInstanceRestart,
		Summary: "Restart an instance and wait until it is running.",
		Handler: api.restart,
	})
	codemode.Register(builder, codemode.Capability[instanceWaitIn, instanceWaitOut]{
		ID:      capabilityInstanceWait,
		Name:    capabilityInstanceWait,
		Summary: "Wait until an instance reaches a readiness stage.",
		Handler: api.wait,
	})
}

func (api instanceAPI) start(
	ctx context.Context,
	_ authz.Subject,
	in instanceStateIn,
) (instanceStateOut, error) {
	instance, err := api.instances.StartInstance(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		deref(in.Force, false),
	)
	if err != nil {
		return instanceStateOut{}, err
	}
	return instanceStateOut{Status: instance.Status}, nil
}

func (api instanceAPI) stop(
	ctx context.Context,
	_ authz.Subject,
	in instanceStateIn,
) (instanceStateOut, error) {
	instance, err := api.instances.StopInstance(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		deref(in.Force, false),
	)
	if err != nil {
		return instanceStateOut{}, err
	}
	return instanceStateOut{Status: instance.Status}, nil
}

func (api instanceAPI) restart(
	ctx context.Context,
	_ authz.Subject,
	in instanceStateIn,
) (instanceStateOut, error) {
	instance, err := api.instances.RestartInstance(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		deref(in.Force, false),
	)
	if err != nil {
		return instanceStateOut{}, err
	}
	return instanceStateOut{Status: instance.Status}, nil
}

func (api instanceAPI) wait(
	ctx context.Context,
	_ authz.Subject,
	in instanceWaitIn,
) (instanceWaitOut, error) {
	var timeout int64
	if in.TimeoutSeconds != nil {
		timeout = *in.TimeoutSeconds
	}
	duration, err := secondsToDuration(timeout)
	if err != nil {
		return instanceWaitOut{}, err
	}
	result, err := api.instances.WaitInstance(ctx, compute.WaitRequest{
		Ref:     compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		Until:   in.Until,
		Timeout: duration,
	})
	if err != nil {
		return instanceWaitOut{}, err
	}
	return instanceWaitOut{
		Status:         result.Status,
		ElapsedSeconds: int64(result.Elapsed / time.Second),
	}, nil
}
