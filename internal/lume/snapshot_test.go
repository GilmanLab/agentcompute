package lume

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestFailedSnapshotCleanupRemainsOwned(t *testing.T) {
	t.Parallel()
	for _, restore := range []bool{false, true} {
		name := "create"
		if restore {
			name = "restore"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const seed = "ac-seed-macos-tahoe-desktop"
			const unrelated = "ac-demo-unmapped"
			ref := compute.Ref{Sandbox: "demo", Name: "web"}
			fixture := &snapshotHost{
				vms: map[string]lumeVM{
					seed:          {Name: seed, OS: "macOS", Status: vmStatusStopped},
					unrelated:     {Name: unrelated, OS: "macOS", Status: vmStatusStopped},
					"ac-demo-web": {Name: "ac-demo-web", OS: "macOS", Status: vmStatusStopped},
				},
				failCleanup: true,
			}
			snapshots := map[string]*snapshotRecord{}
			if restore {
				fixture.vms["ac-demo-pre"] = lumeVM{Name: "ac-demo-pre", OS: "macOS", Status: vmStatusStopped}
				snapshots["pre"] = &snapshotRecord{VM: "ac-demo-pre", CreatedAt: time.Now()}
			}
			client := newTunneledClientWithHost(t, fixture, fixture.host)
			require.NoError(t, client.CreateSandbox(t.Context(), compute.Sandbox{
				Name: ref.Sandbox, Platform: "mac", ExpiresAt: time.Now().Add(time.Hour),
			}))
			require.NoError(t, client.updateSandbox(t.Context(), ref.Sandbox, func(rec *sandboxRecord) error {
				rec.Instances[ref.Name] = &instanceRecord{
					VM: "ac-demo-web", Seed: seed, Image: "macos/tahoe/desktop", Snapshots: snapshots,
				}
				return nil
			}))

			if restore {
				require.Error(t, client.RestoreSnapshot(t.Context(), ref, "pre"))
			} else {
				require.Error(t, client.CreateSnapshot(t.Context(), ref, "pre"))
			}
			fixture.mu.Lock()
			fixture.failCleanup = false
			fixture.mu.Unlock()

			if restore {
				var agentErr *codemode.AgentError
				require.ErrorAs(t, client.RestoreSnapshot(t.Context(), ref, "pre"), &agentErr)
			}
			// The same deletion used by the reaper must still find an uncertain
			// clone, but must not infer ownership from the shared ac-demo- prefix.
			require.NoError(t, client.DeleteSandbox(t.Context(), ref.Sandbox))
			assert.Equal(t, []string{unrelated, seed}, fixture.names())
		})
	}
}

type snapshotHost struct {
	mu          sync.Mutex
	vms         map[string]lumeVM
	failCleanup bool
}

func (h *snapshotHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/lume/vms/clone" {
		var request struct {
			Name    string `json:"name"`
			NewName string `json:"newName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		vm := h.vms[request.Name]
		vm.Name = request.NewName
		h.vms[request.NewName] = vm
		// An uncertain response after the VM was created, followed by a
		// deletion failure, must not discard the durable cleanup mapping.
		http.Error(w, "clone response lost", http.StatusInternalServerError)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/lume/vms/")
	vm, exists := h.vms[name]
	if !exists {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(vm)
	case http.MethodDelete:
		if h.failCleanup {
			http.Error(w, "host storage unavailable", http.StatusServiceUnavailable)
			return
		}
		delete(h.vms, name)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "unexpected operation", http.StatusBadRequest)
	}
}

func (h *snapshotHost) host(script string, _ io.Reader, stdout, _ io.Writer) (bool, error) {
	if !strings.Contains(script, quote(lumeBin)+" ls --format json") {
		return false, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	vms := make([]lumeVM, 0, len(h.vms))
	for _, vm := range h.vms {
		vms = append(vms, vm)
	}
	return true, json.NewEncoder(stdout).Encode(vms)
}

func (h *snapshotHost) names() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := make([]string, 0, len(h.vms))
	for name := range h.vms {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
