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

The installer verifies release downloads, runs browser setup, pulls matching images, and starts Compose in `./runquill`. Use `--dir PATH` for a different directory, `--version v0.1.0` to pin a release, or `--download-only` to download the binary and Compose files first.

### Create and install the GitHub App

Run `setup` from the directory containing `compose.yaml`. To use a source checkout, build the setup binary with Go 1.26 or later.

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

For a fresh App, use `sh install.sh --webhook-url https://runquill.example.com/webhook` or pass the same option to `runquill setup`. Setup subscribes to `workflow_job`, stores GitHub's webhook secret, and sets the polling interval to zero. Set `RUNQUILL_POLL_INTERVAL=30s` in `.env` if you want polling alongside webhooks.

For an existing App, subscribe to **Workflow job**, set its public HTTPS webhook URL, and set the same secret in GitHub and `RUNQUILL_WEBHOOK_SECRET`. Forward `/webhook` to the manager on port 8080. With Caddy on the same server, use the following configuration.

```caddyfile
runquill.example.com {
    reverse_proxy /webhook 127.0.0.1:8080
}
```

### Add Docker worker servers

List the Docker engines served by one manager in `.env`. Each worker needs rootful Docker on Linux with cgroup v2, the image named by `RUNQUILL_RUNNER_IMAGE`, and an SSH account allowed to use its Docker socket. Use a dedicated SSH key and verify the worker's host key when populating `known_hosts`.

```dotenv
RUNQUILL_DOCKER_HOSTS=local,ssh://builder@worker.example.com
RUNQUILL_SSH_DIR=/path/to/runquill-ssh
```

Put the dedicated SSH key, `known_hosts`, and an SSH `config` in that directory. Configure the worker with `IdentityFile /root/.ssh/id_ed25519` (or its actual key path), `BatchMode yes` and `StrictHostKeyChecking yes`.

```sh
docker compose -f compose.yaml -f compose.ssh.yaml up -d
docker compose -f compose.yaml -f compose.ssh.yaml exec manager runquill doctor
```

The manager assigns each runner to the least occupied engine and applies `RUNQUILL_MAX_RUNNERS` across the group. If a worker disconnects, it keeps existing leases and pauses scheduling until it reconnects.

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

## Operate and upgrade

```sh
docker compose exec manager runquill status
```

The snapshot includes running runner containers, occupied slots, and direct GitHub job links. `WAITING` positions follow queue order among jobs ready to start. `RETRY` shows the time until the next attempt and the latest failure or reconciliation reason when available. When using the standalone binary, run `runquill status` with the manager's `RUNQUILL_DB` and `RUNQUILL_MAX_RUNNERS` settings.

- `RUNQUILL_MAX_RUNNERS` defaults to 2. Provisioning failures retry after 15 seconds and allow later jobs to use available slots. GitHub fails self-hosted jobs that remain queued for more than 24 hours. [GitHub runner routing](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#routing-precedence-for-self-hosted-runners)
- `docker compose logs -f manager` shows admissions, cleanup, and API errors. The GitHub workflow retains step output as usual.
- Preserve the Compose `queue` volume during upgrades, including upgrades from the original Python version. For published images, update both tags and repeat the pull commands. A manager restart continues tracking existing runners.
- Runquill polls the GitHub API on startup and waits `RUNQUILL_POLL_INTERVAL` (default `30s`) after each scan. The poller discovers queued jobs across the App installation's private repositories, including jobs in an already-running workflow. Webhooks and polling share one queue record per GitHub job ID. Set the interval to `0` to run with webhooks alone.

## Develop and release

Use Go 1.26 or later. The manager builds as a native binary with the Docker CLI as its external runtime dependency.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o runquill .
```

Run the nested Docker smoke test with `RUNQUILL_TEST_DOCKER_IMAGE=runquill-runner:local go test -run '^TestDockerRunner$' -v` after building the runner image. It checks Docker build, PostgreSQL, bind mounts, and removal with a local stand-in for the GitHub listener.

On a version tag, the [release workflow](.github/workflows/release.yml) runs CI, publishes amd64 and arm64 images to GHCR, and attaches the matching Linux binaries and checksums to the release. After the first publish, set both packages to public if they should be anonymously pullable. The workflow publishes to the repository owner's namespace, so forks use their own image names. [GitHub Container registry access](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

Copyright © 2026 Runquill contributors. [Apache License 2.0](LICENSE)
