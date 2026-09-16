// Package mcpserver builds the transport-agnostic MCP server for this repository.
//
// The server defined here knows nothing about how it is connected to a client:
// the same *mcp.Server is driven by the stdio and http subcommands in
// internal/cli. Keeping transport concerns out of this package is the seam that
// lets a consumer keep one transport and delete the other without ever touching
// the server or its capabilities.
package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/codemode"
	hostmcp "github.com/meigma/codemode/mcpserver"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/templateinfo"
)

const (
	defaultMaxExecutionTime = 15 * time.Minute
	defaultMaxNativeCalls   = 1000

	capabilitySandboxCreate  = "sandbox.create"
	capabilitySandboxList    = "sandbox.list"
	capabilitySandboxGet     = "sandbox.get"
	capabilitySandboxExtend  = "sandbox.extend"
	capabilitySandboxPin     = "sandbox.pin"
	capabilitySandboxDelete  = "sandbox.delete"
	capabilityImageList      = "image.list"
	capabilityInstanceCreate = "instance.create"
	capabilityInstanceList   = "instance.list"
	capabilityInstanceGet    = "instance.get"
	capabilityInstanceDelete = "instance.delete"
	capabilityInstanceExec   = "instance.exec"
	capabilityNetCreate      = "net.create"
	capabilityNetAttach      = "net.attach"

	platformIncus  = "incus"
	platformMac    = "mac"
	kindBridge     = "bridge"
	kindOVN        = "ovn"
	networkDefault = "default"
)

// sandboxService is the sandbox lifecycle surface consumed by sandbox.* handlers.
type sandboxService interface {
	CreateSandbox(
		ctx context.Context,
		name string,
		ttl time.Duration,
		subject, platform string,
		pinned bool,
	) (compute.Sandbox, error)
	ListSandboxes(ctx context.Context) ([]compute.Sandbox, error)
	GetSandbox(ctx context.Context, name string) (compute.Sandbox, []compute.Instance, []compute.Network, error)
	ExtendSandbox(ctx context.Context, name string, ttl time.Duration) (compute.Sandbox, error)
	PinSandbox(ctx context.Context, name string, pinned bool, subject string) (compute.Sandbox, error)
	DeleteSandbox(ctx context.Context, name string) error
}

// instanceService is the guest surface consumed by instance.* handlers.
type instanceService interface {
	CreateInstance(ctx context.Context, req compute.CreateInstance) (compute.Instance, error)
	ListInstances(ctx context.Context, sandbox string) ([]compute.Instance, error)
	GetInstance(ctx context.Context, ref compute.Ref) (compute.Instance, error)
	DeleteInstance(ctx context.Context, ref compute.Ref) error
	Exec(ctx context.Context, req compute.ExecRequest) (compute.ExecResult, error)
	StartInstance(ctx context.Context, ref compute.Ref, force bool) (compute.Instance, error)
	StopInstance(ctx context.Context, ref compute.Ref, force bool) (compute.Instance, error)
	RestartInstance(ctx context.Context, ref compute.Ref, force bool) (compute.Instance, error)
	WaitInstance(ctx context.Context, req compute.WaitRequest) (compute.WaitResult, error)
	ReadFile(ctx context.Context, req compute.FileReadRequest) (compute.FileReadResult, error)
	WriteFile(ctx context.Context, req compute.FileWriteRequest) (compute.FileWriteResult, error)
	CreateSnapshot(ctx context.Context, ref compute.Ref, snapshot string) error
	RestoreSnapshot(ctx context.Context, ref compute.Ref, snapshot string) error
	DeleteSnapshot(ctx context.Context, ref compute.Ref, snapshot string) error
	ListSnapshots(ctx context.Context, ref compute.Ref) ([]compute.Snapshot, error)
	PublishInstance(ctx context.Context, ref compute.Ref, image string) (string, error)
	ResolveImage(ctx context.Context, sandbox, name string) (compute.CatalogImage, error)
}

// networkService is the network surface consumed by net.* handlers.
type networkService interface {
	CreateNetwork(ctx context.Context, sandbox string, network compute.Network) (compute.Network, error)
	AttachNIC(ctx context.Context, ref compute.Ref, network, nic, ip, mac string) (compute.NIC, error)
	ListNetworks(ctx context.Context, sandbox string) ([]compute.Network, error)
	GetNetwork(ctx context.Context, sandbox, name string) (compute.Network, error)
	DeleteNetwork(ctx context.Context, sandbox, name string) error
	DetachNIC(ctx context.Context, ref compute.Ref, nic string) error
	PeerNetworks(ctx context.Context, sandbox, network, peer string) error
	AddACLRule(ctx context.Context, sandbox, network string, rule compute.ACLRule) (compute.ACLRule, error)
	RemoveACLRule(ctx context.Context, sandbox, network, rule string) error
	CreateForward(
		ctx context.Context,
		sandbox, network string,
		ref compute.Ref,
		port, listenPort int64,
		protocol string,
	) (compute.Forward, error)
	ImpairNIC(ctx context.Context, ref compute.Ref, nic string, impairment compute.Impairment) error
}

// imageService is the catalog surface consumed by image.list and instance.create.
type imageService interface {
	CatalogImage(name string) (compute.CatalogImage, error)
	ListImages(os string, desktop *bool, platform string) []compute.CatalogImage
}

// Dependencies holds the consumer services CodeMode handlers close over.
type Dependencies struct {
	// Sandbox is the sandbox lifecycle service consumed by sandbox.* handlers.
	Sandbox sandboxService

	// Instance is the guest service consumed by instance.* handlers.
	Instance instanceService

	// Network is the network service consumed by net.* handlers.
	Network networkService

	// Image is the catalog service consumed by image.list and instance.create.
	Image imageService

	// Desktop proxies the guest Driver and publishes screenshot URLs.
	Desktop desktopService
}

// Options configures the agentcompute MCP server.
type Options struct {
	// Version is the release version reported in the server implementation info.
	Version string

	// Deps carries the shared services the server's capabilities need.
	Deps Dependencies

	// Logger receives server diagnostics. Nil selects a text handler writing
	// to [os.Stderr].
	//
	// WARNING: a logger must never write to [os.Stdout]. The stdio transport
	// reserves stdout for the JSON-RPC message stream, so a single log line
	// there corrupts the protocol. Writing to stderr (the default) is safe for
	// every transport.
	Logger *slog.Logger

	// Resolver resolves the trusted invocation subject from host-owned context.
	// There is no default. The stdio command supplies a process-owned
	// [hostmcp.StaticSubject]; the http command supplies [hostmcp.ContextSubject]
	// and installs identity on each request.
	Resolver hostmcp.InvocationResolver

	// Runtime configures the CodeMode catalog, authorizer, and execution
	// budgets. There is no default authorizer: the CLI supplies
	// [github.com/meigma/codemode/authz.AllowAll]. Zero MaxExecutionTime and
	// MaxNativeCalls receive slice-1 defaults (15m / 1000) before Build; other
	// zero-valued limit fields receive CodeMode defaults at Build.
	Runtime codemode.Options
}

// NewDependencies adapts a compute service to the handler consumer interfaces.
func NewDependencies(svc *compute.Service, driver desktopService) Dependencies {
	return Dependencies{
		Sandbox:  svc,
		Instance: svc,
		Network:  svc,
		Image:    svc,
		Desktop:  driver,
	}
}

// New constructs the agentcompute MCP server and registers its capabilities.
//
// New is transport-agnostic; callers choose a transport when they run the
// returned server (see internal/cli). The official MCP surface is exactly
// search_api, describe_api, and execute. Diagnostics go to [Options.Logger].
func New(options Options) (*mcp.Server, error) {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	runtime := options.Runtime
	runtime.Limits = applySliceLimits(runtime.Limits)

	builder := codemode.New(runtime)
	registerSandbox(builder, options.Deps)
	registerImage(builder, options.Deps)
	registerInstance(builder, options.Deps)
	registerNet(builder, options.Deps)
	registerDesktop(builder, options.Deps)
	service, err := builder.Build()
	if err != nil {
		return nil, fmt.Errorf("build CodeMode runtime: %w", err)
	}

	server, err := hostmcp.New(service, options.Resolver, hostmcp.Options{
		Implementation: &mcp.Implementation{
			Name:    templateinfo.Name,
			Title:   templateinfo.Title,
			Version: options.Version,
		},
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("construct MCP server: %w", err)
	}
	return server, nil
}

func applySliceLimits(limits codemode.Limits) codemode.Limits {
	if limits.MaxExecutionTime == 0 {
		limits.MaxExecutionTime = defaultMaxExecutionTime
	}
	if limits.MaxNativeCalls == 0 {
		limits.MaxNativeCalls = defaultMaxNativeCalls
	}
	return limits
}
