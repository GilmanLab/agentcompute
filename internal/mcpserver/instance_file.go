package mcpserver

import (
	"context"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	capabilityInstanceFileRead  = "instance.file.read"
	capabilityInstanceFileWrite = "instance.file.write"
)

type instanceFileReadIn struct {
	Sandbox  string `json:"sandbox"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	MaxBytes *int64 `json:"max_bytes,omitempty"`
}

type instanceFileReadOut struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

type instanceFileWriteIn struct {
	Sandbox string  `json:"sandbox"`
	Name    string  `json:"name"`
	Path    string  `json:"path"`
	Content string  `json:"content"`
	Mode    *string `json:"mode,omitempty"`
}

type instanceFileWriteOut struct {
	Bytes int64 `json:"bytes"`
}

func registerInstanceFiles(builder *codemode.Builder, api instanceAPI) {
	codemode.Register(builder, codemode.Capability[instanceFileReadIn, instanceFileReadOut]{
		ID:      capabilityInstanceFileRead,
		Name:    capabilityInstanceFileRead,
		Summary: "Read a text-sized file from an instance.",
		Handler: api.readFile,
	})
	codemode.Register(builder, codemode.Capability[instanceFileWriteIn, instanceFileWriteOut]{
		ID:      capabilityInstanceFileWrite,
		Name:    capabilityInstanceFileWrite,
		Summary: "Write a text-sized file to an instance.",
		Handler: api.writeFile,
	})
}

func (api instanceAPI) readFile(
	ctx context.Context,
	_ authz.Subject,
	in instanceFileReadIn,
) (instanceFileReadOut, error) {
	result, err := api.instances.ReadFile(ctx, compute.FileReadRequest{
		Ref:      compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		Path:     in.Path,
		MaxBytes: deref(in.MaxBytes, 0),
	})
	if err != nil {
		return instanceFileReadOut{}, err
	}
	return instanceFileReadOut{Content: result.Content, Truncated: result.Truncated}, nil
}

func (api instanceAPI) writeFile(
	ctx context.Context,
	_ authz.Subject,
	in instanceFileWriteIn,
) (instanceFileWriteOut, error) {
	result, err := api.instances.WriteFile(ctx, compute.FileWriteRequest{
		Ref:     compute.Ref{Sandbox: in.Sandbox, Name: in.Name},
		Path:    in.Path,
		Content: in.Content,
		Mode:    deref(in.Mode, ""),
	})
	if err != nil {
		return instanceFileWriteOut{}, err
	}
	return instanceFileWriteOut{Bytes: result.Bytes}, nil
}
