package mcpserver

import (
	"context"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const capabilityInstancePublish = "instance.publish"

type instancePublishIn struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
	Image   string `json:"image"`
}

type instancePublishOut struct {
	Image string `json:"image"`
}

func registerInstancePublish(builder *codemode.Builder, api instanceAPI) {
	codemode.Register(builder, codemode.Capability[instancePublishIn, instancePublishOut]{
		ID:      capabilityInstancePublish,
		Name:    capabilityInstancePublish,
		Summary: "Publish a sandbox-scoped image from an instance.",
		Handler: api.publish,
	})
}

func (api instanceAPI) publish(
	ctx context.Context,
	_ authz.Subject,
	in instancePublishIn,
) (instancePublishOut, error) {
	image, err := api.instances.PublishInstance(ctx, compute.Ref{Sandbox: in.Sandbox, Name: in.Name}, in.Image)
	if err != nil {
		return instancePublishOut{}, err
	}
	return instancePublishOut{Image: image}, nil
}
