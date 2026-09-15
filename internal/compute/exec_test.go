package compute_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestExecTimeoutSetsFlagNotCancellation(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	ref := compute.Ref{Sandbox: "demo", Name: "web"}
	inst := runningInstance()
	inst.Status = "Ready"
	tc.backend.EXPECT().GetInstance(mock.Anything, ref).Return(inst, nil)
	tc.backend.EXPECT().Exec(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(ctx context.Context, _ compute.ExecRequest, stdout, stderr io.Writer) (int64, error) {
			_, _ = stdout.Write([]byte("out"))
			_, _ = stderr.Write([]byte("err"))
			<-ctx.Done()
			return 0, ctx.Err()
		})

	result, err := tc.service.Exec(t.Context(), compute.ExecRequest{
		Ref:     ref,
		Timeout: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, result.TimedOut)
	assert.Equal(t, int64(-1), result.ExitCode)
	assert.Equal(t, "out", result.Stdout)
	assert.Equal(t, "err", result.Stderr)
}

func TestExecParentCancellationWins(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	ref := compute.Ref{Sandbox: "demo", Name: "web"}
	tc.backend.EXPECT().GetInstance(mock.Anything, ref).Return(runningInstance(), nil)
	tc.backend.EXPECT().Exec(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(ctx context.Context, _ compute.ExecRequest, _, _ io.Writer) (int64, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()

	result, err := tc.service.Exec(ctx, compute.ExecRequest{
		Ref:     ref,
		Timeout: time.Hour,
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, result.TimedOut)
	assert.Equal(t, int64(-1), result.ExitCode)
}

func TestExecDrainsOverflow(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	ref := compute.Ref{Sandbox: "demo", Name: "web"}
	overflow := 64*1024 + 128
	tc.backend.EXPECT().GetInstance(mock.Anything, ref).Return(runningInstance(), nil)
	tc.backend.EXPECT().Exec(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ compute.ExecRequest, stdout, stderr io.Writer) (int64, error) {
			n, err := stdout.Write(bytes.Repeat([]byte("a"), overflow))
			require.NoError(t, err)
			assert.Equal(t, overflow, n)
			n, err = stderr.Write(bytes.Repeat([]byte("b"), overflow))
			require.NoError(t, err)
			assert.Equal(t, overflow, n)
			return 0, nil
		})

	result, err := tc.service.Exec(t.Context(), compute.ExecRequest{Ref: ref})
	require.NoError(t, err)
	assert.False(t, result.TimedOut)
	assert.True(t, result.StdoutTruncated)
	assert.True(t, result.StderrTruncated)
	assert.Len(t, result.Stdout, 64*1024)
	assert.Len(t, result.Stderr, 64*1024)
	assert.Equal(t, int64(0), result.ExitCode)
}

func TestExecRejectsUnsupportedUIDs(t *testing.T) {
	t.Parallel()
	for _, user := range []string{"alice", "-1", "4294967296"} {
		t.Run(user, func(t *testing.T) {
			t.Parallel()
			tc := newTestContext(t)
			_, err := tc.service.Exec(t.Context(), compute.ExecRequest{
				Ref:  compute.Ref{Sandbox: "demo", Name: "web"},
				User: user,
			})
			var actionable *codemode.AgentError
			require.ErrorAs(t, err, &actionable)
		})
	}
}

func TestOpenExecRejectsExpiredSandbox(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(expiredSandbox(), nil)
	_, err := tc.service.OpenExec(t.Context(), compute.ExecRequest{
		Ref:  compute.Ref{Sandbox: "demo", Name: "web"},
		Argv: []string{`C:\ProgramData\agentcompute\cua-driver\cua-driver-proxy.exe`, "mcp"},
	})
	requireAgentContains(t, err, "expired")
}
