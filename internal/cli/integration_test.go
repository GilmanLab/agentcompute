//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/incus"
)

// TestClusterLifecycle exercises the production binary across an actual process death.
func TestClusterLifecycle(t *testing.T) {
	remote := os.Getenv("AGENTCOMPUTE_TEST_REMOTE")
	if remote == "" {
		t.Skip("set AGENTCOMPUTE_TEST_REMOTE to opt into disposable cluster resources")
	}
	host := os.Getenv("AGENTCOMPUTE_TEST_HOST")
	if host == "" {
		host = "lab01"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	dir := t.TempDir()
	binary := filepath.Join(dir, "agentcompute")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/agentcompute")
	build.Dir = root
	output, err := build.CombinedOutput()
	require.NoError(t, err, "%s", output)
	config := filepath.Join(dir, "config.yaml")
	text := fmt.Sprintf(
		"incus:\n  remote: %q\n  host: %q\n  pool: data\nimages_file: %q\n",
		remote,
		host,
		filepath.Join(root, "images", "catalog.yaml"),
	)
	require.NoError(t, os.WriteFile(config, []byte(text), 0o600))
	backend, err := incus.New(ctx, incus.Options{Remote: remote, Host: host, Pool: "data"})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backend.Close()) })

	first, process := startClusterClient(ctx, t, binary, config)
	createStarted := time.Now()
	sandbox := integrationExecute(ctx, t, first, `def main():
    return sandbox.create(ttl_minutes=1)
`)
	t.Logf("MCP sandbox creation: %s", time.Since(createStarted))
	name := sandbox["name"].(string)
	expires := sandbox["expires_at"].(string)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if _, getErr := backend.GetSandbox(cleanup, name); getErr == nil {
			assert.NoError(t, backend.DeleteSandbox(cleanup, name))
		}
	})
	createStarted = time.Now()
	created := integrationExecute(ctx, t, first, fmt.Sprintf(`def main():
    guest = instance.create(sandbox=%q, name="router", image="router", network="none")
    nic = net.attach(sandbox=%q, instance="router", network="default", nic="eth0")
    result = instance.exec(sandbox=%q, name="router", command="printf integration-ok")
    return {"guest": guest, "nic": nic, "exec": result, "observed": instance.get(sandbox=%q, name="router")}
`, name, name, name, name))
	t.Logf("MCP router creation + exec: %s", time.Since(createStarted))
	assert.Equal(t, "integration-ok", created["exec"].(map[string]any)["stdout"])
	observedNICs := created["observed"].(map[string]any)["nics"].([]any)
	require.Len(t, observedNICs, 1)
	assert.Equal(t, "default", observedNICs[0].(map[string]any)["network"])
	duplicate, err := first.CallTool(ctx, &mcp.CallToolParams{
		Name: "execute",
		Arguments: map[string]any{"source": fmt.Sprintf(`def main():
    return net.attach(sandbox=%q, instance="router", network="default", nic="eth0")
`, name)},
	})
	require.NoError(t, err)
	require.True(t, duplicate.IsError, "attaching an existing NIC must not replace it")
	networks, err := backend.ListNetworks(ctx, name)
	require.NoError(t, err)
	require.Len(t, networks, 1)
	physical := networks[0].PhysicalName
	require.NoError(t, process.Process.Kill())
	_ = first.Close()

	second, _ := startClusterClient(ctx, t, binary, config)
	listed := integrationExecute(ctx, t, second, `def main():
    return sandbox.list()
`)
	found := false
	for _, item := range listed["items"].([]any) {
		sb := item.(map[string]any)
		if sb["name"] == name {
			found = true
			assert.Equal(t, expires, sb["expires_at"])
		}
	}
	require.True(t, found, "restart must discover original project")

	execStarted := time.Now()
	noop := integrationExecute(ctx, t, second, fmt.Sprintf(`def main():
    return instance.exec(sandbox=%q, name="router", command="true")
`, name))
	assert.Zero(t, noop["exit_code"])
	t.Logf("MCP noop exec including worker launch: %s", time.Since(execStarted))

	streams := integrationExecute(ctx, t, second, fmt.Sprintf(`def main():
    return instance.exec(sandbox=%q, name="router", command="head -c 1048576 /dev/zero | tr '\\000' o; head -c 1048576 /dev/zero | tr '\\000' e >&2")
`, name))
	assert.Equal(t, true, streams["stdout_truncated"])
	assert.Equal(t, true, streams["stderr_truncated"])
	assert.Len(t, streams["stdout"].(string), 64*1024)
	assert.Len(t, streams["stderr"].(string), 64*1024)

	// No agent calls after this point: only independent Incus observations.
	expiry, err := time.Parse(time.RFC3339, expires)
	require.NoError(t, err)
	deadline := expiry.Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		projects, listErr := backend.ListSandboxes(ctx)
		require.NoError(t, listErr)
		present := false
		for _, sb := range projects {
			present = present || sb.Name == name
		}
		if !present {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	projects, err := backend.ListSandboxes(ctx)
	require.NoError(t, err)
	for _, sb := range projects {
		assert.NotEqual(t, name, sb.Name, "expired sandbox remained")
	}
	for _, member := range []string{"lab01", "nas01"} {
		nets, err := backend.Scoped(ctx, "default", member).GetNetworks()
		require.NoError(t, err)
		for _, network := range nets {
			assert.NotEqual(t, physical, network.Name, "bridge remained on %s", member)
		}
	}
}

func startClusterClient(ctx context.Context, t *testing.T, binary, config string) (*mcp.ClientSession, *exec.Cmd) {
	t.Helper()
	command := exec.CommandContext(ctx, binary, "stdio", "--config", config)
	command.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "integration", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	for _, name := range []string{"sandbox.create", "sandbox.list", "instance.create", "instance.get", "instance.exec", "net.attach"} {
		for _, tool := range []struct{ name, key string }{{"search_api", "query"}, {"describe_api", "name"}} {
			result, err := session.CallTool(
				ctx,
				&mcp.CallToolParams{Name: tool.name, Arguments: map[string]any{tool.key: name}},
			)
			require.NoError(t, err)
			require.False(t, result.IsError, "%v", result.Content)
		}
	}
	return session, command
}

func integrationExecute(ctx context.Context, t *testing.T, session *mcp.ClientSession, source string) map[string]any {
	t.Helper()
	result, err := session.CallTool(
		ctx,
		&mcp.CallToolParams{Name: "execute", Arguments: map[string]any{"source": source}},
	)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var envelope struct {
		Result map[string]any `json:"result"`
	}
	require.NoError(t, json.Unmarshal(data, &envelope))
	return envelope.Result
}
