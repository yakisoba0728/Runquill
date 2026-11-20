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

Pools accept four more optional keys.

```dotenv
RUNQUILL_WORKER_POOLS='{"lint":{"hosts":["local"],"privileged":false,"cache":true,"max_per_repository":2,"images":["ubuntu22.04"]}}'
```

- `privileged` defaults to `true`, which starts each runner with `--privileged` and its own Docker daemon. Set it to `false` to run the runner as UID 1001 with all capabilities dropped and `no-new-privileges`. Those jobs cannot use `docker`, service containers, job containers or `sudo`. A custom `image` for such a pool needs a runner user with UID 1001 that owns `/home/runner`.
- `cache: true` mounts a named Docker volume `runquill-cache-<repository ID>` at `/home/runner/.cache`. Jobs from the same repository on the same host share these package and build-tool caches. The runner mounts `RUNNER_TOOL_CACHE` at `/home/runner/.cache/tools` in a separate volume for the repository, image ID and platform, so `actions/setup-*` reuses tools for the same image. Each host keeps its own volumes. On local and SSH hosts the manager removes a volume that no runner has used for 30 days. List volumes with `docker volume ls -f label=io.runquill.cache=1`. [Tool cache and cleanup](runner-images.md#tool-cache) · [Docker build layers](workflows.md#reuse-docker-build-layers-across-jobs)
- `max_per_repository` caps how many runners one repository can hold in that pool. It overrides `RUNQUILL_MAX_PER_REPOSITORY` for the pool. Without either value, a repository can use every free slot.
- `images` lists the [runner OS](workflows.md#choose-the-runner-os) names that jobs in the pool may select with `os__NAME__`. Omit it to allow every name.

[Outbound workers](outbound-workers.md) in a pool with `privileged: false` or `cache: true` must run the same Runquill release as the manager. Older agents reject runner tasks that carry these settings, so upgrade the workers along with the manager.

The administrator panel's pool editor has checkboxes for Docker-in-Docker and the shared cache, and a field for the allowed runner OS names. It keeps each pool's `max_per_repository` when saving but cannot change it. Once settings have been saved in the panel, they take precedence over `RUNQUILL_WORKER_POOLS`.

## Share capacity across repositories

When slots free up, Runquill starts the next job from the repository with the fewest active runners, and the longest-waiting job breaks ties. Jobs from one repository still start in discovery order. Set `RUNQUILL_MAX_PER_REPOSITORY` to cap each repository's runners per pool across the whole manager, from `0` (unlimited, the default) to `10000`. A job held by the cap waits in the queue without blocking other repositories. The cap also applies on K3s, where all runners share one pool.

`RUNQUILL_SCHEDULER_WORKERS` sets how many runner launches and cleanups one scheduler pass runs in parallel, from `1` (serial) to `32`. The default is `4`. Calls to one Docker host stay serial. Under Compose, pass these settings through an [overlay](operations.md#add-manager-settings).

## Runner images and container engines

Releases publish the runner image as `runquill-runner:<version>` on Ubuntu 24.04 and as `-ubuntu22.04`, `-ubuntu26.04`, `-debian12` and `-debian13` variants. The manager pulls missing runner images onto the `local` host and `ssh://` hosts itself (`RUNQUILL_IMAGE_PULL=missing`, the default, or `always` to refresh them). Load the image yourself on outbound `agent://` workers, for locally built images, and with `RUNQUILL_IMAGE_PULL=never`. The manager also removes images it pulled and cache volumes that no runner has used for 30 days. [Runner images](runner-images.md) `RUNQUILL_CONTAINER_ENGINE=podman` selects the Podman CLI for the `local` and `ssh://` hosts, and runners on a rootless engine start without Docker-in-Docker. Podman and rootless engines have not yet been exercised on a real host. [Engines and images](container-engines.md)

## Elastic runner limits

With `RUNQUILL_ELASTIC=observe` or `apply`, a busy runner on the `local` Docker host can borrow CPU and memory that no other runner or free slot has been promised, up to a multiple of its pool's `cpus` and `memory_mib`. Limits never fall below the pool's values, and memory is never lowered. `observe` only records the decisions, which the **Hosts & pools** screen shows. Elastic limits have not yet been exercised on a real host under load. [Elastic limits](elastic-limits.md)
