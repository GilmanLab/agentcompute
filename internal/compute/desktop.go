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

// OpenExec starts a guest command and returns attached stdin/stdout.
// The caller must Close the stream. The start context does not bound a
// successful stream; Close and process death do. Live sandbox expiry is
// checked at open.
func (s *Service) OpenExec(ctx context.Context, req ExecRequest) (io.ReadWriteCloser, error) {
	if err := validateRef(req.Ref); err != nil {
		return nil, err
	}
	if err := validateExec(req); err != nil {
		return nil, err
	}
	if len(req.Argv) == 0 {
		return nil, agentError("exec argv is required")
	}
	if _, err := s.SandboxExpiry(ctx, req.Ref.Sandbox); err != nil {
		return nil, err
	}
	inst, err := s.GetInstance(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	if inst.Status != statusRunning && inst.Status != "Ready" {
		return nil, agentErrorf("instance %q in sandbox %q is not running", req.Ref.Name, req.Ref.Sandbox)
	}
	if err = validateExecUser(req, inst); err != nil {
		return nil, err
	}
	stream, err := s.backend.OpenExec(ctx, req)
	if err != nil {
		return nil, s.mapBackend(ctx, "open exec", err)
	}
	return stream, nil
}

// DeleteFile removes a guest file for an internal consumer.
// Missing files retain [os.ErrNotExist]. Live sandbox expiry is checked.
func (s *Service) DeleteFile(ctx context.Context, ref Ref, path string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	if err := validateFilePath(path); err != nil {
		return err
	}
	if _, err := s.SandboxExpiry(ctx, ref.Sandbox); err != nil {
		return err
	}
	err := s.backend.DeleteFile(ctx, ref, path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s.mapBackend(ctx, "delete file", err)
	}
	return err
}

// ReadBinaryFile opens a guest file for bounded streaming by an internal consumer.
// Missing files retain [os.ErrNotExist] so optional screenshots need no text parsing.
// The returned expiry is the live sandbox timestamp from the same check, so callers
// can publish without a second control-plane round trip.
func (s *Service) ReadBinaryFile(ctx context.Context, ref Ref, path string) (io.ReadCloser, time.Time, error) {
	if err := validateRef(ref); err != nil {
		return nil, time.Time{}, err
	}
	if err := validateFilePath(path); err != nil {
		return nil, time.Time{}, err
	}
	expiry, err := s.SandboxExpiry(ctx, ref.Sandbox)
	if err != nil {
		return nil, time.Time{}, err
	}
	body, err := s.backend.ReadBinaryFile(ctx, ref, path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, s.mapBackend(ctx, "read binary file", err)
	}
	return body, expiry, err
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
