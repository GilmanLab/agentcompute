package lume

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

type lifecycleHost struct {
	mu    sync.Mutex
	vms   map[string]lumeVM
	calls []string
}

func (h *lifecycleHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/lume/vms/clone":
		var req struct {
			Name    string `json:"name"`
			NewName string `json:"newName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		src := h.vms[req.Name]
		h.vms[req.NewName] = lumeVM{
			Name:       req.NewName,
			OS:         "macOS",
			Status:     vmStatusStopped,
			CPUCount:   src.CPUCount,
			MemorySize: src.MemorySize,
			DiskSize:   src.DiskSize,
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "cloned"})
		return
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/lume/vms/"):
		h.patchVM(w, r)
		return
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/lume/vms/"):
		name := strings.TrimPrefix(r.URL.Path, "/lume/vms/")
		delete(h.vms, name)
		w.WriteHeader(http.StatusOK)
		return
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
		name := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/stop"), "/lume/vms/")
		vm := h.vms[name]
		vm.Status = vmStatusStopped
		h.vms[name] = vm
		_ = json.NewEncoder(w).Encode(map[string]string{"message": vmStatusStopped})
		return
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/lume/vms/"):
		name := strings.TrimPrefix(r.URL.Path, "/lume/vms/")
		vm, ok := h.vms[name]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(vm)
		return
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}
}

func (h *lifecycleHost) patchVM(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/lume/vms/")
	vm := h.vms[name]
	if size, ok := body["diskSize"].(string); ok {
		gb, err := strconv.ParseUint(strings.TrimSuffix(size, "GB"), 10, 64)
		if err != nil || gb*bytesPerGiB <= vm.DiskSize.Total {
			http.Error(w, "disk resize must strictly increase capacity", http.StatusBadRequest)
			return
		}
		vm.DiskSize.Total = gb * bytesPerGiB
	}
	if cpu, ok := body["cpu"].(float64); ok {
		vm.CPUCount = int64(cpu)
	}
	if memory, ok := body["memory"].(string); ok {
		var mb uint64
		_, _ = fmt.Sscanf(memory, "%dMB", &mb)
		vm.MemorySize = mb * bytesPerMiB
	}
	h.vms[name] = vm
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "updated"})
}

func (h *lifecycleHost) host(script string, _ io.Reader, stdout, _ io.Writer) (bool, error) {
	if strings.Contains(script, "/usr/local/bin/lume ls --format json") {
		h.mu.Lock()
		defer h.mu.Unlock()
		vms := make([]lumeVM, 0, len(h.vms))
		for _, vm := range h.vms {
			vms = append(vms, vm)
		}
		return true, json.NewEncoder(stdout).Encode(vms)
	}
	if strings.Contains(script, "machineIdentifier") {
		return true, nil
	}
	return false, nil
}

func (h *lifecycleHost) apiCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slicesCopy(h.calls)
}

func (h *lifecycleHost) names() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := make([]string, 0, len(h.vms))
	for name := range h.vms {
		names = append(names, name)
	}
	return names
}

func slicesCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func seedVM() lumeVM {
	return lumeVM{
		Name:       "ac-seed-macos-tahoe-desktop",
		OS:         "macOS",
		Status:     vmStatusStopped,
		CPUCount:   4,
		MemorySize: 8 << 30,
		DiskSize:   lumeDisk{Total: 100 << 30},
	}
}

func TestStartRefusesThirdGuestBeforeLumeAPI(t *testing.T) {
	const seed = "ac-seed-macos-tahoe-desktop"
	fixture := &lifecycleHost{vms: map[string]lumeVM{
		"owner-one":   {Name: "owner-one", OS: "macOS", Status: vmStatusRunning},
		"owner-two":   {Name: "owner-two", OS: "macOS", Status: vmStatusRunning},
		"ac-demo-web": {Name: "ac-demo-web", OS: "macOS", Status: vmStatusStopped},
		seed:          seedVM(),
	}}
	client := newTunneledClientWithHost(t, fixture, fixture.host)
	require.NoError(t, client.CreateSandbox(t.Context(), compute.Sandbox{
		Name: "demo", Platform: platformMac, ExpiresAt: time.Now().Add(time.Hour),
	}))
	require.NoError(t, client.updateSandbox(t.Context(), "demo", func(rec *sandboxRecord) error {
		rec.Instances["web"] = &instanceRecord{
			VM: "ac-demo-web", Seed: seed, Prepared: true, CPUs: 4, MemoryMB: 8192, DiskGB: 100,
		}
		return nil
	}))
	_, err := client.StartInstance(t.Context(), compute.Ref{Sandbox: "demo", Name: "web"}, false)
	requireAgentMessage(t, err, errLimitOwned)
	assert.Empty(t, fixture.apiCalls(), "capacity refusal must happen before any Lume HTTP call")
}

func TestBeginCreateRefusesThirdGuestBeforeClone(t *testing.T) {
	const seed = "ac-seed-macos-tahoe-desktop"
	fixture := &lifecycleHost{vms: map[string]lumeVM{
		"owner-one": {Name: "owner-one", OS: "macOS", Status: vmStatusRunning},
		"owner-two": {Name: "owner-two", OS: "macOS", Status: vmStatusRunning},
		seed:        seedVM(),
	}}
	client := newTunneledClientWithHost(t, fixture, fixture.host)
	require.NoError(t, client.CreateSandbox(t.Context(), compute.Sandbox{
		Name: "demo", Platform: platformMac, ExpiresAt: time.Now().Add(time.Hour),
	}))
	_, err := client.BeginCreateInstance(t.Context(), compute.CreateInstance{
		Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
		Start: true,
		Image: compute.CatalogImage{Name: "macos/tahoe/desktop", Seed: seed},
		CPUs:  4, MemoryMB: 8192, DiskGB: 100,
	})
	requireAgentMessage(t, err, errLimitOwned)
	assert.Empty(t, fixture.apiCalls(), "third running guest must be refused before clone")
}

func TestCreateAppliesResourcesAndRejectsShrink(t *testing.T) {
	const seed = "ac-seed-macos-tahoe-desktop"
	for _, diskGB := range []int64{100, 120} {
		t.Run(fmt.Sprintf("clone with %dGiB disk", diskGB), func(t *testing.T) {
			fixture := &lifecycleHost{vms: map[string]lumeVM{seed: seedVM()}}
			client := newTunneledClientWithHost(t, fixture, fixture.host)
			require.NoError(t, client.CreateSandbox(t.Context(), compute.Sandbox{
				Name: "demo", Platform: platformMac, ExpiresAt: time.Now().Add(time.Hour),
			}))
			pending, err := client.BeginCreateInstance(t.Context(), compute.CreateInstance{
				Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
				Image: compute.CatalogImage{Name: "macos/tahoe/desktop", Seed: seed},
				CPUs:  6, MemoryMB: 12288, DiskGB: diskGB,
			})
			require.NoError(t, err)
			inst, err := pending.Wait(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "Stopped", inst.Status)
			assert.EqualValues(t, 6, inst.CPUs)
			assert.EqualValues(t, 12288, inst.MemoryMB)
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			assert.EqualValues(t, diskGB*bytesPerGiB, fixture.vms["ac-demo-web"].DiskSize.Total)
		})
	}
	t.Run("rejects a shrink before any Lume HTTP call", func(t *testing.T) {
		fixture := &lifecycleHost{vms: map[string]lumeVM{seed: seedVM()}}
		client := newTunneledClientWithHost(t, fixture, fixture.host)
		require.NoError(t, client.CreateSandbox(t.Context(), compute.Sandbox{
			Name: "demo", Platform: platformMac, ExpiresAt: time.Now().Add(time.Hour),
		}))
		_, err := client.BeginCreateInstance(t.Context(), compute.CreateInstance{
			Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
			Image: compute.CatalogImage{Name: "macos/tahoe/desktop", Seed: seed},
			CPUs:  4, MemoryMB: 8192, DiskGB: 40,
		})
		require.Error(t, err)
		var agent *codemode.AgentError
		require.ErrorAs(t, err, &agent)
		assert.Contains(t, agent.Message, "shrink")
		assert.Empty(t, fixture.apiCalls())
	})
}

func TestDeleteSandboxDeletesExactMappedVMsOnly(t *testing.T) {
	const seed = "ac-seed-macos-tahoe-desktop"
	const extra = "ac-demo-web-extra"
	const other = "ac-demo-other"
	fixture := &lifecycleHost{vms: map[string]lumeVM{
		"ac-demo-web": {Name: "ac-demo-web", OS: "macOS", Status: vmStatusStopped},
		extra:         {Name: extra, OS: "macOS", Status: vmStatusStopped},
		other:         {Name: other, OS: "macOS", Status: vmStatusStopped},
		seed:          seedVM(),
	}}
	client := newTunneledClientWithHost(t, fixture, fixture.host)
	require.NoError(t, client.CreateSandbox(t.Context(), compute.Sandbox{
		Name: "demo", Platform: platformMac, ExpiresAt: time.Now().Add(time.Hour),
	}))
	require.NoError(t, client.updateSandbox(t.Context(), "demo", func(rec *sandboxRecord) error {
		rec.Instances["web"] = &instanceRecord{VM: "ac-demo-web", Seed: seed, Prepared: true}
		return nil
	}))
	require.NoError(t, client.DeleteSandbox(t.Context(), "demo"))
	assert.ElementsMatch(t, []string{extra, other, seed}, fixture.names())
	boxes, err := client.ListSandboxes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, boxes)
}

func TestStartRefusesUnpreparedMappingWithoutClone(t *testing.T) {
	const seed = "ac-seed-macos-tahoe-desktop"
	fixture := &lifecycleHost{vms: map[string]lumeVM{seed: seedVM()}}
	client := newTunneledClientWithHost(t, fixture, fixture.host)
	require.NoError(t, client.CreateSandbox(t.Context(), compute.Sandbox{
		Name: "demo", Platform: platformMac, ExpiresAt: time.Now().Add(time.Hour),
	}))
	require.NoError(t, client.updateSandbox(t.Context(), "demo", func(rec *sandboxRecord) error {
		rec.Instances["web"] = &instanceRecord{
			VM: "ac-demo-web", Seed: seed, Prepared: false, CPUs: 4, MemoryMB: 8192, DiskGB: 100,
		}
		return nil
	}))
	_, err := client.StartInstance(t.Context(), compute.Ref{Sandbox: "demo", Name: "web"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not prepared")
	for _, call := range fixture.apiCalls() {
		assert.False(t, strings.HasSuffix(call, "/run"))
	}
}

func TestLiveLumeLifecycle(t *testing.T) {
	if os.Getenv("AGENTCOMPUTE_TEST_LUME") == "" {
		t.Skip("AGENTCOMPUTE_TEST_LUME not set")
	}
	host := requireLiveEnv(t, "AGENTCOMPUTE_TEST_LUME_HOST")
	identity := requireLiveEnv(t, "AGENTCOMPUTE_TEST_LUME_IDENTITY_FILE")
	knownHosts := requireLiveEnv(t, "AGENTCOMPUTE_TEST_LUME_KNOWN_HOSTS")
	guestKnownHosts := requireLiveEnv(t, "AGENTCOMPUTE_TEST_LUME_GUEST_KNOWN_HOSTS")
	guestKey := requireLiveEnv(t, "AGENTCOMPUTE_TEST_LUME_GUEST_KEY")
	imageName := os.Getenv("AGENTCOMPUTE_TEST_LUME_IMAGE")
	if imageName == "" {
		imageName = "macos/tahoe/desktop"
	}
	seed := os.Getenv("AGENTCOMPUTE_TEST_LUME_SEED")
	if seed == "" {
		seed = "ac-seed-macos-tahoe-desktop"
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	client, err := New(ctx, Options{
		Host:                host,
		IdentityFile:        identity,
		KnownHostsFile:      knownHosts,
		GuestKnownHostsFile: guestKnownHosts,
		GuestKeys:           map[string]string{imageName: guestKey},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	id, err := randomID()
	require.NoError(t, err)
	sandbox := "p8t" + id
	now := time.Now().UTC()
	require.NoError(t, client.CreateSandbox(ctx, compute.Sandbox{
		Name: sandbox, Platform: platformMac, Subject: "lume-lifecycle-test",
		Host: host, CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	}))
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanCancel()
		_ = client.DeleteSandbox(cleanCtx, sandbox)
	})

	got, err := client.GetSandbox(ctx, sandbox)
	require.NoError(t, err)
	assert.Equal(t, sandbox, got.Name)
	nets, err := client.ListNetworks(ctx, sandbox)
	require.NoError(t, err)
	assert.Empty(t, nets)

	ref := compute.Ref{Sandbox: sandbox, Name: "web"}
	pending, err := client.BeginCreateInstance(ctx, compute.CreateInstance{
		Ref: ref,
		Image: compute.CatalogImage{
			Name: imageName, OS: osMacOS, Kind: kindVM, Seed: seed, Desktop: true,
			CPUs: 4, MemoryMB: 8192, DiskGB: 100,
		},
		Kind: kindVM, CPUs: 4, MemoryMB: 8192, DiskGB: 100, Start: true,
	})
	require.NoError(t, err)
	inst, err := pending.Wait(ctx)
	require.NoError(t, err)
	assert.Equal(t, "Running", inst.Status)
	var stdout bytes.Buffer
	code, err := client.Exec(ctx, compute.ExecRequest{
		Ref: ref, Argv: []string{"/usr/bin/sw_vers", "-productName"},
	}, &stdout, io.Discard)
	require.NoError(t, err)
	require.Zero(t, code)
	assert.Equal(t, "macOS\n", stdout.String())
}

func requireLiveEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	require.NotEmpty(t, value, "missing %s", key)
	return value
}

func requireAgentMessage(t *testing.T, err error, message string) {
	t.Helper()
	require.Error(t, err)
	var agent *codemode.AgentError
	require.ErrorAs(t, err, &agent)
	assert.Equal(t, message, agent.Message)
}
