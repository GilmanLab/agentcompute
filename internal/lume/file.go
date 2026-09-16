package lume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/meigma/codemode"
	"github.com/pkg/sftp"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const defaultFileReadLimit = 64 * 1024

// ReadFile pulls a bounded guest file through SFTP.
func (c *Client) ReadFile(ctx context.Context, req compute.FileReadRequest) (compute.FileReadResult, error) {
	body, err := c.ReadBinaryFile(ctx, req.Ref, req.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return compute.FileReadResult{}, fileErrorf(
				"file not found on instance %q in sandbox %q",
				req.Ref.Name,
				req.Ref.Sandbox,
			)
		}
		return compute.FileReadResult{}, err
	}
	defer body.Close()

	limit := req.MaxBytes
	if limit <= 0 {
		limit = defaultFileReadLimit
	}
	buf, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return compute.FileReadResult{}, fmt.Errorf("read file: %w", err)
	}
	truncated := int64(len(buf)) > limit
	if truncated {
		buf = buf[:limit]
	}
	return compute.FileReadResult{Content: string(buf), Truncated: truncated}, nil
}

// ReadBinaryFile opens a guest file without passing bytes through exec or text conversion.
// The caller must close the stream and enforce its own byte bound.
func (c *Client) ReadBinaryFile(ctx context.Context, ref compute.Ref, path string) (io.ReadCloser, error) {
	if err := validateGuestPath(path); err != nil {
		return nil, err
	}
	session, err := c.openSFTP(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err = session.requireRegular(path, false); err != nil {
		_ = session.Close()
		return nil, err
	}
	file, err := session.client.Open(path)
	if err != nil {
		_ = session.Close()
		return nil, session.normalize(err)
	}
	return &fileStream{file: file, session: session}, nil
}

// WriteFile writes a guest file through SFTP, applying requested permissions before its contents.
func (c *Client) WriteFile(ctx context.Context, req compute.FileWriteRequest) (compute.FileWriteResult, error) {
	if err := validateGuestPath(req.Path); err != nil {
		return compute.FileWriteResult{}, err
	}
	var mode os.FileMode
	if req.Mode != "" {
		var err error
		mode, err = parseOctalMode(req.Mode)
		if err != nil {
			return compute.FileWriteResult{}, err
		}
	}
	session, err := c.openSFTP(ctx, req.Ref)
	if err != nil {
		return compute.FileWriteResult{}, err
	}
	defer session.Close()
	if err = session.requireRegular(req.Path, true); err != nil {
		return compute.FileWriteResult{}, err
	}
	file, err := session.client.OpenFile(req.Path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return compute.FileWriteResult{}, session.normalize(err)
	}
	if req.Mode != "" {
		if err := file.Chmod(mode); err != nil {
			return compute.FileWriteResult{}, session.normalize(err)
		}
	}
	n, writeErr := io.WriteString(file, req.Content)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return compute.FileWriteResult{}, session.normalize(err)
	}
	return compute.FileWriteResult{Bytes: int64(n)}, nil
}

// DeleteFile removes one literal guest path through SFTP.
func (c *Client) DeleteFile(ctx context.Context, ref compute.Ref, path string) error {
	if err := validateGuestPath(path); err != nil {
		return err
	}
	session, err := c.openSFTP(ctx, ref)
	if err != nil {
		return err
	}
	defer session.Close()
	return session.normalize(session.client.Remove(path))
}

type fileStream struct {
	file    *sftp.File
	session *fileSession
}

func (s *fileStream) Read(p []byte) (int, error) {
	n, err := s.file.Read(p)
	return n, s.session.normalize(err)
}

func (s *fileStream) Close() error {
	// Disconnect rather than drain: a bounded read must not transfer the remaining file.
	return s.session.Close()
}

type fileSession struct {
	client    *sftp.Client
	cmd       *exec.Cmd
	ctx       context.Context
	cleanup   func()
	closeOnce sync.Once
}

func (s *fileSession) Close() error {
	s.closeOnce.Do(func() {
		// Terminating the SSH process also closes remote handles, including partial reads.
		_ = killProcessGroup(s.cmd)
		_ = s.client.Close()
		_ = s.cmd.Wait()
		s.cleanup()
	})
	return nil
}

func (s *fileSession) normalize(err error) error {
	if err != nil && s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	return err
}

func (s *fileSession) requireRegular(path string, allowMissing bool) error {
	info, err := s.client.Stat(path)
	if allowMissing && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return s.normalize(err)
	}
	if !info.Mode().IsRegular() {
		return fileErrorf("path %q is not a regular file", path)
	}
	return nil
}

func (c *Client) openSFTP(ctx context.Context, ref compute.Ref) (*fileSession, error) {
	if ref.Sandbox == "" || ref.Name == "" {
		return nil, errors.New("instance reference is required")
	}
	address, seed, image, err := c.guestTarget(ctx, ref)
	if err != nil {
		return nil, err
	}
	cmd, cleanup, err := c.guestCmd(ctx, c.sshPath, address, seed, image, "-s", "--", guestHostAlias, "sftp")
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		cleanup()
		return nil, err
	}
	stderr := &capWriter{limit: hostOutputLimit}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		cleanup()
		return nil, err
	}
	client, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		_ = killProcessGroup(cmd)
		_ = stdin.Close()
		_ = stdout.Close()
		_ = cmd.Wait()
		cleanup()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("start guest SFTP: %w: %s", err, strings.TrimSpace(stderr.buf.String()))
	}
	return &fileSession{client: client, cmd: cmd, ctx: ctx, cleanup: cleanup}, nil
}

func validateGuestPath(path string) error {
	if path == "" || strings.ContainsRune(path, '\x00') || !strings.HasPrefix(path, "/") {
		return errors.New("path must be an absolute path")
	}
	return nil
}

func parseOctalMode(mode string) (os.FileMode, error) {
	value, err := strconv.ParseUint(mode, 8, 12)
	if err != nil {
		return 0, fileErrorf("mode %q is not an octal file mode", mode)
	}
	result := os.FileMode(value) & os.ModePerm
	if value&0o4000 != 0 {
		result |= os.ModeSetuid
	}
	if value&0o2000 != 0 {
		result |= os.ModeSetgid
	}
	if value&0o1000 != 0 {
		result |= os.ModeSticky
	}
	return result, nil
}

func fileErrorf(format string, args ...any) error {
	return &codemode.AgentError{Message: fmt.Sprintf(format, args...)}
}
