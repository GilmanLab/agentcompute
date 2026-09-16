package lume

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestNewRefusesHostsThatCannotDisableVNC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts tunnelOpts
	}{
		{
			// Lume 0.5.3 ships --vnc-port and --vnc-password, so a flag scan
			// that stops at the "--vnc" prefix would pass this installation.
			name: "released build without the run policy option",
			opts: tunnelOpts{RunHelp: vncLegacyRunHelp},
		},
		{
			// The binary on disk can be the source build while launchd still
			// serves the old daemon, which drops the unknown key and accepts.
			name: "daemon that ignores the run policy",
			opts: tunnelOpts{LegacyDaemon: true},
		},
		{
			name: "account-local executable missing",
			opts: tunnelOpts{Host: func(script string, _ io.Reader, _, stderr io.Writer) (bool, error) {
				if !strings.Contains(script, "run --help") {
					return false, nil
				}
				_, _ = io.WriteString(stderr, "not installed or not executable")
				return true, errors.New("exit status 1")
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := dialTunnel(t, tt.opts)
			require.Error(t, err)
		})
	}
}

func TestStartupProbesTheDaemonWithoutTouchingMappedVMs(t *testing.T) {
	t.Parallel()
	const seed = "ac-seed-macos-tahoe-desktop"
	fixture := &lifecycleHost{vms: map[string]lumeVM{seed: seedVM()}}
	state := startTunnel(t, tunnelOpts{API: fixture, Host: fixture.host})

	probes := state.daemon.probeCalls()
	require.Len(t, probes, 1)
	// A disabled policy asked for alongside a display is rejected while the
	// request is decoded, so the pinned daemon answers without reaching any VM.
	assert.False(t, probes[0].NoDisplay)
	assert.Equal(t, vncPolicyDisabled, probes[0].Policy)
	assert.NotContains(t, probes[0].VM, ":", "a colon would send 0.5.3 down its image auto-pull path")
	assert.NotContains(t, fixture.names(), probes[0].VM)
	assert.Empty(t, fixture.apiCalls(), "the probe must not reach a real VM route")
	assert.Empty(t, fixture.runCalls())
}

func TestEveryStartSendsTheDisabledRunPolicy(t *testing.T) {
	t.Parallel()
	const seed = "ac-seed-macos-tahoe-desktop"
	const vmName = "ac-demo-web"
	ref := compute.Ref{Sandbox: "demo", Name: "web"}

	tests := []struct {
		name string
		// cancelOnRun stops the readiness wait that follows a start, which
		// needs a live guest this fixture cannot provide.
		cancelOnRun bool
		start       func(ctx context.Context, client *Client) error
	}{
		{
			name: "create",
			start: func(ctx context.Context, client *Client) error {
				_, err := client.BeginCreateInstance(ctx, compute.CreateInstance{
					Ref:   ref,
					Start: true,
					Image: compute.CatalogImage{Name: "macos/tahoe/desktop", Seed: seed},
					CPUs:  4, MemoryMB: 8192, DiskGB: 100,
				})
				return err
			},
		},
		{
			name:        "start",
			cancelOnRun: true,
			start: func(ctx context.Context, client *Client) error {
				_, err := client.StartInstance(ctx, ref, false)
				return err
			},
		},
		{
			name:        "restart",
			cancelOnRun: true,
			start: func(ctx context.Context, client *Client) error {
				_, err := client.RestartInstance(ctx, ref, false)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := &lifecycleHost{vms: map[string]lumeVM{seed: seedVM()}}
			if tt.name != "create" {
				fixture.vms[vmName] = lumeVM{
					Name: vmName, OS: "macOS", Status: vmStatusStopped,
					CPUCount: 4, MemorySize: 8 << 30, DiskSize: lumeDisk{Total: 100 << 30},
				}
			}
			state := startTunnel(t, tunnelOpts{API: fixture, Host: fixture.host})
			client := state.client
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancelOnRun {
				fixture.onRun = cancel
			}
			require.NoError(t, client.CreateSandbox(ctx, compute.Sandbox{
				Name: "demo", Platform: platformMac, ExpiresAt: time.Now().Add(time.Hour),
			}))
			if tt.name != "create" {
				require.NoError(t, client.updateSandbox(ctx, "demo", func(rec *sandboxRecord) error {
					rec.Instances["web"] = &instanceRecord{
						VM: vmName, Seed: seed, Prepared: true, CPUs: 4, MemoryMB: 8192, DiskGB: 100,
					}
					return nil
				}))
			}

			err := tt.start(ctx, client)
			if tt.cancelOnRun {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}

			runs := fixture.runCalls()
			require.Len(t, runs, 1)
			assert.Equal(t, vmName, runs[0].VM)
			assert.Equal(t, vncPolicyDisabled, runs[0].Policy)
			// A display would make Lume reject the disabled policy outright.
			assert.True(t, runs[0].NoDisplay)
		})
	}
}

func TestStartStopsAGuestThatKeptItsVNCListener(t *testing.T) {
	t.Parallel()
	const seed = "ac-seed-macos-tahoe-desktop"
	fixture := &lifecycleHost{
		vms:      map[string]lumeVM{seed: seedVM()},
		vncOnRun: "vnc://:secret@127.0.0.1:52397",
	}
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
	require.Error(t, err)

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	assert.Equal(t, vmStatusStopped, fixture.vms["ac-demo-web"].Status,
		"an exposed guest must not be left running")
}
