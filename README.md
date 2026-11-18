# Runquill

English · [한국어](README.ko.md)

Runquill runs GitHub Actions jobs from private repositories in a personal account or a GitHub organization on your Linux Docker servers or K3s cluster. Install one GitHub App, then opt jobs in through `runs-on`. The default **All repositories** installation discovers new private repositories automatically. You can also [connect selected repositories](#connect-selected-repositories).

[Quickstart](#quickstart) · [First job](#verify-your-first-job) · [Sample panel](#sample-panel) · [Team panel](#web-panel-and-team-access) · [Release downloads](https://github.com/yakisoba0728/Runquill/releases)

## Quickstart

1. Install on [Docker](#install-a-public-release) or [K3s](deploy/k3s/README.md). Approve the private GitHub App and install it on your personal account with **All repositories** access.
2. In a private Git checkout, run `"$HOME/runquill/runquill" workflow init` with the binary from your install directory. Commit the generated workflow to the default branch and select **Actions → Runquill first job → Run workflow**. [First-job walkthrough](#verify-your-first-job)
3. Check the workflow log for a runner name starting with `runquill-`. After the job finishes, look for `runner removed` in the manager log or confirm cleanup in the [web panel](#web-panel-and-team-access).

| Backend | Runner and workload support |
| --- | --- |
| [Docker](#install-a-public-release) | One disposable container per job. Supports `docker build`, service containers and job containers. [Add SSH workers](#add-docker-worker-servers) to share the queue across Docker hosts. |
| [K3s](deploy/k3s/README.md), `plain` | One restricted Pod per job. Runs shell and other steps that can execute directly in the runner Pod. |
| [K3s](deploy/k3s/README.md), `dind` | One privileged Docker-in-Docker Pod per job. Opt in for Docker builds, service containers and job containers. |

One manager and its persistent SQLite queue limit runner concurrency across repositories. Each job receives a repository-scoped runner with a fresh filesystem. Workflows can also select GitHub-hosted runners.

**Trust model** · Runquill uses privileged containers for Docker-in-Docker. Run it on a dedicated server and route only trusted code to it. Use a GitHub-hosted runner for workflows that execute untrusted pull request code. Protect the GitHub App key and the manager's Docker socket: the key can administer the repositories granted to the app, and the socket can control the host daemon.

## Install

Use an amd64 or arm64 Linux server with cgroup v2, rootful Docker Engine, Docker Compose, `curl`, and `sha256sum`. Run one manager per group of Docker hosts. The default polling setup works behind NAT with outbound access to GitHub. Use an HTTPS endpoint for webhook delivery.

### Install a public release

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --dir "$HOME/runquill"
```

1. Open the browser link from the installer. Create the private GitHub App and install it on your personal account with **All repositories** access.
2. Wait for the installer to start Compose and report `Preflight passed.`
3. [Run your first job](#verify-your-first-job) from a private repository you own.

The installer verifies release checksums and pulls matching manager and runner images. The command above installs into `$HOME/runquill`. Use `--version v0.2.0` to pin a release, or `--download-only` to fetch the binary and deployment files before setup.

### Create and install the GitHub App

Run `setup` from the directory containing `compose.yaml`. To use a source checkout, build the setup binary with Go 1.26.11 or later and use the latest patch for your Go version.

```sh
CGO_ENABLED=0 go build -o runquill .
./runquill setup
```

For a published release, download `compose.yaml` from its tag and the matching Linux executable and `SHA256SUMS` from the release assets into one directory. Run these commands there on amd64 hardware. On arm64, use `runquill-linux-arm64`.

```sh
sha256sum --check --ignore-missing SHA256SUMS
chmod +x ./runquill-linux-amd64
./runquill-linux-amd64 setup
```

1. Open the browser link printed by `setup`. If your browser is on another machine, use the SSH port-forwarding instructions printed by the command.
2. In GitHub, approve creation of the private App. `setup` saves `.env` and `github-app.pem` with permissions `0600` and prints an installation link.
3. Follow the link and install the App on your personal account with **All repositories** access so Runquill can discover new private repositories.

Rerun `setup` to resume App installation once the files have been saved. The generated configuration uses GitHub API polling and works without a public HTTPS address.

[GitHub App manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest) · [Installing a GitHub App you created](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app)

### Connect selected repositories

For a new installation, use the same setup with the selected repository scope.

```sh
sh install.sh --dir "$HOME/runquill" --repository-scope selected
# With a standalone binary in the installation directory:
./runquill setup --repository-scope selected
```

On GitHub, choose **Only select repositories** and select the private repositories you want to use. GitHub stores that list and the setup saves `RUNQUILL_REPOSITORY_SCOPE=selected` in `.env`. Add or remove repositories through the GitHub App installation settings. With polling enabled, the next sweep can discover jobs in newly approved repositories without restarting the manager. With webhook-only configuration, jobs are discovered through deliveries from GitHub. Run `runquill doctor --repo OWNER/REPO` in the manager environment to check coverage before your first run. [Change GitHub App repository access](https://docs.github.com/en/apps/using-github-apps/reviewing-and-modifying-installed-github-apps)

To switch an existing installation between **All repositories** and **Only select repositories**, change its GitHub settings and set `RUNQUILL_REPOSITORY_SCOPE` to the corresponding `all` or `selected` in the manager configuration. Recreate the Compose manager or update the K3s Secret and restart the Deployment. A configuration without this setting defaults to `all`.

### Connect a GitHub organization

Run setup as an organization owner and install the generated App on that organization. Each manager handles one GitHub account. Use a fresh directory and queue when switching to a different account or runner registration level.

```sh
./runquill setup --organization YOUR_ORG --repository-scope selected
```

Pass the same options to the public installer for a Compose deployment. For K3s, add `--k3s` to `setup` as usual. By default, Runquill registers each runner on the repository that requested it, with repository **Actions → Read-only** and **Administration → Read and write** permissions. To use an existing App, set `RUNQUILL_OWNER=YOUR_ORG` and `RUNQUILL_OWNER_TYPE=organization` in `.env`. For panel setup in an organization, enter the GitHub user ID of the person who will administer the panel. [GitHub App manifest registration](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)

To register runners in an organization runner group, create an organization-owned group with **Selected repositories** access and public repository access disabled. Choose a group without GitHub's workflow-level restrictions. Add only private repositories covered by the App and pass `--runner-group-id ID` to setup or the installer. For an existing App, set `RUNQUILL_RUNNER_GROUP_ID=ID` in `.env` and grant **Self-hosted runners → Read and write** under organization permissions. Runquill checks group policy and repository access before registering each runner. Use the configured group name if your workflow specifies a runner group, and keep the job-specific label. GitHub routes organization runners by group access and matching labels, so share a group only among repositories that trust each other's workflows. [GitHub runner groups](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access) · [Organization runner API](https://docs.github.com/en/rest/actions/self-hosted-runners#create-configuration-for-a-just-in-time-runner-for-an-organization)

`runquill doctor --repo YOUR_ORG/REPO` checks App coverage and group membership. Use `runquill diagnose YOUR_ORG/REPO` for queued jobs.

### Start the manager

From a source checkout, build the runner image and start the manager.

```sh
docker build --pull -t runquill-runner:local runner
docker compose up -d --build
```

With a release binary, `setup` writes both image names pinned to its release tag. Run the runner image pull command printed by `setup`, then pull and start the manager.

```sh
docker compose pull manager
docker compose up -d --no-build
```

### Verify your first job

From the root of a private Git repository checkout, generate the [first-job workflow](examples/runquill-first-job.yml). Use the path to your Runquill installation if it differs from the quickstart.

```sh
"$HOME/runquill/runquill" workflow init
```

The command creates `.github/workflows/runquill-first-job.yml` and protects any existing file at that path. If your installed version predates `workflow init` or the binary is on a different machine, copy the linked YAML into `.github/workflows/` yourself.

Commit and push the workflow to the repository's default branch or merge it through your usual review process. In GitHub, open **Actions → Runquill first job → Run workflow**. The **Show the runner** step prints a name beginning with `runquill-`. This workflow needs only a shell, so it checks runner assignment independently of application dependencies. [GitHub manual workflow requirements](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow)

Use these commands on the server from the Runquill installation directory to check App coverage and follow runner removal:

```sh
docker compose exec -T manager runquill doctor --repo OWNER/REPO
docker compose logs -f manager
```

`doctor` checks the local configuration, Docker Engine, runner image, GitHub App installation and permissions, and webhook settings if configured. It exits with status 1 if a check fails. After a successful job, look for `runner removed` and confirm the runner has disappeared from the repository's **Settings → Actions → Runners**. Run the [Docker and PostgreSQL smoke workflow](examples/runquill-smoke.yml) when you need to verify service containers and Docker builds.

### Use an existing GitHub App or configure webhooks

For a manually configured App, select **Only on this account**, grant **Actions → Read-only** and **Administration → Read and write**, and create a private key. Install the App on your personal account. Copy [.env.example](.env.example) to `.env`, fill in your GitHub login, App ID, and key path, and set both files to mode `0600`. Set `RUNQUILL_REPOSITORY_SCOPE=selected` if you chose **Only select repositories**. For polling, disable webhooks in the App settings and leave `RUNQUILL_WEBHOOK_SECRET` empty. [GitHub App installation](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app) · [Repository JIT runner API](https://docs.github.com/en/rest/actions/self-hosted-runners#create-configuration-for-a-just-in-time-runner-for-a-repository)

To use published images with a manual `.env`, set the manager and runner image names from the example to the same release tag. Pull the runner image before starting Compose.

For a fresh App, use `sh install.sh --webhook-url https://runquill.example.com/webhook` or pass the same option to `runquill setup`. Setup subscribes to `workflow_job`, stores GitHub's webhook secret, and sets `RUNQUILL_POLL_INTERVAL=30s` to recover missed deliveries. Set the interval to `0` in `.env` for webhook-only operation.

For an existing App, subscribe to **Workflow job**, set its public HTTPS webhook URL, and set the same secret in GitHub and `RUNQUILL_WEBHOOK_SECRET`. Forward `/webhook` to the manager on port 8080. With Caddy on the same server, use the following configuration.

```caddyfile
runquill.example.com {
    reverse_proxy /webhook 127.0.0.1:8080
}
```

### Use Tailscale Funnel for the webhook

Funnel gives GitHub a public HTTPS address for a manager behind NAT. Run it on the host that publishes the Compose manager at `127.0.0.1:8080`. Enable MagicDNS and HTTPS for your tailnet and allow this node to use Funnel in its policy. The example uses HTTPS port 443. [Tailscale Funnel requirements](https://tailscale.com/docs/features/tailscale-funnel)

```sh
tailscale funnel status
tailscale funnel --bg --https=443 http://127.0.0.1:8080
```

Append `/webhook` to the HTTPS address printed by Funnel. Pass that URL to the installer you downloaded above or to `./runquill setup`. For an existing App, update its webhook URL in GitHub and use its configured webhook secret.

```sh
sh install.sh --webhook-url https://runquill.example-tailnet.ts.net/webhook
```

After Compose starts, request `https://runquill.example-tailnet.ts.net/healthz` from outside the tailnet and expect HTTP 200. Run the smoke workflow and check for HTTP 202 on its queued event in the App's **Advanced → Recent deliveries**. If a delivery times out, use **Redeliver** there. On an existing installation with polling disabled, set `RUNQUILL_POLL_INTERVAL=30s` in `.env` and apply it with your usual Compose command to recover missed jobs automatically. [GitHub App webhook redelivery](https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/redelivering-webhooks#redelivering-github-app-webhooks)

Use the command below to remove this Funnel listener. Funnel exposes the manager's HTTP endpoints over the internet. Keep the setup browser endpoint and Docker sockets on their local or private networks.

```sh
tailscale funnel --https=443 off
tailscale funnel status
```

Workers on your tailnet can use their MagicDNS names in `ssh://` entries if the manager container can resolve those names and reach SSH. [Tailscale Funnel CLI](https://tailscale.com/docs/reference/tailscale-cli/funnel)

### Add Docker worker servers

List the Docker engines served by one manager in `.env`. Each worker needs rootful Docker on Linux with cgroup v2, the image named by `RUNQUILL_RUNNER_IMAGE`, and an SSH account allowed to use its Docker socket. Use a dedicated SSH key and verify the worker's host key when populating `known_hosts`.

```dotenv
RUNQUILL_DOCKER_HOSTS=local,ssh://builder@worker.example.com
RUNQUILL_MAX_RUNNERS=3
RUNQUILL_DOCKER_HOST_LIMITS=local=2,ssh://builder@worker.example.com=1
RUNQUILL_SSH_DIR=/path/to/runquill-ssh
```

Put the dedicated SSH key, `known_hosts`, and an SSH `config` in that directory. Configure the worker with `IdentityFile /root/.ssh/id_ed25519` (or its actual key path), `BatchMode yes` and `StrictHostKeyChecking yes`.

```sh
docker compose -f compose.yaml -f compose.ssh.yaml up -d
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill doctor
```

The manager picks the eligible engine with the fewest occupied slots. `RUNQUILL_MAX_RUNNERS` caps the group and `RUNQUILL_DOCKER_HOST_LIMITS` optionally caps individual engines. Each entry uses the exact host name from `RUNQUILL_DOCKER_HOSTS`.

If a worker disconnects, its runner leases and last observed untracked runner slots count toward the global limit. Healthy workers can take jobs in the remaining slots. A worker that has never been reached reserves its configured limit, or the global limit if it has no host-specific limit.

Use `cordon` before maintenance to stop new assignments to a worker while existing jobs finish. `status` shows the worker's connectivity, mode, and slot usage. Wait for its slots to clear before removing it from the fleet. Run `uncordon` to resume assignments. The queue volume preserves this setting across manager restarts.

```sh
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill host cordon ssh://builder@worker.example.com
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill status
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill host uncordon ssh://builder@worker.example.com
```

For workers behind NAT, add [outbound Docker workers](docs/outbound-workers.md) that connect over HTTPS.

### Route jobs to worker pools

Set `RUNQUILL_WORKER_POOLS` to a JSON object in `.env` to opt in. The keys are pool names and `hosts` are exact entries from `RUNQUILL_DOCKER_HOSTS`. A host belongs to one pool. Unlisted hosts belong to `default`. Pool settings can apply to `default` too.

```dotenv
RUNQUILL_WORKER_POOLS='{"build":{"hosts":["ssh://builder@worker.example.com"],"repositories":["you/project"],"cpus":4,"memory_mib":8192}}'
```

The `repositories` list controls which repositories can use the pool. Omit it for access by any eligible repository. An empty list grants access to none. `cpus` and `memory_mib` set Docker limits for each new runner. Omit them for Docker’s defaults. Use the optional `image` key to select a different Runquill-compatible image for a pool. Build or pull that image on each host in the pool before restarting the manager. The global runner limit and per-host limits still apply.

Express the pool in the suffix of the unique job label. A label without the pool prefix uses `default`. For a matrix, include `strategy.job-index` as usual.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-pool__build__compile-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

Waiting jobs use slots in their requested pool, and available pools can advance while another is full. For workloads with different trust requirements, deploy workers on dedicated machines and limit repository access to each pool.

## Choose a runner

The [smoke workflow](examples/runquill-smoke.yml) runs on Runquill by default. On **Run workflow**, select `github_hosted` to use `ubuntu-latest`. The [parallel jobs example](examples/runquill-parallel.yml) applies the same switch to one standalone job and two matrix jobs. Copy it into `.github/workflows/` and run it to see the runner name in each job's log.

Build each label from the run ID, the run attempt, and a suffix unique to the job within that run. Write the job ID literally in the suffix. For example, the `check` job uses:

```yaml
runs-on: ${{ format('runquill-{0}-{1}-check', github.run_id, github.run_attempt) }}
```

Append `strategy.job-index` to the matrix job's own suffix. GitHub starts the index at zero, so the `test` matrix produces `test-0` and `test-1`. A rerun uses a new `run_attempt`.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-test-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

Pass the expression as the job's single `runs-on` label. Start the suffix with a letter or digit, then use letters, digits, hyphens, or underscores. Keep the whole label within 100 characters. Runquill registers each ephemeral runner with its job's label so GitHub can match parallel jobs to their respective runners. [GitHub Actions contexts](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts)

### Reuse Docker build layers across jobs

Copy the [cached build example](examples/runquill-build-cache.yml) into `.github/workflows/` in a repository with a root `Dockerfile`. It builds the committed Git revision on the default branch and keeps the resulting image available for later steps in the same job. A new runner can restore layers through the GitHub Actions cache. Choose a distinct `scope` for each image and use it in both cache settings. Cache export failures leave the build usable. [Docker GitHub Actions cache](https://docs.docker.com/build/cache/backends/gha/)

The example pins both actions to exact commits, uses read-only repository permissions, and writes cache from the default branch. Pass build secrets through BuildKit secret mounts so they stay out of cached image layers. [Docker build secrets](https://docs.docker.com/build/building/secrets/)

## Run on K3s

Deploy the manager and its SQLite volume with Kubernetes and provision one Pod per job across your nodes. Follow the [K3s guide](deploy/k3s/README.md) for installation, resource limits, and runner modes.

## Operate and upgrade

### Sample panel

Once published, the [GitHub Pages demo](https://yakisoba0728.github.io/Runquill/) shows first-job cleanup, recovery from a missed webhook, and an offline worker with synthetic data. Run `"$HOME/runquill/runquill" panel --demo` and open [127.0.0.1:8081](http://127.0.0.1:8081/) to try the same scenarios locally. From a source checkout, use `go run . panel --demo`.

To publish the sample panel, set **Settings → Pages → Source** to **GitHub Actions** and run [Publish sample panel](.github/workflows/demo-pages.yml) from `main`. The workflow publishes the demo's HTML, CSS, scripts, and license. Its run summary links to the deployed site. [GitHub Pages setup](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site)

### Web panel and team access

For a live panel, run `sh install.sh --dir "$HOME/runquill" --panel` with a release that supports guided panel setup. The installer starts Compose with the panel overlay. To add it to an install configured by `setup`, run `./runquill setup --panel` from the install directory and follow the Compose commands it prints. For a browser-accessible HTTPS origin, add `--panel-url https://panel.example.com`. The default origin is `http://127.0.0.1:8081`.

Create a dedicated [GitHub OAuth App](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/creating-an-oauth-app) with the homepage and callback URLs shown by setup. Enter its client ID and client secret in the local setup form. Confirm the first administrator's numeric GitHub account ID, which defaults to the runner App installation owner when available. The account holder can look up their ID with `gh api user --jq .id`. The runner GitHub App continues to handle runner provisioning. After the manager starts, sign in with the enrolled GitHub account to verify the OAuth callback end to end.

For v0.2.0 and manual installs, register the panel origin as the OAuth App homepage and append `/auth/github/callback` for the authorization callback. Put the following values in `.env`.

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
| Viewer | View job, runner, and host status. Create a scoped fleet-reader API token. |
| Operator | Viewer access plus pause and resume host assignments. |
| Admin | Operator access plus enroll or disable team users, manage tokens, and read audit history. |

Team users sign in with their enrolled GitHub account. Tokens expire after 1–90 days, have one scope, and can be revoked from the panel. The secret is shown only when issued. Send `Authorization: Bearer <token>` with `GET /snapshot` for `fleet:read` or `POST /api/hosts/cordon` for `hosts:write`. The latter accepts `{"host":"local","cordoned":true}`. You can pause new work on a host without interrupting its active runners. Audit history records sign-ins and management changes.


Select a job in the panel to see its runner label, current queue or startup explanation, and latest retry reason. The configured GitHub App access scope is shown in **GitHub connection**. The panel displays discovered jobs. For a missing job, use `runquill diagnose OWNER/REPO` to check the App installation, the job's GitHub label and discovery. `runquill status --json` exposes the same job explanation as the optional `diagnostic` object with a `code` and `message`.

### Status and upgrades

For fleet-wide queue depth, slot usage, and runner throughput, enable the separate [Prometheus metrics listener](docs/metrics.md).

```sh
docker compose exec manager runquill status
```

The snapshot includes running runners, per-host slot usage and health, and direct GitHub job links. `WAITING` positions follow queue order among jobs ready to start. `RETRY` shows the time until the next attempt and the latest failure or reconciliation reason when available. When using the standalone binary, run `runquill status` with the manager's queue and runner backend settings.

Use `docker compose exec -T manager runquill status --json` for scripts and monitoring. Schema version 1 includes `slot_limit`, `runners`, `hosts`, and `jobs`. Occupied slots are observed on reachable workers. Reserved slots account for offline workers and leases without an observed host. If Docker reports a conflicting topology, the snapshot still includes jobs and a `docker_error`, with `runners` set to `null` and a nonzero exit status.

If GitHub shows a queued job that Runquill has not started, run `docker compose exec manager runquill diagnose OWNER/REPO`. It checks the App’s access using a repository-scoped read-only token, reports the last recorded poll, and explains invalid labels, retries, and worker capacity for queued jobs.

Use `docker compose exec manager runquill gc --dry-run` to inventory labelled runner containers without a matching queue lease. It shows the Docker host and state, checks GitHub when the queue contains the associated job, and reports incomplete inventory when a host or queue is unavailable. The command only reads containers and queue data.

- `RUNQUILL_MAX_RUNNERS` defaults to 2. Provisioning failures retry after 15 seconds and allow later jobs to use available slots. GitHub fails self-hosted jobs that remain queued for more than 24 hours. [GitHub runner routing](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#routing-precedence-for-self-hosted-runners)
- `docker compose logs -f manager` shows admissions, cleanup, and API errors. The GitHub workflow retains step output as usual.
- Preserve the Compose `queue` volume during upgrades, including upgrades from the original Python version. For published images, update both tags and repeat the pull commands. A manager restart continues tracking existing runners.
- Runquill polls the GitHub API on startup and waits `RUNQUILL_POLL_INTERVAL` (default `30s`) after each scan. Each scan checks queued and in-progress runs in every eligible repository, giving recent activity priority. The poller rotates requested, waiting, and pending runs so each repository receives a full scan within four successful passes if the repository list stays stable. Webhooks and polling share one queue record per GitHub job ID. Set the interval to `0` to run with webhooks alone.
- Job timing records the first discovery source, successful GitHub runner registration, and GitHub’s reported job start when available. The discovery delay measures GitHub job creation to Runquill discovery when GitHub provides a creation timestamp. The Runquill queue duration measures discovery to first registration, or elapsed wait for a queued job. Missing timestamps and durations are omitted in `status --json`.

### Back up and recover the queue

Take a consistent SQLite snapshot while the manager is running. Save it outside the queue volume so you can recover from volume loss.

```sh
umask 077
docker compose exec -T manager runquill backup --output /data/queue-backup.sqlite3
docker compose cp manager:/data/queue-backup.sqlite3 ./queue-backup.sqlite3
```

The versioned snapshot is written with owner-only permissions to a new file. It contains queued jobs, runner leases, host maintenance state, and repository scan history. Treat it as private because it contains repository and job metadata. Keep the GitHub App PEM, configuration and OAuth secrets in your separate secret backup. Panel users, sessions, tokens and audit records are excluded. For a recovered panel, set `RUNQUILL_PANEL_BOOTSTRAP_ID` to the intended administrator's numeric GitHub ID, then enroll the team again and issue fresh tokens.

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

## Develop and release

Use Go 1.26.11 or later. The manager builds as a native binary with the Docker CLI as its external runtime dependency.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o runquill .
```

The runner Dockerfile pins the upstream GitHub Actions runner image by digest. Use `--build-arg RUNQUILL_RUNNER_BASE=IMAGE` to extend a compatible image with your own toolchain. Keep the runner files in `/home/runner` and its user at UID 1001 for Kubernetes plain mode.

Run the nested Docker smoke test with `RUNQUILL_TEST_DOCKER_IMAGE=runquill-runner:local go test -run '^TestDockerRunner$' -v` after building the runner image. It checks Docker build, PostgreSQL, bind mounts, and removal with a local stand-in for the GitHub listener.

On amd64, [CI](.github/workflows/ci.yml) also starts a disposable K3s cluster. It checks that pausing assignments leaves jobs queued, resuming starts their runners, and job completion removes the runner Pods and credential Secrets. The [two-account OAuth check](panel_live_oauth_test.go) can be run locally with a dedicated OAuth App and separate GitHub accounts for the administrator and viewer.

On a version tag, the [release workflow](.github/workflows/release.yml) runs CI, publishes amd64 and arm64 images to GHCR, and attaches the matching Linux binaries and checksums to the release. After publication in this repository, it verifies the exact tag with the public installer in clean Linux containers on both architectures. After the first publish, set both packages to public if they should be anonymously pullable. The workflow publishes to the repository owner's namespace, so forks use their own image names. [GitHub Container registry access](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

Copyright © 2026 Runquill contributors. [Apache License 2.0](LICENSE)
