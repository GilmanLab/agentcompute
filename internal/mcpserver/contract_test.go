package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/codemode"
)

type fieldShape struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

func TestCapabilityCatalogContract(t *testing.T) {
	t.Parallel()

	for _, tt := range capabilityContracts() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			session := newClientSession(t, Options{})
			ctx := context.Background()

			searched, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "search_api",
				Arguments: map[string]any{"query": tt.name},
			})
			require.NoError(t, err, "search_api")
			requireSuccessfulTool(t, searched)
			var search codemode.SearchResponse
			decodeStructured(t, searched, &search)
			require.NotEmpty(t, search.Results, "search_api must find %s", tt.name)
			assert.Equal(t, tt.name, search.Results[0].Name)
			assert.Equal(t, tt.signature, search.Results[0].Signature)

			described, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "describe_api",
				Arguments: map[string]any{"name": tt.name},
			})
			require.NoError(t, err, "describe_api")
			requireSuccessfulTool(t, described)
			var description codemode.Description
			decodeStructured(t, described, &description)
			assert.Equal(t, tt.name, description.Name)
			assert.Equal(t, tt.signature, description.Signature)
			assert.Equal(t, tt.input, decodeShapes(t, description.Input))
			assert.Equal(t, tt.output, decodeShapes(t, description.Output))
		})
	}
}

type capabilityContract struct {
	name      string
	signature string
	input     []fieldShape
	output    []fieldShape
}

func capabilityContracts() []capabilityContract {
	networkType := "{name: str, kind: str, cidr: str, gateway: str}"
	instanceListItemType := "{name: str, kind: str, image: str, status: str, addresses: dict[str, list[str]]}"
	snapshotItemType := "{name: str, created_at: str}"

	return []capabilityContract{
		{
			name:      capabilitySandboxCreate,
			signature: "sandbox.create(*, name: str | None, platform: str | None, ttl_minutes: int | None)",
			input: []fieldShape{
				{Name: "name", Type: "str | None"},
				{Name: "platform", Type: "str | None"},
				{Name: "ttl_minutes", Type: "int | None"},
			},
			output: []fieldShape{
				{Name: "name", Type: "str", Required: true},
				{Name: "platform", Type: "str", Required: true},
				{Name: "expires_at", Type: "str", Required: true},
				{Name: "network", Type: networkType, Required: true},
			},
		},
		{
			name:      capabilitySandboxList,
			signature: "sandbox.list()",
			input:     []fieldShape{},
			output: []fieldShape{
				{
					Name:     "items",
					Type:     "list[{name: str, platform: str, created_at: str, expires_at: str, instances: int}]",
					Required: true,
				},
			},
		},
		{
			name:      capabilitySandboxGet,
			signature: "sandbox.get(*, name: str)",
			input:     []fieldShape{{Name: "name", Type: "str", Required: true}},
			output: []fieldShape{
				{Name: "name", Type: "str", Required: true},
				{Name: "platform", Type: "str", Required: true},
				{Name: "created_at", Type: "str", Required: true},
				{Name: "expires_at", Type: "str", Required: true},
				{Name: "instances", Type: "list[" + instanceListItemType + "]", Required: true},
				{Name: "networks", Type: "list[" + networkType + "]", Required: true},
			},
		},
		{
			name:      capabilitySandboxExtend,
			signature: "sandbox.extend(*, name: str, ttl_minutes: int)",
			input: []fieldShape{
				{Name: "name", Type: "str", Required: true},
				{Name: "ttl_minutes", Type: "int", Required: true},
			},
			output: []fieldShape{{Name: "expires_at", Type: "str", Required: true}},
		},
		{
			name:      capabilitySandboxDelete,
			signature: "sandbox.delete(*, name: str)",
			input:     []fieldShape{{Name: "name", Type: "str", Required: true}},
			output:    []fieldShape{},
		},
		{
			name:      capabilityImageList,
			signature: "image.list(*, os: str | None, desktop: bool | None, platform: str | None)",
			input: []fieldShape{
				{Name: "os", Type: "str | None"},
				{Name: "desktop", Type: "bool | None"},
				{Name: "platform", Type: "str | None"},
			},
			output: []fieldShape{
				{
					Name:     "items",
					Type:     "list[{name: str, os: str, version: str, kind: str, desktop: bool, platform: str, description: str}]",
					Required: true,
				},
			},
		},
		{
			name:      capabilityInstanceCreate,
			signature: "instance.create(*, sandbox: str, name: str, image: str, kind: str | None, cpus: int | None, memory_mb: int | None, disk_gb: int | None, network: str | None, host: str | None, start: bool | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "image", Type: "str", Required: true},
				{Name: "kind", Type: "str | None"},
				{Name: "cpus", Type: "int | None"},
				{Name: "memory_mb", Type: "int | None"},
				{Name: "disk_gb", Type: "int | None"},
				{Name: "network", Type: "str | None"},
				{Name: "host", Type: "str | None"},
				{Name: "start", Type: "bool | None"},
			},
			output: []fieldShape{
				{Name: "name", Type: "str", Required: true},
				{Name: "kind", Type: "str", Required: true},
				{Name: "host", Type: "str", Required: true},
				{Name: "status", Type: "str", Required: true},
				{Name: "addresses", Type: "dict[str, list[str]]", Required: true},
			},
		},
		{
			name:      capabilityInstanceList,
			signature: "instance.list(*, sandbox: str)",
			input:     []fieldShape{{Name: "sandbox", Type: "str", Required: true}},
			output: []fieldShape{
				{Name: "items", Type: "list[" + instanceListItemType + "]", Required: true},
			},
		},
		{
			name:      capabilityInstanceGet,
			signature: "instance.get(*, sandbox: str, name: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
			},
			output: []fieldShape{
				{Name: "name", Type: "str", Required: true},
				{Name: "kind", Type: "str", Required: true},
				{Name: "image", Type: "str", Required: true},
				{Name: "status", Type: "str", Required: true},
				{Name: "cpus", Type: "int", Required: true},
				{Name: "memory_mb", Type: "int", Required: true},
				{Name: "nics", Type: "list[{name: str, network: str, mac: str, addresses: list[str]}]", Required: true},
				{Name: "desktop", Type: "bool", Required: true},
				{Name: "snapshots", Type: "list[str]", Required: true},
			},
		},
		{
			name:      capabilityInstanceDelete,
			signature: "instance.delete(*, sandbox: str, name: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
			},
			output: []fieldShape{},
		},
		{
			name:      capabilityInstanceExec,
			signature: "instance.exec(*, sandbox: str, name: str, command: str, timeout_seconds: int | None, user: str | None, cwd: str | None, stdin: str | None, env: str | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "command", Type: "str", Required: true},
				{Name: "timeout_seconds", Type: "int | None"},
				{Name: "user", Type: "str | None"},
				{Name: "cwd", Type: "str | None"},
				{Name: "stdin", Type: "str | None"},
				{Name: "env", Type: "str | None"},
			},
			output: []fieldShape{
				{Name: "exit_code", Type: "int", Required: true},
				{Name: "stdout", Type: "str", Required: true},
				{Name: "stderr", Type: "str", Required: true},
				{Name: "stdout_truncated", Type: "bool", Required: true},
				{Name: "stderr_truncated", Type: "bool", Required: true},
				{Name: "timed_out", Type: "bool", Required: true},
			},
		},
		{
			name:      capabilityInstanceStart,
			signature: "instance.start(*, sandbox: str, name: str, force: bool | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "force", Type: "bool | None"},
			},
			output: []fieldShape{{Name: "status", Type: "str", Required: true}},
		},
		{
			name:      capabilityInstanceStop,
			signature: "instance.stop(*, sandbox: str, name: str, force: bool | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "force", Type: "bool | None"},
			},
			output: []fieldShape{{Name: "status", Type: "str", Required: true}},
		},
		{
			name:      capabilityInstanceRestart,
			signature: "instance.restart(*, sandbox: str, name: str, force: bool | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "force", Type: "bool | None"},
			},
			output: []fieldShape{{Name: "status", Type: "str", Required: true}},
		},
		{
			name:      capabilityInstanceWait,
			signature: "instance.wait(*, sandbox: str, name: str, until: str, timeout_seconds: int | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "until", Type: "str", Required: true},
				{Name: "timeout_seconds", Type: "int | None"},
			},
			output: []fieldShape{
				{Name: "status", Type: "str", Required: true},
				{Name: "elapsed_seconds", Type: "int", Required: true},
			},
		},
		{
			name:      capabilityInstanceFileRead,
			signature: "instance.file.read(*, sandbox: str, name: str, path: str, max_bytes: int | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "path", Type: "str", Required: true},
				{Name: "max_bytes", Type: "int | None"},
			},
			output: []fieldShape{
				{Name: "content", Type: "str", Required: true},
				{Name: "truncated", Type: "bool", Required: true},
			},
		},
		{
			name:      capabilityInstanceFileWrite,
			signature: "instance.file.write(*, sandbox: str, name: str, path: str, content: str, mode: str | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "path", Type: "str", Required: true},
				{Name: "content", Type: "str", Required: true},
				{Name: "mode", Type: "str | None"},
			},
			output: []fieldShape{{Name: "bytes", Type: "int", Required: true}},
		},
		{
			name:      capabilityInstanceSnapshotCreate,
			signature: "instance.snapshot.create(*, sandbox: str, name: str, snapshot: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "snapshot", Type: "str", Required: true},
			},
			output: []fieldShape{},
		},
		{
			name:      capabilityInstanceSnapshotRestore,
			signature: "instance.snapshot.restore(*, sandbox: str, name: str, snapshot: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "snapshot", Type: "str", Required: true},
			},
			output: []fieldShape{},
		},
		{
			name:      capabilityInstanceSnapshotDelete,
			signature: "instance.snapshot.delete(*, sandbox: str, name: str, snapshot: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "snapshot", Type: "str", Required: true},
			},
			output: []fieldShape{},
		},
		{
			name:      capabilityInstanceSnapshotList,
			signature: "instance.snapshot.list(*, sandbox: str, name: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
			},
			output: []fieldShape{
				{Name: "items", Type: "list[" + snapshotItemType + "]", Required: true},
			},
		},
		{
			name:      capabilityInstancePublish,
			signature: "instance.publish(*, sandbox: str, name: str, image: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "image", Type: "str", Required: true},
			},
			output: []fieldShape{{Name: "image", Type: "str", Required: true}},
		},
		{
			name:      capabilityNetCreate,
			signature: "net.create(*, sandbox: str, name: str, kind: str | None, cidr: str | None, dhcp: bool | None, nat: bool | None, dns: bool | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
				{Name: "kind", Type: "str | None"},
				{Name: "cidr", Type: "str | None"},
				{Name: "dhcp", Type: "bool | None"},
				{Name: "nat", Type: "bool | None"},
				{Name: "dns", Type: "bool | None"},
			},
			output: []fieldShape{
				{Name: "name", Type: "str", Required: true},
				{Name: "kind", Type: "str", Required: true},
				{Name: "cidr", Type: "str", Required: true},
				{Name: "gateway", Type: "str", Required: true},
			},
		},
		{
			name:      capabilityNetAttach,
			signature: "net.attach(*, sandbox: str, instance: str, network: str, nic: str | None, ip: str | None, mac: str | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "instance", Type: "str", Required: true},
				{Name: "network", Type: "str", Required: true},
				{Name: "nic", Type: "str | None"},
				{Name: "ip", Type: "str | None"},
				{Name: "mac", Type: "str | None"},
			},
			output: []fieldShape{
				{Name: "nic", Type: "str", Required: true},
				{Name: "mac", Type: "str", Required: true},
			},
		},
		{
			name:      capabilityNetList,
			signature: "net.list(*, sandbox: str)",
			input:     []fieldShape{{Name: "sandbox", Type: "str", Required: true}},
			output: []fieldShape{
				{Name: "items", Type: "list[" + networkType + "]", Required: true},
			},
		},
		{
			name:      capabilityNetGet,
			signature: "net.get(*, sandbox: str, name: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
			},
			output: []fieldShape{
				{Name: "name", Type: "str", Required: true},
				{Name: "kind", Type: "str", Required: true},
				{Name: "cidr", Type: "str", Required: true},
				{Name: "gateway", Type: "str", Required: true},
			},
		},
		{
			name:      capabilityNetDelete,
			signature: "net.delete(*, sandbox: str, name: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "name", Type: "str", Required: true},
			},
			output: []fieldShape{},
		},
		{
			name:      capabilityNetDetach,
			signature: "net.detach(*, sandbox: str, instance: str, nic: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "instance", Type: "str", Required: true},
				{Name: "nic", Type: "str", Required: true},
			},
			output: []fieldShape{},
		},
		{
			name:      capabilityNetPeer,
			signature: "net.peer(*, sandbox: str, network: str, peer: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "network", Type: "str", Required: true},
				{Name: "peer", Type: "str", Required: true},
			},
			output: []fieldShape{},
		},
		{
			name:      capabilityNetACLAdd,
			signature: "net.acl.add(*, sandbox: str, network: str, direction: str, action: str, protocol: str | None, src: str | None, dst: str | None, port: str | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "network", Type: "str", Required: true},
				{Name: "direction", Type: "str", Required: true},
				{Name: "action", Type: "str", Required: true},
				{Name: "protocol", Type: "str | None"},
				{Name: "src", Type: "str | None"},
				{Name: "dst", Type: "str | None"},
				{Name: "port", Type: "str | None"},
			},
			output: []fieldShape{{Name: "rule", Type: "str", Required: true}},
		},
		{
			name:      capabilityNetACLRemove,
			signature: "net.acl.remove(*, sandbox: str, network: str, rule: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "network", Type: "str", Required: true},
				{Name: "rule", Type: "str", Required: true},
			},
			output: []fieldShape{},
		},
		{
			name:      capabilityNetForward,
			signature: "net.forward(*, sandbox: str, network: str, instance: str, port: int, listen_port: int | None, protocol: str | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "network", Type: "str", Required: true},
				{Name: "instance", Type: "str", Required: true},
				{Name: "port", Type: "int", Required: true},
				{Name: "listen_port", Type: "int | None"},
				{Name: "protocol", Type: "str | None"},
			},
			output: []fieldShape{
				{Name: "address", Type: "str", Required: true},
				{Name: "port", Type: "int", Required: true},
			},
		},
		{
			name:      capabilityNetImpair,
			signature: "net.impair(*, sandbox: str, instance: str, nic: str, latency_ms: int | None, jitter_ms: int | None, loss_percent: float | None, rate_mbit: int | None, clear: bool | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "instance", Type: "str", Required: true},
				{Name: "nic", Type: "str", Required: true},
				{Name: "latency_ms", Type: "int | None"},
				{Name: "jitter_ms", Type: "int | None"},
				{Name: "loss_percent", Type: "float | None"},
				{Name: "rate_mbit", Type: "int | None"},
				{Name: "clear", Type: "bool | None"},
			},
			output: []fieldShape{},
		},
		{
			name:      capabilityDesktopInfo,
			signature: "desktop.info(*, sandbox: str, instance: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "instance", Type: "str", Required: true},
			},
			output: []fieldShape{
				{Name: "ready", Type: "bool", Required: true},
				{Name: "os", Type: "str", Required: true},
				{Name: "driver_version", Type: "str", Required: true},
				{Name: "tools", Type: "list[str]", Required: true},
				{Name: "vnc", Type: "str"},
			},
		},
		{
			name:      capabilityDesktopEnable,
			signature: "desktop.enable(*, sandbox: str, instance: str)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "instance", Type: "str", Required: true},
			},
			output: []fieldShape{{Name: "ready", Type: "bool", Required: true}},
		},
		{
			name:      capabilityDesktopCall,
			signature: "desktop.call(*, sandbox: str, instance: str, tool: str, args: str | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "instance", Type: "str", Required: true},
				{Name: "tool", Type: "str", Required: true},
				{Name: "args", Type: "str | None"},
			},
			output: []fieldShape{
				{Name: "ok", Type: "bool", Required: true},
				{Name: "summary", Type: "str", Required: true},
				{Name: "result", Type: "str", Required: true},
				{Name: "screenshot_url", Type: "str"},
			},
		},
		{
			name:      capabilityDesktopScreenshot,
			signature: "desktop.screenshot(*, sandbox: str, instance: str, pid: int | None, window_id: int | None, max_dimension: int | None)",
			input: []fieldShape{
				{Name: "sandbox", Type: "str", Required: true},
				{Name: "instance", Type: "str", Required: true},
				{Name: "pid", Type: "int | None"},
				{Name: "window_id", Type: "int | None"},
				{Name: "max_dimension", Type: "int | None"},
			},
			output: []fieldShape{
				{Name: "url", Type: "str", Required: true},
				{Name: "width", Type: "int", Required: true},
				{Name: "height", Type: "int", Required: true},
				{Name: "scale", Type: "float", Required: true},
			},
		},
	}
}

func decodeShapes(t *testing.T, value any) []fieldShape {
	t.Helper()

	raw, err := json.Marshal(value)
	require.NoError(t, err)
	if string(raw) == "null" {
		return []fieldShape{}
	}
	var out []fieldShape
	require.NoError(t, json.Unmarshal(raw, &out))
	if out == nil {
		return []fieldShape{}
	}
	return out
}
