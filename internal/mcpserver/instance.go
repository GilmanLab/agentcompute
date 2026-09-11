package mcpserver

import (
	"context"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

type instanceCreateIn struct {
	Sandbox  string  `json:"sandbox"`
	Name     string  `json:"name"`
	Image    string  `json:"image"`
	Kind     *string `json:"kind,omitempty"`
	CPUs     *int64  `json:"cpus,omitempty"`
	MemoryMB *int64  `json:"memory_mb,omitempty"`
	DiskGB   *int64  `json:"disk_gb,omitempty"`
	Network  *string `json:"network,omitempty"`
	Host     *string `json:"host,omitempty"`
	Start    *bool   `json:"start,omitempty"`
}

type instanceCreateOut struct {
	Name      string              `json:"name"`
	Kind      string              `json:"kind"`
	Host      string              `json:"host"`
	Status    string              `json:"status"`
	Addresses map[string][]string `json:"addresses"`
}

type instanceListIn struct {
	Sandbox string `json:"sandbox"`
}

type instanceListItem struct {
	Name      string              `json:"name"`
	Kind      string              `json:"kind"`
	Image     string              `json:"image"`
	Status    string              `json:"status"`
	Addresses map[string][]string `json:"addresses"`
}

type instanceListOut struct {
	Items []instanceListItem `json:"items"`
}

type instanceGetIn struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
}

type nicOut struct {
	Name      string   `json:"name"`
	Network   string   `json:"network"`
	MAC       string   `json:"mac"`
	Addresses []string `json:"addresses"`
}

type instanceGetOut struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Image     string   `json:"image"`
	Status    string   `json:"status"`
	CPUs      int64    `json:"cpus"`
	MemoryMB  int64    `json:"memory_mb"`
	NICs      []nicOut `json:"nics"`
	Desktop   bool     `json:"desktop"`
	Snapshots []string `json:"snapshots"`
}

type instanceDeleteIn struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
}

type instanceDeleteOut struct{}

type instanceExecIn struct {
	Sandbox        string  `json:"sandbox"`
	Name           string  `json:"name"`
	Command        string  `json:"command"`
	TimeoutSeconds *int64  `json:"timeout_seconds,omitempty"`
	User           *string `json:"user,omitempty"`
	Cwd            *string `json:"cwd,omitempty"`
	Stdin          *string `json:"stdin,omitempty"`
	Env            *string `json:"env,omitempty"`
}

type instanceExecOut struct {
	ExitCode        int64  `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	TimedOut        bool   `json:"timed_out"`
}

type instanceAPI struct {
	instances instanceService
	images    imageService
}

//nolint:dupl // Explicit typed registrations keep each capability's contract visible.
func registerInstance(builder *codemode.Builder, deps Dependencies) {
	api := instanceAPI{instances: deps.Instance, images: deps.Image}
	codemode.Register(builder, codemode.Capability[instanceCreateIn, instanceCreateOut]{
		ID:      capabilityInstanceCreate,
		Name:    capabilityInstanceCreate,
		Summary: "Create a container in a sandbox.",
		Handler: api.create,
	})
	codemode.Register(builder, codemode.Capability[instanceListIn, instanceListOut]{
		ID:      capabilityInstanceList,
		Name:    capabilityInstanceList,
		Summary: "List instances in a sandbox.",
		Handler: api.list,
	})
	codemode.Register(builder, codemode.Capability[instanceGetIn, instanceGetOut]{
		ID:      capabilityInstanceGet,
		Name:    capabilityInstanceGet,
		Summary: "Get one instance including NICs and snapshots.",
		Handler: api.get,
	})
	codemode.Register(builder, codemode.Capability[instanceDeleteIn, instanceDeleteOut]{
		ID:      capabilityInstanceDelete,
		Name:    capabilityInstanceDelete,
		Summary: "Delete an instance from a sandbox.",
		Handler: api.delete,
	})
	codemode.Register(builder, codemode.Capability[instanceExecIn, instanceExecOut]{
		ID:      capabilityInstanceExec,
		Name:    capabilityInstanceExec,
		Summary: "Run a shell command in an instance.",
		Handler: api.exec,
	})
}

func (api instanceAPI) create(
	ctx context.Context,
	_ authz.Subject,
	in instanceCreateIn,
) (instanceCreateOut, error) {
	image, err := api.images.CatalogImage(in.Image)
	if err != nil {
		return instanceCreateOut{}, err
	}
	instance, err := api.instances.CreateInstance(ctx, compute.CreateInstance{
		Ref:      compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		Image:    image,
		Kind:     deref(in.Kind, image.Kind),
		Network:  deref(in.Network, ""),
		Host:     deref(in.Host, ""),
		CPUs:     deref(in.CPUs, image.CPUs),
		MemoryMB: deref(in.MemoryMB, image.MemoryMB),
		DiskGB:   deref(in.DiskGB, image.DiskGB),
		Start:    deref(in.Start, true),
	})
	if err != nil {
		return instanceCreateOut{}, err
	}
	return instanceCreateOut{
		Name:      instance.Ref.Name,
		Kind:      instance.Kind,
		Host:      instance.Host,
		Status:    instance.Status,
		Addresses: nicAddresses(instance.NICs),
	}, nil
}

func (api instanceAPI) list(
	ctx context.Context,
	_ authz.Subject,
	in instanceListIn,
) (instanceListOut, error) {
	instances, err := api.instances.ListInstances(ctx, in.Sandbox)
	if err != nil {
		return instanceListOut{}, err
	}
	return instanceListOut{Items: instanceListItemDTOs(instances)}, nil
}

func (api instanceAPI) get(
	ctx context.Context,
	_ authz.Subject,
	in instanceGetIn,
) (instanceGetOut, error) {
	instance, err := api.instances.GetInstance(ctx, compute.Ref{Sandbox: in.Sandbox, Name: in.Name})
	if err != nil {
		return instanceGetOut{}, err
	}
	return instanceGetOut{
		Name:      instance.Ref.Name,
		Kind:      instance.Kind,
		Image:     instance.Image,
		Status:    instance.Status,
		CPUs:      instance.CPUs,
		MemoryMB:  instance.MemoryMB,
		NICs:      nicDTOs(instance.NICs),
		Desktop:   instance.Desktop,
		Snapshots: nonNilStrings(instance.Snapshots),
	}, nil
}

func (api instanceAPI) delete(
	ctx context.Context,
	_ authz.Subject,
	in instanceDeleteIn,
) (instanceDeleteOut, error) {
	if err := api.instances.DeleteInstance(ctx, compute.Ref{Sandbox: in.Sandbox, Name: in.Name}); err != nil {
		return instanceDeleteOut{}, err
	}
	return instanceDeleteOut{}, nil
}

func (api instanceAPI) exec(
	ctx context.Context,
	_ authz.Subject,
	in instanceExecIn,
) (instanceExecOut, error) {
	var timeout int64
	if in.TimeoutSeconds != nil {
		timeout = *in.TimeoutSeconds
	}
	duration, err := secondsToDuration(timeout)
	if err != nil {
		return instanceExecOut{}, err
	}
	var env map[string]string
	if in.Env != nil {
		env, err = parseEnv(*in.Env)
		if err != nil {
			return instanceExecOut{}, err
		}
	}
	result, err := api.instances.Exec(ctx, compute.ExecRequest{
		Ref:     compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		Argv:    []string{"sh", "-c", in.Command},
		User:    deref(in.User, ""),
		Cwd:     deref(in.Cwd, ""),
		Env:     env,
		Stdin:   deref(in.Stdin, ""),
		Timeout: duration,
	})
	if err != nil {
		return instanceExecOut{}, err
	}
	return instanceExecOut{
		ExitCode:        result.ExitCode,
		Stdout:          result.Stdout,
		Stderr:          result.Stderr,
		StdoutTruncated: result.StdoutTruncated,
		StderrTruncated: result.StderrTruncated,
		TimedOut:        result.TimedOut,
	}, nil
}
