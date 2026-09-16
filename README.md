# agentcompute

`agentcompute` is a [CodeMode](https://github.com/meigma/codemode) [Model Context Protocol](https://modelcontextprotocol.io) server. Instead of registering one MCP tool per operation, it registers typed Go capabilities. An agent discovers them and composes several calls in one bounded Starlark program.

The server exposes exactly three MCP tools:

- `search_api` finds capabilities by name, summary, and search terms.
- `describe_api` returns the exact input and output shape for one capability.
- `execute` runs a Starlark program and returns the value from its zero-argument `main()` function.

Capabilities cover sandbox TTLs; instance lifecycle, exec, files, snapshots, and sandbox-local image publishing; and network creation, NICs, peering, ACLs, forwards, and Linux link impairment. Sandboxes are restricted Incus projects. OVN provides cross-member networking; member-local bridges remain available. The startup/30-second reaper retries expired sandboxes when their backend becomes available.

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
  pool: data
sandbox:
  default_ttl_minutes: 240
  max_ttl_minutes: 1440
  default_network_kind: ovn
  pin_identities: []  # Disabled by default; use [omp] to allow that HTTP identity.
images_file: images/catalog.yaml
```

OVN requires the fleet-managed central, chassis TLS configuration, and physical uplink. To use the bridge fallback, set `default_network_kind: bridge` and configure `incus.host`.

Pass its path with `--config` or `AGENTCOMPUTE_CONFIG`. YAML and TOML are strict: unknown keys fail startup. Catalog and credential paths resolve relative to the configuration file.

Run the local STDIO transport:

```sh
go run ./cmd/agentcompute stdio --config agentcompute.yaml
```

Run Streamable HTTP on its loopback default:

```sh
go run ./cmd/agentcompute http --config agentcompute.yaml --addr localhost:8080
```

Both commands build shared backend, catalog, compute, and CodeMode dependencies once. HTTP reuses them across sessions. Startup reconciles digest-pinned Incus images; upstream `remote:alias` images are fetched on first use. Mac `seed:` entries remain host-local and bypass Incus reconciliation.

For Mac guests, configure the optional Lume backend using the [macOS runbook](images/macos/README.md#configure-the-server). Create a sandbox with `platform="mac"` and select `macos/tahoe/desktop`. Mac guests use Lume's NAT network; cluster placement and network overrides are unsupported.

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

An identity listed in `sandbox.pin_identities` can create with `pinned=True` or call `sandbox.pin(name="keep", pinned=True)`. Pinned sandboxes remain usable past expiry and survive reaper scans and server restarts on both Incus and Mac. `sandbox.get` and `sandbox.list` expose `pinned` and `pinned_by`; each reaper scan logs the pin's identity and timestamp at INFO. Repeating a pin preserves its original attribution.

TTL values and the 1440-minute maximum still apply: pinning does not change `expires_at`, and `sandbox.extend` updates it normally. `sandbox.pin(name="keep", pinned=False)` restores that deadline; if it has passed, the next scan deletes the sandbox. Both pin and unpin require an allowlisted identity; `sandbox.delete` remains available to every authenticated identity and removes pinned sandboxes too.

Pins retain an interactive machine, not a reusable image. Instances, snapshots, and `instance.publish` images still belong to their sandbox and die with it. Use a recipe under `images/` for durable, reusable images.

OVN instances may use an explicit online `host`. Without one, placement prefers the most free RAM, then the lowest one-minute load, then member name; automatic placement excludes manual/group-only schedulers. Bridge instances remain on the sandbox's persisted member and reject a different host. Bridge names are opaque; capabilities expose logical names such as `default` and `lan`.

An OVN network with `nat=false` is isolated: it has no direct outside path and consumes no external address. Attach a router instance or use `net.peer` for reachability. Isolated networks reject external forwards. Operator-management and OOB denies are immutable, including against broader user allow rules.

On Incus, restoring a snapshot stages a copy before deleting the current instance, then recreates it under the same agent-visible name and ownership. The Incus identity and NIC MAC can change, so the DHCP lease can change too; the original instance's snapshots are consumed. Low-level Incus access remains blocked. Lume snapshots are stopped host-local clones: creating one temporarily stops a running guest, and restoring one retains the named snapshot for reuse.

Exec retains 64 KiB per stream while draining the rest. `stdout_truncated` and `stderr_truncated` report overflow. An exec-only timeout returns `timed_out=true`; caller cancellation remains an error. Linux `user` accepts a numeric UID or `root`. Mac exec defaults to `lume` (UID 501); other numeric UIDs and `root` require guest sudo access. Windows exec uses the Incus agent service identity and does not accept `user`.

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
- HTTP reads `--auth-tokens-file` (`AGENTCOMPUTE_AUTH_TOKENS_FILE`), a JSON object mapping identity names to bearer tokens. The verified name becomes `TokenInfo.UserID` and `user.agentcompute.subject`; token values are never identities. Load the file through a systemd credential and restart after rotation. Allowed loopback and explicitly insecure unauthenticated modes use `development`.

The CLI passes `authz.AllowAll()` explicitly. Every authenticated identity can manage every agentcompute sandbox; subject metadata records attribution, not ownership authorization. Pinning and unpinning additionally require the subject to be listed in `sandbox.pin_identities` (empty by default). This is a trusted-operator service, not a multi-tenant boundary.

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

Run the cross-member OVN acceptance lane against the durable fleet fabric:

```sh
AGENTCOMPUTE_TEST_REMOTE=nas01 AGENTCOMPUTE_TEST_MEMBERS=lab01,lab03 \
  go test -tags integration ./internal/cli -run '^TestOVNAcceptance$' -count=1 -v -timeout 15m
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
