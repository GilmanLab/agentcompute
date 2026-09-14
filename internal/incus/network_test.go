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

func TestForwardAllocationHandlesCompetingClaims(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode string
	}{
		{name: "retries after a confirmed collision", mode: "collision"},
		{name: "does not duplicate after an uncertain commit", mode: "uncertain commit"},
		{name: "does not retry an unrelated failure", mode: "unrelated failure"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newForwardClaimState(tt.mode)
			client := newForwardClaimClient(t, state)
			result, err := client.CreateForward(
				t.Context(),
				"demo",
				"lan",
				compute.Ref{Sandbox: "demo", Name: "web"},
				80,
				8080,
				"tcp",
			)
			assertForwardClaimResult(t, tt.mode, result, err)
			assertForwardClaimRetention(t, state)
		})
	}
}

type forwardClaimState struct {
	mu        sync.Mutex
	mode      string
	occupied  bool
	attempted bool
	forwards  []api.NetworkForward
	network   api.Network
}

func newForwardClaimState(mode string) *forwardClaimState {
	return &forwardClaimState{
		mode: mode,
		network: api.Network{
			Name: "lan", Type: networkKindOVN, Status: api.NetworkStatusCreated,
			NetworkPut: api.NetworkPut{Config: map[string]string{
				metaSandbox:    "demo",
				metaName:       "lan",
				metaVersion:    versionValue,
				ipv4AddressKey: "192.168.82.1/24",
				ipv4NATKey:     configTrue,
			}},
		},
	}
}

func newForwardClaimClient(t *testing.T, state *forwardClaimState) *Client {
	t.Helper()
	fixture := httptest.NewTLSServer(state.handler(t))
	t.Cleanup(fixture.Close)
	sdk := connectFixture(t, fixture.URL)
	t.Cleanup(sdk.Disconnect)
	return &Client{server: sdk, ovnRanges: "10.10.40.64-10.10.40.65"}
}

func (s *forwardClaimState) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/1.0":
			writeIncusSync(w, api.Server{ServerUntrusted: api.ServerUntrusted{
				Auth: "trusted",
				APIExtensions: []string{
					"projects",
					"instances",
					"instance_get_full",
					"network",
					"networks_all_projects",
					"network_forward",
					"network_allocations",
				},
			}})
		case "/1.0/projects/ac-demo":
			writeIncusSync(w, api.Project{Name: "ac-demo", ProjectPut: api.ProjectPut{
				Config: map[string]string{metaVersion: versionValue, featuresNetworksKey: configTrue},
			}})
		case "/1.0/instances/web":
			writeIncusSync(w, forwardClaimInstance())
		case "/1.0/networks/lan":
			writeIncusSync(w, s.network)
		case "/1.0/networks":
			writeIncusSync(w, []api.Network{s.network})
		case "/1.0/network-allocations":
			s.writeAllocations(w)
		case "/1.0/networks/lan/forwards":
			s.handleForwards(w, r)
		default:
			t.Errorf("unhandled fixture request: %s %s", r.Method, r.URL)
			writeIncusError(w, http.StatusNotFound, "not found")
		}
	})
}

func forwardClaimInstance() api.InstanceFull {
	return api.InstanceFull{
		Instance: api.Instance{Name: "web", InstancePut: api.InstancePut{
			Devices: map[string]map[string]string{
				"eth0": {deviceTypeKey: deviceTypeNIC, deviceNetworkKey: "lan"},
			},
		}},
		State: &api.InstanceState{Network: map[string]api.InstanceStateNetwork{
			"eth0": {
				Addresses: []api.InstanceStateNetworkAddress{
					{Address: "192.168.82.2", Family: "inet", Scope: "global"},
				},
			},
		}},
	}
}

func (s *forwardClaimState) writeAllocations(w http.ResponseWriter) {
	var allocations []api.NetworkAllocations
	if s.occupied {
		allocations = append(allocations, api.NetworkAllocations{Address: "10.10.40.64/32"})
	}
	writeIncusSync(w, allocations)
}

func (s *forwardClaimState) handleForwards(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeIncusSync(w, s.forwards)
		return
	}
	var request api.NetworkForwardsPost
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeIncusError(w, http.StatusBadRequest, err.Error())
		return
	}
	created := api.NetworkForward{
		ListenAddress: request.ListenAddress, NetworkForwardPut: request.NetworkForwardPut,
	}
	if !s.attempted {
		s.attempted = true
		s.occupied = s.mode != "unrelated failure"
		if s.mode == "uncertain commit" {
			s.forwards = append(s.forwards, created)
		}
		writeIncusError(w, http.StatusInternalServerError, "create failed")
		return
	}
	s.forwards = append(s.forwards, created)
	writeIncusSync(w, nil)
}

func assertForwardClaimResult(t *testing.T, mode string, result compute.Forward, err error) {
	t.Helper()
	if mode == "collision" {
		require.NoError(t, err)
		assert.Equal(t, "10.10.40.65", result.Address)
		return
	}
	require.Error(t, err)
	assert.True(t, api.StatusErrorCheck(err, http.StatusInternalServerError))
}

func assertForwardClaimRetention(t *testing.T, state *forwardClaimState) {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.mode == "unrelated failure" {
		assert.Empty(t, state.forwards)
		return
	}
	require.Len(t, state.forwards, 1, "an uncertain commit must not create another forward")
}
