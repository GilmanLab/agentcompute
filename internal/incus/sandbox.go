package incus

import (
	"context"
	"errors"
	"fmt"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

// CreateSandbox creates a restricted project and its default network.
func (c *Client) CreateSandbox(ctx context.Context, sandbox compute.Sandbox) error {
	if sandbox.Name == "" {
		return errors.New("sandbox name is required")
	}
	kind := sandbox.NetworkKind
	if kind == "" {
		kind = networkKindBridge
	}
	switch kind {
	case networkKindOVN:
		return c.createOVNSandbox(ctx, sandbox)
	case networkKindBridge:
		return c.createBridgeSandbox(ctx, sandbox)
	default:
		return fmt.Errorf("network kind %q is not available", kind)
	}
}

func (c *Client) createBridgeSandbox(ctx context.Context, sandbox compute.Sandbox) error {
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
	setSandboxPin(config, sandbox.Pinned, sandbox.PinnedBy, sandbox.PinnedAt)

	err = c.Scoped(ctx, "", "").CreateProject(api.ProjectsPost{
		Name: projectName(sandbox.Name),
		ProjectPut: api.ProjectPut{
			Config: config,
		},
	})
	if err != nil {
		return c.recoverBridgeProjectConflict(ctx, sandbox, host, physical, err)
	}

	_, err = c.createReservedBridge(ctx, physical, sandbox.Name, defaultLogicalNetwork, compute.Network{
		Name: defaultLogicalNetwork,
		Kind: networkKindBridge,
		Host: host,
		DHCP: true,
		NAT:  true,
		DNS:  true,
	}, false)
	return err
}

func (c *Client) recoverBridgeProjectConflict(
	ctx context.Context,
	sandbox compute.Sandbox,
	host, physical string,
	createErr error,
) error {
	if !isConflict(createErr) {
		return mapError(createErr)
	}
	existing, _, getErr := c.getProject(ctx, sandbox.Name)
	if getErr != nil {
		return mapError(createErr)
	}
	reserved := reservedNetworks(existing.Config)[defaultLogicalNetwork]
	if reserved == "" {
		reserved = physical
	}
	_, err := c.createReservedBridge(ctx, reserved, sandbox.Name, defaultLogicalNetwork, compute.Network{
		Name: defaultLogicalNetwork,
		Kind: networkKindBridge,
		Host: host,
		DHCP: true,
		NAT:  true,
		DNS:  true,
	}, true)
	return err
}

func (c *Client) createOVNSandbox(ctx context.Context, sandbox compute.Sandbox) error {
	created := sandbox.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	expires := sandbox.ExpiresAt.UTC()
	config := ovnProjectConfig(sandbox.Subject, created, expires, c.ovnUplinkName())
	setSandboxPin(config, sandbox.Pinned, sandbox.PinnedBy, sandbox.PinnedAt)

	err := c.Scoped(ctx, "", "").CreateProject(api.ProjectsPost{
		Name: projectName(sandbox.Name),
		ProjectPut: api.ProjectPut{
			Config: config,
		},
	})
	if err != nil && !isConflict(err) {
		return mapError(err)
	}
	if isConflict(err) {
		if _, _, getErr := c.getProject(ctx, sandbox.Name); getErr != nil {
			return mapError(err)
		}
	}
	_, err = c.createOVNNetwork(ctx, sandbox.Name, compute.Network{
		Name: defaultLogicalNetwork,
		Kind: networkKindOVN,
		DHCP: true,
		NAT:  true,
		DNS:  true,
	})
	return err
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
		if sandbox.Host == "" && (sandbox.NetworkKind == "" || sandbox.NetworkKind == networkKindBridge) {
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
	if sandbox.Host == "" && (sandbox.NetworkKind == "" || sandbox.NetworkKind == networkKindBridge) {
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

// PinSandbox persists pin metadata with an ETag update, preserving expiry.
func (c *Client) PinSandbox(
	ctx context.Context,
	name string,
	pinned bool,
	subject string,
	since time.Time,
) (compute.Sandbox, error) {
	err := c.patchProject(ctx, name, func(project *api.Project) {
		if project.Config == nil {
			project.Config = map[string]string{}
		}
		setSandboxPin(project.Config, pinned, subject, since)
	})
	if err != nil {
		return compute.Sandbox{}, err
	}
	return c.GetSandbox(ctx, name)
}

func setSandboxPin(config map[string]string, pinned bool, subject string, since time.Time) {
	if !pinned {
		delete(config, metaPinned)
		delete(config, metaPinnedBy)
		delete(config, metaPinnedAt)
		return
	}
	config[metaPinned] = configTrue
	config[metaPinnedBy] = subject
	config[metaPinnedAt] = since.UTC().Format(time.RFC3339Nano)
}

// DeleteSandbox removes sandbox resources in dependency order.
//
// Partial failures are returned so the reaper can retry. The project is never
// deleted while owned networks still exist.
func (c *Client) DeleteSandbox(ctx context.Context, name string) error {
	project, _, projectErr := c.getProject(ctx, name)
	if projectErr != nil && !errors.Is(projectErr, compute.ErrNotFound) {
		return projectErr
	}
	physicals, err := c.sandboxBridgeNames(ctx, name, project)
	if err != nil {
		return err
	}
	ovns, err := c.ownedOVNNetworks(ctx, name)
	if err != nil {
		return err
	}
	if err = c.deleteSandboxForwarding(ctx, name, ovns, physicals); err != nil {
		return err
	}
	if project != nil {
		if err = c.emptySandboxProject(ctx, name); err != nil {
			return err
		}
	}
	if err = c.deleteSandboxNetworks(ctx, name, ovns, physicals); err != nil {
		return err
	}
	remaining, err := c.ownedOVNNetworks(ctx, name)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return errors.New("owned OVN networks still present")
	}
	if project != nil {
		if err = c.deleteSandboxProject(ctx, name); err != nil {
			return err
		}
	}
	if project == nil && len(physicals) == 0 && len(ovns) == 0 {
		return compute.ErrNotFound
	}
	return nil
}

func (c *Client) deleteSandboxForwarding(
	ctx context.Context,
	sandbox string,
	ovns []compute.Network,
	physicals map[string]struct{},
) error {
	var errs []error
	for _, network := range ovns {
		errs = append(errs, c.deleteForwardsInProject(ctx, projectName(sandbox), network.PhysicalName)...)
		errs = append(errs, c.deletePeers(ctx, sandbox, network.PhysicalName)...)
	}
	for physical := range physicals {
		errs = append(errs, c.deleteForwards(ctx, physical)...)
	}
	return errors.Join(errs...)
}

func (c *Client) emptySandboxProject(ctx context.Context, name string) error {
	if err := c.detachSandboxNICs(ctx, name); err != nil {
		return err
	}
	if err := c.deleteProjectContents(ctx, name); err != nil {
		return err
	}
	return c.clearProfileNetworkRefs(ctx, name)
}

func (c *Client) deleteSandboxNetworks(
	ctx context.Context,
	name string,
	ovns []compute.Network,
	physicals map[string]struct{},
) error {
	var errs []error
	for _, network := range ovns {
		if err := c.deleteOVNNetwork(ctx, name, network.PhysicalName); err != nil {
			errs = append(errs, err)
		}
	}
	for physical := range physicals {
		if err := c.deleteBridge(ctx, physical); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Client) deleteSandboxProject(ctx context.Context, name string) error {
	if err := errors.Join(c.deleteOwnedACLs(ctx, name)...); err != nil {
		return err
	}
	if err := c.Scoped(ctx, "", "").DeleteProject(projectName(name)); err != nil &&
		!errors.Is(mapError(err), compute.ErrNotFound) {
		return mapError(err)
	}
	return nil
}

func (c *Client) sandboxBridgeNames(
	ctx context.Context,
	name string,
	project *api.Project,
) (map[string]struct{}, error) {
	physicals := map[string]struct{}{}
	if project != nil && !featuresNetworks(project) {
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
		featuresNetworksKey:               configFalse,
		"restricted":                      configTrue,
		"restricted.containers.nesting":   configBlock,
		"restricted.containers.privilege": "unprivileged",
		"restricted.containers.lowlevel":  configBlock,
		"restricted.cluster.target":       aclActionAllow,
		"restricted.snapshots":            aclActionAllow,
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

func ovnProjectConfig(subject string, created, expires time.Time, uplink string) map[string]string {
	return map[string]string{
		"features.images":                 configTrue,
		"features.profiles":               configTrue,
		featuresNetworksKey:               configTrue,
		"restricted":                      configTrue,
		"restricted.containers.nesting":   configBlock,
		"restricted.containers.privilege": "unprivileged",
		"restricted.containers.lowlevel":  configBlock,
		"restricted.cluster.target":       aclActionAllow,
		"restricted.snapshots":            aclActionAllow,
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
		"restricted.networks.uplinks":     uplink,
		metaVersion:                       versionValue,
		metaCreatedAt:                     created.Format(time.RFC3339Nano),
		metaExpiresAt:                     expires.Format(time.RFC3339Nano),
		metaSubject:                       subject,
	}
}

func (c *Client) detachSandboxNICs(ctx context.Context, sandbox string) error {
	instances, err := c.ListInstances(ctx, sandbox)
	if err != nil && !errors.Is(err, compute.ErrNotFound) {
		return err
	}
	var errs []error
	for _, instance := range instances {
		if _, err := c.StopInstance(ctx, instance.Ref, true); err != nil {
			errs = append(errs, err)
			continue
		}
		for _, nic := range instance.NICs {
			if nic.Name == "" {
				continue
			}
			if err := c.DetachNIC(ctx, instance.Ref, nic.Name); err != nil && !errors.Is(err, compute.ErrNotFound) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Client) deleteProjectContents(ctx context.Context, sandbox string) error {
	srv := c.Scoped(ctx, projectName(sandbox), "")
	var errs []error

	instances, err := srv.GetInstances(api.InstanceTypeAny)
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		return mapError(err)
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
	if err = errors.Join(errs...); err != nil {
		return err
	}

	images, err := srv.GetImages()
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		return mapError(err)
	}
	for _, image := range images {
		if deleteErr := waitDelete(ctx, func() (incusclient.Operation, error) {
			return srv.DeleteImage(image.Fingerprint)
		}); deleteErr != nil {
			errs = append(errs, deleteErr)
		}
	}

	return errors.Join(errs...)
}

func (c *Client) clearProfileNetworkRefs(ctx context.Context, sandbox string) error {
	srv := c.Scoped(ctx, projectName(sandbox), "")
	profiles, err := srv.GetProfiles()
	if err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
		return mapError(err)
	}
	var errs []error
	for _, profile := range profiles {
		if profile.Name == "default" {
			if nicErr := stripDefaultProfileNICs(srv, profile); nicErr != nil {
				errs = append(errs, nicErr)
			}
			continue
		}
		if delErr := srv.DeleteProfile(
			profile.Name,
		); delErr != nil &&
			!errors.Is(mapError(delErr), compute.ErrNotFound) {
			errs = append(errs, mapError(delErr))
		}
	}
	return errors.Join(errs...)
}

func stripDefaultProfileNICs(srv incusclient.InstanceServer, profile api.Profile) error {
	devices := copyDevices(profile.Devices)
	changed := false
	for name, device := range devices {
		if device[deviceTypeKey] == deviceTypeNIC {
			delete(devices, name)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	writable := profile.Writable()
	writable.Devices = devices
	if err := srv.UpdateProfile(profile.Name, writable, ""); err != nil &&
		!errors.Is(mapError(err), compute.ErrNotFound) {
		return mapError(err)
	}
	return nil
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
