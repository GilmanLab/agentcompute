# agentcompute

`agentcompute` is a [CodeMode](https://github.com/meigma/codemode) [Model Context Protocol](https://modelcontextprotocol.io) server. Instead of registering one MCP tool per operation, it registers typed Go capabilities. An agent discovers them and composes several calls in one bounded Starlark program.

The server exposes exactly three MCP tools:

- `search_api` finds capabilities by name, summary, and search terms.
- `describe_api` returns the exact input and output shape for one capability.
- `execute` runs a Starlark program and returns the value from its zero-argument `main()` function.

Slice 1 provides 13 capabilities: sandbox create/list/get/extend/delete, image listing, instance create/list/get/delete/exec, and network create/attach. Sandboxes are restricted Incus projects with persisted TTLs and a default NAT bridge. The startup/30-second reaper deletes expired sandboxes.

## Local bootstrap

Prerequisites:

- [mise](https://mise.jdx.dev), which provisions the pinned Go 1.26.6 toolchain, Moon, Python and uv, the development CLIs, and the release tools from `mise.toml` and `mise.lock`. The server module pins the official MCP Go SDK v1.7.0.
- Docker, only for local container builds and scans.
- An existing Incus remote, cluster member, storage pool, and the Phase 1 `image-build` project. The server identity needs project and default-project network management rights; the restricted image-build CI certificate is not a server identity.

From the repository root:

```sh
mise install
```

`mise install` runs with locked tool resolution. To update a tool, edit `mise.toml`, regenerate `mise.lock` for the supported platforms, and commit both files.

## Run the server

Create `agentcompute.yaml` beside `images/`:

```yaml
incus:
  remote: nas01
  host: lab01
  pool: data
sandbox:
  default_ttl_minutes: 240
  max_ttl_minutes: 1440
  default_network_kind: bridge
images_file: images/catalog.yaml
```

Pass its path with `--config` or `AGENTCOMPUTE_CONFIG`. YAML and TOML are strict: unknown keys fail startup. Catalog and certificate paths resolve relative to the configuration file.

Run the local STDIO transport:

```sh
go run ./cmd/agentcompute stdio --config agentcompute.yaml
```

Run Streamable HTTP on its loopback default:

```sh
go run ./cmd/agentcompute http --config agentcompute.yaml --addr localhost:8080
```

Both commands build shared Incus, catalog, compute, and CodeMode dependencies once. HTTP reuses them across sessions. Startup reconciles digest-pinned images; upstream `remote:alias` images are fetched on first use.

For a local MCP client, build the binary and configure its absolute path:

```sh
go build -o bin/agentcompute ./cmd/agentcompute
```

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

## Compose capability calls

Use `search_api` and `describe_api` to discover the exact signatures before executing:

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

Instances use the sandbox's persisted member. An explicit different `host` is rejected. Bridges have opaque Incus names; capabilities expose only logical names such as `default` and `lan`. Each member's bridge is a separate L2 domain, not a cross-member network.

Exec retains 64 KiB per stream while draining the rest. `stdout_truncated` and `stderr_truncated` report overflow. An exec-only timeout returns `timed_out=true`; caller cancellation remains an error. `user` accepts a numeric UID or `root`. OVN and macOS are not available in this slice.

Only `main()`'s final converted value is returned. Intermediate capability results remain inside the worker and do not enter the model's context.

## Add capabilities

Capabilities live in `internal/mcpserver`. A capability combines:

- a stable deployment and authorization ID;
- a dotted Starlark name such as `instance.exec`;
- discovery metadata;
- non-pointer input and output structs; and
- a typed Go handler that receives `context.Context`, the trusted `authz.Subject`, and the input value.

Use direct exported fields and supported scalar input types. In particular, integer inputs use `int64`, not platform-sized `int`. CodeMode accepts JSON struct tags for capability fields and rejects unrelated struct tags. See [Add a capability](docs/docs/how-to/add-a-capability.md) for the repository procedure and the [canonical CodeMode public API reference](https://meigma.github.io/codemode/reference/public-api/) for the complete type contract.

## Worker entry points

`codemode.ServeWorkerAndExit()` must be the first statement of the final binary's `main` function. Keep it before flag parsing, credential loading, client construction, and all other host setup. CodeMode re-executes the binary for its worker process; late wiring can run privileged host setup in the worker or make the build-time worker probe fail.

Every test binary that calls `Builder.Build` also needs this first statement:

```go
func TestMain(m *testing.M) {
	codemode.ServeWorkerAndExit()
	os.Exit(m.Run())
}
```

Add one applicable `TestMain` per Go package. Do not put setup before the worker call.

## Identity and authorization

The server keeps authentication identity outside program source, tool arguments, and MCP metadata:

- STDIO uses `mcpserver.StaticSubject` with the non-secret subject ID `local`. Process ownership is the authentication boundary.
- HTTP uses `mcpserver.ContextSubject`. The receiving MCP middleware reads the SDK-authenticated `req.GetExtra().TokenInfo.UserID`, stores that non-secret identity with `authz.WithSubject`, and then lets the CodeMode adapter resolve it. Setting an arbitrary value only on the outer `net/http` request context is not sufficient.
- The demo verifier sets `TokenInfo.UserID` to `shared-token`; the token value is not used as identity. Allowed loopback and explicitly insecure unauthenticated modes send `development` through the same receiving bridge.

The CLI currently passes `authz.AllowAll()` explicitly. This permits every capability for every resolved subject; persisted `subject` metadata is not an ownership authorization check. `internal/mcpserver.New` supplies no fallback authorizer. Do not expose this slice as a multi-tenant service without replacing the authentication and authorization seams.

Discovery is not filtered by per-invocation authorization. An authenticated subject can search and describe every statically enabled capability; authorization runs for each native capability call during `execute`. Do not put secrets or tenant-sensitive data in capability names, summaries, descriptions, search terms, or field names. Disable a capability at build time if its existence must be hidden.

## Choose a transport

Transport code is isolated in `internal/cli`:

- `stdio.go` serves clients that spawn the binary as a local subprocess.
- `http.go` serves networked and containerized clients.

HTTP is the deployment transport; STDIO is the workstation development transport.

## Hot reload during development

The checked-in `.mcp.json` starts the development proxy in `tools/proxy`. Export `AGENTCOMPUTE_CONFIG` with an absolute configuration path before starting the client. The proxy rebuilds and swaps the child process behind the existing session.

CodeMode capability changes do not change the outer definitions of `search_api`, `describe_api`, or `execute`, so they normally do not emit `notifications/tools/list_changed`. Validate a reload by calling `search_api`, then `describe_api`, then `execute` and checking the capability result. See [the proxy guide](tools/proxy/README.md) for the exact workflow and its fidelity limits.

## Configuration and logging

Cobra flags take precedence over `AGENTCOMPUTE_*` environment variables, which take precedence over defaults. Common commands include:

```sh
go run ./cmd/agentcompute --version
go run ./cmd/agentcompute stdio --config agentcompute.yaml
go run ./cmd/agentcompute http --config agentcompute.yaml --addr localhost:8080
AGENTCOMPUTE_LOG_LEVEL=debug go run ./cmd/agentcompute stdio --config agentcompute.yaml
```

A local build reports `agentcompute dev (none) built unknown`. GoReleaser supplies version, commit, and date for releases.

Both transports log to stderr. STDIO reserves stdout exclusively for JSON-RPC; never write logs or diagnostics there.

CodeMode executions have a 15-minute limit and at most 1,000 native calls. Other CodeMode limits retain their defaults. These limits have no CLI or environment settings. See the [configuration reference](docs/docs/configuration.md).

## Common tasks

Moon is the task front door:

```sh
moon run root:format       # check formatting
moon run root:format-fix   # apply formatting
moon run root:lint
moon run root:build
moon run root:test
moon run root:check        # formatting, lint, builds, tests, docs, and proxy checks
moon run docs:serve        # documentation preview on http://127.0.0.1:8000
```

Run the opt-in production-binary create/exec/restart/TTL-delete integration lane:

```sh
AGENTCOMPUTE_TEST_REMOTE=nas01 AGENTCOMPUTE_TEST_HOST=lab01 \
  go test -tags integration ./internal/cli -run TestClusterLifecycle -count=1 -v
```

`root:smoke` and release jobs use offline artifact startup checks, without cluster credentials. Go tests exercise the real CodeMode worker and both MCP transports. Run `.github/scripts/mcp_smoke.py -- bin/agentcompute stdio --config agentcompute.yaml` for read-only live discovery and execution. The combined cluster acceptance program and results are retained under `spikes/`.

CI runs the same aggregate check with:

```sh
moon ci --summary minimal
```

## Container image

The local image path builds the binary into a signed Wolfi package with [melange](https://github.com/chainguard-dev/melange), then assembles a minimal non-root image with [apko](https://github.com/chainguard-dev/apko):

```sh
mise run image-local
docker run --rm agentcompute:dev --version
```

The image runs as uid/gid 65532 with CA certificates and timezone data, but no shell. Mount a runtime configuration, catalog, and client credentials readable by that user. Its inherited `--insecure` HTTP entrypoint is unauthenticated; remove that flag and install deployment authentication and authorization before exposing it.

## CI and release configuration

The CI workflows use minimal permissions, pinned external actions, disabled checkout credential persistence, and dependency caches. Documentation builds on pull requests and deploys from the default branch. A scheduled workflow builds and scans the container image and uploads SARIF to GitHub code scanning. Dependabot covers GitHub Actions, both Go modules, and the docs project.

This repository starts from a `0.0.0` Release Please baseline. The first pending release is `0.1.0`; no predecessor release history applies to this repository.

The configured release path is:

1. Release Please maintains a release pull request and creates a version tag plus draft GitHub release after merge.
2. The release dry-run workflow rehearses the GoReleaser binary path and the native-runner melange/apko image path on the release pull request.
3. GoReleaser builds binaries, checksums, and SBOMs without publishing directly. The release workflow validates and uploads them to the draft release.
4. Native runners build signed per-architecture Wolfi packages. apko publishes `ghcr.io/gilmanlab/agentcompute:vX.Y.Z` as a multi-platform image.
5. The isolated reusable `attest.yml` workflow creates GitHub provenance for binary checksums and the image. The release workflow also creates a keyless Cosign image signature and attaches an SBOM attestation.
6. A human inspects the draft before publication.

Before the first release, supply the release app credentials, confirm protected-tag bypass for `glab-release-please`, and run the release dry-run workflow before merging the first release pull request.

## Documentation

- [Getting started](docs/docs/getting-started.md)
- [Add a capability](docs/docs/how-to/add-a-capability.md)
- [Configuration](docs/docs/configuration.md)
- [Security model](docs/docs/security.md)
- [Canonical CodeMode documentation](https://meigma.github.io/codemode/)
- [Go API](https://pkg.go.dev/github.com/GilmanLab/agentcompute)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup and pull request expectations.

## Security

See [SECURITY.md](SECURITY.md) for supported versions and private vulnerability reporting. Read the [security model](docs/docs/security.md) before exposing the HTTP transport or adding privileged handlers.

## License

Licensed under either of:

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE))
- MIT License ([LICENSE-MIT](LICENSE-MIT))

at your option (`SPDX-License-Identifier: Apache-2.0 OR MIT`). Unless you state otherwise, a contribution intentionally submitted for inclusion is dual-licensed under those terms.
