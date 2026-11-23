# Operate Runquill

[Runquill README](../README.md) · [한국어](operations.ko.md)

Run commands from the installation directory with the same Compose files in the same order as the original deployment. Where an example shows plain `docker compose`, add `-f compose.yaml -f compose.panel.yaml` if the panel is enabled, followed by any SSH worker or custom overlays in use.

## Verify a reboot

On a systemd server, check that Docker and the manager are configured to start automatically. The packaged Compose manager uses `restart: unless-stopped`. Before rebooting, leave the manager running and wait for active jobs to finish. Include the cloudflared service if you use Cloudflare Tunnel.

```sh
cd "$HOME/runquill"
systemctl is-enabled docker.service
docker compose -f compose.yaml -f compose.panel.yaml ps
docker compose -f compose.yaml -f compose.panel.yaml exec -T manager runquill status
systemctl is-enabled cloudflared.service
```

After rebooting, reconnect over SSH. Replace the sample public hostname with your own. If you use another access method, substitute its service and panel checks.

```sh
cd "$HOME/runquill"
uptime -s
systemctl is-active docker.service cloudflared.service
docker compose -f compose.yaml -f compose.panel.yaml ps
curl -fsS http://127.0.0.1:8080/readyz
curl -fsS -o /dev/null -w '%{http_code}\n' https://runner-ci.example.com/
```

Look for `ready: true` from `/readyz`. [Run the first job](workflows.md#verify-your-first-job) to verify admission, execution and cleanup without manually starting the manager.

## Verify webhook-only delivery

1. Use a test deployment with a working public HTTPS webhook. Record the existing polling interval.
2. Set the interval to `0` in the panel's administrator **Settings**. If the panel has no saved setting, set `RUNQUILL_POLL_INTERVAL=0` in `.env` and recreate the manager with its usual Compose files. A saved panel setting takes precedence over the environment default.
3. Dispatch a new [first job](workflows.md#verify-your-first-job) from a connected private repository. In the GitHub App's **Advanced → Recent deliveries**, find the `queued` event for the same job ID and check for HTTP `202`. In manager logs, check for `source=webhook` and `added=true`, followed by `runner started` and `runner removed` for that job. Verify success and runner removal on GitHub.
4. Restore the previous interval. The default is `30s` when polling is used to recover missed deliveries.

## Troubleshooting

| Symptom | First checks |
| --- | --- |
| GitHub shows a queued job that Runquill never sees | Use `runquill doctor --repo OWNER/REPO` and `runquill diagnose OWNER/REPO` in the manager to check App coverage, repository ownership, the label and recent polling. |
| Runquill sees the job but no runner starts | Check the panel's queue and hosts for free slots, the selected pool and OS, and the last retry reason. |
| Webhook returns `401` | For a GitHub delivery, verify the App and manager have the same webhook secret. A direct unsigned test should return `401`. |
| Tunnel returns `502` or cannot reach the origin | Check `systemctl status cloudflared`, `curl -fsS http://127.0.0.1:8080/readyz`, Compose status, and the route to the panel on port 8081. |
| OAuth callback fails | Check the scheme, host and port against the browser URL, `RUNQUILL_PANEL_URL`, and the registered `/auth/github/callback` on GitHub. |

## Sample panel

Once published, the [GitHub Pages demo](https://yakisoba0728.github.io/Runquill/) shows first-job cleanup, recovery from a missed webhook, and an offline worker with synthetic data. Run `"$HOME/runquill/runquill" panel --demo` and open [127.0.0.1:8081](http://127.0.0.1:8081/) to try the same scenarios locally. From a source checkout, use `go run . panel --demo`.

To publish the sample panel, set **Settings → Pages → Source** to **GitHub Actions** and run [Publish sample panel](../.github/workflows/demo-pages.yml) from `main`. The workflow publishes the demo's HTML, CSS, scripts, and license. Its run summary links to the deployed site. [GitHub Pages setup](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site)

## Web panel and team access

For a live panel, run `sh install.sh --dir "$HOME/runquill" --panel` with a release that supports guided panel setup. The installer starts Compose with the panel overlay. To add it to an install configured by `setup`, run `./runquill setup --panel` from the install directory and follow the Compose commands it prints. For a browser-accessible HTTPS origin, add `--panel-url https://panel.example.com`. The default origin is `http://127.0.0.1:8081`.

Fresh installs with the panel enabled use one GitHub App for runners and sign-in. Setup registers the panel callback, saves the App credentials locally, and enrolls the personal installation owner as the first administrator. For an organization, enter the first administrator's numeric GitHub user ID. The account holder can find it with `gh api user --jq .id`. After the manager starts, use the enrolled account to sign in.

When adding the panel to an older runner-only installation, setup can accept a dedicated [GitHub OAuth App](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/creating-an-oauth-app). For manual configuration, use the panel origin as the OAuth App homepage, append `/auth/github/callback` for its callback, and put the following values in `.env`.

```dotenv
RUNQUILL_PANEL_URL=http://127.0.0.1:8081
RUNQUILL_PANEL_GITHUB_CLIENT_ID=your-oauth-client-id
RUNQUILL_PANEL_GITHUB_CLIENT_SECRET=your-oauth-client-secret
RUNQUILL_PANEL_BOOTSTRAP_ID=12345678
```

Add those variables to your manager configuration and build with the panel overlay. It publishes port 8081 on loopback while port 8080 remains the webhook service.

```sh
RUNQUILL_MANAGER_IMAGE=runquill-manager:local docker compose -f compose.yaml -f compose.panel.yaml up -d --build
```

For release installs, omit the image override and use `--no-build` with the same Compose files. For multi-host Compose installs, include `-f compose.ssh.yaml`. If the manager runs directly, also set `RUNQUILL_PANEL_ADDR=127.0.0.1:8081`. Use an SSH tunnel for remote loopback access. For a shared panel, set `RUNQUILL_PANEL_URL` to an HTTPS origin and forward it to port 8081 through an HTTPS reverse proxy that preserves the public Host header. Use a separate origin for the public webhook.

| Role | Panel access |
| --- | --- |
| Viewer | View the overview, queue, job history, runners, hosts and pools, and usage. Create `fleet:read` API tokens. |
| Operator | Viewer access plus pause and resume host assignments, cancel or retry waiting jobs, read runner logs, and create `hosts:write` and `jobs:write` tokens. |
| Admin | Operator access plus enroll or disable team users, see and revoke every token, change settings, migrate workflows, view backup status, download queue backups, check repositories, and read audit history. |

Team users sign in with their enrolled GitHub account. Administrators add a teammate by GitHub login or numeric account ID. The panel resolves a login to its account ID through GitHub's public users API and accepts only user accounts. Tokens expire after 1–90 days, have one scope, and can be revoked from the panel. The secret is shown only when issued. Send it as `Authorization: Bearer <token>`. A token reaches only the routes of its own scope, so reading and changing the fleet needs two tokens. Tokens never reach runner logs, migration, backups or other administrator routes. The [API description](../api/openapi.yaml) documents every route.

| Scope | Routes |
| --- | --- |
| `fleet:read` | `GET /snapshot`, `GET /api/events`, `GET /api/jobs`, `GET /api/jobs/{id}`, `GET /api/elastic`, `GET /api/usage`, `POST /mcp` |
| `hosts:write` | `POST /api/hosts/cordon` with `{"host":"local","cordoned":true}`. Pausing a host does not interrupt its active runners. |
| `jobs:write` | `POST /api/jobs/{id}/cancel`, `POST /api/jobs/{id}/retry` |

**Job history** lists jobs newest first with their outcome: waiting, running, finished or failed. Filter it by repository, outcome, and the last hour, 24 hours or 7 days. Runquill keeps finished jobs for seven days, and job logs stay on GitHub. `GET /api/jobs` returns the same rows and accepts `repository`, `outcome`, `since` and `until` (Unix seconds), `limit` (up to 100), and the `next` cursor as `before`.

Operators and administrators can act on a job that waits without a runner, in the panel or with a `jobs:write` token. **Retry now** clears a retry delay so the next scheduler pass tries the job. **Cancel job** marks it failed with `cancelled by @LOGIN`, and Runquill starts no runner for it. The run stays queued on GitHub until you cancel or rerun it there.

Audit history records sign-ins, team, token, host, job, settings and backup changes, runner log views and workflow migrations with the actor's login. Filter it by action type or by actor login or ID, and load older entries page by page. Settings changes list each changed field with its old and new value, and omit values whose names suggest a secret.

The panel updates live over Server-Sent Events and falls back to polling when the stream is unavailable. Press Cmd+K or Ctrl+K for the command palette, and pick a light, dark or system theme. The [panel guide](panel.md) describes each screen and who can use it.

Select a job in the panel to see its runner label, current queue or startup explanation, and latest retry reason. The configured GitHub App access scope is shown in **GitHub connection**. The panel displays discovered jobs. For a missing job, use `runquill diagnose OWNER/REPO` to check the App installation, the job's GitHub label and discovery. `runquill status --json` exposes the same job explanation as the optional `diagnostic` object with a `code` and `message`.

## Usage statistics

The **Usage** screen and `GET /api/usage` report finished jobs, failures, runner time and queue wait by UTC day, repository, pool or host. A job counts once, on the UTC day it finished. Failures are jobs Runquill gave up on or that were cancelled in the panel, not GitHub job conclusions. Daily totals are kept for 400 days in the queue database but are not included in queue backups. [Details](panel.md#usage-statistics)

## Runner logs

Operators and administrators can read or follow the container output of a runner that holds a job, from the **Logs** screen. These are the runner process logs, not GitHub step logs. Only runners on the `local` host and `ssh://` hosts are supported, and API tokens cannot read them. Log streaming has not yet been exercised on a real fleet. [Details](panel.md#runner-logs)

## Remote CLI

Run `runquill fleet status`, `jobs list`, `jobs show`, `runners list`, `hosts list`, `pools list`, `usage`, `elastic`, `hosts cordon`, `hosts uncordon`, `jobs cancel` and `jobs retry` from any machine against the panel API. Set `RUNQUILL_URL` to the panel origin and pass a token through `RUNQUILL_TOKEN` or `--token-file`, and add `--json` for scripts. Each command needs the matching token scope, and `audit tail` always needs the web panel because no token can read the audit log. [Remote CLI guide](remote-cli.md)

## MCP for coding agents

`POST /mcp` serves read-only [Model Context Protocol](https://modelcontextprotocol.io/) tools for fleet status, jobs, hosts, usage and elastic decisions. Use a `fleet:read` token over HTTP, or run `runquill mcp` as a local stdio server that forwards to the panel. Browser sessions are always refused. The endpoint has not yet been tried with a real MCP client. [MCP guide](mcp.md)

## Status and upgrades

For fleet-wide queue depth, slot usage, and runner throughput, enable the separate [Prometheus metrics listener](metrics.md).

```sh
docker compose exec manager runquill status
```

The snapshot includes running runners, per-host slot usage and health, and direct GitHub job links. `WAITING` positions follow discovery order among jobs ready to start. [Repository fairness](workers.md#share-capacity-across-repositories) can start them in a different order. `RETRY` shows the time until the next attempt and the latest failure or reconciliation reason when available. When using the standalone binary, run `runquill status` with the manager's queue and runner backend settings.

The operations panel shows queue positions, wait times, and retry reasons. Filter jobs by repository, job ID, or pool. The worker and runner tables show host health, occupied and reserved slots, maintenance state, and observed runtimes. Pool capacity shows available worker slots before the fleet-wide limit. The overview shows how many of those slots the fleet can actually use.

Administrators can open **Settings** to change the fleet limit, per-worker limits, GitHub polling interval, runner image, Docker workers, and pool rules. The settings are saved with the queue, recorded in the audit history, and applied to new assignments without restarting the manager. The deployment supplies the initial defaults. SSH workers use the deployment's SSH credentials, and outbound agents use the deployment's authorization file. Use the setup wizard for GitHub App, panel address, and Tailscale configuration.

Use `docker compose exec -T manager runquill status --json` for scripts and monitoring. Schema version 1 includes `slot_limit`, `runners`, `hosts`, and `jobs`. Occupied slots are observed on reachable workers. Reserved slots account for offline workers and leases without an observed host. If Docker reports a conflicting topology, the snapshot still includes jobs and a `docker_error`, with `runners` set to `null` and a nonzero exit status.

`runquill doctor --json` prints the checks as one JSON document, such as `{"checks":[{"name":"configuration","ok":false,"error":"set RUNQUILL_OWNER"}],"passed":false}`. Passing checks omit `error`. As with the plain output, the command exits with status 1 when any check fails. When a check fails, the plain output ends with a link to [Troubleshooting](#troubleshooting).

If GitHub shows a queued job that Runquill has not started, run `docker compose exec manager runquill diagnose OWNER/REPO`. It checks the App’s access using a repository-scoped read-only token, reports the last recorded poll, and explains invalid labels, retries, and worker capacity for queued jobs.

Use `docker compose exec manager runquill gc --dry-run` to inventory labelled runner containers without a matching queue lease. It shows the Docker host and state, checks GitHub when the queue contains the associated job, and reports incomplete inventory when a host or queue is unavailable. The command only reads containers and queue data.

To clean up, run `docker compose exec manager runquill gc --remove-orphans`. It removes labelled runner containers that have no queue lease and cannot be running a job, and prints every runner it skips with the reason. `--grace` (default `10m`, at least `1m`) leaves runners younger than that alone so a launch in progress is never touched. The command keeps a runner when its lease is recorded on another host, when it is running and the queue has no record of its job, when GitHub reports the job unfinished or the runner busy, or when GitHub cannot be reached. Before removing a runner whose job is in the queue, it deletes the GitHub runner registration, then removes the container by its Docker ID. It works on `local` and `ssh://` hosts only. `agent://` workers and the Kubernetes backend are reported as skipped. The command exits with status 1 if a host or a removal fails.

`docker compose exec manager runquill gc --unused --dry-run` lists runner images that Runquill pulled and cache volumes once no runner has used them for `RUNQUILL_CLEANUP_AFTER` (default `30d`). Without `--dry-run`, it removes them, as the manager also does every `RUNQUILL_CLEANUP_INTERVAL`. `--older-than` sets another retention. Images you pulled or built yourself are never removed. [Cleanup](runner-images.md#cleanup-of-unused-images-and-cache-volumes)

- `RUNQUILL_MAX_RUNNERS` defaults to 2. A failed launch, or a runner that exits without taking its job, retries after 15 seconds and lets later jobs use available slots. Each further failure in a row doubles the wait, up to 10 minutes, so a broken runner image does not use up the GitHub API rate limit. **Retry now** starts again from 15 seconds. GitHub fails self-hosted jobs that remain queued for more than 24 hours. [GitHub runner routing](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#routing-precedence-for-self-hosted-runners)
- Runquill marks a job failed when it has not started within 24 hours, with the reason and the last retry error. A job cancelled in the panel is also failed. A failed job never returns to the queue. The run stays queued on GitHub until you cancel or rerun it there, and a rerun creates a new job. The reason appears in the panel's job history, and expired jobs also log `job failed`.
- `docker compose logs -f manager` shows admissions, cleanup, and API errors. The GitHub workflow retains step output as usual. To adopt a new GitHub runner version in a source install, rerun `docker build --pull -t runquill-runner:local runner`. Running jobs finish on the image they started with. Set `RUNQUILL_LOG_FORMAT` to `text` or `json` for structured logs and `RUNQUILL_LOG_LEVEL` to `debug`, `info`, `warn` or `error`. Without a format, the manager keeps its plain log lines. An invalid value stops the command with status 2.
- `GET /healthz` on port 8080 returns HTTP 200 when the queue database answers. `GET /readyz` also requires a successful scheduler pass within the last 2 minutes and no current GitHub App authentication failure. It returns HTTP 503 otherwise, with a JSON body whose `scheduler` is `starting`, `failing` or `stalled`, and whose `database` and `github_auth` show the other checks. Error details stay in the manager log.
- Runners are ephemeral, repository-scoped JIT runners. In the default pool, each job gets a new container with its own Docker daemon, so `docker build`, `services:` and `container:` work. After the job, the container with its inner images, volumes and work directory is deleted. Named cache volumes for pools with `cache` enabled persist between jobs and follow the cleanup retention on local and SSH hosts.
- Run one manager per fleet. Preserve the Compose `queue` volume during upgrades, including upgrades from the original Python version. A manager restart continues tracking existing runners.
- Runquill polls the GitHub API on startup and waits `RUNQUILL_POLL_INTERVAL` (default `30s`) after each scan. Each scan checks queued and in-progress runs in every eligible repository, giving recent activity priority. The poller rotates requested, waiting, and pending runs so each repository receives a full scan within four successful passes if the repository list stays stable. Webhooks and polling share one queue record per GitHub job ID. Set the interval to `0` to run with webhooks alone.
- Job timing records the first discovery source, successful GitHub runner registration, and GitHub’s reported job start when available. The discovery delay measures GitHub job creation to Runquill discovery when GitHub provides a creation timestamp. The Runquill queue duration measures discovery to first registration, or elapsed wait for a queued job. Missing timestamps and durations are omitted in `status --json`. The job history, `GET /api/jobs/{id}` and `runquill jobs show` also add GitHub's completion time and the execution duration from GitHub's start to completion. [Job stage histograms](metrics.md#job-stage-histograms) aggregate the same stages.

## Upgrade

With a release that supports `--upgrade`, download the new installer and point it at the existing installation directory. The default `--dir` is `./runquill`.

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --upgrade --dir "$HOME/runquill"
```

The installer verifies the checksums, updates changed release files, and moves the prior copies into `.previous/`. Restore the previous release files with `cp -p .previous/* .` from the installation directory. To roll back the manager, also restore the previous image tags and check the release notes for database compatibility. [Back up the queue](#back-up-and-recover-the-queue) before upgrading. `--upgrade` accepts `--version`, `--dir`, `--yes` and `--no-install` and operates on release files only.

The installer does not restart anything. If `.env` names published images, it prints the new `RUNQUILL_MANAGER_IMAGE` and `RUNQUILL_RUNNER_IMAGE` values for you to set, followed by the pull, `docker compose up -d --no-build` and `doctor` commands to run with your usual Compose files. The manager pulls the new runner image onto the `local` host and `ssh://` hosts. On outbound workers, load the new image and upgrade the `runquill worker` binary. On K3s, it prints `kubectl apply -f runquill-k3s.yaml` instead.

For a release without `--upgrade` or a manual install, set both image tags in `.env` to the new release and repeat the pull commands from [Start the manager](install.md#start-the-manager).

For Compose with the panel, apply the new tags using the following commands. Wait for running jobs to finish, keep the `queue` volume attached, and check that pending jobs are still present. Then [run the first job](workflows.md#verify-your-first-job).

```sh
cd "$HOME/runquill"
docker compose -f compose.yaml -f compose.panel.yaml pull manager
docker compose -f compose.yaml -f compose.panel.yaml up -d --no-build
docker compose -f compose.yaml -f compose.panel.yaml exec -T manager runquill doctor
docker compose -f compose.yaml -f compose.panel.yaml exec -T manager runquill status
```

## Add manager settings

Compose passes only the variables listed in `compose.yaml` to the manager. For optional settings such as `RUNQUILL_MAX_PER_REPOSITORY`, `RUNQUILL_SCHEDULER_WORKERS`, `RUNQUILL_LOG_FORMAT`, `RUNQUILL_LOG_LEVEL`, the [notification](#send-notifications) variables, `RUNQUILL_ELASTIC*`, `RUNQUILL_BACKUP_*`, `RUNQUILL_METRICS_*`, `RUNQUILL_IMAGE_PULL*`, `RUNQUILL_RUNNER_IMAGES`, `RUNQUILL_CLEANUP_*`, a [`RUNQUILL_REGISTRY_MIRROR`](runner-images.md#docker-hub-mirror) you run without `compose.mirror.yaml` and [`RUNQUILL_ALLOW_PUBLIC_REPOSITORIES`](workflows.md#run-jobs-from-public-repositories), keep the values in `.env` and save an overlay such as `compose.settings.yaml` beside `compose.yaml`. `--upgrade` replaces `compose.yaml` but leaves this file in place.

```yaml
services:
  manager:
    environment:
      RUNQUILL_MAX_PER_REPOSITORY: ${RUNQUILL_MAX_PER_REPOSITORY:-0}
      RUNQUILL_SCHEDULER_WORKERS: ${RUNQUILL_SCHEDULER_WORKERS:-4}
      RUNQUILL_LOG_FORMAT: ${RUNQUILL_LOG_FORMAT:-}
      RUNQUILL_LOG_LEVEL: ${RUNQUILL_LOG_LEVEL:-}
      RUNQUILL_NOTIFY_URL: ${RUNQUILL_NOTIFY_URL:-}
      RUNQUILL_NOTIFY_FORMAT: ${RUNQUILL_NOTIFY_FORMAT:-}
      RUNQUILL_NOTIFY_COOLDOWN: ${RUNQUILL_NOTIFY_COOLDOWN:-}
      RUNQUILL_NOTIFY_STALL_AFTER: ${RUNQUILL_NOTIFY_STALL_AFTER:-}
      RUNQUILL_IMAGE_PULL: ${RUNQUILL_IMAGE_PULL:-}
      RUNQUILL_IMAGE_PULL_INTERVAL: ${RUNQUILL_IMAGE_PULL_INTERVAL:-}
      RUNQUILL_RUNNER_IMAGES: ${RUNQUILL_RUNNER_IMAGES:-}
      RUNQUILL_CLEANUP_AFTER: ${RUNQUILL_CLEANUP_AFTER:-}
      RUNQUILL_CLEANUP_IMAGES_AFTER: ${RUNQUILL_CLEANUP_IMAGES_AFTER:-}
      RUNQUILL_CLEANUP_CACHE_AFTER: ${RUNQUILL_CLEANUP_CACHE_AFTER:-}
      RUNQUILL_CLEANUP_INTERVAL: ${RUNQUILL_CLEANUP_INTERVAL:-}
      RUNQUILL_ALLOW_PUBLIC_REPOSITORIES: ${RUNQUILL_ALLOW_PUBLIC_REPOSITORIES:-false}
```

Apply it with `docker compose -f compose.yaml -f compose.settings.yaml up -d`, adding your other overlays and `--no-build` for published images. Scheduled backups also need a mounted directory and a passphrase file, so use the overlay in the [backup guide](backups.md#compose). The [elastic limits guide](elastic-limits.md#compose) lists its variables. Pulls from a private registry also need a Docker login mounted into the manager with `DOCKER_CONFIG`, as shown in [private registries](runner-images.md#private-registries). `RUNQUILL_CONTAINER_ENGINE=podman` cannot be used under Compose because the manager image contains only the Docker CLI. On K3s, the manager reads every variable in the `runquill-config` Secret, so add them to `.env`, recreate the Secret, and restart the Deployment.

## Send notifications

Set `RUNQUILL_NOTIFY_URL` to an incoming webhook, such as a Slack webhook, to receive fleet alerts. Treat the URL as a secret because services often embed a token in it. Runquill never logs it. The URL must use HTTPS, or HTTP to a loopback host, and redirects to another host are refused.

| Event | Sent when |
| --- | --- |
| `queue_stalled`, `queue_recovered` | The oldest queued job without a runner has waited longer than `RUNQUILL_NOTIFY_STALL_AFTER` (default `10m`), and again once that wait drops below the threshold. |
| `host_offline`, `host_recovered` | A Docker host, outbound worker or the Kubernetes backend cannot be inventoried, and again once it is back. |
| `job_failed` | Runquill marks a job failed after 24 hours without a start. Panel cancellations do not notify. |

When a per-repository cap is configured, jobs of a repository that already has an active runner are left out of the stalled-queue age, so a cap alone does not raise `queue_stalled`. A condition that persists is repeated after `RUNQUILL_NOTIFY_COOLDOWN` (default `30m`). Recovery is sent once and only after its problem was notified.

`RUNQUILL_NOTIFY_FORMAT=slack` (the default) posts `text` plus `source`, `kind`, `host`, `repository`, `job` (the runner label), `job_id`, `reason`, `waiting_seconds` and `timestamp`. Use `text` for receivers that accept only `{"text": ...}`. Each event gets up to three attempts with a 10-second timeout, retrying on network errors, HTTP 429 and 5xx. Up to 64 events wait in memory, and the oldest is dropped when the queue is full. On shutdown, the manager waits up to 10 seconds for pending deliveries. With [metrics](metrics.md) enabled, the `runquill_notifications_*` counters track the results.

## Back up and recover the queue

Take a consistent SQLite snapshot while the manager is running. Save it outside the queue volume so you can recover from volume loss.

```sh
umask 077
docker compose exec -T manager runquill backup --output /data/queue-backup.sqlite3
docker compose cp manager:/data/queue-backup.sqlite3 ./queue-backup.sqlite3
```

Administrators can also download the same snapshot from the panel. The download shows its progress, can run for up to 30 minutes for a large queue, and does not block other panel operations once the copy is ready.

The versioned snapshot is written with owner-only permissions to a new file. It contains queued jobs, runner leases, host maintenance state, repository scan history, operational settings saved in the panel, and enrolled worker identities and token digests. Treat it as private because it contains repository and job metadata. Keep the GitHub App PEM, deployment configuration and OAuth secrets in your separate secret backup. Panel users, sessions, tokens and audit records are excluded. For a recovered panel, set `RUNQUILL_PANEL_BOOTSTRAP_ID` to the intended administrator's numeric GitHub ID, then enroll the team again and issue fresh tokens.

Stop the previous manager and preserve its queue file. Use the same `RUNQUILL_OWNER` and `RUNQUILL_APP_ID` to prepare a new queue on the destination volume.

```sh
docker compose stop manager
docker compose run --rm --no-deps -e RUNQUILL_DB=/data/recovered.sqlite3 \
    -v "$PWD/queue-backup.sqlite3:/run/queue-backup.sqlite3:ro" \
    manager restore --from /run/queue-backup.sqlite3
```

The prepared queue blocks manager startup. Check old workers and GitHub for runners created after the backup, using `gc --dry-run` against the prepared queue where applicable. Resolve untracked runners and make sure the previous manager stays stopped before activating the recovery. Then set `RUNQUILL_DB=/data/recovered.sqlite3` in `.env` and run:

```sh
docker compose run --rm --no-deps manager restore --activate
docker compose up -d
```

With SSH workers or the panel overlay, use your usual Compose files. With K3s, prepare and activate the new file on the manager PVC while the old manager is stopped, then set the deployment's `RUNQUILL_DB` to the new path. The recovery retains the original file for rollback. Each fleet needs exactly one running manager.

## Schedule, seal and upload backups

Set `RUNQUILL_BACKUP_INTERVAL` and `RUNQUILL_BACKUP_DIR` to take backups automatically, and `RUNQUILL_BACKUP_PASSPHRASE` or `RUNQUILL_BACKUP_PASSPHRASE_FILE` to encrypt them. Administrators can check the newest retained local copy with **Verify latest backup** in the panel. `runquill backup --seal` writes the same sealed format by hand, and `runquill restore --from` opens it with the passphrase. With the `RUNQUILL_BACKUP_S3_*` variables, the manager also copies each backup to an S3-compatible bucket and `runquill restore --from-s3` restores from it. Runquill refuses to upload unsealed backups unless you set `RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true`. [Backup guide](backups.md)

## Uninstall

Cordon the hosts and let running jobs finish first. [Back up the queue](#back-up-and-recover-the-queue) if you might reinstall. From the installation directory, stop the manager with the Compose files you use. `-v` deletes the `queue` volume with every queued job, lease and saved panel setting, and `--rmi all` removes the manager image.

```sh
docker compose -f compose.yaml -f compose.panel.yaml down -v --rmi all
```

Compose does not manage runner containers, cache volumes or the runner image. Check each Docker host, including SSH and outbound workers, and remove what remains. Use the image named by `RUNQUILL_RUNNER_IMAGE`, any pool `image`, and the catalogue images that jobs selected.

```sh
docker ps -a --filter label=io.runquill.runner=1
docker rm -f $(docker ps -aq --filter label=io.runquill.runner=1)
docker volume rm $(docker volume ls -q -f label=io.runquill.cache=1)
docker image rm ghcr.io/yakisoba0728/runquill-runner:YOUR_RELEASE_TAG
```

Then delete the installation directory, which holds `.env`, the GitHub App key and any token files. Turn off a Tailscale Funnel or Serve listener you created, and stop `runquill worker` on outbound workers. If you used Cloudflare Tunnel, remove only the DNS record and routes for this deployment. Keep any tunnel shared by other services.

The installer's host changes stay in place. To undo them, remove your user from the `docker` group with `sudo gpasswd -d "$USER" docker`. Remove Docker by following [Docker's uninstall steps](https://docs.docker.com/engine/install/ubuntu/#uninstall-docker-engine) for your distribution, including `/etc/apt/keyrings/docker.asc` and `/etc/apt/sources.list.d/docker.sources`, or `/etc/yum.repos.d/docker-ce.repo`.

On K3s, `kubectl delete -f runquill-k3s.yaml` removes both Runquill namespaces with the manager, runner Pods, Secrets and the queue PVC.

Finally, delete the GitHub App in **Settings → Developer settings → GitHub Apps → *your App* → Advanced → Delete GitHub App**, under the organization's settings for an organization App. This uninstalls it everywhere. Delete a separate panel OAuth App the same way, and remove any leftover `runquill-` runners in the repository's **Settings → Actions → Runners**. [Deleting a GitHub App](https://docs.github.com/en/apps/maintaining-github-apps/deleting-a-github-app)

## Feature status

The Go tests cover the features described in the Runquill documentation, and CI runs real containers in the nested Docker test and the K3s lifecycle check. The features below are implemented and tested only against fakes or recorded output. They have not yet been run in a real environment:

- Podman and rootless container engines
- workflow migration against real GitHub repositories
- the MCP endpoint and `runquill mcp` with a real MCP client
- scheduler parallelism (`RUNQUILL_SCHEDULER_WORKERS`) on K3s, SSH and outbound workers
- elastic limits on a real Docker host under load (they apply to the `local` host only)
- runner log streaming from a real fleet
- the tool cache with a real `actions/setup-*` job
- on-demand runner image pulls on `ssh://` hosts
- runner image pulls from private registries
- runner OS selection (`os__NAME__`) with outbound `agent://` workers

Automatic runner image pulls and the cleanup against real Docker, all five OS variants started as runners, and the tool cache directory persisting per repository have been exercised on a developer machine, but not yet on a production fleet.

Sealed scheduled uploads and restores have been exercised against an authenticated local SeaweedFS service. [S3 smoke test](backups.md#upload-to-s3-compatible-storage)

Report problems with these features as issues.
