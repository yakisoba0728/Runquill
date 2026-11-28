# Docker workers and pools

[Runquill README](../README.md) · [한국어](workers.ko.md)

## Add Docker worker servers

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

For workers behind NAT, add [outbound Docker workers](outbound-workers.md) that connect over HTTPS.

## Route jobs to worker pools

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

Pools accept five more optional keys.

```dotenv
RUNQUILL_WORKER_POOLS='{"lint":{"hosts":["local"],"privileged":false,"cache":true,"cache_writes":"default_branch","max_per_repository":2,"images":["ubuntu22.04"]}}'
```

- `privileged` defaults to `true`, which starts each runner with `--privileged` and its own Docker daemon. Set it to `false` to run the runner as UID 1001 with all capabilities dropped and `no-new-privileges`. Those jobs cannot use `docker`, service containers, job containers or `sudo`. A custom `image` for such a pool needs a runner user with UID 1001 that owns `/home/runner`.
- `cache: true` mounts a named Docker volume `runquill-cache-<repository ID>` at `/home/runner/.cache`. Jobs from the same repository on the same host share these package and build-tool caches, including the npm, Go module, NuGet and Bun caches that the runner points there. The runner mounts `RUNNER_TOOL_CACHE` at `/home/runner/.cache/tools` in a separate volume for the repository, image ID and platform, so `actions/setup-*` reuses tools for the same image. Each host keeps its own volumes. On local and SSH hosts the manager removes a volume that no runner has used for 30 days. List volumes with `docker volume ls -f label=io.runquill.cache=1`. [Tool cache and cleanup](runner-images.md#tool-cache) · [Docker build layers](workflows.md#reuse-docker-build-layers-across-jobs)
- `cache_writes` chooses which jobs of a `cache: true` pool use the shared cache volumes. `all` lets every job read and write them. `default_branch` gives them only to runs on the default branch, and other jobs start with an empty cache. Without the key, private repositories use `all` and public repositories use `default_branch`. [Protect the shared cache](#protect-the-shared-cache)
- `max_per_repository` caps how many runners one repository can hold in that pool. It overrides `RUNQUILL_MAX_PER_REPOSITORY` for the pool. Without either value, a repository can use every free slot.
- `images` lists the [runner OS](workflows.md#choose-the-runner-os) names that jobs in the pool may select with `os__NAME__`. Omit it to allow every name.

[Outbound workers](outbound-workers.md) in a pool with `privileged: false`, `cache: true` or `cache_writes` must run the same Runquill release as the manager. Older agents reject runner tasks that carry these settings, so upgrade the workers along with the manager.

The administrator panel's pool editor lets you set Docker-in-Docker, shared caching, allowed runner OS names, `warm` and `warm_max_idle_minutes`. It preserves `cache_writes` and `max_per_repository` when you save. Once settings have been saved in the panel, they take precedence over `RUNQUILL_WORKER_POOLS`.

### Protect the shared cache

With `cache_writes` set to `all`, every job of a repository on a host writes the same cache volumes. A pull request job can then leave altered packages or tools that a later default-branch job runs. With `"cache_writes":"default_branch"`, a job mounts the repository's shared cache volumes only when its workflow run meets all of these:

- The event is `push`, `schedule` or `workflow_dispatch`.
- The run's head branch is the repository's default branch.
- The run's head repository is the repository itself.

Every other job gets an empty anonymous volume at `/home/runner/.cache` instead, with the same cache variables and `RUNNER_TOOL_CACHE`. This includes `pull_request`, `pull_request_target`, `merge_group`, `issue_comment`, `workflow_run` and pushes to other branches. Tools write to it as usual, and the engine removes it with the runner, so nothing it writes reaches another job. These jobs do not read the shared cache either. A read-only mount was not used, because npm, Go and `actions/setup-*` fail when they cannot write their cache, and a privileged runner can remount it writable.

Runquill decides just before it registers the runner. It reads the event, head branch and head repository from GitHub's workflow run API, and the default branch from the repository API, with the installation token. Nothing the job runs can change these values. GitHub gives the runner to any queued job in the repository with the same label, so for a default-branch run Runquill also lists the repository's unfinished workflow runs and their jobs. If a run that may not write the cache has an unfinished job with the same label, the job gets an empty cache instead. Each launch in the pool costs one run lookup, plus, for a default-branch run, three run listings and a job listing for each unfinished run that may not write the cache. If a lookup fails, the job waits and retries.

- A job that asks for the same label in the seconds between the check and the runner taking its job can still get the shared cache.
- GitHub does not say whether a run's head branch is a branch or a tag. A tag with the default branch's name, pushed by someone with write access, counts as the default branch. Block such tags with a tag ruleset.
- With `privileged` left at `true`, a job is root on the host, so it can reach the shared volumes anyway. Use `privileged: false` where the protection must hold against a hostile job.
- Outbound workers need this release. Older agents reject the task, and the job retries until the worker is upgraded.

## Keep runners warm

A cold runner waits for its container to start and, with Docker-in-Docker, for its own Docker daemon. Set `warm` on a pool to keep that many runners started on each `local` and `ssh://` host in the pool. They wait without a job or any credential. When a job is placed on the host, the manager renames one to the job's runner name and writes the job's single-use JIT config into it, so the job skips container and daemon startup.

```dotenv
RUNQUILL_WORKER_POOLS='{"default":{"warm":2,"warm_max_idle_minutes":30}}'
```

- `warm` is the number per host, from `0` (off, the default) to `100`. `warm_max_idle_minutes` replaces a warm runner that no job took, up to `1440`. The default is `30`.
- Warm runners fill only free slots. While idle they do not count toward `RUNQUILL_MAX_RUNNERS`, host limits or repository caps. A job that takes one uses its slot like any other runner, and the manager removes warm runners when jobs or the global limit need their slots. A job that selects another image with `os__NAME__` starts cold and retires one warm runner for its slot. Until the end of a scheduler pass, the fleet can hold more containers than `RUNQUILL_MAX_RUNNERS`, at most the number of idle warm runners, when jobs start cold on other hosts while warm runners wait.
- The manager starts at most one warm runner per host in each scheduler pass. It skips hosts that are cordoned, unreachable or waiting for their runner image, and cordoning a host removes its idle warm runners. A warm runner that exits early, for example from an image without warm support, makes the manager back off on that host, doubling from 15 seconds to 10 minutes.
- A job takes a warm runner only if it matches the pool's current image name, CPU and memory limits, Docker-in-Docker mode and idle time. The manager replaces the others. A tag that moves to a new image is picked up as warm runners reach their idle time.
- After a restart the manager adopts the warm runners already running. If the manager is gone, a warm runner exits on its own 5 minutes after its idle time.
- `warm` cannot be combined with `cache: true`, because the cache volume belongs to the job's repository, which is unknown before the job arrives. The manager rejects `warm` for pools with outbound `agent://` workers, and K3s does not use worker pools.
- Warm runners need the runner image from this release or later. A custom pool image must keep its entrypoint.
- Each warm runner takes at most one job and is removed afterwards, like any runner. Idle warm runners still hold memory and, with Docker-in-Docker, a running daemon, so keep `warm` within the host's capacity.
- Warm runners are named `runquill-warm-…`. See idle counts and configured targets in **Hosts** or in the `warm` and `warm_target` fields of `runquill status --json`. `gc --dry-run` omits idle warm runners, and `gc --remove-orphans` skips them. List them with `docker ps --filter name=runquill-warm-`. Podman has not been tested with warm runners.

## Share capacity across repositories

When slots free up, Runquill starts the next job from the repository with the fewest active runners, and the longest-waiting job breaks ties. Jobs from one repository still start in discovery order. Set `RUNQUILL_MAX_PER_REPOSITORY` to cap each repository's runners per pool across the whole manager, from `0` (unlimited, the default) to `10000`. A job held by the cap waits in the queue without blocking other repositories. The cap also applies on K3s, where all runners share one pool.

`RUNQUILL_SCHEDULER_WORKERS` sets how many runner launches and cleanups one scheduler pass runs in parallel, from `1` (serial) to `32`. The default is `4`. Calls to one Docker host stay serial. Under Compose, pass these settings through an [overlay](operations.md#add-manager-settings).

## Runner images and container engines

Releases publish the runner image as `runquill-runner:<version>` on Ubuntu 24.04 and as `-ubuntu22.04`, `-ubuntu26.04`, `-debian12` and `-debian13` variants. The manager pulls missing runner images onto the `local` host and `ssh://` hosts itself (`RUNQUILL_IMAGE_PULL=missing`, the default, or `always` to refresh them). Load the image yourself on outbound `agent://` workers, for locally built images, and with `RUNQUILL_IMAGE_PULL=never`. The manager also removes images it pulled and cache volumes that no runner has used for 30 days. [Runner images](runner-images.md) `RUNQUILL_CONTAINER_ENGINE=podman` selects the Podman CLI for the `local` and `ssh://` hosts, and runners on a rootless engine start without Docker-in-Docker. Podman and rootless engines have not yet been exercised on a real host. [Engines and images](container-engines.md)

## Elastic runner limits

With `RUNQUILL_ELASTIC=observe` or `apply`, a busy runner on the `local` Docker host can borrow CPU and memory that no other runner or free slot has been promised, up to a multiple of its pool's `cpus` and `memory_mib`. Limits never fall below the pool's values, and memory is never lowered. `observe` only records the decisions, which the **Hosts** screen shows. Elastic limits have not yet been exercised on a real host under load. [Elastic limits](elastic-limits.md)
