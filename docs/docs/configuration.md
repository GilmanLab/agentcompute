---
title: Configuration
description: CLI flags, environment variables, CodeMode options, limits, and transports.
---

# Configuration

The CLI uses Cobra and an instance-scoped Viper configuration. Flags take precedence over environment variables, which take precedence over defaults. `internal/templateinfo.Name` derives the `AGENTCOMPUTE_*` environment prefix.

## Commands

| Command | Purpose |
| --- | --- |
| `agentcompute stdio` | Serve over STDIO for a local client-launched subprocess. |
| `agentcompute http` | Serve over Streamable HTTP. |
| `agentcompute --version` | Print version, commit, and build date. |

A local build prints `agentcompute dev (none) built unknown`. Release builds receive their metadata through linker flags.

## Global flags

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--config` | `AGENTCOMPUTE_CONFIG` | Required | Path to a strict YAML or TOML runtime configuration. |
| `--log-level` | `AGENTCOMPUTE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `--log-format` | `AGENTCOMPUTE_LOG_FORMAT` | `text` | `text` or `json`. |

Invalid values fail at startup. Logs always go to stderr. STDIO reserves stdout for JSON-RPC.

## Runtime configuration file

The file is decoded independently of Viper. Its fields are not individually environment-backed. Unknown keys, multiple YAML documents, invalid TTL ranges, and unsupported file extensions fail startup. Relative paths resolve beside the configuration file.

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

| Field | Contract |
| --- | --- |
| `incus.remote` | Existing workstation Incus remote; mutually exclusive with `incus.url`. |
| `incus.url` | Explicit daemon URL instead of a named remote. |
| `incus.client_cert`, `incus.client_key` | PEM file paths for the explicit-URL client identity. |
| `incus.server_cert` | Optional PEM file path for a pinned server certificate. Otherwise normal CA verification applies. |
| `incus.host` | Required member for newly created bridge-backed sandboxes. Placement is persisted in project metadata. |
| `incus.pool` | Required storage pool for instance root disks. |
| `sandbox.default_ttl_minutes` | Positive creation default, 240 minutes if omitted. |
| `sandbox.max_ttl_minutes` | Positive upper bound, 1440 minutes if omitted; must be at least the default. |
| `sandbox.default_network_kind` | Only `bridge` is available. |
| `images_file` | Schema-version-1 catalog path; defaults to `images/catalog.yaml`. |
| `screenshots.dir`, `screenshots.base_url` | Accepted schema fields reserved for the desktop slice; no screenshot service in slice 1. |

`sandbox.extend` replaces the expiry with **now + TTL**, rather than adding time to the old expiry. Explicit deletion first expires the project so a partial failure is retried by the reaper. The reaper scans at startup and every 30 seconds.

The default bridge enables IPv4 DHCP and NAT. `net.create` without L3 options creates a bare bridge. Physical network names are opaque `ac` plus eight lowercase hex characters; `user.agentcompute.sandbox`, `.name`, and `.version` metadata resolve their logical names. Pending creations also reserve the physical name on the project for cleanup.

All members receive bridge definitions before activation, but each member has a separate L2/dnsmasq/NAT instance. All guests in a bridge-backed sandbox must use its persisted member. These bridges are not OVN networks.

The runtime identity must manage projects and default-project networks. The Phase 1 image-build-only CI certificate cannot do this. No new trust identity is created by the server.

## HTTP flags

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--addr` | `AGENTCOMPUTE_ADDR` | `localhost:8080` | Listen address. |
| `--auth-token` | `AGENTCOMPUTE_AUTH_TOKEN` | Empty | Demonstration shared bearer token; empty disables token validation. |
| `--insecure` | `AGENTCOMPUTE_INSECURE` | `false` | Permit a non-loopback bind without authentication. |

A non-loopback bind without `--auth-token` fails unless `--insecure` explicitly permits unauthenticated exposure. Cross-origin protection is enabled independently of this bind check.

The shared token is not a production credential system. It does not validate a signed token, issuer, audience, expiry, or per-client scope. See [Security](security.md).

## Subject resolution by transport

| Mode | Resolver | Subject ID | Trust boundary |
| --- | --- | --- | --- |
| STDIO | `mcpserver.StaticSubject` | `local` | Ownership of the launched process. |
| HTTP with valid demo token | `mcpserver.ContextSubject` | `shared-token` | The demo SDK verifier sets `auth.TokenInfo.UserID` after constant-time token validation. The token value is not stored as identity. |
| HTTP on loopback without a token | `mcpserver.ContextSubject` | `development` | Explicit unauthenticated development identity passed through the MCP receiving bridge. |
| HTTP with `--insecure` and no token | `mcpserver.ContextSubject` | `development` | Explicit unauthenticated network identity passed through the MCP receiving bridge. |

For HTTP, the SDK authentication verifier supplies a stable, non-secret `auth.TokenInfo.UserID`. The `installHTTPSubject` receiving middleware reads `req.GetExtra().TokenInfo.UserID` from each MCP request and stores an `authz.Subject` with `authz.WithSubject` on the MCP handler context. `mcpserver.ContextSubject` resolves that value. Setting a value only on the outer `net/http` request context is not sufficient because the SDK establishes the receiving handler context. MCP tool input, Starlark source, request `_meta`, and unvalidated headers are not trusted identity sources.

## Server options

`internal/mcpserver.New` has this repository-owned API:

```text
New(options Options) (*mcp.Server, error)
```

`Options` contains:

| Field | Contract |
| --- | --- |
| `Version string` | Release version reported in the MCP implementation metadata. |
| `Deps Dependencies` | Shared host collaborators closed over by capability handlers. |
| `Logger *slog.Logger` | Server diagnostics; the CLI supplies a stderr logger. |
| `Resolver codemodemcp.InvocationResolver` | Required trusted-subject resolver selected by the transport. |
| `Runtime codemode.Options` | CodeMode authorizer, static capability filters, and execution/discovery limits. |

The CLI sets `Runtime.Authorizer` to `authz.AllowAll()` explicitly. Persisted subject metadata does not restrict access to other subjects' sandboxes. `internal/mcpserver.New` does not replace a missing authorizer. Replace this policy and the authentication seam before multi-tenant deployment.

The HTTP command calls `internal/mcpserver.New` once before serving and reuses the returned server for all sessions. Do not move construction into the per-session server factory. The shared runtime's `MaxConcurrentExecutions` limit then applies across the process, and shared dependencies are not recreated for each session.

## Runtime limits

The server applies the following execution limits. Other zero-valued fields receive CodeMode defaults during `Builder.Build`:

| Field | Default |
| --- | ---: |
| `MaxSourceBytes` | 65,536 bytes |
| `MaxExecutionSteps` | 1,000,000 |
| `MaxExecutionTime` | 15 minutes |
| `MaxNativeCalls` | 1,000 |
| `MaxValueDepth` | 32 |
| `MaxValueBytes` | 1,048,576 bytes |
| `MaxIntermediateValueBytes` | 8,388,608 bytes |
| `MaxSearchQueryBytes` | 256 bytes |
| `MaxSearchResults` | 20 |
| `MaxConcurrentExecutions` | 8 |

The CLI does not expose limit flags or environment variables. For exact accounting and validation rules, see the [CodeMode limits reference](https://meigma.github.io/codemode/reference/public-api/#limits).

`instance.exec` drains both streams and retains at most 64 KiB from each, with independent truncation flags. An exec-only deadline returns `timed_out=true`; caller cancellation or an earlier caller deadline remains an error. `user` accepts a numeric UID or `root`.

## Upstream MCP adapter options

The repository wrapper eventually calls the CodeMode adapter with the required three-argument signature:

```go
srv, err := codemodemcp.New(
	service,
	resolver,
	codemodemcp.Options{
		Implementation: &mcp.Implementation{
			Name:    templateinfo.Name,
			Title:   templateinfo.Title,
			Version: options.Version,
		},
		Logger: logger,
	},
)
```

The complete API is:

```text
mcpserver.New(service Service, resolver InvocationResolver, options Options) (*mcp.Server, error)
```

The third argument is required; use `mcpserver.Options{}` to accept upstream defaults. A nil `Options.Implementation` uses implementation name `codemode` and version `2`. `Options.Logger` is optional and a nil value uses the MCP SDK default. The server supplies its own implementation name, title, version, and logger.

See the [canonical `mcpserver` API reference](https://meigma.github.io/codemode/reference/public-api/#mcpserver) for service, resolver, and error contracts.

## Transports

Both subcommands use the same `internal/mcpserver` constructor:

- STDIO exchanges JSON-RPC over stdin/stdout and uses a process-owned static subject.
- Streamable HTTP uses SDK `TokenInfo.UserID`, a receiving-middleware bridge to per-request context subjects, cross-origin protection, a loopback default, and graceful shutdown.

The runtime and reaper belong to the process, not an MCP session. HTTP is the deployment transport; STDIO is for workstation development.
