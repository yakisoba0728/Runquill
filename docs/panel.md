# Web panel

[Runquill README](../README.md) · [한국어](panel.ko.md)

The panel is a single page served by the manager on `RUNQUILL_PANEL_ADDR`. Set it up as described in [Web panel and team access](operations.md#web-panel-and-team-access). This guide covers what each screen shows and who can use it. The [API description](../api/openapi.yaml) lists every route the panel calls.

The manager and `runquill panel --demo` serve panel v2 by default. Use an evergreen browser (the latest two major releases of Chrome, Edge, Firefox or Safari, from 2024 onward). To use v1 for this release, set `RUNQUILL_PANEL_V2=0` (or `false`) in the manager or demo environment and restart. The manager logs a deprecation warning on startup. The opt-out is removed in the next release.

## Navigation

Screens use URL fragments such as `#/jobs`. An unknown or unavailable screen opens the first screen your role can see.

| Location | Screens | Minimum role |
| --- | --- | --- |
| Operations | Overview, Jobs, Runners, Hosts, Usage | Viewer |
| Administration | Settings, Team, Audit log, Backups, Workflow migration | Admin |
| User menu | API tokens | Viewer |

The sidebar shows counts for items needing attention, waiting jobs, running runners and unavailable hosts.

- **Overview** shows what needs attention, summarizes capacity, host health and the GitHub connection, and lists the first-job checklist and the longest-waiting jobs.
- **Jobs** combines the old queue and job history. **Waiting**, **Running** and **Cleaned up** show the live queue with search and pool filters. The pool column notes the runner OS a job selects with `os__NAME__`. **History** shows seven days of jobs with repository, outcome and time filters. Select a job or open `#/jobs/ID` to see its runner label, OS and image, stage timings, and why it is waiting or how it ended. Local Docker jobs can also show sampled CPU and memory peaks and whether the runner hit its memory limit. Operators can retry or cancel eligible jobs. Administrators can run a repository check from **Missing a job?**.
- **Runners** lists runner containers or Pods from the latest snapshot. Select a runner to see its job, runner image, OS and provisioning time. Operators and administrators can open the [**Logs** tab](#runner-logs) at `#/runners/NAME/logs` for runners on `local` or `ssh://` hosts.
- **Hosts** has **Hosts**, **Worker pools**, **Server resources** and **Elastic limits** tabs. It shows host health, slots and pool capacity, and lets operators pause or resume assignments. While the manager [pulls runner images](runner-images.md#automatic-pulls), each `local` and `ssh://` host also shows whether its image is ready, missing, being pulled or failed to pull, with the pull error. Hosts of a pool with [warm runners](workers.md#keep-runners-warm) show how many warm runners are idle out of the pool's `warm` setting, and the pool table sums them. An offline host shows when it last answered, once it has answered since the manager started. With [elastic limits](elastic-limits.md) enabled, it also shows the mode, per-host surplus and recent resize decisions from `GET /api/elastic`. **Server resources** is described [below](#server-resources).
- **Usage** charts [usage statistics](#usage-statistics) by day, repository, pool or host, and lists [right-sizing](#right-sizing) verdicts.
- **Workflow migration** walks an administrator through [workflow migration](workflow-migration.md).
- **Backups** shows the [scheduled backup](backups.md) settings and last run, and downloads a queue backup. Restore stays on the command line.
- **Audit log** lists audited actions with action and actor filters.
- **Team** lets administrators manage up to 100 team members per page.
- **API tokens** in the user menu lets each signed-in user manage the tokens allowed by their role.
- **Settings** edits the fleet limit, polling interval, runner image, Docker workers and pools, including the runner OS names each pool allows and its `warm` and `warm_max_idle_minutes` settings, and applies them without a restart. It lists the [runner image catalogue](runner-images.md#the-runner-image-catalogue) read-only. Outbound `agent://` workers come only from `RUNQUILL_DOCKER_HOSTS`. If the deployment no longer matches the saved settings at startup, the manager uses the deployment's `agent://` workers and drops limit and pool entries for workers it no longer has. An `agent://` worker missing from the saved settings keeps its deployment limit and pool. Saved settings that still cannot be applied stop the manager from starting, so saved pool restrictions are never dropped. It logs each change and `runquill doctor` lists it. The saved row is unchanged until you save **Settings** again. The save bar counts unsaved changes and offers **Save** and **Revert**. If you navigate away, the bar keeps you on Settings until you save or choose **Discard and leave**. Reloading saved settings asks for confirmation, and closing or refreshing the browser tab uses the browser’s unsaved-changes prompt.

Queue diagnostics use the same reason codes in the snapshot, the job history API, the CLI and read-only MCP. They distinguish worker availability, repository caps, image preparation and assignment. New jobs also record when Runquill requested JIT registration, GitHub returned it, and the worker runtime accepted the launch. Kubernetes accepts a launch when it creates the Pod. Time between then and GitHub reporting a start can include Pod scheduling and image pulls. Jobs from older releases show the timestamps Runquill recorded at the time.

### Links from panel v1

The panel rewrites old bookmarks when it opens them.

| Previous URL | New URL |
| --- | --- |
| `#/queue` | `#/jobs` |
| `#/queue?job=ID`, `#/jobs?job=ID` | `#/jobs/ID` |
| `#/runners?runner=NAME` | `#/runners/NAME` |
| `#/logs` | `#/runners` |
| `#/logs?runner=NAME` | `#/runners/NAME/logs` |
| `#/hosts?host=NAME` | `#/hosts/NAME` |
| `#/team` for viewers and operators | `#/tokens` |

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
| `fleet:read` | `GET /snapshot`, `GET /api/events`, `GET /api/jobs`, `GET /api/jobs/{id}`, `GET /api/elastic`, `GET /api/hosts/resources`, `GET /api/usage`, `GET /api/usage/rightsizing`, `POST /mcp` |
| `hosts:write` | `POST /api/hosts/cordon` |
| `jobs:write` | `POST /api/jobs/{id}/cancel`, `POST /api/jobs/{id}/retry` |

## Server resources

The **Server resources** table under **Hosts** comes from `GET /api/hosts/resources`, which any role and a `fleet:read` token can read. Each number comes from data the manager already has. Opening the screen runs no command on a host, except one engine `info` call while the manager has not yet read that host since it started or since settings changed. A host whose engine did not answer is asked again at most every 30 seconds:

| Column | Source |
| --- | --- |
| CPUs, Memory | The engine's `info` report, which the manager reads once per host to place runners and then keeps. |
| Slots used | The fleet snapshot: occupied and reserved slots out of the host's slot limit. |
| Reserved CPU, Reserved memory | What runners holding a slot were promised: the pool's `cpus` and `memory_mib` times the occupied slots. When [elastic limits](elastic-limits.md) sample the host, the runners' current limits instead. A pool without a limit shows **Unlimited**. |
| Runner usage | The elastic limits loop's last sample. It is shown only for the `local` host with `RUNQUILL_ELASTIC` set and a pool that sets `cpus` or `memory_mib`, and otherwise reads **Not sampled**. |
| Disk | Runquill-pulled runner images still on the host and the runner cache volumes, from `docker system df -v` and `image inspect`. |

Disk use is measured only when someone selects **Measure disk use** (`GET /api/hosts/resources?disk=1`), because `docker system df -v` walks every volume. One measurement runs at a time for all users and its result is kept for 10 minutes, or for 1 minute when a host could not be measured. A measurement that takes longer than 3 seconds finishes in the background, and the panel asks again for its result. Podman cannot size volumes cheaply, so the cache size reads **size unknown** there.

The manager cannot read outbound `agent://` workers or Kubernetes nodes. Their capacity, usage and disk read **Not available** instead of zero. An `agent://` worker still shows its reservations, which come from the pool settings. A `local` or `ssh://` host whose engine does not answer shows the error instead of its capacity.

## Command palette and themes

Press Cmd+K on macOS or Ctrl+K elsewhere, or `/` outside a text field, to open the command palette. It lists the screens you can open, commands of the current screen, language and theme switches, refresh and sign-out, and matching runners, jobs and hosts once you type. Use the arrow keys, Page Up and Page Down, Enter and Esc.

The theme follows the system by default. Choose Light or Dark from the user menu or the palette. The choice is stored in the browser under `runquill-theme`. The language follows the browser unless you pick English or 한국어, which is stored under `runquill-language`.

## Live updates

The panel subscribes to `GET /api/events`, a Server-Sent Events stream of the fleet snapshot. The manager checks for changes every 2 seconds, sends a keep-alive every 15 seconds and closes each stream after 10 minutes so the browser reconnects and is authorized again. One manager accepts 64 streams in total and 4 per user.

If the stream does not reopen within 10 seconds, or the server refuses it, the panel polls `GET /snapshot` every 3 seconds and backs off up to 30 seconds while the manager is unreachable. It tries the stream again after 60 seconds. The indicator in the sidebar shows **Live**, **Polling**, **Reconnecting**, **Connection lost** or **Paused while hidden**.

## Usage statistics

Usage counts finished jobs: jobs, failures, runner time and queue wait. Each job counts once, on the UTC day it finished, grouped by day, repository, pool or host.

- **Failed** means Runquill gave up on the job (24 hours without a start) or it was cancelled in the panel. It is not the GitHub job conclusion.
- **Runner time** runs from runner provisioning to runner cleanup.
- **Wait** runs from queueing to provisioning, or to the failure for jobs that never got a runner. Median and 95th percentile are estimated from fixed histogram buckets.

The scheduler folds finished jobs into a daily rollup before the seven-day job history removes them. Rollup rows are kept for 400 UTC days, and one request covers at most 400 days. The rollup lives in the queue database but is not part of queue backups, so a restored queue starts with empty statistics.

## Right-sizing

The **Right-sizing** table on **Usage** comes from `GET /api/usage/rightsizing`, the same report as `runquill usage --rightsizing`. Choose a pool or workflow job grouping and 1 to 7 days. For CPU and memory, each row shows the largest sampled peak against the newest job's limit and a verdict: **Tight** (a peak above 80% of the limit, or a memory kill), **Oversized** (every peak below 25% of the limit, over at least 3 sampled jobs), **Fits**, **Too few jobs** or **No limit**. Only jobs whose runners were sampled count, which today means the `local` Docker host.

## Runner logs

Open **Runners**, select a runner and choose **Logs**. Operators and administrators can read the container output of a runner that currently holds a job. This is the output of the runner process (`docker logs --timestamps`, or `podman logs` with `RUNQUILL_CONTAINER_ENGINE=podman`), not the GitHub step log, which stays on GitHub. Each view is audited as `runner.logs`.

- Only runners on the `local` Docker host and `ssh://` hosts are supported. Kubernetes and `agent://` runners answer that logs are unavailable.
- A request returns up to 2000 recent lines, or follows new lines for up to 15 minutes. Output stops at 8 MiB, and runs of 200 or more base64 characters are replaced with `[redacted]` as a second guard for runner credentials.
- At most 8 log requests run at once. API tokens cannot read logs.
- The manager reads logs with the CLI that `RUNQUILL_CONTAINER_ENGINE` selects and reaches `ssh://` hosts the same way it starts runners there.

Runner log streaming has been tested against a fake engine command only. It has not yet been exercised on a real fleet.
