package compute

import (
	"context"
	"errors"
	"time"
)

const reaperInterval = 30 * time.Second

// Reap deletes expired sandboxes. A failed delete leaves the project in place
// for the next scan.
func (s *Service) Reap(ctx context.Context) error {
	if s.onReap != nil {
		s.onReap()
	}
	boxes, err := s.backend.ListSandboxes(ctx)
	if err != nil {
		return s.backendError(ctx, "list sandboxes", err)
	}

	var failed error
	for _, box := range boxes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if box.ExpiresAt.After(time.Now()) {
			continue
		}
		if err := s.reapSandbox(ctx, box.Name); err != nil {
			s.log.ErrorContext(ctx, "reaper delete failed", "sandbox", box.Name, "err", err)
			failed = errors.Join(failed, err)
		}
	}
	return failed
}

// RunReaper deletes expired sandboxes at startup and every 30 seconds until ctx ends.
func (s *Service) RunReaper(ctx context.Context) error {
	if err := s.Reap(ctx); err != nil && ctx.Err() == nil {
		s.log.ErrorContext(ctx, "reaper scan failed", "err", err)
	}
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.Reap(ctx); err != nil && ctx.Err() == nil {
				s.log.ErrorContext(ctx, "reaper scan failed", "err", err)
			}
		}
	}
}

func (s *Service) reapSandbox(ctx context.Context, name string) error {
	unlock, err := s.gate.Lock(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()

	box, err := s.backend.GetSandbox(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return s.backendError(ctx, "get sandbox", err)
	}
	if box.ExpiresAt.After(time.Now()) {
		return nil
	}
	if s.onSandboxExpired != nil {
		s.onSandboxExpired(name)
	}
	if err := s.backend.DeleteSandbox(ctx, name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return s.backendError(ctx, "delete sandbox", err)
	}
	return nil
}
