package compute

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/meigma/codemode"
)

const (
	// WaitUntilRunning means the guest has reached Running or Ready.
	WaitUntilRunning = "running"
	// WaitUntilAgent means the Incus agent can serve exec and files.
	WaitUntilAgent = "agent"
	// WaitUntilNetwork means at least one NIC has a non-link address.
	WaitUntilNetwork = "network"
	// WaitUntilDesktop is reserved for Phase 6.
	WaitUntilDesktop = "desktop"
	// WaitUntilStopped means the guest has reached Stopped.
	WaitUntilStopped = "stopped"

	fileReadDefault = 64 * 1024
	fileReadMax     = 1024 * 1024
	fileWriteMax    = 64 * 1024
	fileModeBits    = 32
)

// WaitRequest is a bounded readiness poll.
type WaitRequest struct {
	// Ref identifies the guest.
	Ref Ref
	// Until is running, agent, network, desktop, or stopped.
	Until string
	// Timeout is the wait-only budget; request cancellation still wins.
	Timeout time.Duration
}

// WaitResult is the observed state after a wait.
type WaitResult struct {
	// Status is the observed Incus state.
	Status string
	// Elapsed is time spent waiting.
	Elapsed time.Duration
}

// FileReadRequest is a bounded guest file read.
type FileReadRequest struct {
	// Ref identifies the guest.
	Ref Ref
	// Path is an absolute guest path.
	Path string
	// MaxBytes is the read cap; zero selects 64 KiB.
	MaxBytes int64
}

// FileReadResult is truncated text content.
type FileReadResult struct {
	// Content contains at most MaxBytes of file data.
	Content string
	// Truncated reports discarded trailing bytes.
	Truncated bool
}

// FileWriteRequest is a bounded guest file write.
type FileWriteRequest struct {
	// Ref identifies the guest.
	Ref Ref
	// Path is an absolute guest path.
	Path string
	// Content is the exact text to write.
	Content string
	// Mode is an optional octal mode such as 0644.
	Mode string
}

// FileWriteResult reports how many bytes were written.
type FileWriteResult struct {
	// Bytes is the number of content bytes written.
	Bytes int64
}

// Snapshot is a named instance snapshot.
type Snapshot struct {
	// Name is the agent-facing snapshot name.
	Name string
	// CreatedAt is the snapshot creation time.
	CreatedAt time.Time
}

// StartInstance starts a guest and blocks until it is running.
func (s *Service) StartInstance(ctx context.Context, ref Ref, force bool) (Instance, error) {
	return s.changeInstance(ctx, ref, func() (Instance, error) {
		return s.backend.StartInstance(ctx, ref, force)
	}, "start instance")
}

// StopInstance stops a guest and blocks until it is stopped.
func (s *Service) StopInstance(ctx context.Context, ref Ref, force bool) (Instance, error) {
	return s.changeInstance(ctx, ref, func() (Instance, error) {
		return s.backend.StopInstance(ctx, ref, force)
	}, "stop instance")
}

// RestartInstance restarts a guest and blocks until it is running.
func (s *Service) RestartInstance(ctx context.Context, ref Ref, force bool) (Instance, error) {
	return s.changeInstance(ctx, ref, func() (Instance, error) {
		return s.backend.RestartInstance(ctx, ref, force)
	}, "restart instance")
}

// WaitInstance polls until a readiness stage or the wait budget expires.
func (s *Service) WaitInstance(ctx context.Context, req WaitRequest) (WaitResult, error) {
	if err := validateRef(req.Ref); err != nil {
		return WaitResult{}, err
	}
	if err := validateWaitUntil(req.Until); err != nil {
		return WaitResult{}, err
	}
	waitCtx, cancel := execContext(ctx, req.Timeout)
	defer cancel()
	started := time.Now()
	result, err := s.backend.WaitInstance(waitCtx, req)
	result.Elapsed = time.Since(started)
	if err == nil {
		return result, nil
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return result, agentErrorf(
			"instance %q in sandbox %q did not become %s",
			req.Ref.Name,
			req.Ref.Sandbox,
			req.Until,
		)
	}
	if errors.Is(err, ErrNotFound) {
		return result, instanceNotFound(req.Ref)
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, s.mapBackend(ctx, "wait instance", err)
}

// ReadFile returns a bounded guest file.
func (s *Service) ReadFile(ctx context.Context, req FileReadRequest) (FileReadResult, error) {
	if err := validateRef(req.Ref); err != nil {
		return FileReadResult{}, err
	}
	if err := validateFilePath(req.Path); err != nil {
		return FileReadResult{}, err
	}
	req.MaxBytes = clampReadLimit(req.MaxBytes)
	result, err := s.backend.ReadFile(ctx, req)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return FileReadResult{}, instanceNotFound(req.Ref)
		}
		return FileReadResult{}, s.mapBackend(ctx, "read file", err)
	}
	if int64(len(result.Content)) > req.MaxBytes {
		result.Content = result.Content[:req.MaxBytes]
		result.Truncated = true
	}
	return result, nil
}

// WriteFile writes a bounded guest file.
func (s *Service) WriteFile(ctx context.Context, req FileWriteRequest) (FileWriteResult, error) {
	if err := validateRef(req.Ref); err != nil {
		return FileWriteResult{}, err
	}
	if err := validateFilePath(req.Path); err != nil {
		return FileWriteResult{}, err
	}
	if err := validateFileMode(req.Mode); err != nil {
		return FileWriteResult{}, err
	}
	if len(req.Content) > fileWriteMax {
		return FileWriteResult{}, agentErrorf(
			"file content exceeds %d bytes; use instance.exec to transfer larger files",
			fileWriteMax,
		)
	}
	result, err := s.backend.WriteFile(ctx, req)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return FileWriteResult{}, instanceNotFound(req.Ref)
		}
		return FileWriteResult{}, s.mapBackend(ctx, "write file", err)
	}
	return result, nil
}

// CreateSnapshot creates a named instance snapshot.
func (s *Service) CreateSnapshot(ctx context.Context, ref Ref, snapshot string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	if err := validateName(snapshot); err != nil {
		return err
	}
	return s.withLiveSandbox(ctx, ref.Sandbox, func(Sandbox) error {
		if err := s.backend.CreateSnapshot(ctx, ref, snapshot); err != nil {
			if errors.Is(err, ErrNotFound) {
				return instanceNotFound(ref)
			}
			return s.mapBackend(ctx, "create snapshot", err)
		}
		return nil
	})
}

// RestoreSnapshot restores a guest from a snapshot.
func (s *Service) RestoreSnapshot(ctx context.Context, ref Ref, snapshot string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	if err := validateName(snapshot); err != nil {
		return err
	}
	return s.withLiveSandbox(ctx, ref.Sandbox, func(Sandbox) error {
		if err := s.backend.RestoreSnapshot(ctx, ref, snapshot); err != nil {
			if errors.Is(err, ErrNotFound) {
				return instanceNotFound(ref)
			}
			return s.mapBackend(ctx, "restore snapshot", err)
		}
		return nil
	})
}

// DeleteSnapshot deletes a named instance snapshot.
func (s *Service) DeleteSnapshot(ctx context.Context, ref Ref, snapshot string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	if err := validateName(snapshot); err != nil {
		return err
	}
	return s.withLiveSandbox(ctx, ref.Sandbox, func(Sandbox) error {
		if err := s.backend.DeleteSnapshot(ctx, ref, snapshot); err != nil {
			if errors.Is(err, ErrNotFound) {
				return instanceNotFound(ref)
			}
			return s.mapBackend(ctx, "delete snapshot", err)
		}
		return nil
	})
}

// ListSnapshots returns snapshots for one guest.
func (s *Service) ListSnapshots(ctx context.Context, ref Ref) ([]Snapshot, error) {
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	if _, err := s.GetInstance(ctx, ref); err != nil {
		return nil, err
	}
	snapshots, err := s.backend.ListSnapshots(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, instanceNotFound(ref)
		}
		return nil, s.mapBackend(ctx, "list snapshots", err)
	}
	return nonNil(snapshots), nil
}

// PublishInstance publishes a sandbox-scoped image from a guest.
func (s *Service) PublishInstance(ctx context.Context, ref Ref, image string) (string, error) {
	if err := validateRef(ref); err != nil {
		return "", err
	}
	if err := validateName(image); err != nil {
		return "", err
	}
	if _, ok := s.catalog.Lookup(image); ok {
		return "", agentErrorf("image %q is reserved by the catalog", image)
	}
	var name string
	err := s.withLiveSandbox(ctx, ref.Sandbox, func(Sandbox) error {
		published, pubErr := s.backend.PublishInstance(ctx, ref, image)
		if pubErr != nil {
			if errors.Is(pubErr, ErrNotFound) {
				return instanceNotFound(ref)
			}
			return s.mapBackend(ctx, "publish instance", pubErr)
		}
		name = published
		return nil
	})
	if err != nil {
		return "", err
	}
	return name, nil
}

// ResolveImage returns a curated catalog entry or a sandbox-published image.
func (s *Service) ResolveImage(ctx context.Context, sandbox, name string) (CatalogImage, error) {
	if name == "" {
		return CatalogImage{}, imageNotFound(name)
	}
	if image, ok := s.catalog.Lookup(name); ok {
		return image, nil
	}
	if err := validateName(sandbox); err != nil {
		return CatalogImage{}, err
	}
	if _, err := s.backend.GetSandbox(ctx, sandbox); err != nil {
		if errors.Is(err, ErrNotFound) {
			return CatalogImage{}, sandboxNotFound(sandbox)
		}
		return CatalogImage{}, s.mapBackend(ctx, "get sandbox", err)
	}
	image, err := s.backend.GetSandboxImage(ctx, sandbox, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return CatalogImage{}, imageNotFound(name)
		}
		return CatalogImage{}, s.mapBackend(ctx, "get sandbox image", err)
	}
	return image, nil
}

func (s *Service) changeInstance(
	ctx context.Context,
	ref Ref,
	fn func() (Instance, error),
	op string,
) (Instance, error) {
	if err := validateRef(ref); err != nil {
		return Instance{}, err
	}
	var inst Instance
	err := s.withLiveSandbox(ctx, ref.Sandbox, func(Sandbox) error {
		var changeErr error
		inst, changeErr = fn()
		if changeErr != nil {
			if errors.Is(changeErr, ErrNotFound) {
				return instanceNotFound(ref)
			}
			return s.mapBackend(ctx, op, changeErr)
		}
		return nil
	})
	if err != nil {
		return Instance{}, err
	}
	return inst, nil
}

func (s *Service) mapBackend(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var agent *codemode.AgentError
	if errors.As(err, &agent) {
		return err
	}
	return s.backendError(ctx, op, err)
}

func validateWaitUntil(until string) error {
	switch until {
	case WaitUntilRunning, WaitUntilAgent, WaitUntilNetwork, WaitUntilStopped:
		return nil
	case WaitUntilDesktop:
		return agentErrorf("until %q is not available yet", until)
	default:
		return agentErrorf("until %q is not available yet", until)
	}
}

func validateFilePath(path string) error {
	if path == "" || !strings.HasPrefix(path, "/") {
		return agentError("path must be an absolute path")
	}
	return nil
}

func validateFileMode(mode string) error {
	if mode == "" {
		return nil
	}
	if _, err := strconv.ParseUint(mode, 8, fileModeBits); err != nil {
		return agentErrorf("mode %q is not an octal file mode", mode)
	}
	return nil
}

func clampReadLimit(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return fileReadDefault
	}
	if maxBytes > fileReadMax {
		return fileReadMax
	}
	return maxBytes
}
