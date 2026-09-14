package incus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestReadFileReturnsAtLimitWithoutDrainingRemainder(t *testing.T) {
	t.Parallel()
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1.0":
			writeIncusSync(w, api.Server{ServerUntrusted: api.ServerUntrusted{
				Auth: "trusted", APIExtensions: []string{"projects", "instances"},
			}})
		case "/1.0/projects/ac-demo":
			writeIncusSync(w, api.Project{Name: "ac-demo", ProjectPut: api.ProjectPut{
				Config: map[string]string{metaVersion: versionValue},
			}})
		case "/1.0/instances/web":
			writeIncusSync(w, api.Instance{Name: "web"})
		case "/1.0/instances/web/files":
			w.Header().Set("X-Incus-type", "file")
			_, _ = w.Write([]byte("012345678"))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		default:
			writeIncusError(w, http.StatusNotFound, "not found")
		}
	}))
	t.Cleanup(fixture.Close)
	sdk := connectFixture(t, fixture.URL)
	t.Cleanup(sdk.Disconnect)
	client := &Client{server: sdk}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	result, err := client.ReadFile(ctx, compute.FileReadRequest{
		Ref: compute.Ref{Sandbox: "demo", Name: "web"}, Path: "/tmp/stream", MaxBytes: 8,
	})
	require.NoError(t, err)
	require.NoError(t, ctx.Err(), "bounded reads must not wait for the rest of the file")
	assert.Equal(t, "01234567", result.Content)
	assert.True(t, result.Truncated)
}
