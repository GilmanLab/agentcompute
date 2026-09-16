package lume

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestGuestCommandPreservesArgvEnvAndCwd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	remote, err := guestCommand(compute.ExecRequest{
		Ref:  compute.Ref{Sandbox: "demo", Name: "web"},
		Argv: []string{"printf", "%s\n", "hello world", "it's", "$HOME", "a;b", "`id`"},
		Cwd:  dir,
		Env:  map[string]string{"FOO": "bar baz", "IT": "it's"},
	})
	require.NoError(t, err)

	out, err := exec.Command("sh", "-c", remote).Output()
	require.NoError(t, err)
	assert.Equal(t, "hello world\nit's\n$HOME\na;b\n`id`\n", string(out))

	envRemote, err := guestCommand(compute.ExecRequest{
		Ref:  compute.Ref{Sandbox: "demo", Name: "web"},
		Argv: []string{"/bin/sh", "-c", "printf %s \"$FOO\""},
		Env:  map[string]string{"FOO": "bar baz"},
	})
	require.NoError(t, err)
	out, err = exec.Command("sh", "-c", envRemote).Output()
	require.NoError(t, err)
	assert.Equal(t, "bar baz", string(out))

	cwdRemote, err := guestCommand(compute.ExecRequest{
		Ref:  compute.Ref{Sandbox: "demo", Name: "web"},
		Argv: []string{"/bin/pwd"},
		Cwd:  dir,
	})
	require.NoError(t, err)
	out, err = exec.Command("sh", "-c", cwdRemote).Output()
	require.NoError(t, err)
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestGuestCommandSeedUsersSkipSudo(t *testing.T) {
	t.Parallel()
	ref := compute.Ref{Sandbox: "demo", Name: "web"}
	for _, user := range []string{"", "lume", "501"} {
		remote, err := guestCommand(compute.ExecRequest{Ref: ref, Argv: []string{"true"}, User: user})
		require.NoError(t, err, user)
		assert.NotContains(t, remote, "sudo", user)
		assert.True(t, strings.HasPrefix(remote, "exec "), user)
	}

	root, err := guestCommand(compute.ExecRequest{Ref: ref, Argv: []string{"true"}, User: rootUser})
	require.NoError(t, err)
	assert.Contains(t, root, "exec sudo -n -- ")
	assert.NotContains(t, root, "sudo -n -u")

	other, err := guestCommand(compute.ExecRequest{Ref: ref, Argv: []string{"true"}, User: "502"})
	require.NoError(t, err)
	assert.Contains(t, other, "sudo -n -u "+quote("#502")+" -- ")
	assert.NotContains(t, other, "sudo -n -u #502")
}

func TestGuestCommandRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  compute.ExecRequest
		want string
	}{
		{
			name: "missing argv",
			req:  compute.ExecRequest{Ref: compute.Ref{Sandbox: "demo", Name: "web"}},
			want: "exec argv is required",
		},
		{
			name: "relative cwd",
			req: compute.ExecRequest{
				Ref:  compute.Ref{Sandbox: "demo", Name: "web"},
				Argv: []string{"true"},
				Cwd:  "tmp",
			},
			want: "cwd must be an absolute path",
		},
		{
			name: "named user",
			req: compute.ExecRequest{
				Ref:  compute.Ref{Sandbox: "demo", Name: "web"},
				Argv: []string{"true"},
				User: "nobody",
			},
			want: "exec user must be lume, root, or a numeric UID",
		},
		{
			name: "bad env name",
			req: compute.ExecRequest{
				Ref:  compute.Ref{Sandbox: "demo", Name: "web"},
				Argv: []string{"true"},
				Env:  map[string]string{"FOO=BAR": "1"},
			},
			want: `invalid env name "FOO=BAR"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := guestCommand(tt.req)
			require.EqualError(t, err, tt.want)
		})
	}
}
func TestExecThroughJumpHonorsStreamsAndLumeUser(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	var stdout, stderr bytes.Buffer
	code, err := gt.client.Exec(t.Context(), compute.ExecRequest{
		Ref:  gt.ref,
		Argv: []string{"/bin/sh", "-c", "printf %s out; printf %s err >&2; exit 7"},
		User: "lume",
	}, &stdout, &stderr)
	require.NoError(t, err)
	assert.Equal(t, int64(7), code)
	assert.Equal(t, "out", stdout.String())
	assert.Equal(t, "err", stderr.String())

	stdout.Reset()
	code, err = gt.client.Exec(t.Context(), compute.ExecRequest{
		Ref:  gt.ref,
		Argv: []string{"printf", "%s", "hello world", "it's"},
		User: "lume",
	}, &stdout, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, int64(0), code)
	assert.Equal(t, "hello worldit's", stdout.String())
}

func TestExecCancellationKillsProcessGroup(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	code, err := gt.client.Exec(ctx, compute.ExecRequest{
		Ref:  gt.ref,
		Argv: []string{"sleep", "30"},
		User: "lume",
	}, io.Discard, io.Discard)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(-1), code)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestOpenExecStreamsThenReapsOnClose(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	stream, err := gt.client.OpenExec(t.Context(), compute.ExecRequest{
		Ref:  gt.ref,
		Argv: []string{"/bin/sh", "-c", "printf %s ready; sleep 30"},
		User: "lume",
	})
	require.NoError(t, err)
	buf := make([]byte, 5)
	n, err := io.ReadFull(stream, buf)
	require.NoError(t, err)
	assert.Equal(t, "ready", string(buf[:n]))
	start := time.Now()
	require.NoError(t, stream.Close())
	require.NoError(t, stream.Close())
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestOpenExecStartCancellationDoesNotLeaveProcess(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := gt.client.OpenExec(ctx, compute.ExecRequest{
		Ref:  gt.ref,
		Argv: []string{"sleep", "30"},
		User: "lume",
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestCommandCancelDoesNotOrphan(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cmd := commandWithCancel(ctx, "/bin/sleep", "30")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	cancel()
	err := cmd.Wait()
	require.Error(t, err)
	assert.Error(t, syscall.Kill(pid, 0), "sleep must be gone after cancel")
}
