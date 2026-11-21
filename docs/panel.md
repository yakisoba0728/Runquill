# Web panel

The panel is a single page served by the manager on `RUNQUILL_PANEL_ADDR`. Set it up as described in [Web panel and team access](operations.md#web-panel-and-team-access). This guide covers what each screen shows and who can use it. The [API description](../api/openapi.yaml) lists every route the panel calls.

## Navigation

Screens are addressed by the URL fragment, such as `#/queue`. An unknown screen or one your role cannot open falls back to the first screen you can see.

| Group | Screen | Role needed |
| --- | --- | --- |
| Operations | Overview, Queue, Jobs, Runners, Hosts & pools, Usage, Logs | Viewer |
| Administration | Migrate, Backups, Audit, Settings | Admin |
| Administration | Team & tokens | Viewer (the team list is admin only) |

The sidebar shows counts of queued jobs, running runners and offline hosts.

- **Overview** summarizes capacity, host health, the GitHub connection, a first-job checklist and the longest-waiting jobs.
- **Queue** lists waiting, assigned and recently cleaned jobs with a repository, job ID and pool filter. The pool column notes the runner OS a job selects with `os__NAME__`. Select a job to see its runner label, runner OS and image, timings and the current queue or startup explanation, including a wait for a runner image. Administrators can run a repository check (`POST /api/diagnose`) from **Missing a job?**.
- **Jobs** is the seven-day job history with repository, outcome and time filters. The pool column notes the runner OS a job selected.
- **Runners** lists the runner containers or Pods in the latest snapshot, with the runner image and OS of the job each one holds.
- **Hosts & pools** shows host health, slots and pool capacity, and lets operators pause or resume assignments. While the manager [pulls runner images](runner-images.md#automatic-pulls), each `local` and `ssh://` host also shows whether its image is ready, missing, being pulled or failed to pull, with the pull error. With [elastic limits](elastic-limits.md) enabled, it also shows the mode, per-host surplus and recent resize decisions from `GET /api/elastic`.
- **Usage** charts [usage statistics](#usage-statistics) by day, repository, pool or host.
- **Logs** streams [runner logs](#runner-logs) for operators and administrators.
- **Migrate** walks an administrator through [workflow migration](workflow-migration.md).
- **Backups** shows the [scheduled backup](backups.md) settings and last run, and downloads a queue backup. Restore stays on the command line.
- **Audit** lists audited actions with action and actor filters.
- **Team & tokens** manages API tokens for every user and team members for administrators.
- **Settings** edits the fleet limit, polling interval, runner image, Docker workers and pools, including the runner OS names each pool allows, and applies them without a restart. It lists the [runner image catalogue](runner-images.md#the-runner-image-catalogue) read-only.

Queue diagnostics use the same reason codes in the snapshot, the job history API, the CLI and read-only MCP. They distinguish worker availability, repository caps, image preparation and assignment. New jobs also record when Runquill requested JIT registration, GitHub returned it, and the worker runtime accepted the launch. Kubernetes accepts a launch when it creates the Pod. Time between then and GitHub reporting a start can include Pod scheduling and image pulls. Jobs from older releases show the timestamps Runquill recorded at the time.

## Roles and tokens

| Action | Viewer | Operator | Admin |
| --- | --- | --- | --- |
| View the Operations screens, create a `fleet:read` token | Yes | Yes | Yes |
| Pause or resume a host, cancel or retry a waiting job, create `hosts:write` and `jobs:write` tokens | | Yes | Yes |
| Read runner logs (signed-in session only) | | Yes | Yes |
| Team, Audit, Settings, Migrate, Backups, queue backup download, repository check | | | Yes |

A token has exactly one scope and reaches only the routes of that scope. A `hosts:write` or `jobs:write` token cannot read the fleet. Tokens never reach browser-only or administrator routes, including runner logs, migration and backups.

| Scope | Routes |
| --- | --- |
| `fleet:read` | `GET /snapshot`, `GET /api/events`, `GET /api/jobs`, `GET /api/jobs/{id}`, `GET /api/elastic`, `GET /api/usage`, `POST /mcp` |
| `hosts:write` | `POST /api/hosts/cordon` |
| `jobs:write` | `POST /api/jobs/{id}/cancel`, `POST /api/jobs/{id}/retry` |

## Command palette and themes

Press Cmd+K on macOS or Ctrl+K elsewhere, or `/` outside a text field, to open the command palette. It lists the screens you can open, commands of the current screen, language and theme switches, refresh and sign-out, and matching runners, jobs and hosts once you type. Use the arrow keys, Page Up and Page Down, Enter and Esc.

The theme follows the system by default. Choose Light or Dark from the top bar or the palette. The choice is stored in the browser under `runquill-theme`. The language follows the browser unless you pick English or 한국어, which is stored under `runquill-language`.

## Live updates

The panel subscribes to `GET /api/events`, a Server-Sent Events stream of the fleet snapshot. The manager checks for changes every 2 seconds, sends a keep-alive every 15 seconds and closes each stream after 10 minutes so the browser reconnects and is authorized again. One manager accepts 64 streams in total and 4 per user.

If the stream does not reopen within 10 seconds, or the server refuses it, the panel polls `GET /snapshot` every 3 seconds and backs off up to 30 seconds while the manager is unreachable. It tries the stream again after 60 seconds. The indicator in the top bar shows **Live**, **Polling**, **Reconnecting**, **Connection lost** or **Paused while hidden**.

## Usage statistics

Usage counts finished jobs: jobs, failures, runner time and queue wait. Each job counts once, on the UTC day it finished, grouped by day, repository, pool or host.

- **Failed** means Runquill gave up on the job (24 hours without a start) or it was cancelled in the panel. It is not the GitHub job conclusion.
- **Runner time** runs from runner provisioning to runner cleanup.
- **Wait** runs from queueing to provisioning, or to the failure for jobs that never got a runner. Median and 95th percentile are estimated from fixed histogram buckets.

The scheduler folds finished jobs into a daily rollup before the seven-day job history removes them. Rollup rows are kept for 400 UTC days, and one request covers at most 400 days. The rollup lives in the queue database but is not part of queue backups, so a restored queue starts with empty statistics.

## Runner logs

Operators and administrators can read the container output of a runner that currently holds a job. This is the output of the runner process (`docker logs --timestamps`, or `podman logs` with `RUNQUILL_CONTAINER_ENGINE=podman`), not the GitHub step log, which stays on GitHub. Each view is audited as `runner.logs`.

- Only runners on the `local` Docker host and `ssh://` hosts are supported. Kubernetes and `agent://` runners answer that logs are unavailable.
- A request returns up to 2000 recent lines, or follows new lines for up to 15 minutes. Output stops at 8 MiB, and runs of 200 or more base64 characters are replaced with `[redacted]` as a second guard for runner credentials.
- At most 8 log requests run at once. API tokens cannot read logs.
- The manager reads logs with the CLI that `RUNQUILL_CONTAINER_ENGINE` selects and reaches `ssh://` hosts the same way it starts runners there.

Runner log streaming has been tested against a fake engine command only. It has not yet been exercised on a real fleet.
