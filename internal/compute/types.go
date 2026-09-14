// Package compute orchestrates disposable Incus sandboxes.
package compute

import (
	"context"
	"io"
	"time"
)

// Ref identifies an instance within a sandbox.
type Ref struct {
	// Sandbox is the agent-facing sandbox name.
	Sandbox string
	// Name is the instance name within the project.
	Name string
}

// Sandbox is persistent project metadata discovered from Incus.
type Sandbox struct {
	// Name is the agent-facing lifecycle unit.
	Name string
	// Platform identifies the compute backend.
	Platform string
	// Subject records the creator without enforcing ownership policy.
	Subject string
	// Host is the member shared by this sandbox's bridge-backed instances.
	Host string
	// NetworkKind records the default fabric; empty denotes a legacy bridge sandbox.
	NetworkKind string
	// CreatedAt is the original creation time.
	CreatedAt time.Time
	// ExpiresAt is the persisted reaper deadline.
	ExpiresAt time.Time
}

// Instance describes observed guest state.
type Instance struct {
	// Ref identifies the instance.
	Ref Ref
	// Image is the catalog name used to create the guest.
	Image string
	// Kind is container or vm.
	Kind string
	// Host is the actual member.
	Host string
	// Status is the observed Incus state.
	Status string
	// CPUs is the configured CPU count.
	CPUs int64
	// MemoryMB is the configured memory in MiB.
	MemoryMB int64
	// DiskGB is the root disk limit in GiB.
	DiskGB int64
	// Desktop marks desktop-capable images.
	Desktop bool
	// NICs contains observed network devices.
	NICs []NIC
	// Snapshots contains guest snapshot names.
	Snapshots []string
}

// NIC describes an attached interface using agent-facing network names.
type NIC struct {
	// Name is the guest device name.
	Name string
	// Network is the metadata-resolved agent-facing network name.
	Network string
	// MAC is the observed hardware address.
	MAC string
	// Addresses contains observed guest IP addresses.
	Addresses []string
}

// Network holds logical identity separately from the physical bridge name.
type Network struct {
	// Name is the agent-facing network name.
	Name string
	// PhysicalName is the opaque Incus bridge identifier, never exposed in DTOs.
	PhysicalName string
	// Project is the Incus project containing the managed network.
	Project string
	// Kind is bridge or ovn.
	Kind string
	// CIDR is the configured network prefix.
	CIDR string
	// Gateway is the bridge gateway address, if any.
	Gateway string
	// Host identifies the sandbox's member-local L2 domain.
	Host string
	// DHCP enables address assignment.
	DHCP bool
	// NAT enables outbound translation.
	NAT bool
	// DNS enables DNS service.
	DNS bool
}

// CatalogImage is an immutable curated image entry.
type CatalogImage struct {
	// Name is the catalog lookup key.
	Name string
	// OS is the operating system family.
	OS string
	// Version is the guest operating system version.
	Version string
	// Platform identifies the backend.
	Platform string
	// Kind is the default guest kind.
	Kind string
	// Kinds lists supported guest kinds.
	Kinds []string
	// Desktop marks images with desktop tooling.
	Desktop bool
	// Description is optional catalog guidance.
	Description string
	// Reference is the immutable imgoci release or upstream remote alias.
	Reference string
	// Fingerprint is derived during reconciliation, not an image build identity.
	Fingerprint string
	// CPUs is the default CPU count.
	CPUs int64
	// MemoryMB is the default memory limit in MiB.
	MemoryMB int64
	// DiskGB is the default root disk limit in GiB.
	DiskGB int64
}

// CreateInstance specifies a guest after catalog defaults have been resolved.
type CreateInstance struct {
	// Ref identifies the new guest.
	Ref Ref
	// Image supplies the reconciled catalog source.
	Image CatalogImage
	// Kind selects a supported guest kind.
	Kind string
	// Network is default, none, or an agent-facing network name.
	Network string
	// Host optionally pins the sandbox's configured member.
	Host string
	// CPUs is the requested CPU count.
	CPUs int64
	// MemoryMB is the memory limit in MiB.
	MemoryMB int64
	// DiskGB is the root disk limit in GiB.
	DiskGB int64
	// Start requests a running rather than stopped guest.
	Start bool
}

// ExecRequest is a bounded guest shell invocation.
type ExecRequest struct {
	// Ref identifies the guest.
	Ref Ref
	// Argv contains sh, -c, and the agent's command.
	Argv []string
	// User is a numeric UID; empty selects root.
	User string
	// Cwd is the optional absolute guest working directory.
	Cwd string
	// Env contains parsed KEY=VALUE entries.
	Env map[string]string
	// Stdin is the bounded input string.
	Stdin string
	// Timeout is the exec-only budget; request cancellation still wins.
	Timeout time.Duration
}

// ExecResult separates process exit from exec-only timeout.
type ExecResult struct {
	// ExitCode is the guest process exit status, or -1 on timeout.
	ExitCode int64
	// Stdout contains at most 64 KiB of standard output.
	Stdout string
	// Stderr contains at most 64 KiB of standard error.
	Stderr string
	// StdoutTruncated reports discarded standard output bytes.
	StdoutTruncated bool
	// StderrTruncated reports discarded standard error bytes.
	StderrTruncated bool
	// TimedOut reports an exec-only deadline, not caller cancellation.
	TimedOut bool
}

// PendingInstance represents an accepted create, whose wait does not hold the mutation gate.
type PendingInstance interface {
	Wait(context.Context) (Instance, error)
}

// Backend is the Incus consumer seam used by Service; it contains only slice-one calls.
// The adapter imports these value types; compute does not import the adapter.
type Backend interface {
	CreateSandbox(context.Context, Sandbox) error
	ListSandboxes(context.Context) ([]Sandbox, error)
	GetSandbox(context.Context, string) (Sandbox, error)
	ExtendSandbox(context.Context, string, time.Time) (Sandbox, error)
	DeleteSandbox(context.Context, string) error
	BeginCreateInstance(context.Context, CreateInstance) (PendingInstance, error)
	ListInstances(context.Context, string) ([]Instance, error)
	GetInstance(context.Context, Ref) (Instance, error)
	DeleteInstance(context.Context, Ref) error
	Exec(context.Context, ExecRequest, io.Writer, io.Writer) (int64, error)
	ListNetworks(context.Context, string) ([]Network, error)
	CreateNetwork(context.Context, string, Network) (Network, error)
	AttachNIC(context.Context, Ref, string, string, string, string) (NIC, error)
	GetNetwork(context.Context, string, string) (Network, error)
	DeleteNetwork(context.Context, string, string) error
	DetachNIC(context.Context, Ref, string) error
	PeerNetworks(context.Context, string, string, string) error
	AddACLRule(context.Context, string, string, ACLRule) (ACLRule, error)
	RemoveACLRule(context.Context, string, string, string) error
	CreateForward(context.Context, string, string, Ref, int64, int64, string) (Forward, error)
	StartInstance(context.Context, Ref, bool) (Instance, error)
	StopInstance(context.Context, Ref, bool) (Instance, error)
	RestartInstance(context.Context, Ref, bool) (Instance, error)
	WaitInstance(context.Context, WaitRequest) (WaitResult, error)
	ReadFile(context.Context, FileReadRequest) (FileReadResult, error)
	WriteFile(context.Context, FileWriteRequest) (FileWriteResult, error)
	CreateSnapshot(context.Context, Ref, string) error
	RestoreSnapshot(context.Context, Ref, string) error
	DeleteSnapshot(context.Context, Ref, string) error
	ListSnapshots(context.Context, Ref) ([]Snapshot, error)
	PublishInstance(context.Context, Ref, string) (string, error)
	GetSandboxImage(context.Context, string, string) (CatalogImage, error)
}
