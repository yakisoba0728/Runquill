# Container engines and runner images

## Engine selection

`RUNQUILL_CONTAINER_ENGINE` selects the CLI the manager uses for the `local` host and `ssh://` hosts: `docker` (the default) or `podman`. One manager uses one engine for all of these hosts. An outbound worker reads the variable in its own environment. Kubernetes mode refuses any value other than `docker`.

- The manager image published for Compose contains only the Docker CLI. To use Podman, run the standalone `runquill` binary on a machine with the `podman` CLI installed.
- For Podman `ssh://` hosts without a path, Runquill connects to `/run/podman/podman.sock`. Append another socket path to the host entry when needed, such as `ssh://builder@worker.example.com/run/user/1000/podman/podman.sock` for a rootless user.
- Podman runners log through `k8s-file` with a 10 MB size limit and no rotation count. Cache volumes are created with `podman volume create` before the runner.
- [Runner logs](panel.md#runner-logs) and [elastic limits](elastic-limits.md) always call the `docker` CLI and therefore do not support Podman hosts.

`runquill doctor` reports the engine, its version, platform, cgroup version and whether it is rootful or rootless for every host.

## Rootless engines

Runquill asks each engine whether it is rootless before it starts a runner there, and `doctor` reports the same.

- Runners on a rootless host start in plain mode: UID 1001, all capabilities dropped, `no-new-privileges` and no Docker-in-Docker, as with a pool that sets `privileged: false`. Jobs on that host cannot use `docker`, service containers, job containers or `sudo`.
- A pool that sets `"privileged": true` explicitly on a rootless host is refused with an error, because Docker-in-Docker is unreliable there.

Podman and rootless engines are implemented and covered by unit tests with recorded engine output. They have not yet been exercised on a real Podman or rootless host. The supported and tested setup is rootful Docker on Linux with cgroup v2.

## Runner image variants

Releases publish the runner image in three variants for amd64 and arm64.

| Tag | Base |
| --- | --- |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.0` | Ubuntu 24.04, the upstream GitHub Actions runner image |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.0-ubuntu22.04` | Ubuntu 22.04 |
| `ghcr.io/yakisoba0728/runquill-runner:v0.3.0-debian12` | Debian 12 |

All variants carry the same pinned Actions runner and Docker binaries and a `runner` user with UID 1001. To build one from source, pass `--build-arg RUNQUILL_RUNNER_OS=ubuntu22.04` or `debian12` to `docker build runner`. Use a variant as the fleet-wide `RUNQUILL_RUNNER_IMAGE` or as a pool's `image`.

CI builds the Ubuntu 22.04 and Debian 12 variants and checks the runner user and versions. Only the default variant runs the nested Docker test.

## Pull images on every host

Runquill starts runners with `--pull never`, so every host must already have the image. Pull or build the image named by `RUNQUILL_RUNNER_IMAGE`, and each pool `image`, on every Docker host, SSH worker and outbound worker before the manager uses it. `runquill doctor` reports a missing image or one built for another platform. A job sent to a host without the image fails to start and is retried.
