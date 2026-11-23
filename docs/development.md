# Develop, test and release Runquill

[Runquill README](../README.md) · [한국어](development.ko.md)

## Repository layout

| Path | Purpose |
| --- | --- |
| `main.go` | Executable entry point for `go build .` and `go run .`. |
| `internal/app/` | CLI, manager, persistent queue, runner backends, GitHub integration and panel API. Integration tests live beside the manager code. |
| `internal/workflow/` and `internal/route/` | Offline workflow inspection and rewriting, and the runner label selectors shared with the scheduler. |
| `internal/webui/` | Embedded web panel and setup wizard assets. The release demo is packaged from the panel files. |
| `internal/backupseal/` | The sealed backup format and its streaming encryption and decryption. |
| `runner/`, `deploy/`, `api/`, `examples/` | The runner image, K3s manifest, OpenAPI contract and starter workflows. The installer and Compose files are at the root. |

## Develop and release

Use Go 1.26.11 or later. The manager builds as a native binary with the Docker CLI as its external runtime dependency.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o runquill .
```

The manager and worker run only on Linux, and the tests run there. Windows and macOS builds are for the [remote CLI](remote-cli.md) and `runquill workflow check`, and CI cross-compiles both.

The runner Dockerfile pins the upstream GitHub Actions runner image by digest. Use `--build-arg RUNQUILL_RUNNER_BASE=IMAGE` to extend a compatible image with your own toolchain. Pass `--build-arg RUNQUILL_RUNNER_OS=` with `ubuntu22.04`, `ubuntu26.04`, `debian12` or `debian13` to build another base. Keep the runner files in `/home/runner` and its user at UID 1001 for Kubernetes plain mode and pools with `privileged: false`.

Run the nested Docker smoke test with `RUNQUILL_TEST_DOCKER_IMAGE=runquill-runner:local go test ./internal/app -run '^TestDockerRunner$' -v` after building the runner image. It checks Docker build, PostgreSQL, bind mounts, and removal with a local stand-in for the GitHub listener. `-run '^TestDockerWarmRunner$'` with the same image starts [warm runners](workers.md#keep-runners-warm) in both runner modes and hands each a JIT config.

[CI](../.github/workflows/ci.yml) runs the Go checks in parallel with the native amd64 and arm64 image builds and nested Docker smoke tests. The amd64 runtime check also runs K3s to verify host maintenance, runner assignment, and Pod and Secret cleanup. Changes to the runner image or its build workflows trigger the four additional OS builds. The [two-account OAuth check](../internal/app/panel_live_oauth_test.go) can be run with a dedicated OAuth App and separate GitHub accounts for the administrator and viewer.

On a version tag, the [release workflow](../.github/workflows/release.yml) builds each image once across native architecture and OS jobs. It publishes the tested images to GHCR and takes the standalone binaries from those same manager images. It then verifies the published release with the public installer on both architectures. Forks publish to their own GitHub owner's namespace. Set both packages to public to allow anonymous pulls. [GHCR access](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

## Installer and upgrade checks

On Linux, run the shell syntax check and the stubbed host setup tests. The offline tests use a fake host filesystem and substitute commands.

```sh
sh -n install.sh
sh .github/scripts/test-install-offline.sh dash
sh .github/scripts/test-install-offline.sh bash
```

With a Docker daemon and Compose, run the same release-pair rehearsal as CI:

```sh
python3 .github/scripts/smoke-docker-upgrade.py v0.3.4 v0.4.0
```

It seeds panel settings and a queued job in the older release, upgrades the same persistent queue, then enrolls a separate Docker worker and checks that it finishes the original job and cleans up the runner. Following publication, the release workflow selects the previous public release and tests it against the new tag. The rehearsal uses a local GitHub API fixture.

## Live GitHub App webhook test

For an existing deployment, use the [operations runbook](operations.md#verify-webhook-only-delivery). The [live smoke script](../.github/scripts/smoke-live-webhook.py) instead creates a disposable manager, Docker worker, and HTTPS relay. Run it manually from a private fixture's default branch on Linux with Docker, Python 3, OpenSSL and curl. Build the source checkout with `docker build -t runquill-live-manager:ci .` first.

Use a dedicated personal-account GitHub App installed on that one private repository with **Only select repositories**. Configure an initial webhook URL for the fixture App with HTTPS verification and JSON delivery, then subscribe to **Workflow job**. Use a fixture that no other manager or runner can claim. On the fixture's default branch, add `.github/workflows/runquill-webhook-job.yml` with a required string input named `nonce` under `workflow_dispatch` and one short job with the [per-job Runquill label](workflows.md#choose-a-runner).

Pass `GITHUB_REPOSITORY=OWNER/REPO`, `GITHUB_REF=refs/heads/DEFAULT_BRANCH`, and a `GITHUB_TOKEN` that can read and dispatch the fixture's Actions workflow. Pass `RUNQUILL_APP_ID`, `RUNQUILL_APP_PRIVATE_KEY` and `RUNQUILL_WEBHOOK_SECRET` from protected CI secrets or variables. Invoke `python3 .github/scripts/smoke-live-webhook.py` from the Runquill checkout. The script disables polling, checks signed queued and completed deliveries, verifies execution on its Docker worker, and checks container and registration removal. It temporarily changes the fixture App's webhook URL and restores it on exit. An always-run step in the same runner environment can also call the script with `--restore-webhook` to recover an interrupted run.
