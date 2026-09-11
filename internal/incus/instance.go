package incus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/units"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

type pendingInstance struct {
	client  *Client
	ref     compute.Ref
	project string
	start   bool
	op      incusclient.Operation
}

// Wait finishes an accepted create after the mutation gate is released.
func (p *pendingInstance) Wait(ctx context.Context) (compute.Instance, error) {
	if err := waitOp(ctx, p.op); err != nil {
		return compute.Instance{}, err
	}
	if p.start {
		if err := p.client.startInstance(ctx, p.project, p.ref.Name); err != nil {
			return compute.Instance{}, err
		}
		if err := p.client.waitRunning(ctx, p.project, p.ref.Name); err != nil {
			return compute.Instance{}, err
		}
	}
	return p.client.GetInstance(ctx, p.ref)
}

// BeginCreateInstance copies the image if needed and accepts the create request.
func (c *Client) BeginCreateInstance(ctx context.Context, req compute.CreateInstance) (compute.PendingInstance, error) {
	if req.Ref.Sandbox == "" || req.Ref.Name == "" {
		return nil, errors.New("instance reference is required")
	}
	project, _, err := c.getProject(ctx, req.Ref.Sandbox)
	if err != nil {
		return nil, err
	}
	sandbox, ok := parseSandbox(*project)
	if !ok {
		return nil, compute.ErrNotFound
	}
	host := sandbox.Host
	if host == "" {
		host = c.host
	}
	if req.Host != "" && req.Host != host {
		return nil, fmt.Errorf("host %q is not the sandbox member %q", req.Host, host)
	}

	source, err := c.instanceSource(ctx, projectName(req.Ref.Sandbox), req.Image)
	if err != nil {
		return nil, err
	}

	devices := api.DevicesMap{
		rootDeviceName: {
			deviceTypeKey: "disk",
			"path":        "/",
			"pool":        c.pool,
			"size":        fmt.Sprintf("%dGiB", req.DiskGB),
		},
	}
	if req.Network != configNone {
		logical := req.Network
		if logical == "" {
			logical = defaultLogicalNetwork
		}
		physical, resolveErr := c.resolvePhysical(ctx, req.Ref.Sandbox, logical)
		if resolveErr != nil {
			return nil, resolveErr
		}
		devices[defaultNICName] = map[string]string{
			deviceTypeKey: deviceTypeNIC,
			"network":     physical,
			"name":        defaultNICName,
		}
	}

	kind := api.InstanceTypeContainer
	if req.Kind == kindVM {
		kind = api.InstanceTypeVM
	}

	op, err := c.Scoped(ctx, projectName(req.Ref.Sandbox), host).CreateInstance(api.InstancesPost{
		Name:   req.Ref.Name,
		Type:   kind,
		Source: source,
		InstancePut: api.InstancePut{
			Profiles: []string{},
			Config: map[string]string{
				"limits.cpu":    strconv.FormatInt(req.CPUs, 10),
				"limits.memory": fmt.Sprintf("%dMiB", req.MemoryMB),
				metaImage:       req.Image.Name,
				metaDesktop:     strconv.FormatBool(req.Image.Desktop),
			},
			Devices: devices,
		},
	})
	if err != nil {
		return nil, mapError(err)
	}

	return &pendingInstance{
		client:  c,
		ref:     req.Ref,
		project: projectName(req.Ref.Sandbox),
		start:   req.Start,
		op:      op,
	}, nil
}

// ListInstances returns guests in a sandbox project.
func (c *Client) ListInstances(ctx context.Context, sandbox string) ([]compute.Instance, error) {
	if _, _, err := c.getProject(ctx, sandbox); err != nil {
		return nil, err
	}
	full, err := c.Scoped(ctx, projectName(sandbox), "").GetInstancesFull(api.InstanceTypeAny)
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return []compute.Instance{}, nil
		}
		return nil, mapError(err)
	}
	out := make([]compute.Instance, 0, len(full))
	for i := range full {
		mapped, err := c.mapInstance(ctx, sandbox, &full[i])
		if err != nil {
			return nil, err
		}
		out = append(out, mapped)
	}
	return out, nil
}

// GetInstance returns observed guest state from GetInstanceFull.
func (c *Client) GetInstance(ctx context.Context, ref compute.Ref) (compute.Instance, error) {
	if _, _, err := c.getProject(ctx, ref.Sandbox); err != nil {
		return compute.Instance{}, err
	}
	full, _, err := c.Scoped(ctx, projectName(ref.Sandbox), "").GetInstanceFull(ref.Name)
	if err != nil {
		return compute.Instance{}, mapError(err)
	}
	return c.mapInstance(ctx, ref.Sandbox, full)
}

// DeleteInstance force-stops and deletes one guest, including snapshots.
func (c *Client) DeleteInstance(ctx context.Context, ref compute.Ref) error {
	if _, _, err := c.getProject(ctx, ref.Sandbox); err != nil {
		return err
	}
	return c.forceDeleteInstance(ctx, ref.Sandbox, ref.Name)
}

// Exec runs a command and drains stdout/stderr. Cancellation sends SIGKILL
// over the exec control websocket and then waits a bounded teardown.
func (c *Client) Exec(ctx context.Context, req compute.ExecRequest, stdout, stderr io.Writer) (int64, error) {
	if req.Ref.Sandbox == "" || req.Ref.Name == "" {
		return -1, errors.New("instance reference is required")
	}
	if len(req.Argv) == 0 {
		return -1, errors.New("exec argv is required")
	}
	if _, _, err := c.getProject(ctx, req.Ref.Sandbox); err != nil {
		return -1, err
	}

	out := &execStream{writer: stdout}
	errOut := &execStream{writer: stderr}
	defer out.close()
	defer errOut.close()

	post := api.InstanceExecPost{
		Command:     req.Argv,
		WaitForWS:   true,
		Interactive: false,
		Environment: req.Env,
		Cwd:         req.Cwd,
	}
	if req.User != "" && req.User != "root" {
		uid, err := strconv.ParseUint(req.User, 10, 32)
		if err != nil {
			return -1, fmt.Errorf("exec user: %w", err)
		}
		post.User = uint32(uid)
	}

	finished := make(chan struct{})
	dataDone := make(chan bool)
	args := &incusclient.InstanceExecArgs{
		Stdin:    strings.NewReader(req.Stdin),
		Stdout:   out,
		Stderr:   errOut,
		DataDone: dataDone,
		Control: func(conn *websocket.Conn) {
			defer conn.Close()
			select {
			case <-ctx.Done():
			case <-finished:
			}
			if ctx.Err() != nil {
				_ = conn.SetWriteDeadline(time.Now().Add(execTeardownBound))
				_ = conn.WriteJSON(api.InstanceExecControl{Command: "signal", Signal: execSignalKill})
			}
		},
	}

	op, err := c.Scoped(ctx, projectName(req.Ref.Sandbox), "").ExecInstance(req.Ref.Name, post, args)
	if err != nil {
		close(finished)
		return -1, mapError(err)
	}

	waitErr := waitOp(ctx, op)
	drainErr := drainExec(ctx, dataDone)
	close(finished)
	if waitErr == nil {
		waitErr = drainErr
	}

	raw, hasStatus := op.Get().Metadata["return"].(float64)
	code := int64(raw)
	if waitErr != nil {
		return -1, waitErr
	}
	if !hasStatus {
		return -1, errors.New("exec returned no exit status")
	}
	return code, nil
}

func drainExec(ctx context.Context, dataDone <-chan bool) error {
	select {
	case <-dataDone:
		return nil
	case <-ctx.Done():
	}
	timer := time.NewTimer(execTeardownBound)
	defer timer.Stop()
	select {
	case <-dataDone:
	case <-timer.C:
	}
	return ctx.Err()
}

// execStream detaches the result writer before Exec returns, even if the
// daemon leaves an output websocket open after cancellation.
type execStream struct {
	mu     sync.Mutex
	writer io.Writer
}

func (s *execStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		return len(p), nil
	}
	return s.writer.Write(p)
}

func (s *execStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writer = nil
}

func (c *Client) forceDeleteInstance(ctx context.Context, sandbox, name string) error {
	srv := c.Scoped(ctx, projectName(sandbox), "")
	instance, _, err := srv.GetInstance(name)
	if err != nil {
		return mapError(err)
	}

	snapshots, snapErr := srv.GetInstanceSnapshots(name)
	if snapErr == nil {
		for _, snapshot := range snapshots {
			op, delErr := srv.DeleteInstanceSnapshot(name, snapshot.Name)
			if delErr != nil {
				if errors.Is(mapError(delErr), compute.ErrNotFound) {
					continue
				}
				return mapError(delErr)
			}
			if waitErr := waitOp(ctx, op); waitErr != nil && !errors.Is(waitErr, compute.ErrNotFound) {
				return waitErr
			}
		}
	}

	if instance.StatusCode != api.Stopped {
		op, stopErr := srv.UpdateInstanceState(name, api.InstanceStatePut{
			Action:  "stop",
			Timeout: -1,
			Force:   true,
		}, "")
		if stopErr == nil {
			_ = waitOp(ctx, op)
		}
	}

	op, err := srv.DeleteInstance(name)
	if err != nil {
		return mapError(err)
	}
	return waitOp(ctx, op)
}

func (c *Client) startInstance(ctx context.Context, project, name string) error {
	srv := c.Scoped(ctx, project, "")
	state, _, err := srv.GetInstanceState(name)
	if err != nil {
		return mapError(err)
	}
	if state.StatusCode == api.Running || state.StatusCode == api.Ready {
		return nil
	}
	op, err := srv.UpdateInstanceState(name, api.InstanceStatePut{
		Action:  "start",
		Timeout: -1,
	}, "")
	if err != nil {
		if state, _, stateErr := srv.GetInstanceState(name); stateErr == nil &&
			(state.StatusCode == api.Running || state.StatusCode == api.Ready) {
			return nil
		}
		return mapError(err)
	}
	return waitOp(ctx, op)
}

func (c *Client) waitRunning(ctx context.Context, project, name string) error {
	srv := c.Scoped(ctx, project, "")
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, _, err := srv.GetInstanceState(name)
		if err != nil {
			return mapError(err)
		}
		if state.StatusCode == api.Running || state.StatusCode == api.Ready {
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

func (c *Client) instanceSource(
	ctx context.Context,
	project string,
	image compute.CatalogImage,
) (api.InstanceSource, error) {
	if isUpstreamRef(image.Reference) {
		remote, alias, _ := splitRemoteAlias(image.Reference)
		server, err := c.RemoteImage(ctx, remote)
		if err != nil {
			return api.InstanceSource{}, err
		}
		info, err := server.GetConnectionInfo()
		if err != nil {
			return api.InstanceSource{}, mapError(err)
		}
		source := api.InstanceSource{
			Type:        sourceTypeImage,
			Alias:       alias,
			Server:      info.URL,
			Protocol:    info.Protocol,
			Certificate: info.Certificate,
		}
		if image.Fingerprint != "" {
			source.Fingerprint = image.Fingerprint
			source.Alias = ""
		}
		return source, nil
	}

	fingerprint, err := c.copyImage(ctx, project, image)
	if err != nil {
		return api.InstanceSource{}, err
	}
	return api.InstanceSource{
		Type:        sourceTypeImage,
		Fingerprint: fingerprint,
	}, nil
}

func (c *Client) copyImage(ctx context.Context, project string, image compute.CatalogImage) (string, error) {
	fingerprint := image.Fingerprint
	if fingerprint == "" {
		alias, _, err := c.Scoped(ctx, imageBuildProject, "").GetImageAlias(image.Name)
		if err != nil {
			return "", mapError(err)
		}
		fingerprint = alias.Target
	}

	dest := c.Scoped(ctx, project, "")
	if _, _, err := dest.GetImage(fingerprint); err == nil {
		return fingerprint, nil
	} else if !errors.Is(mapError(err), compute.ErrNotFound) {
		return "", mapError(err)
	}

	source := c.Scoped(ctx, imageBuildProject, "")
	info, err := source.GetConnectionInfo()
	if err != nil {
		return "", mapError(err)
	}
	secret, err := source.GetImageSecret(fingerprint)
	if err != nil {
		return "", mapError(err)
	}
	op, err := dest.CreateImage(api.ImagesPost{
		ImagePut: api.ImagePut{
			Profiles: []string{},
		},
		Source: &api.ImagesPostSource{
			ImageSource: api.ImageSource{Server: info.URL, Protocol: info.Protocol, Certificate: info.Certificate},
			Mode:        "pull",
			Secret:      secret,
			Type:        sourceTypeImage,
			Fingerprint: fingerprint,
			Project:     imageBuildProject,
		},
	}, nil)
	if err != nil {
		if isConflict(err) {
			return fingerprint, nil
		}
		return "", mapError(err)
	}
	if err := waitOp(ctx, op); err != nil {
		return "", err
	}
	return fingerprint, nil
}

func (c *Client) mapInstance(ctx context.Context, sandbox string, full *api.InstanceFull) (compute.Instance, error) {
	kind := kindContainer
	if full.Type == string(api.InstanceTypeVM) {
		kind = kindVM
	}
	nics, err := c.mapNICs(ctx, sandbox, full)
	if err != nil {
		return compute.Instance{}, err
	}
	snapshots := make([]string, 0, len(full.Snapshots))
	for _, snapshot := range full.Snapshots {
		snapshots = append(snapshots, snapshot.Name)
	}
	config := full.ExpandedConfig
	if len(config) == 0 {
		config = full.Config
	}
	devices := full.ExpandedDevices
	if len(devices) == 0 {
		devices = full.Devices
	}
	return compute.Instance{
		Ref:       compute.Ref{Sandbox: sandbox, Name: full.Name},
		Image:     config[metaImage],
		Kind:      kind,
		Host:      full.Location,
		Status:    full.Status,
		CPUs:      parseInt64(config["limits.cpu"]),
		MemoryMB:  bytesToMB(config["limits.memory"]),
		DiskGB:    diskGB(devices[rootDeviceName]),
		Desktop:   isTrue(config[metaDesktop]),
		NICs:      nics,
		Snapshots: snapshots,
	}, nil
}

func (c *Client) mapNICs(ctx context.Context, sandbox string, full *api.InstanceFull) ([]compute.NIC, error) {
	devices := full.Devices
	if len(devices) == 0 {
		devices = full.ExpandedDevices
	}
	logicalByPhysical := map[string]string{}
	networks, err := c.ListNetworks(ctx, sandbox)
	if err != nil && !errors.Is(err, compute.ErrNotFound) {
		return nil, err
	}
	for _, network := range networks {
		logicalByPhysical[network.PhysicalName] = network.Name
	}

	nics := make([]compute.NIC, 0)
	for name, device := range devices {
		if device[deviceTypeKey] != deviceTypeNIC {
			continue
		}
		iface := device["name"]
		if iface == "" {
			iface = name
		}
		physical := device["network"]
		logical := logicalByPhysical[physical]
		mac, addresses := observedNIC(full.State, iface)
		if configuredMAC := device["hwaddr"]; configuredMAC != "" {
			mac = configuredMAC
		}
		nics = append(nics, compute.NIC{
			Name:      iface,
			Network:   logical,
			MAC:       mac,
			Addresses: addresses,
		})
	}
	return nics, nil
}

func observedNIC(state *api.InstanceState, iface string) (string, []string) {
	addresses := []string{}
	if state == nil {
		return "", addresses
	}
	network := state.Network[iface]
	for _, address := range network.Addresses {
		if address.Scope != "link" && address.Scope != "local" && address.Address != "" {
			addresses = append(addresses, address.Address)
		}
	}
	return network.Hwaddr, addresses
}

func parseInt64(value string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func bytesToMB(value string) int64 {
	if value == "" {
		return 0
	}
	n, err := units.ParseByteSizeString(value)
	if err != nil {
		n, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0
		}
	}
	return n / bytesPerMiB
}

func diskGB(device map[string]string) int64 {
	if device == nil {
		return 0
	}
	n, err := units.ParseByteSizeString(device["size"])
	if err != nil {
		return 0
	}
	return n / bytesPerGiB
}
