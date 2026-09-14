package compute

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

const (
	desktopJSONLimit    = 4 << 20
	desktopPollInterval = 500 * time.Millisecond
)

// ExecJSON runs a guest argv with a 4 MiB output bound and rejects truncation.
// It uses the same draining, deadline, and cancellation path as Exec.
func (s *Service) ExecJSON(ctx context.Context, req ExecRequest) (ExecResult, error) {
	result, err := s.exec(ctx, req, desktopJSONLimit)
	if err != nil {
		return result, err
	}
	if result.StdoutTruncated || result.StderrTruncated {
		return ExecResult{}, agentError("Driver output exceeds the 4 MiB JSON limit")
	}
	return result, nil
}

// ReadBinaryFile opens a guest file for bounded streaming by an internal consumer.
// Missing files retain [os.ErrNotExist] so optional screenshots need no text parsing.
func (s *Service) ReadBinaryFile(ctx context.Context, ref Ref, path string) (io.ReadCloser, error) {
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	if err := validateFilePath(path); err != nil {
		return nil, err
	}
	if _, err := s.SandboxExpiry(ctx, ref.Sandbox); err != nil {
		return nil, err
	}
	body, err := s.backend.ReadBinaryFile(ctx, ref, path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, s.mapBackend(ctx, "read binary file", err)
	}
	return body, err
}

// SandboxExpiry reads live metadata without taking the control-plane mutation gate.
func (s *Service) SandboxExpiry(ctx context.Context, name string) (time.Time, error) {
	if err := validateName(name); err != nil {
		return time.Time{}, err
	}
	box, err := s.backend.GetSandbox(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return time.Time{}, sandboxNotFound(name)
		}
		return time.Time{}, s.backendError(ctx, "get sandbox", err)
	}
	if !box.ExpiresAt.After(time.Now()) {
		return time.Time{}, sandboxExpired(name)
	}
	return box.ExpiresAt, nil
}

func (s *Service) waitDesktop(ctx context.Context, ref Ref) (WaitResult, error) {
	if s.desktopReady == nil {
		return WaitResult{}, agentError("desktop readiness is not configured")
	}
	result, err := s.backend.WaitInstance(ctx, WaitRequest{Ref: ref, Until: WaitUntilAgent})
	if err != nil {
		return result, err
	}
	ticker := time.NewTicker(desktopPollInterval)
	defer ticker.Stop()
	for {
		ready, err := s.desktopReady(ctx, ref)
		if err != nil {
			return result, err
		}
		if ready {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-ticker.C:
		}
	}
}
