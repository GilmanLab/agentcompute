---
title: Getting started
description: Create, use, and delete an Incus container sandbox through CodeMode.
---

# Getting started

This tutorial uses a workstation Incus identity and the Phase 1 `image-build` project to create a disposable router container through MCP.

## Build and configure

```sh
mise install
go build -o bin/agentcompute ./cmd/agentcompute
```

Create `agentcompute.yaml` in the repository root. Replace the remote, member, and pool with existing resources your identity can manage:

```yaml
incus:
  remote: nas01
  host: lab01
  pool: data
images_file: images/catalog.yaml
```

This example uses the member-local bridge fallback. For cross-member networking, set `sandbox.default_network_kind` to `ovn`; `incus.host` can then be omitted. OVN requires a provisioned central, chassis configuration, and physical uplink.

The identity must manage sandbox projects and bridges in the default project. The image-build-only CI certificate is insufficient. See [Configuration](configuration.md) for explicit-URL credentials and TTL settings.

## Connect over STDIO

Configure an MCP client with absolute paths:

```json
{
  "mcpServers": {
    "agentcompute": {
      "command": "/absolute/path/to/agentcompute/bin/agentcompute",
      "args": ["stdio", "--config", "/absolute/path/to/agentcompute/agentcompute.yaml"]
    }
  }
}
```

Startup reconciles the image catalog. The client sees exactly `search_api`, `describe_api`, and `execute`; compute capabilities live behind those tools. STDIO sends JSON-RPC to stdout and diagnostics to stderr.

## Discover the capabilities

Call `search_api` with `{"query":"sandbox"}` and then `describe_api` with `{"name":"sandbox.create"}`. Its signature is:

```text
sandbox.create(*, name: str | None, platform: str | None, ttl_minutes: int | None, pinned: bool | None)
```

Repeat discovery and description for `instance.create`, `instance.exec`, and `sandbox.delete`. Use the returned field names rather than guessing arguments.

## Create, use, and delete a router

Pass this source to `execute`:

```python
def main():
    sb = sandbox.create()
    instance.create(sandbox=sb["name"], name="router", image="router")
    result = instance.exec(
        sandbox=sb["name"],
        name="router",
        command="ip -br addr && nft list ruleset",
    )
    sandbox.delete(name=sb["name"])
    return result
```

The result contains the command's exit code, stdout, stderr, timeout flag, and per-stream truncation flags. Only the final value returned by `main()` enters the successful MCP result.

An omitted sandbox name is generated. With this bridge configuration, its default network provides DHCP and NAT on the configured member, and all guests stay on that member. If execution fails before explicit deletion, use `sandbox.list` to find the sandbox; its persisted TTL survives a server restart and is enforced by the reaper.

## Use HTTP

```sh
bin/agentcompute http --config agentcompute.yaml --addr localhost:8080
```

HTTP is the deployment transport. This slice retains the template's development authentication and `AllowAll` policy: it is not a multi-tenant authorization boundary. Read [Security](security.md) before exposing it beyond a trusted workstation.

## Verify the installation

```sh
moon run root:check
uv run .github/scripts/mcp_smoke.py -- bin/agentcompute stdio --config agentcompute.yaml
```

The live smoke performs read-only discovery and execution. The opt-in integration lane creates disposable cluster resources, kills and restarts the production binary, verifies original expiry, drains large exec output, and waits for TTL cleanup:

```sh
AGENTCOMPUTE_TEST_REMOTE=nas01 AGENTCOMPUTE_TEST_HOST=lab01 \
  go test -tags integration ./internal/cli -run TestClusterLifecycle -count=1 -v
```

For the complete network lifecycle program and observed results, see `spikes/acceptance.star` and `spikes/results.json` in the repository. Use the [CodeMode documentation](https://meigma.github.io/codemode/) for the language and worker contracts.
