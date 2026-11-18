# Runquill

English · [한국어](README.ko.md)

Runquill starts short-lived GitHub Actions runners on your Linux server for private repositories owned by your personal GitHub account. Install one GitHub App with **All repositories** access, then opt jobs in through `runs-on`. New private repositories use the same installation.

- Each job receives a repository-scoped runner with a fresh filesystem and its own Docker daemon. Workflows can use `docker build`, service containers, and job containers.
- A SQLite queue limits concurrent runners across repositories and provisions them in job discovery order. The queue and runner records survive a manager restart.
- A workflow can select a GitHub-hosted runner for any job.

**Trust model** · Runquill uses privileged containers for Docker-in-Docker. Run it on a dedicated server and route only trusted code to it. Use a GitHub-hosted runner for workflows that execute untrusted pull request code. Protect the GitHub App key and the manager's Docker socket: the key can administer the repositories granted to the app, and the socket can control the host daemon.

## Install

Use an amd64 or arm64 Linux server with cgroup v2, rootful Docker Engine, and Docker Compose. Run one manager per group of Docker hosts.

### Install a public release

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh
```

The installer verifies release downloads, runs browser setup, pulls matching images, and starts Compose in `./runquill`. Use `--dir PATH` for a different directory, `--version v0.1.2` to pin a release, or `--download-only` to download the binary and Compose files first.

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

Run the preflight in the manager's environment.

```sh
docker compose exec manager runquill doctor
```

Add `--repo OWNER/REPO` to verify App installation coverage for a particular repository. The command checks the local configuration, Docker Engine, runner image, GitHub App installation and permissions. When a webhook secret is set, it also checks the event subscription and webhook configuration. It exits with status 1 if a check fails.

To verify the full lifecycle, create a private repository after installing the App. Copy [the smoke workflow](examples/runquill-smoke.yml) to `.github/workflows/` and run it with the default settings. Check for successful Docker and PostgreSQL steps in GitHub Actions, then confirm the manager logs `runner removed` and that the runner has disappeared from the repository's **Settings → Actions → Runners**.

### Use an existing GitHub App or configure webhooks

For a manually configured App, select **Only on this account**, grant **Actions → Read-only** and **Administration → Read and write**, and create a private key. Install the App on your personal account with **All repositories** access. Copy [.env.example](.env.example) to `.env`, fill in your GitHub login, App ID, and key path, and set both files to mode `0600`. For polling, disable webhooks in the App settings and leave `RUNQUILL_WEBHOOK_SECRET` empty. [GitHub App installation](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app) · [Repository JIT runner API](https://docs.github.com/en/rest/actions/self-hosted-runners#create-configuration-for-a-just-in-time-runner-for-a-repository)

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

### Route jobs to worker pools

Set `RUNQUILL_WORKER_POOLS` to a JSON object in `.env` to opt in. The keys are pool names and `hosts` are exact entries from `RUNQUILL_DOCKER_HOSTS`. A host belongs to one pool. Unlisted hosts belong to `default`. Pool settings can apply to `default` too.

```dotenv
RUNQUILL_WORKER_POOLS='{"build":{"hosts":["ssh://builder@worker.example.com"],"repositories":["you/project"],"cpus":4,"memory_mib":8192}}'
```

The `repositories` list controls which repositories can use the pool. Omit it for access by any eligible repository. An empty list grants access to none. `cpus` and `memory_mib` set Docker limits for each new runner. Omit them for Docker’s defaults. The global runner limit and per-host limits still apply.

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

## Operate and upgrade

```sh
docker compose exec manager runquill status
```

The snapshot includes running runner containers, per-host slot usage and health, and direct GitHub job links. `WAITING` positions follow queue order among jobs ready to start. `RETRY` shows the time until the next attempt and the latest failure or reconciliation reason when available. When using the standalone binary, run `runquill status` with the manager's queue and Docker environment settings.

Use `docker compose exec -T manager runquill status --json` for scripts and monitoring. Schema version 1 includes `slot_limit`, `runners`, `hosts`, and `jobs`. Occupied slots are observed on reachable workers. Reserved slots account for offline workers and leases without an observed host. If Docker reports a conflicting topology, the snapshot still includes jobs and a `docker_error`, with `runners` set to `null` and a nonzero exit status.

If GitHub shows a queued job that Runquill has not started, run `docker compose exec manager runquill diagnose OWNER/REPO`. It checks the App’s access using a repository-scoped read-only token, reports the last recorded poll, and explains invalid labels, retries, and worker capacity for queued jobs.

- `RUNQUILL_MAX_RUNNERS` defaults to 2. Provisioning failures retry after 15 seconds and allow later jobs to use available slots. GitHub fails self-hosted jobs that remain queued for more than 24 hours. [GitHub runner routing](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#routing-precedence-for-self-hosted-runners)
- `docker compose logs -f manager` shows admissions, cleanup, and API errors. The GitHub workflow retains step output as usual.
- Preserve the Compose `queue` volume during upgrades, including upgrades from the original Python version. For published images, update both tags and repeat the pull commands. A manager restart continues tracking existing runners.
- Runquill polls the GitHub API on startup and waits `RUNQUILL_POLL_INTERVAL` (default `30s`) after each scan. Each scan checks queued and in-progress runs in every eligible repository, giving recent activity priority. The poller rotates requested, waiting, and pending runs so each repository receives a full scan within four successful passes if the repository list stays stable. Webhooks and polling share one queue record per GitHub job ID. Set the interval to `0` to run with webhooks alone.
- Job timing records the first discovery source, successful GitHub runner registration, and GitHub’s reported job start when available. The discovery delay measures GitHub job creation to Runquill discovery when GitHub provides a creation timestamp. The Runquill queue duration measures discovery to first registration, or elapsed wait for a queued job. Missing timestamps and durations are omitted in `status --json`.

## Develop and release

Use Go 1.26.11 or later. The manager builds as a native binary with the Docker CLI as its external runtime dependency.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o runquill .
```

Run the nested Docker smoke test with `RUNQUILL_TEST_DOCKER_IMAGE=runquill-runner:local go test -run '^TestDockerRunner$' -v` after building the runner image. It checks Docker build, PostgreSQL, bind mounts, and removal with a local stand-in for the GitHub listener.

On a version tag, the [release workflow](.github/workflows/release.yml) runs CI, publishes amd64 and arm64 images to GHCR, and attaches the matching Linux binaries and checksums to the release. After the first publish, set both packages to public if they should be anonymously pullable. The workflow publishes to the repository owner's namespace, so forks use their own image names. [GitHub Container registry access](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

Copyright © 2026 Runquill contributors. [Apache License 2.0](LICENSE)
