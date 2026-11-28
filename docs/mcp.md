# MCP for coding agents

[Runquill README](../README.md) · [한국어](mcp.ko.md)

Runquill exposes a read-only [Model Context Protocol](https://modelcontextprotocol.io/) server so an agent can inspect the fleet. It cannot change anything.

## Tools

| Tool | Arguments | Returns |
| --- | --- | --- |
| `fleet_status` | none | Queue depth, running jobs, host totals and current problems from the latest snapshot. `runquill fleet status --json` prints the same result. |
| `list_jobs` | `repository`, `outcome`, `since` (such as `90m`, `24h`, `7d`; at most `7d`), `before`, `limit` (1–100, default 20) | Job history, newest first, with recorded stage timings and current diagnostics where available. |
| `get_job` | `id` (required) | One job with outcome, GitHub conclusion, workflow name and branch, failure or retry reason, timing, runner, live status, and its [recorded resources](elastic-limits.md#job-resources) with whether the runner was killed for memory. |
| `list_hosts` | none | Hosts with status, slots and up to 100 runners each. |
| `usage_summary` | `window` (`24h` or `7d`), `group` (`day`, `repository`, `pool` or `host`) | Finished jobs, Runquill failures, GitHub successes and failures (`github_succeeded`, `github_failed`), runner time and queue wait. With `group`, also `series`: the same statistics per group, busiest first and at most 500, read like `GET /api/usage` over the whole UTC days `from` and `to` that contain the window. |
| `elastic_decisions` | `limit` (1–200, default 20) | Recent [elastic limit](elastic-limits.md) decisions. |
| `job_rightsizing` | `group` (`pool` or `job`), `days` (1–7, default 7) | CPU and memory peaks of recently finished jobs against their limits, memory kills and a verdict per resource, read like `GET /api/usage/rightsizing`. |
| `pool_rightsizing` | `days` (1–7, default 7) | Per-pool CPU peaks in cores, memory peaks in MiB, out-of-memory kills and a verdict per resource for recently finished jobs. Uses the same report as `GET /api/usage/rightsizing?group=pool`. |

Results come back as JSON text and as structured content. Repository, job and runner names come from GitHub users, so the server instructions tell the agent to treat them as data.

## Inspect a job with an agent

1. Call `fleet_status` and check queue depth, online hosts and available slots.
2. Call `list_jobs` with the repository and a short time window. When you have a GitHub job ID, pass it directly to `get_job`.
3. Read the job's outcome, last retry or failure reason, host and timing. Use `list_hosts` when the job needs a pool or its worker is offline.
4. For a job missing from Runquill, use the [server-side checks](operations.md#troubleshooting) to check App coverage and verify whether GitHub delivered the webhook.

Use `fleet:read` for agent inspection. For a state change authorized by the operator, use the [panel](panel.md) or the [remote CLI](remote-cli.md) with the appropriate write scope.

## Access

Create a `fleet:read` API token under **API tokens** in the panel’s user menu. The endpoint accepts API tokens only: a browser session cookie is always refused with HTTP 401, so a web page cannot use your panel session to call it. A request that carries an `Origin` header must come from the panel origin. One manager runs at most 4 MCP requests at a time, each limited to 5 seconds.

The endpoint speaks MCP revision `2026-07-28` statelessly and revision `2025-11-25` for clients that start with `initialize`. Clients that only support older revisions are refused with an unsupported-version error.

## HTTP

Point a client that supports Streamable HTTP at `https://panel.example.com/mcp` and send the token as a bearer header. The exact file and keys depend on the client. A typical entry looks like this:

```json
{
  "mcpServers": {
    "runquill": {
      "type": "http",
      "url": "https://panel.example.com/mcp",
      "headers": { "Authorization": "Bearer rqk_..." }
    }
  }
}
```

Use your client's mechanism for secrets or environment variables rather than writing the token into a shared file.

## Standard input and output

`runquill mcp` runs a local stdio server that forwards each message to the panel's `/mcp` endpoint. It reads the origin from `--url` or `RUNQUILL_URL` and the token from `--token-file` or `RUNQUILL_TOKEN`, like the [remote CLI](remote-cli.md). Standard output carries only JSON-RPC messages, and diagnostics go to standard error.

```json
{
  "mcpServers": {
    "runquill": {
      "command": "/usr/local/bin/runquill",
      "args": ["mcp", "--url", "https://panel.example.com", "--token-file", "/home/me/.config/runquill/token"]
    }
  }
}
```

The MCP server and the stdio proxy are covered by protocol tests in this repository. They have not yet been exercised with a real MCP client, so check the tool list in your client before relying on it.
