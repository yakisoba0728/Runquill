# Runner images

[Runquill README](../README.md) · [한국어](runner-images.ko.md)

Every Runquill runner starts from a runner image. This guide covers the published OS variants, how a job selects one in `runs-on`, how the manager pulls images onto its hosts, the tool cache of cached pools, the Docker Hub mirror for Docker-in-Docker runners, and the cleanup of images and cache volumes nobody uses any more.

## OS variants

Releases publish five variants of the runner image for amd64 and arm64.

| Tag | Base | Catalogue name |
| --- | --- | --- |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3` | Ubuntu 24.04, the upstream GitHub Actions runner image | `ubuntu24.04` |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu22.04` | Ubuntu 22.04 | `ubuntu22.04` |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu26.04` | Ubuntu 26.04 | `ubuntu26.04` |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian12` | Debian 12 | `debian12` |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian13` | Debian 13 | `debian13` |

All variants carry the same digest-pinned Actions runner and Docker binaries and a `runner` user with UID 1001, so each works in privileged pools, in pools with `privileged: false` and in K3s `plain` mode. To build one from source, pass `--build-arg RUNQUILL_RUNNER_OS=NAME` to `docker build runner`. CI builds every variant and checks its runner user and versions. Only the default variant runs the nested Docker test.

A variant can be the fleet-wide `RUNQUILL_RUNNER_IMAGE`, a pool's `image`, or an entry in the catalogue that jobs select by name.

## The runner image catalogue

The catalogue maps names to image references. A workflow can name a catalogue entry, never an image reference: runners may run privileged, so which images they start from stays an administrator decision.

| Name | Image | Source |
| --- | --- | --- |
| `ubuntu-latest` | The default runner image (`RUNQUILL_RUNNER_IMAGE` or the panel's default runner image) | `default` |
| `ubuntu24.04` | `<release>-runner:<version>` | `release` |
| `ubuntu22.04`, `ubuntu26.04`, `debian12`, `debian13` | `<release>-runner:<version>-<name>` | `release` |

The `release` entries exist in release builds: the published manager image that Compose and K3s run, and the standalone `runquill-linux-*` binary. A source build has no release image name, so its catalogue holds only `ubuntu-latest` and `ubuntu24.04`, both pointing at the default runner image. There, add the variants you want with `RUNQUILL_RUNNER_IMAGES`.

`RUNQUILL_RUNNER_IMAGES` is a JSON object of `NAME: image reference`. It adds entries and replaces built-in entries with the same name.

```dotenv
RUNQUILL_RUNNER_IMAGES='{"ubuntu22.04":"ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu22.04","ubuntu26.04":"ghcr.io/yakisoba0728/runquill-runner:v0.3.3-ubuntu26.04","debian12":"ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian12","debian13":"ghcr.io/yakisoba0728/runquill-runner:v0.3.3-debian13"}'
```

- A name has 1 to 40 lowercase letters, digits, dots or hyphens and starts with a letter or digit. The object may hold up to 64 names.
- Names are compared without dots, hyphens and case, so `ubuntu22.04` and `ubuntu-2204` are the same entry. Two names that fold to the same key are refused.
- The image must be a Docker image reference without a scheme or credentials. Log the engine in to the registry instead (see [Private registries](#private-registries)).
- An invalid value stops the manager at startup. Change the variable and restart the manager to apply a new catalogue.

Under Compose, pass the variable through the [settings overlay](operations.md#add-manager-settings). On K3s, add it to the `runquill-config` Secret.

`runquill doctor` prints every entry as `runner image os__LABEL__ = IMAGE (source)` and, on a Docker manager, whether the local engine already holds it. `GET /api/settings` returns the same list as the read-only `runner_images` field.

## Select an OS in `runs-on`

A job selects a catalogue entry inside its own per-job label, after the optional pool selector.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-os__ubuntu2204__build', github.run_id, github.run_attempt) }}
runs-on: ${{ format('runquill-{0}-{1}-pool__big__os__debian12__build', github.run_id, github.run_attempt) }}
runs-on: ${{ format('runquill-{0}-{1}-os__debian13__test-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

The forms are `os__NAME__JOB` and `pool__POOL__os__NAME__JOB`. Route labels cannot contain dots, so write the name without them: `ubuntu2204`, `ubuntu2404`, `ubuntu2604`, `debian12`, `debian13` or `ubuntu-latest`. The label form allows 1 to 40 lowercase letters, digits and hyphens. Because dots and hyphens are ignored when names are compared, `ubuntu2204` and `ubuntu-22-04` both select `ubuntu22.04`. `doctor` prints the label form of each entry.

A job without `os__` runs its host's image: the pool's `image`, or the default runner image.

A bare `runs-on: ubuntu-22.04` is deliberately not supported. Runquill pairs each ephemeral runner with one job by registering it with a label that no other job uses. A label shared by many jobs would let GitHub hand a runner a different job than the one it was started for, and GitHub-hosted runners would compete for the same jobs.

### Restrict a pool

A pool's optional `images` key lists the catalogue names its jobs may select.

```dotenv
RUNQUILL_WORKER_POOLS='{"build":{"hosts":["ssh://builder@worker.example.com"],"images":["ubuntu22.04","debian12"]}}'
```

Omit the key to allow every name. The names are validated like catalogue names but need not exist, so removing a catalogue entry never makes saved settings unloadable. The panel's pool editor has an **Allowed runner OS** field for this list.

### When a job cannot get its image

- A name that is not in the catalogue, or that the job's pool does not allow, keeps the job waiting with a retry reason such as `select runner image: runner image "ubuntu2004" is not in the runner image catalogue; add it to RUNQUILL_RUNNER_IMAGES`. Fix the catalogue or the pool and the job starts on a later pass. Only the 24-hour expiry fails it.
- If no host in the job's pool can start the image, the reason says so. An outbound `agent://` worker starts only the image it was configured with, so it takes an `os__` job only when the selected image is that same image.
- While a host pulls the image, the job waits with `waiting for runner image IMAGE on HOST`. No GitHub runner registration is created until a host has the image.

### Kubernetes

On K3s, the selected image replaces the runner Pod's image, and the kubelet pulls it like any other image. Use `RUNQUILL_K8S_IMAGE_PULL_SECRET` for a private registry. K3s has no worker pools, so `images` does not apply there.

#### Share pulled images between K3s nodes

Each node pulls a runner image from the registry the first time a Pod there uses it, and the [job stage histograms](metrics.md#job-stage-histograms) count that pull in `runquill_job_github_assign_seconds`. K3s can embed Spegel, a distributed registry mirror, so that a node pulls an image another node already has over the cluster network. It is disabled by default. Enable it on every server node with `--embedded-registry` or `embedded-registry: true` in the K3s configuration file, and list the registry in `/etc/rancher/k3s/registries.yaml` on every node. Runquill's runner images come from `ghcr.io`:

```yaml
mirrors:
  ghcr.io:
```

All nodes must reach each other on TCP ports 5001 and 6443 over their internal IP addresses. Only nodes that list a registry share its images. The mirror does not share the `latest` tag between nodes, and Runquill's runner images use version tags, so this does not affect them.

Know the trust model before enabling it:

- K3s assumes the same privilege and trust between all cluster members. Anyone who can push images into containerd on one node may be able to poison an image tag for the other nodes. Pin images by digest where integrity matters, for example in `RUNQUILL_RUNNER_IMAGE` and `RUNQUILL_RUNNER_IMAGES`.
- Images pulled with registry credentials, such as through `RUNQUILL_K8S_IMAGE_PULL_SECRET`, are served to other nodes without those credentials.
- With `RUNQUILL_K8S_RUNNER_MODE=dind`, a job controls a privileged Pod on its node. Enable the mirror only where the cluster is already dedicated to trusted workflows.

Runquill has not been exercised with the embedded mirror. [K3s embedded registry mirror](https://docs.k3s.io/installation/registry-mirror)

### Workflow migration and checks

When the panel's **Migrate** screen rewrites a job whose `runs-on` has one GitHub-hosted Ubuntu version label, such as `ubuntu-22.04`, and the manager's catalogue has that entry, the new label keeps the OS with `os__ubuntu2204__`. `ubuntu-latest` maps to a plain label because it is the default image. A job ID that starts with `os__` is left alone, because it would read as a selector. [Workflow migration](workflow-migration.md)

`runquill workflow check` reports an invalid `os__` suffix under `runs-on`, and a `runner-image` warning for a name missing from the catalogue or not allowed by the job's pool. It checks against the catalogue of the binary that runs it plus `RUNQUILL_RUNNER_IMAGES` in its environment, so set the same `RUNQUILL_RUNNER_IMAGES` there as on the manager.

`runquill status` prints `os=NAME image=IMAGE` under jobs that select an OS. `status --json`, the panel's queue and runner tables and `GET /snapshot` carry `runner_os` and `runner_image`, and job history carries `runner_os`.

## Automatic pulls

The manager pulls runner images onto the `local` host and `ssh://` hosts itself. `RUNQUILL_IMAGE_PULL` sets the policy.

| Value | Behaviour |
| --- | --- |
| `missing` (default) | Pull an image a host does not have. |
| `always` | Also pull images a host already has again every `RUNQUILL_IMAGE_PULL_INTERVAL` (default `6h`, between `1m` and `168h`). A failed refresh keeps the image already on the host. |
| `never` | Pull nothing. Each host must already hold its images, as before v0.3.1. |

The puller covers each host's configured image (the default runner image or its pool's `image`) and, on demand, catalogue images that a waiting job selected on that host. An on-demand image is pulled on one eligible host, the least occupied or the one already pulling it, not on every host, and is forgotten an hour after the last job asked for it.

- The puller inspects the images every 30 seconds, runs at most two pulls at once and one per host, and stops a pull after 15 minutes. A failed pull is retried after 30 seconds, then with a doubling delay of up to 10 minutes. An image that appears on a host meanwhile, for example a local build, is used at once.
- While a host's own image is missing, the manager still starts. The host shows as unavailable with the image state (`missing`, `pulling` or `failed` with the pull error) on the panel's **Hosts & pools** screen and in `GET /snapshot` (`image_status`). `runquill status` runs in its own process and sees only whether the image is there (`ready` or `missing`). Jobs that would run there wait with `waiting for runner image IMAGE on HOST`.
- Credentials typed into an image reference are removed from logs and pull errors.

The exceptions:

- Outbound `agent://` workers keep their own preloaded image. The manager cannot pull there, so load the image on the worker yourself. [Outbound workers](outbound-workers.md)
- A locally built image such as `runquill-runner:local` exists in no registry. Build it on every host that needs it. With `missing`, a present local image is used as is. With `always`, its refresh fails each interval and the local image stays in use, so prefer `missing` or `never` for local builds.
- With `never`, nothing is pulled. A host without its image is reported unavailable, and a job that selects a catalogue image waits until the image appears. Runquill checks for it at most every 15 seconds.
- On K3s, the kubelet pulls images. `RUNQUILL_IMAGE_PULL` is still validated but has no effect.

`runquill doctor` reports a missing image as pending while pulls are on, instead of failing the check. Under Compose, set the variables through the [settings overlay](operations.md#add-manager-settings).

## Private registries

The manager runs `docker pull` itself, with `--host ssh://…` for SSH hosts, so the registry login must be in the Docker client configuration of the manager, not of the hosts. Create a directory holding a `config.json` with an `auths` entry for the registry (credential helpers are not available inside the manager image), mount it read-only into the manager, and point `DOCKER_CONFIG` at it.

```yaml
services:
  manager:
    environment:
      DOCKER_CONFIG: /run/docker-config
    volumes:
      - ./docker-config:/run/docker-config:ro
```

Protect that directory like the GitHub App key. Outbound workers pull nothing for the manager and need their own image supply. On K3s, use `RUNQUILL_K8S_IMAGE_PULL_SECRET`. Private registries have not yet been exercised.

## Tool cache

In a pool with `cache: true`, each runner mounts the repository's existing `runquill-cache-<repository ID>` volume at `/home/runner/.cache` for package and build-tool caches. A separate volume at `/home/runner/.cache/tools` scopes `RUNNER_TOOL_CACHE` to that repository and the runner's actual image ID and platform. Jobs on the same host reuse downloaded `actions/setup-*` tools when those values match. Jobs can read and modify their mounted caches. Moving a tag to a new image ID starts a fresh tool cache.

Tools that already keep their download cache under `~/.cache`, such as pip, uv, Poetry, Yarn 1 and the Go build cache, reuse it with no setting. The runner also points these download caches into the volume:

| Variable | Path | Default it replaces |
| --- | --- | --- |
| `npm_config_cache` | `/home/runner/.cache/npm` | `~/.npm` |
| `GOMODCACHE` | `/home/runner/.cache/go-mod` | `~/go/pkg/mod` |
| `NUGET_PACKAGES` | `/home/runner/.cache/nuget-packages` | `~/.nuget/packages` |
| `BUN_INSTALL_CACHE_DIR` | `/home/runner/.cache/bun` | `~/.bun/install/cache` |

A job or workflow `env` that sets one of them takes precedence. Runquill does not move a whole tool home, because it also holds credentials, settings and installed binaries: set `GRADLE_USER_HOME`, `CARGO_HOME` or Maven's `-Dmaven.repo.local` in the workflow yourself if you accept sharing them. With these variables set, the `cache:` option of `actions/setup-node` and `actions/setup-go` saves and restores the persistent directory through the GitHub Actions cache, which a cached pool usually no longer needs. `actions/setup-go` caches by default, and its restore can report tar errors over the read-only module files a previous job left. Set `cache: false` on it in cached pools. Every job of the repository on that host can write these caches, so a job can leave altered packages for the next one. Outbound workers set the variables after they are upgraded.

After upgrading, the first job for each image starts with an empty tool cache. The old shared tool files stay in the repository volume under the new mount, and other repository cache files remain available. The cache works on `local`, `ssh://`, and outbound `agent://` workers. Upgrade outbound workers to enable the new tool cache layout on each worker.

The Docker integration test exercises cache reuse and isolation with a toolchain marker. A real `actions/setup-*` job is still a separate verification step.

## Docker Hub mirror

A privileged (Docker-in-Docker) runner starts its own `dockerd` with empty storage, so every `docker pull`, `docker run` and `docker build` base image of a job downloads from Docker Hub again. `RUNQUILL_REGISTRY_MIRROR` points that inner `dockerd` at a Docker Hub pull-through cache near your hosts with `--registry-mirror`. It is off by default.

The value must be an `http://` or `https://` origin without a path, query or credentials, such as `http://10.0.0.5:5000`. Runquill refuses to start with any other value, including `localhost` and loopback addresses, because inside a runner they name the runner container itself. Whoever runs `docker create` reads it:

| Host | Where to set it | Address to use |
| --- | --- | --- |
| `local` | the manager | The private address of the host the mirror publishes on. Runner containers reach a port published on that address through Docker's port forwarding; a port published on `127.0.0.1` is not reachable. On Docker Desktop, `http://host.docker.internal:PORT` also works. |
| `ssh://` | the manager | One value applies to `local` and every SSH host, so use an address that all of them can route to, such as the mirror host's private LAN address. |
| `agent://` | the worker process | The manager's value does not reach outbound workers. Set it in the worker's own environment, which can point at a mirror near that worker. See [outbound workers](outbound-workers.md#connect). |
| K3s | `runquill-config` | Applies only with `RUNQUILL_K8S_RUNNER_MODE=dind`. Use an address the Pod network reaches, such as a node's private address or a Service name. The Compose overlay below does not apply. |

Only privileged runners receive the variable. Runners in a pool with `privileged: false` have no inner `dockerd` and are unchanged.

### Compose

`compose.mirror.yaml` runs the CNCF Distribution `registry` image as a Docker Hub proxy with its cache in the `registry-mirror` volume, and passes `RUNQUILL_REGISTRY_MIRROR` to the manager. Add to `.env`:

```sh
RUNQUILL_REGISTRY_MIRROR=http://10.0.0.5:5000
RUNQUILL_REGISTRY_MIRROR_BIND=10.0.0.5:5000
# Optional: how long cached content is kept, 168h by default.
RUNQUILL_REGISTRY_MIRROR_TTL=168h
```

`RUNQUILL_REGISTRY_MIRROR_BIND` is the host address and port the mirror publishes on. Use a private address, not `0.0.0.0` on a host with a public address, and allow only your runner hosts in the firewall: the mirror has no authentication, so anyone who reaches it can pull public images through it and fill its disk. Then apply it with your other overlays:

```sh
docker compose -f compose.yaml -f compose.mirror.yaml up -d
```

To use a mirror you already run instead, leave out `compose.mirror.yaml` and add `RUNQUILL_REGISTRY_MIRROR` to your [settings overlay](operations.md#add-manager-settings). Compose passes only the variables it lists, so a value only in `.env` does nothing.

The overlay deliberately configures no Docker Hub login. With upstream credentials, the mirror would serve that account's private images to every job of every repository on these hosts, and to anyone else who reaches it. Plain `http://` keeps registry traffic unencrypted on your network; use `https://` behind a TLS proxy if that network is not trusted.

### Limits

- **It saves the download, not the extraction.** Each job still unpacks every layer into the fresh `vfs` storage of its inner `dockerd`, which takes time and disk.
- **Docker Hub only.** Docker applies mirrors to `docker.io` images. Images from `ghcr.io`, `quay.io` or other registries still download directly.
- **It needs the runner image of this release.** The runner entrypoint adds `--registry-mirror`. Older runner images ignore the variable and pull from Docker Hub.
- **BuildKit containers are not covered.** `docker pull`, `docker run` and `docker build` with the default builder use the mirror. A `docker-container` builder, such as the one `docker/setup-buildx-action` creates by default, runs its own BuildKit with its own registry configuration and needs the mirror in its [`buildkitd.toml`](https://docs.docker.com/build/buildkit/toml-configuration/).
- **The cache is bounded only by the TTL and the disk.** The registry deletes cached content once it is older than the TTL and fetches it again on the next pull. It has no size limit, so watch the `registry-mirror` volume.
- **A failing mirror is silent.** If the mirror refuses the connection or fails, `dockerd` logs a warning and pulls from Docker Hub directly. An address whose packets are dropped can add a connect timeout to every pull. Check the registry log or `curl http://ADDRESS/v2/_catalog` to confirm that runners use it.

## Cleanup of unused images and cache volumes

The manager removes runner images and cache volumes that nobody has used for a while on the `local` host and `ssh://` hosts. It records, per host, when it last started a runner with each image and cache volume, and which images its own puller pulled.

| Variable | Default | Effect |
| --- | --- | --- |
| `RUNQUILL_CLEANUP_AFTER` | `30d` | Retention for both kinds. `off` or `0` turns the automatic cleanup off. |
| `RUNQUILL_CLEANUP_IMAGES_AFTER` | the value above | Retention for images only, or `off`. |
| `RUNQUILL_CLEANUP_CACHE_AFTER` | the value above | Retention for cache volumes only, or `off`. |
| `RUNQUILL_CLEANUP_INTERVAL` | `6h` | Time between passes, from `5m` to `7d`. The first pass runs at startup. |

A retention is a Go duration such as `720h` or a whole number of days such as `30d`, and at least `1h`.

Only two kinds of object are ever removed:

- a cache volume labelled `io.runquill.cache=1` and named `runquill-cache-<digits>` or `runquill-cache-<digits>-tools-<hash>`, mounted by no container and unused for the retention;
- an image that Runquill's puller pulled on that host and that still has the image ID it pulled, which no configuration uses, no container uses and nobody has started a runner from for the retention.

Never removed:

- images you pulled or built yourself, including images pulled by the installer or by `docker pull`, and a tag that a local rebuild now points at;
- the default runner image, every pool `image`, catalogue images a waiting job asked for within the last hour, and any image with the same ID as one of them;
- images and volumes in use by any container, and an image being pulled;
- anything on outbound `agent://` workers and on K3s, which are skipped.

Removal re-checks each object first, removes images by reference and without force, and leaves alone what the engine refuses. One host's failure does not stop the others.

**First start after the upgrade.** An object Runquill has no record of is first seen when the cleanup finds it. Existing cache volumes are therefore kept for at least one full retention after the upgrade, and images pulled before the upgrade were not pulled by Runquill's puller, so they are never removed.

Run the same selection by hand:

```sh
docker compose exec manager runquill gc --unused --dry-run
docker compose exec manager runquill gc --unused
docker compose exec manager runquill gc --unused --older-than 7d
```

`--dry-run` lists host, kind, name, idle time and size and changes nothing, not even the records. Without it, the command removes and prints a result per object. `--older-than` replaces the retention for both kinds and is required when `RUNQUILL_CLEANUP_AFTER=off`. `--unused` cannot be combined with `--remove-orphans`.

`runquill doctor` prints the cleanup policy and, per `local` and `ssh://` host, how many cache volumes and Runquill-pulled images it holds and their size.

## Feature status

Exercised on a developer machine with real Docker: pulls from a registry, cleanup against real images and volumes, and all five OS variants started as runners. The Docker smoke test alternates between Ubuntu 24.04 and Debian 13 for the same repository, checks that each image reuses only its own tool cache, and verifies that the package cache stays shared. It runs in both plain and privileged mode. The Docker Hub mirror was exercised with `compose.mirror.yaml` and a privileged runner image on Docker Desktop: `docker pull` and `docker build` went through an `http://` mirror without `--insecure-registry`, and a later pull was served from the cache while the mirror could not reach Docker Hub.

Not yet exercised: a real `actions/setup-*` job using the tool cache, real npm, Go, NuGet and Bun jobs using the package cache variables, on-demand pulls on `ssh://` hosts, private registries, outbound `agent://` workers with `os__` selectors, and the Docker Hub mirror on Linux hosts, `ssh://` hosts, outbound workers, K3s and with `docker-container` builders.
