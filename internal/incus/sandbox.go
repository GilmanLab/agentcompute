package incus

import (
	"context"
	"errors"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

// CreateSandbox creates a restricted project and its default all-member bridge.
func (c *Client) CreateSandbox(ctx context.Context, sandbox compute.Sandbox) error {
	if sandbox.Name == "" {
		return errors.New("sandbox name is required")
	}
	host := sandbox.Host
	if host == "" {
		host = c.host
	}
	physical, err := c.allocatePhysicalName(ctx)
	if err != nil {
		return err
	}

	created := sandbox.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	expires := sandbox.ExpiresAt.UTC()
	config := sandboxProjectConfig(host, physical, sandbox.Subject, created, expires)

	err = c.Scoped(ctx, "", "").CreateProject(api.ProjectsPost{
		Name: projectName(sandbox.Name),
		ProjectPut: api.ProjectPut{
			Config: config,
		},
	})
	if err != nil {
		return mapError(err)
	}

	_, err = c.createReservedBridge(ctx, physical, sandbox.Name, defaultLogicalNetwork, compute.Network{
		Name: defaultLogicalNetwork,
		Kind: networkKindBridge,
		Host: host,
		DHCP: true,
		NAT:  true,
		DNS:  true,
	}, false)
	if err != nil {
		return err
	}
	return nil
}

// ListSandboxes returns owned sandbox projects.
func (c *Client) ListSandboxes(ctx context.Context) ([]compute.Sandbox, error) {
	projects, err := c.Scoped(ctx, "", "").GetProjects()
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]compute.Sandbox, 0, len(projects))
	for _, project := range projects {
		sandbox, ok := parseSandbox(project)
		if !ok {
			continue
		}
		if sandbox.Host == "" {
			sandbox.Host = c.host
		}
		out = append(out, sandbox)
	}
	return out, nil
}

// GetSandbox returns one owned sandbox.
func (c *Client) GetSandbox(ctx context.Context, name string) (compute.Sandbox, error) {
	project, _, err := c.getProject(ctx, name)
	if err != nil {
		return compute.Sandbox{}, err
	}
	sandbox, ok := parseSandbox(*project)
	if !ok {
		return compute.Sandbox{}, compute.ErrNotFound
	}
	if sandbox.Host == "" {
		sandbox.Host = c.host
	}
	return sandbox, nil
}

// ExtendSandbox writes a new expiry with an ETag update.
func (c *Client) ExtendSandbox(ctx context.Context, name string, expires time.Time) (compute.Sandbox, error) {
	err := c.patchProject(ctx, name, func(project *api.Project) {
		if project.Config == nil {
			project.Config = map[string]string{}
		}
		project.Config[metaExpiresAt] = expires.UTC().Format(time.RFC3339Nano)
	})
	if err != nil {
		return compute.Sandbox{}, err
	}
	return c.GetSandbox(ctx, name)
}

// DeleteSandbox removes sandbox resources in dependency order.
//
// Partial failures are returned so the reaper can retry. The project is never
// deleted while owned bridges still exist.
func (c *Client) DeleteSandbox(ctx context.Context, name string) error {
	project, _, projectErr := c.getProject(ctx, name)
	if projectErr != nil && !errors.Is(projectErr, compute.ErrNotFound) {
		return projectErr
	}

	physicals, err := c.sandboxBridgeNames(ctx, name, project)
	if err != nil {
		return err
	}
	var errs []error
	for physical := range physicals {
		errs = append(errs, c.deleteForwards(ctx, physical)...)
	}

	if project != nil {
		if err := c.deleteProjectContents(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}

	var remaining []string
	for physical := range physicals {
		if err := c.deleteBridge(ctx, physical); err != nil {
			errs = append(errs, err)
			remaining = append(remaining, physical)
		}
	}
	if len(remaining) > 0 {
		errs = append(errs, errors.New("bridges still present"))
		return errors.Join(errs...)
	}

	if project != nil {
		if err := c.Scoped(ctx, "", "").
			DeleteProject(projectName(name)); err != nil &&
			!errors.Is(mapError(err), compute.ErrNotFound) {
			errs = append(errs, mapError(err))
		}
	}

	joined := errors.Join(errs...)
	if joined != nil {
		return joined
	}
	if project == nil && len(physicals) == 0 {
		return compute.ErrNotFound
	}
	return nil
}

func (c *Client) sandboxBridgeNames(
	ctx context.Context,
	name string,
	project *api.Project,
) (map[string]struct{}, error) {
	physicals := map[string]struct{}{}
	if project != nil {
		for _, physical := range reservedNetworks(project.Config) {
			physicals[physical] = struct{}{}
		}
	}
	owned, err := c.ownedBridges(ctx, name)
	if err != nil && !errors.Is(err, compute.ErrNotFound) {
		return nil, err
	}
	for _, network := range owned {
		physicals[network.PhysicalName] = struct{}{}
	}
	for physical := range physicals {
		if err := c.checkBridgeOwnership(ctx, name, physical); err != nil {
			return nil, err
		}
	}
	return physicals, nil
}

func sandboxProjectConfig(host, physical, subject string, created, expires time.Time) map[string]string {
	return map[string]string{
		"features.images":                 configTrue,
		"features.profiles":               configTrue,
		"features.networks":               "false",
		"restricted":                      configTrue,
		"restricted.containers.nesting":   configBlock,
		"restricted.containers.privilege": "unprivileged",
		"restricted.containers.lowlevel":  configBlock,
		"restricted.cluster.target":       "allow",
		"restricted.devices.nic":          configManaged,
		"restricted.devices.disk":         configManaged,
		"restricted.devices.gpu":          configBlock,
		"restricted.devices.pci":          configBlock,
		"restricted.devices.proxy":        configBlock,
		"restricted.devices.usb":          configBlock,
		"restricted.devices.unix-block":   configBlock,
		"restricted.devices.unix-char":    configBlock,
		"restricted.devices.unix-hotplug": configBlock,
		"restricted.devices.infiniband":   configBlock,
		"restricted.networks.access":      physical,
		metaVersion:                       versionValue,
		metaHost:                          host,
		metaCreatedAt:                     created.Format(time.RFC3339Nano),
		metaExpiresAt:                     expires.Format(time.RFC3339Nano),
		metaSubject:                       subject,
		networkKey(defaultLogicalNetwork): physical,
	}
}

func (c *Client) deleteProjectContents(ctx context.Context, sandbox string) error {
	srv := c.Scoped(ctx, projectName(sandbox), "")
	var errs []error

	instances, err := srv.GetInstances(api.InstanceTypeAny)
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		errs = append(errs, mapError(err))
	}
	for _, instance := range instances {
		if deleteErr := c.forceDeleteInstance(
			ctx,
			sandbox,
			instance.Name,
		); deleteErr != nil &&
			!errors.Is(deleteErr, compute.ErrNotFound) {
			errs = append(errs, deleteErr)
		}
	}

	images, err := srv.GetImages()
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		errs = append(errs, mapError(err))
	}
	for _, image := range images {
		if deleteErr := waitDelete(ctx, func() (incusclient.Operation, error) {
			return srv.DeleteImage(image.Fingerprint)
		}); deleteErr != nil {
			errs = append(errs, deleteErr)
		}
	}

	profiles, err := srv.GetProfiles()
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		errs = append(errs, mapError(err))
	}
	for _, profile := range profiles {
		if profile.Name == "default" {
			continue
		}
		if err := srv.DeleteProfile(profile.Name); err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
			errs = append(errs, mapError(err))
		}
	}

	return errors.Join(errs...)
}

func waitDelete(ctx context.Context, fn func() (incusclient.Operation, error)) error {
	op, err := fn()
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return nil
		}
		return mapError(err)
	}
	return waitOp(ctx, op)
}
