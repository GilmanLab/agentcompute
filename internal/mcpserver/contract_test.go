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
