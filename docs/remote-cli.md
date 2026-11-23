# Remote CLI

[Runquill README](../README.md) · [한국어](remote-cli.ko.md)

The `runquill` binary can query and operate a manager from another machine through the panel API with an API token. Create the token under **Team & tokens** in the panel. Nothing else needs to be configured on the client. Releases include only Linux binaries, so on Windows or macOS build the client from a source checkout with `go build -o runquill .` (`-o runquill.exe` on Windows).

## Connect

| Setting | Effect |
| --- | --- |
| `RUNQUILL_URL` or `--url` | Panel origin, exactly as in `RUNQUILL_PANEL_URL`: HTTPS, or HTTP on a loopback address. The panel refuses other host names. |
| `RUNQUILL_TOKEN` | The token, `rqk_` followed by 43 characters. |
| `--token-file FILE` | Read the token from a file instead. The CLI warns when other users can read it. |

A `--token` argument is refused because other local users could see it in the process list. The client never follows a redirect to another origin and gives up after 15 seconds.

```sh
export RUNQUILL_URL=https://panel.example.com
runquill jobs list --token-file ~/.config/runquill/token --outcome failed --since 24h
```

## Commands

| Command | Token scope | Output |
| --- | --- | --- |
| `fleet status [--check]` | `fleet:read` | Queue depth, running jobs, host totals and current problems from the latest snapshot, the same summary as the MCP `fleet_status` tool. |
| `jobs list [--repo OWNER/NAME] [--outcome waiting\|running\|finished\|failed] [--since 24h\|7d] [--limit 1-100]` | `fleet:read` | Newest jobs first from the seven-day history. |
| `jobs show JOB_ID` | `fleet:read` | One job from the history: outcome, failure or retry reason, diagnostic, times, wait, stage timing, sampled CPU and memory against the limits, whether the runner was killed for memory, and its GitHub URL. |
| `runners list` | `fleet:read` | Runners in the latest snapshot. |
| `hosts list` | `fleet:read` | Hosts with status, maintenance state and slots. |
| `pools list` | `fleet:read` | Hosts summarized by pool. |
| `usage [--group day\|repository\|pool\|host] [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--repo OWNER/NAME]` | `fleet:read` | Finished jobs, failures, runner time and average queue wait per group, with a `(total)` row. The range is in UTC days and defaults to the last 30, at most 400. |
| `usage --rightsizing [--group pool\|job] [--days 1-7]` | `fleet:read` | Per pool, or per repository and runner label, the jobs with [recorded resources](elastic-limits.md#job-resources), memory kills, and CPU and memory peaks against the limits with a verdict such as `tight` or `oversized`. Covers finished jobs from the last 7 days by default. |
| `elastic` | `fleet:read` | The [elastic limits](elastic-limits.md) mode, per-host surplus, runner limits and the newest 20 resize decisions. |
| `hosts cordon HOST`, `hosts uncordon HOST` | `hosts:write` | Pause or resume new assignments to a host from `hosts list`. |
| `jobs cancel JOB_ID`, `jobs retry JOB_ID` | `jobs:write` | Fail a job that waits without a runner, or clear its retry delay. |
| `audit tail [--limit 1-200]` | none | Always refused: the audit log needs an administrator session, which no token scope grants. Use the panel. |

`hosts:write` and `jobs:write` tokens need an operator or administrator owner. Because a token has one scope, reading and changing the fleet needs two tokens.

`runquill GROUP --help`, such as `runquill hosts --help`, prints every command on standard output; `--help` after a command prints its flags there too and exits with status 0. Add `--json` to any command for machine-readable output. `jobs list` prints the panel's `GET /api/jobs` page, `jobs show` prints the `GET /api/jobs/{id}` row, `runners list` and `hosts list` print the snapshot's `runner_inventory` and `fleet.hosts` arrays, `usage`, `usage --rightsizing` and `elastic` print `GET /api/usage`, `GET /api/usage/rightsizing` and `GET /api/elastic`, which also hold the p50 and p95 queue wait and every kept decision, `fleet status` prints the MCP `fleet_status` result, and `pools list` prints its own summary. The change commands print `{"job_id":…,"action":…}` or `{"host":…,"cordoned":…}`.

The CLI exits with status 0 on success, 1 when the panel refuses or fails the request, and 2 for invalid arguments or a missing URL or token. `fleet status --check` also exits with status 1 when the summary lists a problem, after printing it, so a scheduled check can alert on it. When the address answers with a bare `404 page not found`, the CLI adds that it serves no panel API there: `runquill panel --demo` has no API, and an older manager lacks newer endpoints.

The remote CLI is covered by tests against a local test server. Use it against your own panel before you script it.
