package compute

import (
	"context"
	"errors"
	"io"
	"time"
)

// router dispatches Backend calls to Incus or Mac by durable sandbox lookup.
// List and create uniqueness span both backends; names are not cached.
type router struct {
	incus Backend
	mac   Backend
}

func newRouter(incus, mac Backend) Backend {
	return &router{incus: incus, mac: mac}
}

func (r *router) lookup(ctx context.Context, name string) (Backend, Sandbox, error) {
	box, err := r.incus.GetSandbox(ctx, name)
	if err == nil {
		return r.incus, box, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, Sandbox{}, err
	}
	box, err = r.mac.GetSandbox(ctx, name)
	if err != nil {
		return nil, Sandbox{}, err
	}
	return r.mac, box, nil
}

func (r *router) backendFor(ctx context.Context, name string) (Backend, error) {
	backend, _, err := r.lookup(ctx, name)
	return backend, err
}

func (r *router) createBackend(platform string) Backend {
	if platform == platformMac {
		return r.mac
	}
	return r.incus
}

func (r *router) CreateSandbox(ctx context.Context, sandbox Sandbox) error {
	return r.createBackend(sandbox.Platform).CreateSandbox(ctx, sandbox)
}

func (r *router) ListSandboxes(ctx context.Context) ([]Sandbox, error) {
	incusBoxes, err := r.incus.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	macBoxes, err := r.mac.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Sandbox, 0, len(incusBoxes)+len(macBoxes))
	out = append(out, incusBoxes...)
	out = append(out, macBoxes...)
	return out, nil
}

func (r *router) GetSandbox(ctx context.Context, name string) (Sandbox, error) {
	_, box, err := r.lookup(ctx, name)
	return box, err
}

func (r *router) ExtendSandbox(ctx context.Context, name string, expires time.Time) (Sandbox, error) {
	backend, err := r.backendFor(ctx, name)
	if err != nil {
		return Sandbox{}, err
	}
	return backend.ExtendSandbox(ctx, name, expires)
}

func (r *router) PinSandbox(
	ctx context.Context,
	name string,
	pinned bool,
	subject string,
	since time.Time,
) (Sandbox, error) {
	backend, err := r.backendFor(ctx, name)
	if err != nil {
		return Sandbox{}, err
	}
	return backend.PinSandbox(ctx, name, pinned, subject, since)
}

func (r *router) DeleteSandbox(ctx context.Context, name string) error {
	backend, err := r.backendFor(ctx, name)
	if err != nil {
		return err
	}
	return backend.DeleteSandbox(ctx, name)
}

func (r *router) BeginCreateInstance(ctx context.Context, req CreateInstance) (PendingInstance, error) {
	backend, err := r.backendFor(ctx, req.Ref.Sandbox)
	if err != nil {
		return nil, err
	}
	return backend.BeginCreateInstance(ctx, req)
}

func (r *router) ListInstances(ctx context.Context, sandbox string) ([]Instance, error) {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	return backend.ListInstances(ctx, sandbox)
}

func (r *router) GetInstance(ctx context.Context, ref Ref) (Instance, error) {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return Instance{}, err
	}
	return backend.GetInstance(ctx, ref)
}

func (r *router) DeleteInstance(ctx context.Context, ref Ref) error {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	return backend.DeleteInstance(ctx, ref)
}

func (r *router) Exec(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (int64, error) {
	backend, err := r.backendFor(ctx, req.Ref.Sandbox)
	if err != nil {
		return 0, err
	}
	return backend.Exec(ctx, req, stdout, stderr)
}

func (r *router) OpenExec(ctx context.Context, req ExecRequest) (io.ReadWriteCloser, error) {
	backend, err := r.backendFor(ctx, req.Ref.Sandbox)
	if err != nil {
		return nil, err
	}
	return backend.OpenExec(ctx, req)
}

func (r *router) ListNetworks(ctx context.Context, sandbox string) ([]Network, error) {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	return backend.ListNetworks(ctx, sandbox)
}

func (r *router) CreateNetwork(ctx context.Context, sandbox string, network Network) (Network, error) {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return Network{}, err
	}
	return backend.CreateNetwork(ctx, sandbox, network)
}

func (r *router) AttachNIC(ctx context.Context, ref Ref, network, nic, ip, mac string) (NIC, error) {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return NIC{}, err
	}
	return backend.AttachNIC(ctx, ref, network, nic, ip, mac)
}

func (r *router) GetNetwork(ctx context.Context, sandbox, name string) (Network, error) {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return Network{}, err
	}
	return backend.GetNetwork(ctx, sandbox, name)
}

func (r *router) DeleteNetwork(ctx context.Context, sandbox, name string) error {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return err
	}
	return backend.DeleteNetwork(ctx, sandbox, name)
}

func (r *router) DetachNIC(ctx context.Context, ref Ref, nic string) error {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	return backend.DetachNIC(ctx, ref, nic)
}

func (r *router) PeerNetworks(ctx context.Context, sandbox, network, peer string) error {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return err
	}
	return backend.PeerNetworks(ctx, sandbox, network, peer)
}

func (r *router) AddACLRule(ctx context.Context, sandbox, network string, rule ACLRule) (ACLRule, error) {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return ACLRule{}, err
	}
	return backend.AddACLRule(ctx, sandbox, network, rule)
}

func (r *router) RemoveACLRule(ctx context.Context, sandbox, network, rule string) error {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return err
	}
	return backend.RemoveACLRule(ctx, sandbox, network, rule)
}

func (r *router) CreateForward(
	ctx context.Context,
	sandbox, network string,
	ref Ref,
	port, listenPort int64,
	protocol string,
) (Forward, error) {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return Forward{}, err
	}
	return backend.CreateForward(ctx, sandbox, network, ref, port, listenPort, protocol)
}

func (r *router) InstanceForward(ctx context.Context, ref Ref, targetPort int64, protocol string) (Forward, error) {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return Forward{}, err
	}
	return backend.InstanceForward(ctx, ref, targetPort, protocol)
}

func (r *router) StartInstance(ctx context.Context, ref Ref, force bool) (Instance, error) {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return Instance{}, err
	}
	return backend.StartInstance(ctx, ref, force)
}

func (r *router) StopInstance(ctx context.Context, ref Ref, force bool) (Instance, error) {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return Instance{}, err
	}
	return backend.StopInstance(ctx, ref, force)
}

func (r *router) RestartInstance(ctx context.Context, ref Ref, force bool) (Instance, error) {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return Instance{}, err
	}
	return backend.RestartInstance(ctx, ref, force)
}

func (r *router) WaitInstance(ctx context.Context, req WaitRequest) (WaitResult, error) {
	backend, err := r.backendFor(ctx, req.Ref.Sandbox)
	if err != nil {
		return WaitResult{}, err
	}
	return backend.WaitInstance(ctx, req)
}

func (r *router) ReadFile(ctx context.Context, req FileReadRequest) (FileReadResult, error) {
	backend, err := r.backendFor(ctx, req.Ref.Sandbox)
	if err != nil {
		return FileReadResult{}, err
	}
	return backend.ReadFile(ctx, req)
}

func (r *router) ReadBinaryFile(ctx context.Context, ref Ref, path string) (io.ReadCloser, error) {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return nil, err
	}
	return backend.ReadBinaryFile(ctx, ref, path)
}

func (r *router) DeleteFile(ctx context.Context, ref Ref, path string) error {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	return backend.DeleteFile(ctx, ref, path)
}

func (r *router) WriteFile(ctx context.Context, req FileWriteRequest) (FileWriteResult, error) {
	backend, err := r.backendFor(ctx, req.Ref.Sandbox)
	if err != nil {
		return FileWriteResult{}, err
	}
	return backend.WriteFile(ctx, req)
}

func (r *router) CreateSnapshot(ctx context.Context, ref Ref, snapshot string) error {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	return backend.CreateSnapshot(ctx, ref, snapshot)
}

func (r *router) RestoreSnapshot(ctx context.Context, ref Ref, snapshot string) error {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	return backend.RestoreSnapshot(ctx, ref, snapshot)
}

func (r *router) DeleteSnapshot(ctx context.Context, ref Ref, snapshot string) error {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	return backend.DeleteSnapshot(ctx, ref, snapshot)
}

func (r *router) ListSnapshots(ctx context.Context, ref Ref) ([]Snapshot, error) {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return nil, err
	}
	return backend.ListSnapshots(ctx, ref)
}

func (r *router) PublishInstance(ctx context.Context, ref Ref, image string) (string, error) {
	backend, err := r.backendFor(ctx, ref.Sandbox)
	if err != nil {
		return "", err
	}
	return backend.PublishInstance(ctx, ref, image)
}

func (r *router) GetSandboxImage(ctx context.Context, sandbox, name string) (CatalogImage, error) {
	backend, err := r.backendFor(ctx, sandbox)
	if err != nil {
		return CatalogImage{}, err
	}
	return backend.GetSandboxImage(ctx, sandbox, name)
}
