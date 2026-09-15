package lume

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestQuotePreservesShellMetacharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
	}{
		{name: "spaces", value: "hello world"},
		{name: "single quotes", value: "it's"},
		{name: "dollars and backticks", value: "$HOME `id` $(pwd)"},
		{name: "semicolons and pipes", value: "a; b | c && d"},
		{name: "globs", value: "* ? [a-z]"},
		{name: "empty", value: ""},
		{name: "newlines", value: "line1\nline2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command("sh", "-c", "printf %s "+quote(tt.value))
			out, err := cmd.Output()
			require.NoError(t, err, "quoted value must survive one shell parse")
			assert.Equal(t, tt.value, string(out))
		})
	}
}

func TestNewRejectsMissingOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "host",
			opts: Options{IdentityFile: "id", KnownHostsFile: "kh", GuestKnownHostsFile: "gkh"},
			want: "lume host is required",
		},
		{
			name: "identity",
			opts: Options{Host: "mac", KnownHostsFile: "kh", GuestKnownHostsFile: "gkh"},
			want: "lume identity file is required",
		},
		{
			name: "known hosts",
			opts: Options{Host: "mac", IdentityFile: "id", GuestKnownHostsFile: "gkh"},
			want: "lume known hosts file is required",
		},
		{
			name: "guest known hosts",
			opts: Options{Host: "mac", IdentityFile: "id", KnownHostsFile: "kh"},
			want: "lume guest known hosts file is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(t.Context(), tt.opts)
			require.EqualError(t, err, tt.want)
		})
	}
}

func TestAPITunnelJSONDeleteAndErrors(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotMethod, gotPath, gotBody string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(body)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/lume/vms/web":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"web","status":"running"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/lume/vms/web":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/lume/missing":
			http.Error(w, `{"message":"missing"}`, http.StatusNotFound)
		case r.URL.Path == "/lume/down":
			http.Error(w, `{"message":"overloaded"}`, http.StatusServiceUnavailable)
		case r.URL.Path == "/lume/bad":
			http.Error(w, `{"message":"no such vm"}`, http.StatusBadRequest)
		case r.URL.Path == "/lume/hang":
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	})
	client := newTunneledClient(t, handler)

	var vm struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	require.NoError(t, client.api(t.Context(), http.MethodGet, "/lume/vms/web", nil, &vm))
	assert.Equal(t, "web", vm.Name)
	assert.Equal(t, "running", vm.Status)

	require.NoError(t, client.api(t.Context(), http.MethodDelete, "/lume/vms/web", nil, &vm))
	mu.Lock()
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/lume/vms/web", gotPath)
	assert.Empty(t, gotBody)
	mu.Unlock()

	err := client.api(t.Context(), http.MethodGet, "/lume/missing", nil, &vm)
	require.ErrorIs(t, err, compute.ErrNotFound)

	err = client.api(t.Context(), http.MethodGet, "/lume/down", nil, &vm)
	require.ErrorIs(t, err, compute.ErrUnavailable)

	err = client.api(t.Context(), http.MethodGet, "/lume/bad", nil, &vm)
	require.Error(t, err)
	require.NotErrorIs(t, err, compute.ErrNotFound, "Lume 400 is not mapped to ErrNotFound")
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, err.Error(), "no such vm")
	assert.NotContains(t, err.Error(), "IdentityFile")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = client.api(ctx, http.MethodGet, "/lume/hang", nil, &vm)
	require.ErrorIs(t, err, context.Canceled)
}

func TestHostScriptStdinOversizeAndCancel(t *testing.T) {
	t.Parallel()
	client := newTunneledClient(t, http.NotFoundHandler())

	out, err := client.host(t.Context(), "cat", strings.NewReader("payload-from-stdin"))
	require.NoError(t, err)
	assert.Equal(t, "payload-from-stdin", string(out))

	_, err = client.host(t.Context(), "head -c 2000000 /dev/zero", nil)
	require.EqualError(t, err, "host command output exceeds limit")

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = client.host(ctx, "sleep 30", nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second, "cancel must not wait for the host command")
}

func TestHostShellUsesIsolatedHome(t *testing.T) {
	t.Parallel()
	client := newTunneledClient(t, http.NotFoundHandler())
	out, err := client.host(t.Context(), `printf %s "$HOME"`, nil)
	require.NoError(t, err)
	userHome, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.NotEqual(t, userHome, string(out))
	assert.NotEqual(t, os.Getenv("HOME"), string(out))
}

func TestCloseRemovesTempDir(t *testing.T) {
	t.Parallel()
	client := newTunneledClient(t, http.NotFoundHandler())
	dir := client.tmpDir
	require.DirExists(t, dir)
	require.NoError(t, client.Close())
	require.NoError(t, client.Close())
	assert.NoDirExists(t, dir)
	_, err := client.host(t.Context(), "true", nil)
	require.EqualError(t, err, "lume client is closed")
}

func TestGuestSSHConfigKeepsHostAndGuestIdentitiesApart(t *testing.T) {
	t.Parallel()
	cfg := guestSSHConfig(sshConfigParams{
		hostName:            "mac.example",
		hostPort:            "22",
		identityFile:        "/keys/host",
		knownHostsFile:      "/keys/host.known",
		guestName:           "192.168.64.12",
		guestPort:           "22",
		guestKey:            "/keys/guest",
		guestKnownHostsFile: "/keys/guest.known",
		seed:                "ac-seed-macos-tahoe-desktop",
	})
	assert.Contains(t, cfg, "Host ac-host")
	assert.Contains(t, cfg, "Host ac-guest")
	assert.Contains(t, cfg, `IdentityFile "/keys/host"`)
	assert.Contains(t, cfg, `IdentityFile "/keys/guest"`)
	assert.Contains(t, cfg, `UserKnownHostsFile "/keys/host.known"`)
	assert.Contains(t, cfg, `UserKnownHostsFile "/keys/guest.known"`)
	assert.Contains(t, cfg, `HostKeyAlias "ac-seed-macos-tahoe-desktop"`)
	assert.Contains(t, cfg, "ProxyJump ac-host")
	assert.Contains(t, cfg, "IdentityAgent none")
	assert.Contains(t, cfg, "ForwardAgent no")
	assert.Contains(t, cfg, "ForwardX11 no")
	assert.Contains(t, cfg, "BatchMode yes")
	assert.Contains(t, cfg, "StrictHostKeyChecking yes")
	assert.NotContains(t, cfg, "ForwardAgent yes")
	assert.NotContains(t, cfg, "lume ssh")
}

func TestHostCallbackRunsBeforeShell(t *testing.T) {
	t.Parallel()
	client := newTunneledClientWithHost(
		t,
		http.NotFoundHandler(),
		func(script string, _ io.Reader, stdout io.Writer, _ io.Writer) (bool, error) {
			if strings.Contains(script, "lume ls") {
				_, _ = io.WriteString(stdout, `[{"name":"fake","os":"macOS","status":"running"}]`)
				return true, nil
			}
			return false, nil
		},
	)
	out, err := client.host(t.Context(), "/usr/local/bin/lume ls --format json", nil)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"name":"fake"`)

	out, err = client.host(t.Context(), `printf %s "$HOME"`, nil)
	require.NoError(t, err)
	userHome, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.NotEqual(t, userHome, string(out))
}
