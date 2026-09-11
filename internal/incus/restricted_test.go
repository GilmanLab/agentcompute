package incus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

// TestRestrictedIdentityFiltersReadsAndRejectsMutation exercises the Incus
// SDK against an HTTP fixture that mimics restricted-certificate filtering:
// reads outside the identity return an empty list (never 403), mutations
// return 403. No live cluster identity is used.
func TestRestrictedIdentityFiltersReadsAndRejectsMutation(t *testing.T) {
	fixture := newRestrictedDaemon(t)
	t.Cleanup(fixture.Close)

	sdk := connectFixture(t, fixture.URL)
	t.Cleanup(sdk.Disconnect)
	client := &Client{server: sdk, host: "lab01", pool: "data"}
	ctx := context.Background()

	t.Run("default project instance list is empty not forbidden", func(t *testing.T) {
		instances, err := sdk.UseProject(api.ProjectDefaultName).GetInstances(api.InstanceTypeAny)
		require.NoError(t, err)
		assert.Empty(t, instances)
	})

	t.Run("default project instance create is a mutation error", func(t *testing.T) {
		_, err := sdk.UseProject(api.ProjectDefaultName).CreateInstance(api.InstancesPost{
			Name: "web",
			Type: api.InstanceTypeContainer,
			Source: api.InstanceSource{
				Type: "none",
			},
		})
		require.Error(t, err)
		assert.True(t, api.StatusErrorCheck(err, http.StatusForbidden))
		mapped := mapError(err)
		require.NotErrorIs(t, mapped, compute.ErrNotFound)
		require.NotErrorIs(t, mapped, compute.ErrUnavailable)
	})

	t.Run("adapter lists no instances when the backend returns an empty filter", func(t *testing.T) {
		got, err := client.ListInstances(ctx, "demo")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("adapter create sandbox surfaces the mutation error", func(t *testing.T) {
		err := client.CreateSandbox(ctx, compute.Sandbox{
			Name: "blocked",
			Host: "lab01",
		})
		require.Error(t, err)
		require.NotErrorIs(t, err, compute.ErrNotFound)
		require.NotErrorIs(t, err, compute.ErrUnavailable)
		assert.True(t, api.StatusErrorCheck(err, http.StatusForbidden))
	})
}

func newRestrictedDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeIncusError(w, http.StatusForbidden, "Not authorized")
			return
		}
		switch {
		case r.URL.Path == "/1.0" || r.URL.Path == "/1.0/":
			writeIncusSync(w, api.Server{
				ServerUntrusted: api.ServerUntrusted{
					APIStatus:  "stable",
					APIVersion: "1.0",
					Auth:       "trusted",
					APIExtensions: []string{
						"network",
						"projects",
						"clustering",
						"instance_get_full",
						"container_full",
						"etag",
					},
				},
				Environment: api.ServerEnvironment{},
			})
		case r.URL.Path == "/1.0/instances":
			writeIncusSync(w, []api.Instance{})
		case r.URL.Path == "/1.0/projects/ac-demo":
			writeIncusSync(w, api.Project{
				Name: "ac-demo",
				ProjectPut: api.ProjectPut{
					Config: map[string]string{
						metaVersion: versionValue,
						metaHost:    "lab01",
					},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/1.0/networks/"):
			writeIncusError(w, http.StatusNotFound, "Network not found")
		default:
			writeIncusError(w, http.StatusNotFound, "not found")
		}
	}))
}

func connectFixture(t *testing.T, rawURL string) incusclient.InstanceServer {
	t.Helper()
	server, err := incusclient.ConnectIncusWithContext(context.Background(), rawURL, &incusclient.ConnectionArgs{
		InsecureSkipVerify: true,
		SkipGetEvents:      true,
	})
	require.NoError(t, err)
	return server
}

func writeIncusSync(w http.ResponseWriter, metadata any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.ResponseRaw{
		Type:       api.SyncResponse,
		Status:     "Success",
		StatusCode: http.StatusOK,
		Metadata:   metadata,
	})
}

func writeIncusError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(api.ResponseRaw{
		Type:  api.ErrorResponse,
		Code:  code,
		Error: message,
	})
}
