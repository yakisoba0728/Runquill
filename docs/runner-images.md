# Runner images

Every Runquill runner starts from a runner image. This guide covers the published OS variants, how a job selects one in `runs-on`, how the manager pulls images onto its hosts, the tool cache of cached pools, and the cleanup of images and cache volumes nobody uses any more.

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

Under Compose, pass the variable through the [settings overlay](../README.md#add-manager-settings). On K3s, add it to the `runquill-config` Secret.

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

`runquill doctor` reports a missing image as pending while pulls are on, instead of failing the check. Under Compose, set the variables through the [settings overlay](../README.md#add-manager-settings).

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

After upgrading, the first job for each image starts with an empty tool cache. The old shared tool files stay in the repository volume under the new mount, and other repository cache files remain available. The cache works on `local`, `ssh://`, and outbound `agent://` workers. Upgrade outbound workers to enable the new tool cache layout on each worker.

The Docker integration test exercises cache reuse and isolation with a toolchain marker. A real `actions/setup-*` job is still a separate verification step.

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

Exercised on a developer machine with real Docker: pulls from a registry, cleanup against real images and volumes, and all five OS variants started as runners. The Docker smoke test alternates between Ubuntu 24.04 and Debian 13 for the same repository, checks that each image reuses only its own tool cache, and verifies that the package cache stays shared. It runs in both plain and privileged mode.

Not yet exercised: a real `actions/setup-*` job using the tool cache, on-demand pulls on `ssh://` hosts, private registries and outbound `agent://` workers with `os__` selectors.
