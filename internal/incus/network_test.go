package incus

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestBridgeCollisionRetriesWithoutAdoptingAnotherSandboxNetwork(t *testing.T) {
	t.Parallel()
	const occupied = "ac12345678"
	var mu sync.Mutex
	projectConfig := map[string]string{
		metaVersion:                  versionValue,
		networkKey("lan"):            occupied,
		"restricted.networks.access": occupied,
	}
	networks := map[string]api.Network{occupied: {
		Name:   occupied,
		Type:   networkKindBridge,
		Status: api.NetworkStatusCreated,
		NetworkPut: api.NetworkPut{
			Config: map[string]string{metaSandbox: "other", metaName: "lan", metaVersion: versionValue},
		},
	}}
	var changedOccupied bool
	updateProject := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			var update api.ProjectPut
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				writeIncusError(w, http.StatusBadRequest, err.Error())
				return
			}
			projectConfig = update.Config
		}
		writeIncusSync(w, api.Project{Name: "ac-demo", ProjectPut: api.ProjectPut{Config: projectConfig}})
	}
	createNetwork := func(w http.ResponseWriter, r *http.Request) {
		var request api.NetworksPost
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeIncusError(w, http.StatusBadRequest, err.Error())
			return
		}
		if request.Name == occupied {
			writeIncusError(w, http.StatusConflict, "already exists")
			return
		}
		status := api.NetworkStatusPending
		if r.URL.Query().Get("target") == "" {
			status = api.NetworkStatusCreated
		}
		networks[request.Name] = api.Network{
			Name: request.Name, Type: request.Type, Status: status, NetworkPut: request.NetworkPut,
		}
		writeIncusSync(w, nil)
	}
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/1.0":
			writeIncusSync(
				w,
				api.Server{
					ServerUntrusted: api.ServerUntrusted{
						Auth:          "trusted",
						APIExtensions: []string{"network", "projects", "clustering"},
					},
				},
			)
		case r.URL.Path == "/1.0/projects/ac-demo":
			updateProject(w, r)
		case r.URL.Path == "/1.0/networks" && r.Method == http.MethodPost:
			createNetwork(w, r)
		case r.URL.Path == "/1.0/networks":
			writeIncusSync(w, slices.Collect(maps.Values(networks)))
		case strings.HasPrefix(r.URL.Path, "/1.0/networks/"):
			name := strings.TrimPrefix(r.URL.Path, "/1.0/networks/")
			if name == occupied && r.Method != http.MethodGet {
				changedOccupied = true
			}
			writeFixtureNetwork(w, networks, name)
		default:
			writeIncusError(w, http.StatusNotFound, "not found")
		}
	}))
	t.Cleanup(fixture.Close)
	sdk := connectFixture(t, fixture.URL)
	t.Cleanup(sdk.Disconnect)
	client := &Client{server: sdk, host: "lab01", pool: "data"}
	physical, err := client.createReservedBridge(
		t.Context(),
		occupied,
		"demo",
		"lan",
		compute.Network{Name: "lan", Kind: "bridge"},
		false,
	)
	require.NoError(t, err)
	assert.Regexp(t, `^ac[0-9a-f]{8}$`, physical)
	assert.NotEqual(t, occupied, physical)
	require.ErrorIs(t, client.checkBridgeOwnership(t.Context(), "demo", occupied), errBridgeCollision)
	mu.Lock()
	assert.False(t, changedOccupied)
	assert.Equal(t, "none", networks[physical].Config["ipv4.address"], "bare bridges must have no L3 address")
	assert.Equal(t, "false", networks[physical].Config["ipv4.nat"])
	assert.Equal(t, "none", networks[physical].Config["dns.mode"])
	assert.Equal(t, physical, projectConfig["restricted.networks.access"])
	// Reservations recover pending creations; active lookup must use network metadata.
	projectConfig[networkKey("lan")] = occupied
	mu.Unlock()
	resolved, err := client.resolvePhysical(t.Context(), "demo", "lan")
	require.NoError(t, err)
	assert.Equal(t, physical, resolved)
}

func writeFixtureNetwork(w http.ResponseWriter, networks map[string]api.Network, name string) {
	network, exists := networks[name]
	if !exists {
		writeIncusError(w, http.StatusNotFound, "not found")
		return
	}
	writeIncusSync(w, network)
}
