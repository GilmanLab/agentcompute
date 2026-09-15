package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net"
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
