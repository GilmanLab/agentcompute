package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	computemocks "github.com/GilmanLab/agentcompute/internal/compute/mocks"
)

func TestWindowsCallDoesNotReplayAfterLostResponse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ref := compute.Ref{Sandbox: "demo", Name: "desktop"}
	backend := computemocks.NewMockBackend(t)
	backend.EXPECT().GetSandbox(mock.Anything, ref.Sandbox).
		Return(compute.Sandbox{ExpiresAt: time.Now().Add(time.Hour)}, nil)
	backend.EXPECT().GetInstance(mock.Anything, ref).
		Return(compute.Instance{Ref: ref, OS: "windows", Status: "Running"}, nil)
	backend.EXPECT().DeleteFile(mock.Anything, ref, mock.Anything).Return(nil)

	var effects atomic.Int32
	backend.EXPECT().OpenExec(mock.Anything, mock.Anything).
		RunAndReturn(func(context.Context, compute.ExecRequest) (io.ReadWriteCloser, error) {
			clientConn, serverConn := net.Pipe()
			server := mcp.NewServer(&mcp.Implementation{Name: "faulting-driver", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "click", InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					effects.Add(1)
					// The effect occurred, but its response never reached the caller.
					_ = serverConn.Close()
					return nil, io.ErrUnexpectedEOF
				})
			go func() {
				_ = server.Run(ctx, &mcp.IOTransport{Reader: serverConn, Writer: serverConn})
			}()
			t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
			return clientConn, nil
		})
	service, err := compute.New(backend, nil, compute.Options{})
	require.NoError(t, err)
	driver := NewDriver(service, nil)
	t.Cleanup(func() { _ = driver.Close() })

	_, err = driver.Call(ctx, ref, "click", `{}`)
	require.Error(t, err)
	require.Equal(t, int32(1), effects.Load(), "a transport failure must not duplicate a GUI action")
}

func TestMacDriverReadyUsesLumeHome(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ref := compute.Ref{Sandbox: "demo", Name: "desk"}
	backend := computemocks.NewMockBackend(t)
	backend.EXPECT().GetSandbox(mock.Anything, ref.Sandbox).
		Return(compute.Sandbox{ExpiresAt: time.Now().Add(time.Hour)}, nil)
	inst := compute.Instance{Ref: ref, OS: "macos", Status: "Running"}
	backend.EXPECT().GetInstance(mock.Anything, ref).Return(inst, nil).Times(3)
	backend.EXPECT().Exec(mock.Anything, mock.MatchedBy(func(req compute.ExecRequest) bool {
		if req.User != "lume" || req.Cwd != "/Users/lume" || req.Env["HOME"] != "/Users/lume" {
			return false
		}
		joined := strings.Join(req.Argv, " ")
		return strings.HasPrefix(joined, "/usr/local/bin/cua-driver status") && !strings.Contains(joined, "--socket")
	}), mock.Anything, mock.Anything).Return(int64(0), nil)

	service, err := compute.New(backend, nil, compute.Options{})
	require.NoError(t, err)
	driver := NewDriver(service, nil)
	t.Cleanup(func() { _ = driver.Close() })

	ready, err := driver.Ready(ctx, ref)
	require.NoError(t, err)
	require.True(t, ready)
}
