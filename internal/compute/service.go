package compute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTTL            = 240 * time.Minute
	maxTTL                = 1440 * time.Minute
	kindBridge            = "bridge"
	kindOVN               = "ovn"
	kindContainer         = "container"
	kindVM                = "vm"
	statusRunning         = "Running"
	numericUIDBase        = 10
	numericUIDBitSize     = 32
	instanceCreateTimeout = 5 * time.Minute
)

// Options configures a compute Service.
type Options struct {
	// Host is the member used to pin bridge-backed sandboxes.
	Host string
	// DefaultTTL is used when create or extend omits a TTL. Zero selects 240 minutes.
	DefaultTTL time.Duration
	// MaxTTL is the upper bound for create and extend. Zero selects 1440 minutes.
	MaxTTL time.Duration
	// DefaultNetworkKind selects ovn or bridge for new sandboxes. Empty selects ovn.
	DefaultNetworkKind string
	// Logger receives operational logs. Nil selects a no-op logger.
	Logger *slog.Logger
	// OnSandboxExpired purges transient artifacts once a sandbox is expired.
	OnSandboxExpired func(string)
	// OnReap expires transient artifacts before each backend reaper scan.
	OnReap func()
	// DesktopReady probes the guest session after Incus agent readiness.
	DesktopReady func(context.Context, Ref) (bool, error)
}

// Service orchestrates sandboxes against a Backend and an immutable catalog.
type Service struct {
	backend            Backend
	catalog            *Catalog
	gate               *gate
	log                *slog.Logger
	host               string
	defaultTTL         time.Duration
	maxTTL             time.Duration
	defaultNetworkKind string
	onSandboxExpired   func(string)
	onReap             func()
	desktopReady       func(context.Context, Ref) (bool, error)
}

// New constructs a Service. Bridge defaults require Host; zero TTLs select the documented defaults.
func New(backend Backend, catalog *Catalog, opts Options) (*Service, error) {
	if backend == nil {
		return nil, errors.New("backend is required")
	}
	if catalog == nil {
		empty, err := NewCatalog(nil)
		if err != nil {
			return nil, err
		}
		catalog = empty
	}
	resolvedDefault := opts.DefaultTTL
	if resolvedDefault == 0 {
		resolvedDefault = defaultTTL
	}
	resolvedMax := opts.MaxTTL
	if resolvedMax == 0 {
		resolvedMax = maxTTL
	}
	if resolvedDefault > resolvedMax {
		return nil, errors.New("default TTL exceeds maximum TTL")
	}
	kind := opts.DefaultNetworkKind
	if kind == "" {
		kind = kindOVN
	}
	if kind != kindBridge && kind != kindOVN {
		return nil, fmt.Errorf("unsupported default network kind %q", kind)
	}
	if kind == kindBridge && opts.Host == "" {
		return nil, errors.New("host is required for bridge sandboxes")
	}
	return &Service{
		backend:            backend,
		catalog:            catalog,
		gate:               newGate(),
		log:                loggerOrDiscard(opts.Logger),
		host:               opts.Host,
		defaultTTL:         resolvedDefault,
		maxTTL:             resolvedMax,
		defaultNetworkKind: kind,
		onSandboxExpired:   opts.OnSandboxExpired,
		onReap:             opts.OnReap,
		desktopReady:       opts.DesktopReady,
	}, nil
}

// CatalogImage returns the named catalog entry.
func (s *Service) CatalogImage(name string) (CatalogImage, error) {
	image, ok := s.catalog.Lookup(name)
	if !ok {
		return CatalogImage{}, imageNotFound(name)
	}
	return image, nil
}

// ListImages returns catalog entries matching the optional filters.
func (s *Service) ListImages(osName string, desktop *bool, platform string) []CatalogImage {
	out := make([]CatalogImage, 0)
	for _, image := range s.catalog.images {
		if osName != "" && image.OS != osName {
			continue
		}
		if platform != "" && image.Platform != platform {
			continue
		}
		if desktop != nil && image.Desktop != *desktop {
			continue
		}
		out = append(out, cloneCatalogImage(image))
	}
	return out
}

// CreateSandbox creates a sandbox project, generating an adjective-noun name when name is empty.
func (s *Service) CreateSandbox(ctx context.Context, name string, ttl time.Duration, subject string) (Sandbox, error) {
	ttl, err := s.resolveTTL(ttl)
	if err != nil {
		return Sandbox{}, err
	}
	if name == "" {
		return s.createGeneratedSandbox(ctx, ttl, subject)
	}
	if err := validateName(name); err != nil {
		return Sandbox{}, err
	}
	return s.createNamedSandbox(ctx, name, ttl, subject)
}

// ListSandboxes returns every owned sandbox discovered from the backend.
func (s *Service) ListSandboxes(ctx context.Context) ([]Sandbox, error) {
	boxes, err := s.backend.ListSandboxes(ctx)
	if err != nil {
		return nil, s.backendError(ctx, "list sandboxes", err)
	}
	return nonNil(boxes), nil
}

// GetSandbox returns the sandbox together with its instances and networks.
func (s *Service) GetSandbox(ctx context.Context, name string) (Sandbox, []Instance, []Network, error) {
	if err := validateName(name); err != nil {
		return Sandbox{}, nil, nil, err
	}
	box, err := s.backend.GetSandbox(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Sandbox{}, nil, nil, sandboxNotFound(name)
		}
		return Sandbox{}, nil, nil, s.backendError(ctx, "get sandbox", err)
	}
	instances, err := s.backend.ListInstances(ctx, name)
	if err != nil {
		return Sandbox{}, nil, nil, s.backendError(ctx, "list instances", err)
	}
	networks, err := s.backend.ListNetworks(ctx, name)
	if err != nil {
		return Sandbox{}, nil, nil, s.backendError(ctx, "list networks", err)
	}
	return box, nonNil(instances), nonNil(networks), nil
}

// ExtendSandbox writes expires_at = now + ttl after re-reading expiry under the gate.
func (s *Service) ExtendSandbox(ctx context.Context, name string, ttl time.Duration) (Sandbox, error) {
	ttl, err := s.resolveTTL(ttl)
	if err != nil {
		return Sandbox{}, err
	}
	var extended Sandbox
	err = s.withLiveSandbox(ctx, name, func(Sandbox) error {
		expires := time.Now().Add(ttl)
		var extErr error
		extended, extErr = s.backend.ExtendSandbox(ctx, name, expires)
		if extErr != nil {
			if errors.Is(extErr, ErrNotFound) {
				return sandboxNotFound(name)
			}
			return s.backendError(ctx, "extend sandbox", extErr)
		}
		return nil
	})
	if err != nil {
		return Sandbox{}, err
	}
	return extended, nil
}

// DeleteSandbox marks expiry as now, then deletes; a partial failure is retried by the reaper.
func (s *Service) DeleteSandbox(ctx context.Context, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	unlock, err := s.gate.Lock(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()

	if _, err := s.backend.GetSandbox(ctx, name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return sandboxNotFound(name)
		}
		return s.backendError(ctx, "get sandbox", err)
	}
	if _, err := s.backend.ExtendSandbox(ctx, name, time.Now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			return sandboxNotFound(name)
		}
		return s.backendError(ctx, "expire sandbox", err)
	}
	if s.onSandboxExpired != nil {
		s.onSandboxExpired(name)
	}
	if err := s.backend.DeleteSandbox(ctx, name); err != nil {
		return s.backendError(ctx, "delete sandbox", err)
	}
	return nil
}

// CreateInstance accepts the guest under the sandbox gate, then waits without holding it.
func (s *Service) CreateInstance(ctx context.Context, req CreateInstance) (Instance, error) {
	createCtx, cancel := context.WithTimeout(ctx, instanceCreateTimeout)
	defer cancel()
	pending, err := s.beginInstance(createCtx, req)
	if err != nil {
		return Instance{}, err
	}
	inst, err := pending.Wait(createCtx)
	if err != nil {
		if errors.Is(createCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return Instance{}, agentErrorf(
				"instance %q creation timed out in sandbox %q; inspect it before retrying",
				req.Ref.Name,
				req.Ref.Sandbox,
			)
		}
		if errors.Is(err, ErrNotFound) {
			return Instance{}, instanceNotFound(req.Ref)
		}
		return Instance{}, s.backendError(ctx, "wait instance", err)
	}
	if inst.Desktop && req.Start {
		if _, err := s.WaitInstance(createCtx, WaitRequest{Ref: req.Ref, Until: WaitUntilDesktop}); err != nil {
			return Instance{}, err
		}
		return s.GetInstance(createCtx, req.Ref)
	}
	return inst, nil
}

// ListInstances returns guests in the named sandbox.
func (s *Service) ListInstances(ctx context.Context, sandbox string) ([]Instance, error) {
	if err := validateName(sandbox); err != nil {
		return nil, err
	}
	if _, err := s.backend.GetSandbox(ctx, sandbox); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, sandboxNotFound(sandbox)
		}
		return nil, s.backendError(ctx, "get sandbox", err)
	}
	instances, err := s.backend.ListInstances(ctx, sandbox)
	if err != nil {
		return nil, s.backendError(ctx, "list instances", err)
	}
	return nonNil(instances), nil
}

// GetInstance returns one guest, mapping misses to an agent-facing not-found error.
func (s *Service) GetInstance(ctx context.Context, ref Ref) (Instance, error) {
	if err := validateRef(ref); err != nil {
		return Instance{}, err
	}
	inst, err := s.backend.GetInstance(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Instance{}, instanceNotFound(ref)
		}
		return Instance{}, s.backendError(ctx, "get instance", err)
	}
	return inst, nil
}

// DeleteInstance removes a guest under the sandbox gate.
func (s *Service) DeleteInstance(ctx context.Context, ref Ref) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	unlock, err := s.gate.Lock(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	defer unlock()

	if err := s.backend.DeleteInstance(ctx, ref); err != nil {
		if errors.Is(err, ErrNotFound) {
			return instanceNotFound(ref)
		}
		return s.backendError(ctx, "delete instance", err)
	}
	return nil
}

// Exec runs a bounded command without holding the mutation gate.
func (s *Service) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	return s.exec(ctx, req, execOutputLimit)
}

func (s *Service) exec(ctx context.Context, req ExecRequest, outputLimit int) (ExecResult, error) {
	if err := validateRef(req.Ref); err != nil {
		return ExecResult{}, err
	}
	if err := validateExec(req); err != nil {
		return ExecResult{}, err
	}
	inst, err := s.GetInstance(ctx, req.Ref)
	if err != nil {
		return ExecResult{}, err
	}
	if inst.Status != statusRunning && inst.Status != "Ready" {
		return ExecResult{}, agentErrorf("instance %q in sandbox %q is not running", req.Ref.Name, req.Ref.Sandbox)
	}
	if strings.HasPrefix(strings.ToLower(inst.OS), "windows") && req.User != "" {
		return ExecResult{}, agentError("Windows exec uses the Incus agent service identity; user is unsupported")
	}

	stdout := newDrainingWriter(outputLimit)
	stderr := newDrainingWriter(outputLimit)
	execCtx, cancel := execContext(ctx, req.Timeout)
	defer cancel()

	code, err := s.backend.Exec(execCtx, req, stdout, stderr)
	result := ExecResult{
		ExitCode:        code,
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
	}
	if ctx.Err() != nil {
		result.ExitCode = -1
		return result, ctx.Err()
	}
	if err == nil {
		return result, nil
	}
	if errors.Is(err, context.Canceled) {
		result.ExitCode = -1
		return result, err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		result.ExitCode = -1
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.TimedOut = true
		return result, nil
	}
	return result, s.backendError(ctx, "exec", err)
}

// CreateNetwork creates an additional agent-facing network in a live sandbox.
func (s *Service) CreateNetwork(ctx context.Context, sandbox string, network Network) (Network, error) {
	network, err := prepareNetwork(network)
	if err != nil {
		return Network{}, err
	}
	var created Network
	err = s.withLiveSandbox(ctx, sandbox, func(box Sandbox) error {
		var createErr error
		created, createErr = s.createPreparedNetwork(ctx, box, sandbox, network)
		return createErr
	})
	if err != nil {
		return Network{}, err
	}
	return created, nil
}

// AttachNIC attaches an instance to a metadata-resolved network.
func (s *Service) AttachNIC(ctx context.Context, ref Ref, network, nic, ip, mac string) (NIC, error) {
	if err := validateAttachment(ref, network, nic); err != nil {
		return NIC{}, err
	}
	var attached NIC
	err := s.withLiveSandbox(ctx, ref.Sandbox, func(Sandbox) error {
		instance, getErr := s.backend.GetInstance(ctx, ref)
		if getErr != nil {
			if errors.Is(getErr, ErrNotFound) {
				return instanceNotFound(ref)
			}
			return s.backendError(ctx, "get instance", getErr)
		}
		for _, existing := range instance.NICs {
			if nic != "" && existing.Name == nic {
				return agentErrorf("nic %q already exists on instance %q", nic, ref.Name)
			}
		}
		var attachErr error
		attached, attachErr = s.backend.AttachNIC(ctx, ref, network, nic, ip, mac)
		if attachErr != nil {
			if errors.Is(attachErr, ErrNotFound) {
				return networkNotFound(network, ref.Sandbox)
			}
			return s.backendError(ctx, "attach nic", attachErr)
		}
		return nil
	})
	if err != nil {
		return NIC{}, err
	}
	return attached, nil
}

func validateAttachment(ref Ref, network, nic string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	if network != reservedDefault {
		if err := validateName(network); err != nil {
			return err
		}
	}
	if nic != "" {
		return validateName(nic)
	}
	return nil
}

func prepareNetwork(network Network) (Network, error) {
	if err := validateName(network.Name); err != nil {
		return Network{}, err
	}
	if err := validateNetworkKind(network.Kind); err != nil {
		return Network{}, err
	}
	if network.CIDR != "" {
		if err := validateCIDR(network.CIDR); err != nil {
			return Network{}, err
		}
	}
	if network.Kind == "" {
		network.Kind = kindOVN
	}
	if network.Kind == kindOVN && !network.NAT && network.Gateway != "" {
		return Network{}, agentError(
			"nat=false networks have no uplink gateway; attach a router instance or use net.peer",
		)
	}
	return network, nil
}

func (s *Service) createPreparedNetwork(
	ctx context.Context,
	box Sandbox,
	sandbox string,
	network Network,
) (Network, error) {
	fabric := sandboxNetworkKind(box)
	if network.Kind != fabric {
		return Network{}, agentErrorf("cannot create a %q network in a %q sandbox", network.Kind, fabric)
	}
	if fabric == kindBridge {
		if network.Host == "" {
			network.Host = box.Host
		}
		if network.Host != box.Host {
			return Network{}, agentErrorf("host %q is not sandbox member %q", network.Host, box.Host)
		}
	} else {
		network.Host = ""
	}
	created, err := s.backend.CreateNetwork(ctx, sandbox, network)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Network{}, sandboxNotFound(sandbox)
		}
		return Network{}, s.backendError(ctx, "create network", err)
	}
	return created, nil
}

func (s *Service) createGeneratedSandbox(ctx context.Context, ttl time.Duration, subject string) (Sandbox, error) {
	for range nameGenerateTries {
		name, err := generateName()
		if err != nil {
			return Sandbox{}, err
		}
		box, err := s.createNamedSandbox(ctx, name, ttl, subject)
		if err == nil {
			return box, nil
		}
		if isAlreadyExists(err, name) {
			continue
		}
		return Sandbox{}, err
	}
	return Sandbox{}, agentError("unable to allocate a sandbox name")
}

func (s *Service) createNamedSandbox(
	ctx context.Context,
	name string,
	ttl time.Duration,
	subject string,
) (Sandbox, error) {
	unlock, err := s.gate.Lock(ctx, name)
	if err != nil {
		return Sandbox{}, err
	}
	defer unlock()

	_, err = s.backend.GetSandbox(ctx, name)
	if err == nil {
		return Sandbox{}, agentErrorf("sandbox %q already exists", name)
	} else if !errors.Is(err, ErrNotFound) {
		return Sandbox{}, s.backendError(ctx, "get sandbox", err)
	}

	now := time.Now()
	box := Sandbox{
		Name:        name,
		Platform:    platformIncus,
		Subject:     subject,
		NetworkKind: s.defaultNetworkKind,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
	}
	if s.defaultNetworkKind == kindBridge {
		box.Host = s.host
	}
	if err := s.backend.CreateSandbox(ctx, box); err != nil {
		return Sandbox{}, s.backendError(ctx, "create sandbox", err)
	}
	return box, nil
}

func (s *Service) beginInstance(ctx context.Context, req CreateInstance) (PendingInstance, error) {
	if err := validateRef(req.Ref); err != nil {
		return nil, err
	}
	if err := validateInstanceImage(req); err != nil {
		return nil, err
	}
	var pending PendingInstance
	err := s.withLiveSandbox(ctx, req.Ref.Sandbox, func(box Sandbox) error {
		prepared, err := s.prepareCreate(box, req)
		if err != nil {
			return err
		}
		if _, getErr := s.backend.GetInstance(ctx, prepared.Ref); getErr == nil {
			return agentErrorf("instance %q already exists in sandbox %q", prepared.Ref.Name, prepared.Ref.Sandbox)
		} else if !errors.Is(getErr, ErrNotFound) {
			return s.backendError(ctx, "get instance", getErr)
		}
		pending, err = s.backend.BeginCreateInstance(ctx, prepared)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return sandboxNotFound(prepared.Ref.Sandbox)
			}
			return s.backendError(ctx, "create instance", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pending, nil
}

func (s *Service) prepareCreate(box Sandbox, req CreateInstance) (CreateInstance, error) {
	if isBridgeSandbox(box) {
		if box.Host == "" {
			box.Host = s.host
		}
		if req.Host == "" {
			req.Host = box.Host
		}
		if req.Host != box.Host {
			return CreateInstance{}, agentErrorf("host %q is not sandbox member %q", req.Host, box.Host)
		}
	}
	if req.Kind == "" {
		req.Kind = req.Image.Kind
	}
	if err := validateInstanceKind(req.Kind, req.Image); err != nil {
		return CreateInstance{}, err
	}
	if req.Network == "" {
		req.Network = reservedDefault
	}
	if req.Network != reservedNone && req.Network != reservedDefault {
		if err := validateName(req.Network); err != nil {
			return CreateInstance{}, err
		}
	}
	return req, nil
}

func (s *Service) withLiveSandbox(ctx context.Context, name string, fn func(Sandbox) error) error {
	if err := validateName(name); err != nil {
		return err
	}
	unlock, err := s.gate.Lock(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()

	box, err := s.backend.GetSandbox(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return sandboxNotFound(name)
		}
		return s.backendError(ctx, "get sandbox", err)
	}
	if box.Host == "" {
		box.Host = s.host
	}
	if !box.ExpiresAt.After(time.Now()) {
		return sandboxExpired(name)
	}
	return fn(box)
}

func (s *Service) resolveTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		ttl = s.defaultTTL
	}
	if ttl < 0 {
		return 0, agentError("ttl must be non-negative")
	}
	if ttl > s.maxTTL {
		return 0, agentErrorf("ttl exceeds maximum of %d minutes", int64(s.maxTTL/time.Minute))
	}
	return ttl, nil
}

func (s *Service) backendError(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrUnavailable) {
		s.log.WarnContext(ctx, "backend unavailable", "op", op)
		return ErrUnavailable
	}
	s.log.ErrorContext(ctx, "backend operation failed", "op", op, "err", err)
	return fmt.Errorf("%s: %w", op, err)
}

func validateRef(ref Ref) error {
	if err := validateName(ref.Sandbox); err != nil {
		return err
	}
	return validateName(ref.Name)
}

func validateExec(req ExecRequest) error {
	if req.User != "" && req.User != "root" {
		if _, err := strconv.ParseUint(req.User, numericUIDBase, numericUIDBitSize); err != nil {
			return agentError("user must be a numeric UID")
		}
	}
	if req.Cwd != "" && !absoluteGuestPath(req.Cwd) {
		return agentError("cwd must be an absolute path")
	}
	return nil
}

func validateInstanceImage(req CreateInstance) error {
	platform := req.Image.Platform
	if platform == "" {
		platform = platformIncus
	}
	if platform != platformIncus {
		return agentErrorf("platform %q is not available yet", platform)
	}
	return nil
}

func validateInstanceKind(kind string, image CatalogImage) error {
	if kind == kindOVN {
		return agentErrorf("kind %q is not available yet", kind)
	}
	if kind != kindContainer && kind != kindVM {
		return agentErrorf("kind %q is not available yet", kind)
	}
	if len(image.Kinds) > 0 && !slices.Contains(image.Kinds, kind) {
		return agentErrorf("kind %q is not supported for image %q", kind, image.Name)
	}
	if len(image.Kinds) == 0 && image.Kind != "" && kind != image.Kind {
		return agentErrorf("kind %q is not supported for image %q", kind, image.Name)
	}
	return nil
}

func validateNetworkKind(kind string) error {
	if kind == "" || kind == kindBridge || kind == kindOVN {
		return nil
	}
	return agentErrorf("kind %q is not available yet", kind)
}

func execContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Add(timeout).Before(deadline) {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

func loggerOrDiscard(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.DiscardHandler)
}
