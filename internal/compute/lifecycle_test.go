package compute_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestWriteFileRejectsOversizedContentAndRelativePath(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	ref := compute.Ref{Sandbox: "demo", Name: "web"}

	_, err := tc.service.WriteFile(t.Context(), compute.FileWriteRequest{
		Ref:     ref,
		Path:    "relative.txt",
		Content: "hi",
	})
	requireAgentContains(t, err, "absolute path")

	_, err = tc.service.WriteFile(t.Context(), compute.FileWriteRequest{
		Ref:     ref,
		Path:    "/tmp/out",
		Content: strings.Repeat("a", 64*1024+1),
	})
	requireAgentContains(t, err, "exceeds")
}

func TestWriteFileRejectsInvalidMode(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	_, err := tc.service.WriteFile(t.Context(), compute.FileWriteRequest{
		Ref:     compute.Ref{Sandbox: "demo", Name: "web"},
		Path:    "/tmp/out",
		Content: "hi",
		Mode:    "rwx",
	})
	requireAgentContains(t, err, "octal")
}

func TestPublishInstanceRejectsCatalogName(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	_, err := tc.service.PublishInstance(t.Context(), compute.Ref{Sandbox: "demo", Name: "web"}, "router")
	requireAgentContains(t, err, "reserved")
}

func TestStartInstanceRejectsExpiredSandbox(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(expiredSandbox(), nil)
	_, err := tc.service.StartInstance(t.Context(), compute.Ref{Sandbox: "demo", Name: "web"}, false)
	requireAgentContains(t, err, "expired")
}

func TestReadFileTruncatesBackendOverflow(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	ref := compute.Ref{Sandbox: "demo", Name: "web"}
	tc.backend.EXPECT().ReadFile(mock.Anything, mock.Anything).Return(compute.FileReadResult{
		Content:   strings.Repeat("x", 100),
		Truncated: false,
	}, nil)

	result, err := tc.service.ReadFile(t.Context(), compute.FileReadRequest{
		Ref:      ref,
		Path:     "/etc/hostname",
		MaxBytes: 8,
	})
	require.NoError(t, err)
	assert.Equal(t, "xxxxxxxx", result.Content)
	assert.True(t, result.Truncated)
}

func TestWaitInstanceTimesOutWithActionableError(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	ref := compute.Ref{Sandbox: "demo", Name: "web"}
	tc.backend.EXPECT().WaitInstance(mock.Anything, mock.Anything).
		RunAndReturn(func(ctx context.Context, _ compute.WaitRequest) (compute.WaitResult, error) {
			<-ctx.Done()
			return compute.WaitResult{Status: "Stopped"}, ctx.Err()
		})

	_, err := tc.service.WaitInstance(t.Context(), compute.WaitRequest{
		Ref:     ref,
		Until:   compute.WaitUntilRunning,
		Timeout: 20 * time.Millisecond,
	})
	requireAgentContains(t, err, "did not become")
}

func TestWaitInstanceDoesNotHoldGate(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	ref := compute.Ref{Sandbox: "demo", Name: "web"}
	waitStarted := make(chan struct{})
	releaseWait := make(chan struct{})
	live := liveSandbox("demo")

	tc.backend.EXPECT().WaitInstance(mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, compute.WaitRequest) (compute.WaitResult, error) {
			close(waitStarted)
			<-releaseWait
			return compute.WaitResult{Status: "Running"}, nil
		})
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(live, nil)
	tc.backend.EXPECT().ExtendSandbox(mock.Anything, "demo", mock.AnythingOfType("time.Time")).
		Return(live, nil)

	errCh := make(chan error, 1)
	waitCtx, cancelWait := context.WithCancel(t.Context())
	defer cancelWait()
	defer func() {
		close(releaseWait)
		assert.NoError(t, <-errCh)
	}()
	go func() {
		_, err := tc.service.WaitInstance(waitCtx, compute.WaitRequest{
			Ref:   ref,
			Until: compute.WaitUntilRunning,
		})
		errCh <- err
	}()

	select {
	case <-waitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not start; gate likely held")
	}

	extendCtx, cancelExtend := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelExtend()
	_, err := tc.service.ExtendSandbox(extendCtx, "demo", time.Hour)
	require.NoError(t, err)
}

func requireAgentContains(t *testing.T, err error, substr string) {
	t.Helper()
	require.Error(t, err)
	var agent *codemode.AgentError
	require.ErrorAs(t, err, &agent)
	assert.Contains(t, agent.Message, substr)
}
