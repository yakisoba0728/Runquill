# Runquill

English · [한국어](README.ko.md)

Runquill runs GitHub Actions jobs from private repositories in a personal account or a GitHub organization on your Linux Docker servers or K3s cluster. Install one GitHub App, then opt jobs in through `runs-on`. The default **All repositories** installation discovers new private repositories automatically. You can also [connect selected repositories](#connect-selected-repositories).

[Quickstart](#quickstart) · [First job](#verify-your-first-job) · [Sample panel](#sample-panel) · [Team panel](#web-panel-and-team-access) · [Remote CLI](docs/remote-cli.md) · [Release downloads](https://github.com/yakisoba0728/Runquill/releases)

## Quickstart

1. Install on [Docker](#install-a-public-release) or [K3s](deploy/k3s/README.md). Approve the private GitHub App and install it on your personal account with **All repositories** access.
2. In a private Git checkout, run `"$HOME/runquill/runquill" workflow init` with the binary from your install directory. Commit the generated workflow to the default branch and select **Actions → Runquill first job → Run workflow**. [First-job walkthrough](#verify-your-first-job)
3. Check the workflow log for a runner name starting with `runquill-`. After the job finishes, look for `runner removed` in the manager log or confirm cleanup in the [web panel](#web-panel-and-team-access).

| Backend | Runner and workload support |
| --- | --- |
| [Docker](#install-a-public-release) | One disposable container per job. Supports `docker build`, service containers and job containers unless its [pool](#route-jobs-to-worker-pools) disables `privileged`. [Add SSH workers](#add-docker-worker-servers) to share the queue across Docker hosts. |
| [K3s](deploy/k3s/README.md), `plain` | One restricted Pod per job. Runs shell and other steps that can execute directly in the runner Pod. |
| [K3s](deploy/k3s/README.md), `dind` | One privileged Docker-in-Docker Pod per job. Opt in for Docker builds, service containers and job containers. |

One manager and its persistent SQLite queue limit runner concurrency across repositories. Each job receives a repository-scoped runner with a fresh filesystem, except for the per-repository cache directory of a Docker [worker pool with `cache` enabled](#route-jobs-to-worker-pools). Workflows can also select GitHub-hosted runners.

**Trust model** · Runquill uses privileged containers for Docker-in-Docker by default. Run it on a dedicated server and route only trusted code to it. Use a GitHub-hosted runner for workflows that execute untrusted pull request code. Protect the GitHub App key and the manager's Docker socket: the key can administer the repositories granted to the app, and the socket can control the host daemon.

## Install

Use an amd64 or arm64 Linux server with cgroup v2. Download the installer and run it on that server. It prepares the host, downloads the release and opens the setup wizard:

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --dir "$HOME/runquill" --wizard
```

Run one manager per group of Docker hosts. The default polling setup works behind NAT with outbound access to GitHub. Use an HTTPS endpoint for webhook delivery.

### Prepare the host

Before downloading, the installer checks the host and prints one line per item:

- OS and architecture
- cgroup v2
- the tools it uses: `curl`, `sha256sum`, `awk`, `grep`, `sed`, `cmp`, `mktemp` and `df`
- Docker Engine, the Compose v2 plugin, the daemon and whether it starts at boot
- whether your user can reach the daemon, and whether Docker is rootless
- free space where Docker stores data, with a warning below 20 GB
- HTTPS to github.com and ghcr.io

It also suggests a runner count and per-runner CPU and memory limits for the host.

When something is missing, the installer lists every command it will run and asks once. It uses `sudo` only for those commands, and not at all when you run it as root.

| Missing | What the installer runs |
| --- | --- |
| Tools | The distribution's package manager: `apt-get`, `dnf`, `yum`, `zypper` or `apk` |
| Docker Engine and Compose on Ubuntu or Debian | Docker's apt repository, with its key in `/etc/apt/keyrings/docker.asc` and the source in `/etc/apt/sources.list.d/docker.sources` |
| Docker Engine and Compose on Fedora, RHEL, CentOS, Rocky Linux or AlmaLinux | Docker's dnf repository in `/etc/yum.repos.d/docker-ce.repo` |
| Docker Engine and Compose on Alpine | Alpine's `docker` and `docker-cli-compose` packages. Docker's repository does not cover Alpine |
| Daemon | `systemctl enable --now docker`, or `rc-update` and `rc-service` on OpenRC |
| Docker access | `usermod -aG docker $USER` |

- **Key check.** Before trusting Docker's signing key, the installer checks that it has Docker's published fingerprint.
- **Docker group.** Membership gives root-equivalent access to the host. The installer continues the same run with the new group active, so you do not need to log in again. Shells that were already open need a new login before `docker` works without `sudo`.
- **What it leaves alone.** It never replaces or upgrades a working Docker, and it does not change a rootless or snap Docker. It never opens ports or changes the firewall. On other distributions it links to Docker's install guide instead.
- **Reruns.** Running it again on a ready host changes nothing and asks nothing. A host without systemd or OpenRC must start `dockerd` itself. The installer says so and stops.

| Flag | Effect |
| --- | --- |
| `--yes`, `-y` | Make the listed changes without asking, for unattended installs |
| `--no-install` | Never change the system. Fail and list what is missing |
| `--prepare-only` | Prepare the host and stop |
| `--no-docker` | Skip the Docker checks, for example on a K3s host |
| `--download-only` | Skip the host checks. Download and verify the release only |

Without a terminal and without `--yes`, the installer changes nothing and prints the command to rerun with `--yes`.

### Prepare the host manually

To prepare the host yourself, install Docker Engine and the Compose plugin with [Docker's guide](https://docs.docker.com/engine/install/). Then start Docker and give your user access:

```sh
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"
```

Log out and back in. Then `sh install.sh --prepare-only --no-install` checks the host without changing it.

### Set up in a browser

Run `sh install.sh --dir "$HOME/runquill" --wizard` to connect GitHub, configure panel sign-in, deploy the manager, and download the first workflow in one browser session. The wizard supports English and Korean, plus optional Tailscale Serve and Funnel on a Docker Compose host. [Browser setup](docs/browser-setup.md)

The wizard listens only on `127.0.0.1`. Over SSH, the installer chooses the port and prints a command to run on your own computer, for example:

```text
ssh -L 18080:127.0.0.1:18080 alice@203.0.113.10
```

Keep that session open and open the printed `http://127.0.0.1:18080/?key=…` URL in your local browser. Use the same port number on both sides. The `key` is a secret for this setup session. If you reach the server under another name or through NAT, use that address in the `ssh` command.

### Install a public release

To set up from the terminal instead of the browser, leave out `--wizard`:

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --dir "$HOME/runquill"
```

1. Open the browser link from the installer. Create the private GitHub App and install it on your personal account with **All repositories** access.
2. Wait for the installer to start Compose and report `Preflight passed.`
3. [Run your first job](#verify-your-first-job) from a private repository you own.

The installer verifies release checksums and pulls matching manager and runner images. The command above installs into `$HOME/runquill`. Use `--version v0.3.2` to pin a release, or `--download-only` to fetch the binary and deployment files before setup.

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

With a release binary, `setup` writes both image names pinned to its release tag. The manager pulls a missing runner image onto the `local` host and `ssh://` hosts itself, so the runner image pull command printed by `setup` is optional and only gets the image ready before the first job. Pull and start the manager.

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

To use published images with a manual `.env`, set the manager and runner image names from the example to the same release tag. The manager pulls the runner image when a host lacks it.

For an existing App, subscribe to **Workflow job**, set its public HTTPS webhook URL, and set the same secret in GitHub and `RUNQUILL_WEBHOOK_SECRET`. Generate the secret with `openssl rand -hex 32`. Forward `/webhook` to the manager on port 8080. With Caddy on the same server, use the following configuration.

```caddyfile
runquill.example.com {
    reverse_proxy /webhook 127.0.0.1:8080
}
```

### Connect an HTTPS webhook

For a fresh App, use `sh install.sh --webhook-url https://runquill.example.com/webhook` or pass the same option to `runquill setup`. The App subscribes to `workflow_job`, and setup stores GitHub's webhook secret and sets `RUNQUILL_POLL_INTERVAL=30s` to recover missed deliveries. Forward `/webhook` to the local manager through your HTTPS proxy as shown above. Set the interval to `0` in `.env` for webhook-only operation.

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

List the Docker engines served by one manager in `.env`. Each worker needs rootful Docker on Linux with cgroup v2 and an SSH account allowed to use its Docker socket. The manager pulls the image named by `RUNQUILL_RUNNER_IMAGE` onto each worker over that connection. Use a dedicated SSH key and verify the worker's host key when populating `known_hosts`.

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

The `repositories` list controls which repositories can use the pool. Omit it for access by any eligible repository. An empty list grants access to none. `cpus` and `memory_mib` set Docker limits for each new runner. Omit them for Docker’s defaults. Use the optional `image` key to select a different Runquill-compatible image for a pool. The manager pulls it on each host in the pool. Build it on those hosts yourself if it exists in no registry. The global runner limit and per-host limits still apply.

Express the pool in the suffix of the unique job label. A label without the pool prefix uses `default`. For a matrix, include `strategy.job-index` as usual.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-pool__build__compile-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

Waiting jobs use slots in their requested pool, and available pools can advance while another is full. For workloads with different trust requirements, deploy workers on dedicated machines and limit repository access to each pool.

A label that names an unconfigured pool, or a repository the pool does not allow, keeps the job waiting with a retry reason such as `worker pool "build" is not configured`. Fix the pool in the panel's **Settings** and the job starts on a later pass without a manager restart. Only the 24-hour expiry fails it.

Pools accept four more optional keys.

```dotenv
RUNQUILL_WORKER_POOLS='{"lint":{"hosts":["local"],"privileged":false,"cache":true,"max_per_repository":2,"images":["ubuntu22.04"]}}'
```

- `privileged` defaults to `true`, which starts each runner with `--privileged` and its own Docker daemon. Set it to `false` to run the runner as UID 1001 with all capabilities dropped and `no-new-privileges`. Those jobs cannot use `docker`, service containers, job containers or `sudo`. A custom `image` for such a pool needs a runner user with UID 1001 that owns `/home/runner`.
- `cache: true` mounts a named Docker volume `runquill-cache-<repository ID>` at `/home/runner/.cache`. Later jobs from the same repository on the same host reuse its contents, and concurrent jobs share it. Every job of that repository in the pool can read and change the cache, so it no longer starts with a fully fresh filesystem. The runner also points `RUNNER_TOOL_CACHE` into this volume, so toolchains that `actions/setup-*` download are kept too. Each host keeps its own volumes, and the manager removes a volume that no runner has used for 30 days ([cleanup](docs/runner-images.md#cleanup-of-unused-images-and-cache-volumes)). List them with `docker volume ls -f label=io.runquill.cache=1`. This cache is separate from [Docker build layers](#reuse-docker-build-layers-across-jobs).
- `max_per_repository` caps how many runners one repository can hold in that pool. It overrides `RUNQUILL_MAX_PER_REPOSITORY` for the pool. Without either value, a repository can use every free slot.
- `images` lists the [runner OS](#choose-the-runner-os) names that jobs in the pool may select with `os__NAME__`. Omit it to allow every name.

[Outbound workers](docs/outbound-workers.md) in a pool with `privileged: false` or `cache: true` must run the same Runquill release as the manager. Older agents reject runner tasks that carry these settings, so upgrade the workers along with the manager.

The administrator panel's pool editor has checkboxes for Docker-in-Docker and the shared cache, and a field for the allowed runner OS names. It keeps each pool's `max_per_repository` when saving but cannot change it. Once settings have been saved in the panel, they take precedence over `RUNQUILL_WORKER_POOLS`.

### Share capacity across repositories

When slots free up, Runquill starts the next job from the repository with the fewest active runners, and the longest-waiting job breaks ties. Jobs from one repository still start in discovery order. Set `RUNQUILL_MAX_PER_REPOSITORY` to cap each repository's runners per pool across the whole manager, from `0` (unlimited, the default) to `10000`. A job held by the cap waits in the queue without blocking other repositories. The cap also applies on K3s, where all runners share one pool.

`RUNQUILL_SCHEDULER_WORKERS` sets how many runner launches and cleanups one scheduler pass runs in parallel, from `1` (serial) to `32`. The default is `4`. Calls to one Docker host stay serial. Under Compose, pass these settings through an [overlay](#add-manager-settings).

### Runner images and container engines

Releases publish the runner image as `runquill-runner:<version>` on Ubuntu 24.04 and as `-ubuntu22.04`, `-ubuntu26.04`, `-debian12` and `-debian13` variants. The manager pulls missing runner images onto the `local` host and `ssh://` hosts itself (`RUNQUILL_IMAGE_PULL=missing`, the default, or `always` to refresh them). Load the image yourself on outbound `agent://` workers, for locally built images, and with `RUNQUILL_IMAGE_PULL=never`. The manager also removes images it pulled and cache volumes that no runner has used for 30 days. [Runner images](docs/runner-images.md) `RUNQUILL_CONTAINER_ENGINE=podman` selects the Podman CLI for the `local` and `ssh://` hosts, and runners on a rootless engine start without Docker-in-Docker. Podman and rootless engines have not yet been exercised on a real host. [Engines and images](docs/container-engines.md)

### Elastic runner limits

With `RUNQUILL_ELASTIC=observe` or `apply`, a busy runner on the `local` Docker host can borrow CPU and memory that no other runner or free slot has been promised, up to a multiple of its pool's `cpus` and `memory_mib`. Limits never fall below the pool's values, and memory is never lowered. `observe` only records the decisions, which the **Hosts & pools** screen shows. Elastic limits have not yet been exercised on a real host under load. [Elastic limits](docs/elastic-limits.md)

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

### Choose the runner OS

Select an OS variant inside the same per-job label with `os__NAME__`, after an optional pool selector. Write the name without dots, such as `ubuntu2204`, `ubuntu2604`, `debian12` or `debian13`. A job without it runs its host's image.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-os__ubuntu2204__build', github.run_id, github.run_attempt) }}
```

Names come from the runner image catalogue. Release builds include every published variant, and `RUNQUILL_RUNNER_IMAGES` adds or replaces entries. A bare `runs-on: ubuntu-22.04` is not supported, because Runquill pairs each runner with one job through its unique label. [Runner images](docs/runner-images.md)

### Reuse Docker build layers across jobs

Copy the [cached build example](examples/runquill-build-cache.yml) into `.github/workflows/` in a repository with a root `Dockerfile`. It builds the committed Git revision on the default branch and keeps the resulting image available for later steps in the same job. A new runner can restore layers through the GitHub Actions cache. Choose a distinct `scope` for each image and use it in both cache settings. Cache export failures leave the build usable. [Docker GitHub Actions cache](https://docs.docker.com/build/cache/backends/gha/)

The example pins both actions to exact commits, uses read-only repository permissions, and writes cache from the default branch. Pass build secrets through BuildKit secret mounts so they stay out of cached image layers. [Docker build secrets](https://docs.docker.com/build/building/secrets/)

### Move existing workflows

Administrators can open a pull request that moves jobs from GitHub-hosted labels to Runquill labels from the panel's **Migrate** screen. It needs Contents, Pull requests and Workflows write access, which only `runquill setup --migrate` requests for a new App. Runquill previews the diff first, changes only `runs-on`, leaves jobs it cannot rewrite safely alone with a reason, and never merges. A GitHub-hosted Ubuntu version label such as `ubuntu-22.04` becomes an `os__` selector when the manager's catalogue has that OS. Migration has not yet been exercised against real GitHub. [Workflow migration](docs/workflow-migration.md)

`runquill workflow check` checks local workflow files for malformed Runquill labels, unknown pools and runner OS names, risky triggers on self-hosted runners, missing permissions and timeouts, and unpinned actions. It exits with status 1 for a high-severity finding, or for any finding with `--strict`, and `--json` prints the findings. [Check codes](docs/workflow-migration.md#check-workflows-offline)

## Run on K3s

Deploy the manager and its SQLite volume with Kubernetes and provision one Pod per job across your nodes. Follow the [K3s guide](deploy/k3s/README.md) for installation, resource limits, and runner modes.

## Operate and upgrade

### Sample panel

Once published, the [GitHub Pages demo](https://yakisoba0728.github.io/Runquill/) shows first-job cleanup, recovery from a missed webhook, and an offline worker with synthetic data. Run `"$HOME/runquill/runquill" panel --demo` and open [127.0.0.1:8081](http://127.0.0.1:8081/) to try the same scenarios locally. From a source checkout, use `go run . panel --demo`.

To publish the sample panel, set **Settings → Pages → Source** to **GitHub Actions** and run [Publish sample panel](.github/workflows/demo-pages.yml) from `main`. The workflow publishes the demo's HTML, CSS, scripts, and license. Its run summary links to the deployed site. [GitHub Pages setup](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site)

### Web panel and team access

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

Team users sign in with their enrolled GitHub account. Administrators add a teammate by GitHub login or numeric account ID. The panel resolves a login to its account ID through GitHub's public users API and accepts only user accounts. Tokens expire after 1–90 days, have one scope, and can be revoked from the panel. The secret is shown only when issued. Send it as `Authorization: Bearer <token>`. A token reaches only the routes of its own scope, so reading and changing the fleet needs two tokens. Tokens never reach runner logs, migration, backups or other administrator routes. The [API description](api/openapi.yaml) documents every route.

| Scope | Routes |
| --- | --- |
| `fleet:read` | `GET /snapshot`, `GET /api/events`, `GET /api/jobs`, `GET /api/elastic`, `GET /api/usage`, `POST /mcp` |
| `hosts:write` | `POST /api/hosts/cordon` with `{"host":"local","cordoned":true}`. Pausing a host does not interrupt its active runners. |
| `jobs:write` | `POST /api/jobs/{id}/cancel`, `POST /api/jobs/{id}/retry` |

**Job history** lists jobs newest first with their outcome: waiting, running, finished or failed. Filter it by repository, outcome, and the last hour, 24 hours or 7 days. Runquill keeps finished jobs for seven days, and job logs stay on GitHub. `GET /api/jobs` returns the same rows and accepts `repository`, `outcome`, `since` and `until` (Unix seconds), `limit` (up to 100), and the `next` cursor as `before`.

Operators and administrators can act on a job that waits without a runner, in the panel or with a `jobs:write` token. **Retry now** clears a retry delay so the next scheduler pass tries the job. **Cancel job** marks it failed with `cancelled by @LOGIN`, and Runquill starts no runner for it. The run stays queued on GitHub until you cancel or rerun it there.

Audit history records sign-ins, team, token, host, job, settings and backup changes, runner log views and workflow migrations with the actor's login. Filter it by action type or by actor login or ID, and load older entries page by page. Settings changes list each changed field with its old and new value, and omit values whose names suggest a secret.

The panel updates live over Server-Sent Events and falls back to polling when the stream is unavailable. Press Cmd+K or Ctrl+K for the command palette, and pick a light, dark or system theme. The [panel guide](docs/panel.md) describes each screen and who can use it.

Select a job in the panel to see its runner label, current queue or startup explanation, and latest retry reason. The configured GitHub App access scope is shown in **GitHub connection**. The panel displays discovered jobs. For a missing job, use `runquill diagnose OWNER/REPO` to check the App installation, the job's GitHub label and discovery. `runquill status --json` exposes the same job explanation as the optional `diagnostic` object with a `code` and `message`.

### Usage statistics

The **Usage** screen and `GET /api/usage` report finished jobs, failures, runner time and queue wait by UTC day, repository, pool or host. A job counts once, on the UTC day it finished. Failures are jobs Runquill gave up on or that were cancelled in the panel, not GitHub job conclusions. Daily totals are kept for 400 days in the queue database but are not included in queue backups. [Details](docs/panel.md#usage-statistics)

### Runner logs

Operators and administrators can read or follow the container output of a runner that holds a job, from the **Logs** screen. These are the runner process logs, not GitHub step logs. Only runners on the `local` host and `ssh://` hosts are supported, and API tokens cannot read them. Log streaming has not yet been exercised on a real fleet. [Details](docs/panel.md#runner-logs)

### Remote CLI

Run `runquill jobs list`, `runners list`, `hosts list`, `pools list`, `hosts cordon`, `hosts uncordon`, `jobs cancel` and `jobs retry` from any machine against the panel API. Set `RUNQUILL_URL` to the panel origin and pass a token through `RUNQUILL_TOKEN` or `--token-file`, and add `--json` for scripts. Each command needs the matching token scope, and `audit tail` always needs the web panel because no token can read the audit log. [Remote CLI guide](docs/remote-cli.md)

### MCP for coding agents

`POST /mcp` serves read-only [Model Context Protocol](https://modelcontextprotocol.io/) tools for fleet status, jobs, hosts, usage and elastic decisions. Use a `fleet:read` token over HTTP, or run `runquill mcp` as a local stdio server that forwards to the panel. Browser sessions are always refused. The endpoint has not yet been tried with a real MCP client. [MCP guide](docs/mcp.md)

### Status and upgrades

For fleet-wide queue depth, slot usage, and runner throughput, enable the separate [Prometheus metrics listener](docs/metrics.md).

```sh
docker compose exec manager runquill status
```

The snapshot includes running runners, per-host slot usage and health, and direct GitHub job links. `WAITING` positions follow discovery order among jobs ready to start. [Repository fairness](#share-capacity-across-repositories) can start them in a different order. `RETRY` shows the time until the next attempt and the latest failure or reconciliation reason when available. When using the standalone binary, run `runquill status` with the manager's queue and runner backend settings.

The operations panel shows queue positions, wait times, and retry reasons. Filter jobs by repository, job ID, or pool. The worker and runner tables show host health, occupied and reserved slots, maintenance state, and observed runtimes. Pool capacity shows available worker slots before the fleet-wide limit. The overview shows how many of those slots the fleet can actually use.

Administrators can open **Settings** to change the fleet limit, per-worker limits, GitHub polling interval, runner image, Docker workers, and pool rules. The settings are saved with the queue, recorded in the audit history, and applied to new assignments without restarting the manager. The deployment supplies the initial defaults. SSH workers use the deployment's SSH credentials, and outbound agents use the deployment's authorization file. Use the setup wizard for GitHub App, panel address, and Tailscale configuration.

Use `docker compose exec -T manager runquill status --json` for scripts and monitoring. Schema version 1 includes `slot_limit`, `runners`, `hosts`, and `jobs`. Occupied slots are observed on reachable workers. Reserved slots account for offline workers and leases without an observed host. If Docker reports a conflicting topology, the snapshot still includes jobs and a `docker_error`, with `runners` set to `null` and a nonzero exit status.

If GitHub shows a queued job that Runquill has not started, run `docker compose exec manager runquill diagnose OWNER/REPO`. It checks the App’s access using a repository-scoped read-only token, reports the last recorded poll, and explains invalid labels, retries, and worker capacity for queued jobs.

Use `docker compose exec manager runquill gc --dry-run` to inventory labelled runner containers without a matching queue lease. It shows the Docker host and state, checks GitHub when the queue contains the associated job, and reports incomplete inventory when a host or queue is unavailable. The command only reads containers and queue data.

To clean up, run `docker compose exec manager runquill gc --remove-orphans`. It removes labelled runner containers that have no queue lease and cannot be running a job, and prints every runner it skips with the reason. `--grace` (default `10m`, at least `1m`) leaves runners younger than that alone so a launch in progress is never touched. The command keeps a runner when its lease is recorded on another host, when it is running and the queue has no record of its job, when GitHub reports the job unfinished or the runner busy, or when GitHub cannot be reached. Before removing a runner whose job is in the queue, it deletes the GitHub runner registration, then removes the container by its Docker ID. It works on `local` and `ssh://` hosts only. `agent://` workers and the Kubernetes backend are reported as skipped. The command exits with status 1 if a host or a removal fails.

`docker compose exec manager runquill gc --unused --dry-run` lists runner images that Runquill pulled and cache volumes once no runner has used them for `RUNQUILL_CLEANUP_AFTER` (default `30d`). Without `--dry-run`, it removes them, as the manager also does every `RUNQUILL_CLEANUP_INTERVAL`. `--older-than` sets another retention. Images you pulled or built yourself are never removed. [Cleanup](docs/runner-images.md#cleanup-of-unused-images-and-cache-volumes)

- `RUNQUILL_MAX_RUNNERS` defaults to 2. Provisioning failures retry after 15 seconds and allow later jobs to use available slots. GitHub fails self-hosted jobs that remain queued for more than 24 hours. [GitHub runner routing](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#routing-precedence-for-self-hosted-runners)
- Runquill marks a job failed when it has not started within 24 hours, with the reason and the last retry error. A job cancelled in the panel is also failed. A failed job never returns to the queue. The run stays queued on GitHub until you cancel or rerun it there, and a rerun creates a new job. The reason appears in the panel's job history, and expired jobs also log `job failed`.
- `docker compose logs -f manager` shows admissions, cleanup, and API errors. The GitHub workflow retains step output as usual. To adopt a new GitHub runner version in a source install, rerun `docker build --pull -t runquill-runner:local runner`. Running jobs finish on the image they started with. Set `RUNQUILL_LOG_FORMAT` to `text` or `json` for structured logs and `RUNQUILL_LOG_LEVEL` to `debug`, `info`, `warn` or `error`. Without a format, the manager keeps its plain log lines. An invalid value stops the command with status 2.
- `GET /healthz` on port 8080 returns HTTP 200 when the queue database answers. `GET /readyz` also requires a successful scheduler pass within the last 2 minutes and no current GitHub App authentication failure. It returns HTTP 503 otherwise, with a JSON body whose `scheduler` is `starting`, `failing` or `stalled`, and whose `database` and `github_auth` show the other checks. Error details stay in the manager log.
- Runners are ephemeral, repository-scoped JIT runners. In the default pool, each job gets a new container with its own Docker daemon, so `docker build`, `services:` and `container:` work. After the job, the container with its inner images, volumes and work directory is deleted. Only the per-repository cache volume of a pool with `cache` enabled remains, until it has been unused for the cleanup retention.
- Run one manager per fleet. Preserve the Compose `queue` volume during upgrades, including upgrades from the original Python version. A manager restart continues tracking existing runners.
- Runquill polls the GitHub API on startup and waits `RUNQUILL_POLL_INTERVAL` (default `30s`) after each scan. Each scan checks queued and in-progress runs in every eligible repository, giving recent activity priority. The poller rotates requested, waiting, and pending runs so each repository receives a full scan within four successful passes if the repository list stays stable. Webhooks and polling share one queue record per GitHub job ID. Set the interval to `0` to run with webhooks alone.
- Job timing records the first discovery source, successful GitHub runner registration, and GitHub’s reported job start when available. The discovery delay measures GitHub job creation to Runquill discovery when GitHub provides a creation timestamp. The Runquill queue duration measures discovery to first registration, or elapsed wait for a queued job. Missing timestamps and durations are omitted in `status --json`.

### Upgrade

With a release that supports `--upgrade`, download the new installer and point it at the existing installation directory. The default `--dir` is `./runquill`.

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --upgrade --dir "$HOME/runquill"
```

The installer verifies the release checksums and replaces only the release files that changed: the `runquill` binary, `compose.yaml`, `compose.ssh.yaml`, `LICENSE`, and `compose.panel.yaml`, `compose.worker.yaml` and `runquill-k3s.yaml` when the release includes them. It moves the replaced copies into `.previous/`, which holds only the most recent upgrade. Roll back with `cp -p .previous/* .`. `.env`, the GitHub App key, other files and the queue are untouched. `--upgrade` accepts only `--version`, `--dir`, `--yes` and `--no-install`. It runs the host checks too, but because it restarts nothing, it only reports Docker problems and never installs Docker.

The installer does not restart anything. If `.env` names published images, it prints the new `RUNQUILL_MANAGER_IMAGE` and `RUNQUILL_RUNNER_IMAGE` values for you to set, followed by the pull, `docker compose up -d --no-build` and `doctor` commands to run with your usual Compose files. The manager pulls the new runner image onto the `local` host and `ssh://` hosts. On outbound workers, load the new image and upgrade the `runquill worker` binary. On K3s, it prints `kubectl apply -f runquill-k3s.yaml` instead.

For a release without `--upgrade` or a manual install, set both image tags in `.env` to the new release and repeat the pull commands from [Start the manager](#start-the-manager).

### Add manager settings

Compose passes only the variables listed in `compose.yaml` to the manager. For optional settings such as `RUNQUILL_MAX_PER_REPOSITORY`, `RUNQUILL_SCHEDULER_WORKERS`, `RUNQUILL_LOG_FORMAT`, `RUNQUILL_LOG_LEVEL`, the [notification](#send-notifications) variables, `RUNQUILL_ELASTIC*`, `RUNQUILL_BACKUP_*`, `RUNQUILL_METRICS_*`, `RUNQUILL_IMAGE_PULL*`, `RUNQUILL_RUNNER_IMAGES` and `RUNQUILL_CLEANUP_*`, keep the values in `.env` and save an overlay such as `compose.settings.yaml` beside `compose.yaml`. `--upgrade` replaces `compose.yaml` but leaves this file in place.

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
```

Apply it with `docker compose -f compose.yaml -f compose.settings.yaml up -d`, adding your other overlays and `--no-build` for published images. Scheduled backups also need a mounted directory and a passphrase file, so use the overlay in the [backup guide](docs/backups.md#compose). The [elastic limits guide](docs/elastic-limits.md#compose) lists its variables. Pulls from a private registry also need a Docker login mounted into the manager with `DOCKER_CONFIG`, as shown in [private registries](docs/runner-images.md#private-registries). `RUNQUILL_CONTAINER_ENGINE=podman` cannot be used under Compose because the manager image contains only the Docker CLI. On K3s, the manager reads every variable in the `runquill-config` Secret, so add them to `.env`, recreate the Secret, and restart the Deployment.

### Send notifications

Set `RUNQUILL_NOTIFY_URL` to an incoming webhook, such as a Slack webhook, to receive fleet alerts. Treat the URL as a secret because services often embed a token in it. Runquill never logs it. The URL must use HTTPS, or HTTP to a loopback host, and redirects to another host are refused.

| Event | Sent when |
| --- | --- |
| `queue_stalled`, `queue_recovered` | The oldest queued job without a runner has waited longer than `RUNQUILL_NOTIFY_STALL_AFTER` (default `10m`), and again once that wait drops below the threshold. |
| `host_offline`, `host_recovered` | A Docker host, outbound worker or the Kubernetes backend cannot be inventoried, and again once it is back. |
| `job_failed` | Runquill marks a job failed after 24 hours without a start. Panel cancellations do not notify. |

When a per-repository cap is configured, jobs of a repository that already has an active runner are left out of the stalled-queue age, so a cap alone does not raise `queue_stalled`. A condition that persists is repeated after `RUNQUILL_NOTIFY_COOLDOWN` (default `30m`). Recovery is sent once and only after its problem was notified.

`RUNQUILL_NOTIFY_FORMAT=slack` (the default) posts `text` plus `source`, `kind`, `host`, `repository`, `job` (the runner label), `job_id`, `reason`, `waiting_seconds` and `timestamp`. Use `text` for receivers that accept only `{"text": ...}`. Each event gets up to three attempts with a 10-second timeout, retrying on network errors, HTTP 429 and 5xx. Up to 64 events wait in memory, and the oldest is dropped when the queue is full. On shutdown, the manager waits up to 10 seconds for pending deliveries. With [metrics](docs/metrics.md) enabled, the `runquill_notifications_*` counters track the results.

### Back up and recover the queue

Take a consistent SQLite snapshot while the manager is running. Save it outside the queue volume so you can recover from volume loss.

```sh
umask 077
docker compose exec -T manager runquill backup --output /data/queue-backup.sqlite3
docker compose cp manager:/data/queue-backup.sqlite3 ./queue-backup.sqlite3
```

Administrators can also download the same snapshot from the panel. The download shows its progress, can run for up to 30 minutes for a large queue, and does not block other panel operations once the copy is ready.

The versioned snapshot is written with owner-only permissions to a new file. It contains queued jobs, runner leases, host maintenance state, repository scan history, and operational settings saved in the panel. Treat it as private because it contains repository and job metadata. Keep the GitHub App PEM, deployment configuration and OAuth secrets in your separate secret backup. Panel users, sessions, tokens and audit records are excluded. For a recovered panel, set `RUNQUILL_PANEL_BOOTSTRAP_ID` to the intended administrator's numeric GitHub ID, then enroll the team again and issue fresh tokens.

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

### Schedule, seal and upload backups

Set `RUNQUILL_BACKUP_INTERVAL` and `RUNQUILL_BACKUP_DIR` to take backups automatically, and `RUNQUILL_BACKUP_PASSPHRASE` or `RUNQUILL_BACKUP_PASSPHRASE_FILE` to encrypt them. `runquill backup --seal` writes the same sealed format by hand, and `runquill restore --from` opens it with the passphrase. With the `RUNQUILL_BACKUP_S3_*` variables, the manager also copies each backup to an S3-compatible bucket and `runquill restore --from-s3` restores from it. Runquill refuses to upload unsealed backups unless you set `RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true`. The S3 upload has not yet been exercised against a real bucket. [Backup guide](docs/backups.md)

### Uninstall

Cordon the hosts and let running jobs finish first. [Back up the queue](#back-up-and-recover-the-queue) if you might reinstall. From the installation directory, stop the manager with the Compose files you use. `-v` deletes the `queue` volume with every queued job, lease and saved panel setting, and `--rmi all` removes the manager image.

```sh
docker compose -f compose.yaml -f compose.panel.yaml down -v --rmi all
```

Compose does not manage runner containers, cache volumes or the runner image. Check each Docker host, including SSH and outbound workers, and remove what remains. Use the image named by `RUNQUILL_RUNNER_IMAGE`, any pool `image`, and the catalogue images that jobs selected.

```sh
docker ps -a --filter label=io.runquill.runner=1
docker rm -f $(docker ps -aq --filter label=io.runquill.runner=1)
docker volume rm $(docker volume ls -q -f label=io.runquill.cache=1)
docker image rm ghcr.io/yakisoba0728/runquill-runner:v0.3.2
```

Then delete the installation directory, which holds `.env`, the GitHub App key and any token files. Turn off a Tailscale Funnel or Serve listener you created, and stop `runquill worker` on outbound workers.

The installer's host changes stay in place. To undo them, remove your user from the `docker` group with `sudo gpasswd -d "$USER" docker`. Remove Docker by following [Docker's uninstall steps](https://docs.docker.com/engine/install/ubuntu/#uninstall-docker-engine) for your distribution, including `/etc/apt/keyrings/docker.asc` and `/etc/apt/sources.list.d/docker.sources`, or `/etc/yum.repos.d/docker-ce.repo`.

On K3s, `kubectl delete -f runquill-k3s.yaml` removes both Runquill namespaces with the manager, runner Pods, Secrets and the queue PVC.

Finally, delete the GitHub App in **Settings → Developer settings → GitHub Apps → *your App* → Advanced → Delete GitHub App**, under the organization's settings for an organization App. This uninstalls it everywhere. Delete a separate panel OAuth App the same way, and remove any leftover `runquill-` runners in the repository's **Settings → Actions → Runners**. [Deleting a GitHub App](https://docs.github.com/en/apps/maintaining-github-apps/deleting-a-github-app)

## Feature status

The Go tests cover the features in this README, and CI runs real containers in the nested Docker test and the K3s lifecycle check. The features below are implemented and tested only against fakes or recorded output. They have not yet been run in a real environment:

- Podman and rootless container engines
- uploads to and restores from real S3 buckets
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

Report problems with these features as issues.

## Develop and release

Use Go 1.26.11 or later. The manager builds as a native binary with the Docker CLI as its external runtime dependency.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o runquill .
```

The runner Dockerfile pins the upstream GitHub Actions runner image by digest. Use `--build-arg RUNQUILL_RUNNER_BASE=IMAGE` to extend a compatible image with your own toolchain. Pass `--build-arg RUNQUILL_RUNNER_OS=` with `ubuntu22.04`, `ubuntu26.04`, `debian12` or `debian13` to build another base. Keep the runner files in `/home/runner` and its user at UID 1001 for Kubernetes plain mode and pools with `privileged: false`.

Run the nested Docker smoke test with `RUNQUILL_TEST_DOCKER_IMAGE=runquill-runner:local go test -run '^TestDockerRunner$' -v` after building the runner image. It checks Docker build, PostgreSQL, bind mounts, and removal with a local stand-in for the GitHub listener.

[CI](.github/workflows/ci.yml) runs the Go tests and the nested Docker check on amd64 and arm64 Linux. On amd64, it also starts a disposable K3s cluster. It checks that pausing assignments leaves jobs queued, resuming starts their runners, and job completion removes the runner Pods and credential Secrets. The [two-account OAuth check](panel_live_oauth_test.go) can be run locally with a dedicated OAuth App and separate GitHub accounts for the administrator and viewer.

On a version tag, the [release workflow](.github/workflows/release.yml) runs CI, publishes amd64 and arm64 images to GHCR, and attaches the matching Linux binaries and checksums to the release. After publication in this repository, it verifies the exact tag with the public installer in clean Linux containers on both architectures. After the first publish, set both packages to public if they should be anonymously pullable. The workflow publishes to the repository owner's namespace, so forks use their own image names. [GitHub Container registry access](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

Copyright © 2026 Runquill contributors. [Apache License 2.0](LICENSE)
