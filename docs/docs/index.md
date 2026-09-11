---
title: agentcompute
slug: /
description: A CodeMode-native Model Context Protocol server.
---

# agentcompute

`agentcompute` is a [CodeMode](https://github.com/meigma/codemode) [Model Context Protocol](https://modelcontextprotocol.io) server. You register typed Go capabilities; an agent uses the fixed `search_api`, `describe_api`, and `execute` MCP tools to discover and compose them in bounded Starlark programs.

Slice 1 manages time-limited Incus container sandboxes: 13 capabilities cover sandbox lifecycle, curated images, instances and bounded exec, and bridge networks. State survives process restarts in Incus project metadata.

## Documentation

- **[Getting started](getting-started.md)** — configure the server and create, use, and delete a router sandbox.
- **[Add a capability](how-to/add-a-capability.md)** — extend the typed capability catalog.
- **[Configuration](configuration.md)** — CLI flags, `AGENTCOMPUTE_*` environment variables, runtime options, and default limits.
- **[Security](security.md)** — trusted identity, authorization, worker isolation, cancellation, and deployment boundaries.

Use the [canonical CodeMode documentation](https://meigma.github.io/codemode/) for the complete public Go API, fixed MCP tool contracts, supported Starlark surface, and runtime security model. The repository Go API is published at [pkg.go.dev](https://pkg.go.dev/github.com/GilmanLab/agentcompute).
