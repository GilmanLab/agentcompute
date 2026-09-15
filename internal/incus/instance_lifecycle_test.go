package incus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestWaitForAgentSurvivesVMMonitorReconnect(t *testing.T) {
	t.Parallel()
	var polls atomic.Int32
	base := (&forwardClaimState{}).handler(t)
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1.0/instances/web" || r.URL.Query().Get("recursion") != "1" {
			base.ServeHTTP(w, r)
			return
		}
		full := api.InstanceFull{
			Instance: api.Instance{
				Name:       "web",
				Type:       string(api.InstanceTypeVM),
				Status:     "Running",
				StatusCode: api.Running,
			},
			State: &api.InstanceState{Processes: 1},
		}
		if polls.Add(1) == 1 {
			full.Status, full.StatusCode = "Error", api.Error
		}
		writeIncusSync(w, full)
	}))
	t.Cleanup(fixture.Close)
	sdk := connectFixture(t, fixture.URL)
	t.Cleanup(sdk.Disconnect)
	client := &Client{server: sdk}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	result, err := client.WaitInstance(ctx, compute.WaitRequest{
		Ref: compute.Ref{Sandbox: "demo", Name: "web"}, Until: compute.WaitUntilAgent,
	})
	require.NoError(t, err)
	assert.Equal(t, "Running", result.Status)
}
