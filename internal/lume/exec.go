package lume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

// Exec runs a guest command and drains stdout/stderr. Cancellation kills the
// ssh process group and then waits a bounded teardown.
func (c *Client) Exec(ctx context.Context, req compute.ExecRequest, stdout, stderr io.Writer) (int64, error) {
	if err := validateExecRequest(req); err != nil {
		return -1, err
	}
	remote, err := guestCommand(req)
	if err != nil {
		return -1, err
	}
	address, seed, image, err := c.guestTarget(ctx, req.Ref)
	if err != nil {
		return -1, err
	}

	cmd, cleanup, err := c.guestCmd(ctx, c.sshPath, address, seed, image, "--", guestHostAlias, remote)
	if err != nil {
		return -1, err
	}
	defer cleanup()

	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}

	err = cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	return sshExitStatus(err)
}

// OpenExec starts a guest command and returns attached stdin/stdout.
// Close kills the ssh process group and reaps it. The start context does
// not bound a successful stream.
func (c *Client) OpenExec(ctx context.Context, req compute.ExecRequest) (io.ReadWriteCloser, error) {
	if err := validateExecRequest(req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	remote, err := guestCommand(req)
	if err != nil {
		return nil, err
	}
	address, seed, image, err := c.guestTarget(ctx, req.Ref)
	if err != nil {
		return nil, err
	}

	cmd, cleanup, err := c.guestCmd(context.Background(), c.sshPath, address, seed, image, "--", guestHostAlias, remote)
	if err != nil {
		return nil, err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("guest exec stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		cleanup()
		return nil, fmt.Errorf("guest exec stdout: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		cleanup()
		return nil, fmt.Errorf("guest exec: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = killProcessGroup(cmd)
		_ = cmd.Wait()
		_ = stdin.Close()
		_ = stdout.Close()
		cleanup()
		return nil, err
	}
	return &execConn{
		stdin:   stdin,
		stdout:  stdout,
		cmd:     cmd,
		cleanup: cleanup,
	}, nil
}

type execConn struct {
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	cmd       *exec.Cmd
	cleanup   func()
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func (e *execConn) Read(p []byte) (int, error) {
	return e.stdout.Read(p)
}

func (e *execConn) Write(p []byte) (int, error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	return e.stdin.Write(p)
}

func (e *execConn) Close() error {
	e.closeOnce.Do(func() {
		_ = e.stdin.Close()
		_ = killProcessGroup(e.cmd)
		waitCtx, cancel := context.WithTimeout(context.Background(), execTeardownBound)
		defer cancel()
		waitDone := make(chan struct{})
		go func() {
			_ = e.cmd.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-waitCtx.Done():
			_ = killProcessGroup(e.cmd)
			select {
			case <-waitDone:
			case <-time.After(execTeardownBound):
			}
		}
		_ = e.stdout.Close()
		if e.cleanup != nil {
			e.cleanup()
		}
	})
	return nil
}

func guestCommand(req compute.ExecRequest) (string, error) {
	if err := validateExecRequest(req); err != nil {
		return "", err
	}

	var b strings.Builder
	if req.Cwd != "" {
		b.WriteString("cd ")
		b.WriteString(quote(req.Cwd))
		b.WriteString(" && ")
	}
	b.WriteString("exec ")
	switch {
	case seedUser(req.User):
	case req.User == rootUser || req.User == "0":
		b.WriteString("sudo -n -- ")
	default:
		b.WriteString("sudo -n -u ")
		b.WriteString(quote("#" + req.User))
		b.WriteString(" -- ")
	}
	if len(req.Env) > 0 {
		b.WriteString("env")
		keys := slices.Sorted(maps.Keys(req.Env))
		for _, key := range keys {
			b.WriteByte(' ')
			b.WriteString(key)
			b.WriteByte('=')
			b.WriteString(quote(req.Env[key]))
		}
		b.WriteByte(' ')
	}
	for i, arg := range req.Argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quote(arg))
	}
	return b.String(), nil
}

func validateExecRequest(req compute.ExecRequest) error {
	if req.Ref.Sandbox == "" || req.Ref.Name == "" {
		return errors.New("instance reference is required")
	}
	if len(req.Argv) == 0 {
		return errors.New("exec argv is required")
	}
	if err := validateExecUser(req.User); err != nil {
		return err
	}
	if req.Cwd != "" {
		if strings.ContainsRune(req.Cwd, '\x00') || !strings.HasPrefix(req.Cwd, "/") {
			return errors.New("cwd must be an absolute path")
		}
	}
	for key, value := range req.Env {
		if err := validateEnvName(key); err != nil {
			return err
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("env %q contains NUL", key)
		}
	}
	for _, arg := range req.Argv {
		if strings.ContainsRune(arg, '\x00') {
			return errors.New("exec argv contains NUL")
		}
	}
	return nil
}

func validateExecUser(user string) error {
	if seedUser(user) || user == rootUser {
		return nil
	}
	if _, err := strconv.ParseUint(user, 10, 32); err != nil {
		return errors.New("exec user must be lume, root, or a numeric UID")
	}
	return nil
}

func seedUser(user string) bool {
	return user == "" || user == guestUser || user == guestUID
}

func validateEnvName(name string) error {
	if name == "" {
		return errors.New("env name is required")
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return fmt.Errorf("invalid env name %q", name)
		}
	}
	return nil
}

func commandWithCancel(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return killProcessGroup(cmd)
	}
	cmd.WaitDelay = execTeardownBound
	return cmd
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func sshExitStatus(err error) (int64, error) {
	if err == nil {
		return 0, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return -1, err
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		const sshFailure = 255
		code := exitErr.ExitCode()
		if code == sshFailure {
			return -1, errors.New("guest ssh transport error")
		}
		if code < 0 {
			return -1, fmt.Errorf("guest ssh: %w", err)
		}
		return int64(code), nil
	}
	return -1, fmt.Errorf("guest ssh: %w", err)
}
