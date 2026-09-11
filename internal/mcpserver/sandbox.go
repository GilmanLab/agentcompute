package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"
)

type sandboxCreateIn struct {
	Name       *string `json:"name,omitempty"`
	Platform   *string `json:"platform,omitempty"`
	TTLMinutes *int64  `json:"ttl_minutes,omitempty"`
}

type sandboxCreateOut struct {
	Name      string     `json:"name"`
	Platform  string     `json:"platform"`
	ExpiresAt string     `json:"expires_at"`
	Network   networkOut `json:"network"`
}

type sandboxListIn struct{}

type sandboxListItem struct {
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Instances int64  `json:"instances"`
}

type sandboxListOut struct {
	Items []sandboxListItem `json:"items"`
}

type sandboxGetIn struct {
	Name string `json:"name"`
}

type sandboxGetOut struct {
	Name      string             `json:"name"`
	Platform  string             `json:"platform"`
	CreatedAt string             `json:"created_at"`
	ExpiresAt string             `json:"expires_at"`
	Instances []instanceListItem `json:"instances"`
	Networks  []networkOut       `json:"networks"`
}

type sandboxExtendIn struct {
	Name       string `json:"name"`
	TTLMinutes int64  `json:"ttl_minutes"`
}

type sandboxExtendOut struct {
	ExpiresAt string `json:"expires_at"`
}

type sandboxDeleteIn struct {
	Name string `json:"name"`
}

type sandboxDeleteOut struct{}

type sandboxAPI struct {
	sandboxes sandboxService
	instances instanceService
}

//nolint:dupl // Explicit typed registrations keep each capability's contract visible.
func registerSandbox(builder *codemode.Builder, deps Dependencies) {
	api := sandboxAPI{sandboxes: deps.Sandbox, instances: deps.Instance}
	codemode.Register(builder, codemode.Capability[sandboxCreateIn, sandboxCreateOut]{
		ID:      capabilitySandboxCreate,
		Name:    capabilitySandboxCreate,
		Summary: "Create a time-limited sandbox and its default NAT network.",
		Handler: api.create,
	})
	codemode.Register(builder, codemode.Capability[sandboxListIn, sandboxListOut]{
		ID:      capabilitySandboxList,
		Name:    capabilitySandboxList,
		Summary: "List sandboxes with expiry and instance counts.",
		Handler: api.list,
	})
	codemode.Register(builder, codemode.Capability[sandboxGetIn, sandboxGetOut]{
		ID:      capabilitySandboxGet,
		Name:    capabilitySandboxGet,
		Summary: "Get one sandbox with its instances and networks.",
		Handler: api.get,
	})
	codemode.Register(builder, codemode.Capability[sandboxExtendIn, sandboxExtendOut]{
		ID:      capabilitySandboxExtend,
		Name:    capabilitySandboxExtend,
		Summary: "Extend a sandbox TTL from now.",
		Handler: api.extend,
	})
	codemode.Register(builder, codemode.Capability[sandboxDeleteIn, sandboxDeleteOut]{
		ID:      capabilitySandboxDelete,
		Name:    capabilitySandboxDelete,
		Summary: "Delete a sandbox and every resource inside it.",
		Handler: api.delete,
	})
}

func (api sandboxAPI) create(
	ctx context.Context,
	subject authz.Subject,
	in sandboxCreateIn,
) (sandboxCreateOut, error) {
	if err := validatePlatform(in.Platform); err != nil {
		return sandboxCreateOut{}, err
	}
	var ttl time.Duration
	if in.TTLMinutes != nil {
		var err error
		ttl, err = minutesToDuration(*in.TTLMinutes)
		if err != nil {
			return sandboxCreateOut{}, err
		}
	}
	sandbox, err := api.sandboxes.CreateSandbox(ctx, deref(in.Name, ""), ttl, string(subject.ID))
	if err != nil {
		return sandboxCreateOut{}, err
	}
	_, _, networks, err := api.sandboxes.GetSandbox(ctx, sandbox.Name)
	if err != nil {
		return sandboxCreateOut{}, err
	}
	network, ok := defaultNetwork(networks)
	if !ok {
		return sandboxCreateOut{}, fmt.Errorf("sandbox %q has no default network", sandbox.Name)
	}
	return sandboxCreateOut{
		Name:      sandbox.Name,
		Platform:  sandbox.Platform,
		ExpiresAt: formatTime(sandbox.ExpiresAt),
		Network:   networkDTO(network),
	}, nil
}

func (api sandboxAPI) list(
	ctx context.Context,
	_ authz.Subject,
	_ sandboxListIn,
) (sandboxListOut, error) {
	sandboxes, err := api.sandboxes.ListSandboxes(ctx)
	if err != nil {
		return sandboxListOut{}, err
	}
	items := make([]sandboxListItem, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		instances, err := api.instances.ListInstances(ctx, sandbox.Name)
		if err != nil {
			return sandboxListOut{}, err
		}
		items = append(items, sandboxListItemDTO(sandbox, int64(len(instances))))
	}
	return sandboxListOut{Items: items}, nil
}

func (api sandboxAPI) get(
	ctx context.Context,
	_ authz.Subject,
	in sandboxGetIn,
) (sandboxGetOut, error) {
	sandbox, instances, networks, err := api.sandboxes.GetSandbox(ctx, in.Name)
	if err != nil {
		return sandboxGetOut{}, err
	}
	return sandboxGetOut{
		Name:      sandbox.Name,
		Platform:  sandbox.Platform,
		CreatedAt: formatTime(sandbox.CreatedAt),
		ExpiresAt: formatTime(sandbox.ExpiresAt),
		Instances: instanceListItemDTOs(instances),
		Networks:  networkDTOs(networks),
	}, nil
}

func (api sandboxAPI) extend(
	ctx context.Context,
	_ authz.Subject,
	in sandboxExtendIn,
) (sandboxExtendOut, error) {
	ttl, err := minutesToDuration(in.TTLMinutes)
	if err != nil {
		return sandboxExtendOut{}, err
	}
	sandbox, err := api.sandboxes.ExtendSandbox(ctx, in.Name, ttl)
	if err != nil {
		return sandboxExtendOut{}, err
	}
	return sandboxExtendOut{ExpiresAt: formatTime(sandbox.ExpiresAt)}, nil
}

func (api sandboxAPI) delete(
	ctx context.Context,
	_ authz.Subject,
	in sandboxDeleteIn,
) (sandboxDeleteOut, error) {
	if err := api.sandboxes.DeleteSandbox(ctx, in.Name); err != nil {
		return sandboxDeleteOut{}, err
	}
	return sandboxDeleteOut{}, nil
}

func validatePlatform(platform *string) error {
	value := deref(platform, platformIncus)
	if value == "" {
		value = platformIncus
	}
	switch value {
	case platformIncus:
		return nil
	case platformMac:
		return agentErrorf("platform %q is not available yet", platformMac)
	default:
		return agentErrorf("unsupported platform %q", value)
	}
}
