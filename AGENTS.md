# Working on Runquill

[README](README.md) · [한국어](README.ko.md) · [Development and release](docs/development.md)

Runquill is a Go manager and CLI for ephemeral GitHub Actions runners in private repositories, and in public ones only with `RUNQUILL_ALLOW_PUBLIC_REPOSITORIES`. The GitHub App receives or polls jobs, SQLite stores the queue and leases, and the scheduler starts runners on Docker hosts or K3s. The panel UI is shipped with the binary.

## Where to start

`main.go` is the executable entry point. The [repository layout](docs/development.md#repository-layout) lists the packages and deployment files.

| Task | Source and contract |
| --- | --- |
| Install or upgrade | `install.sh`, `internal/app/setup*.go`, `internal/app/wizard*.go`, `compose*.yaml`, `deploy/k3s/`. See [installation](docs/install.md) and [operations](docs/operations.md). |
| Match a workflow or accept a job | `internal/workflow/`, `internal/route/`, `internal/app/webhook.go`, `internal/app/poller.go`, `examples/`. Use the [job-specific label rules](docs/workflows.md). |
| Retry, place or clean up jobs | In `internal/app/`, start at `queue.go`, `scheduler.go`, `worker_*.go`, `docker*.go`, `kubernetes*.go`, or `gc.go`. Preserve GitHub job ID deduplication and runner lease reconciliation. Never start a runner for a public repository's workflow run from a fork. |
| Debug runner images, OS or cache | `runner/`, `internal/app/runner_images*.go`, `internal/app/runner_runtime.go`, `internal/app/worker_pools.go`. See [runner images](docs/runner-images.md). |
| Change the panel, CLI or MCP | `internal/webui/`, `internal/app/panel*.go`, `internal/app/cli*.go`, `internal/app/mcp*.go`, `api/openapi.yaml`. See [panel roles](docs/panel.md#roles-and-tokens), [remote CLI](docs/remote-cli.md) and [MCP](docs/mcp.md). |
| Change backup storage or the sealed format | `internal/app/backup*.go`, `internal/backupseal/`. See [backups](docs/backups.md). |
| Check release and integration tests | `.github/workflows/`, `.github/scripts/`. See [development and release](docs/development.md). |

## Work and verify

- Check `go.mod` for the required Go version. Run the tests closest to your change, `go test -race ./...` and `go vet ./...` for Go changes. The panel is embedded static HTML, CSS and JavaScript and has no frontend build step.
- For an installer change, run `sh -n install.sh` and `sh .github/scripts/test-install-offline.sh sh`. See [development](docs/development.md) for Docker and GitHub App smoke tests.
- For a workflow example, run `go run . workflow check --strict path/to/workflow.yml`. Use a different Runquill label suffix for each job and add `strategy.job-index` to matrix jobs.
- If you change behavior described in an English and Korean guide, update both. Keep the README install and first-job anchors usable by already released binaries and panels.
- Use local tests or a private fixture for integration checks. Keep real `.env` files, App PEMs, webhook secrets, panel tokens, setup links, and tunnel credentials out of commits and output.

## Inspect a live deployment

Start with the [operations checks](docs/operations.md) and the Compose files that started the manager. Use `runquill status --json`, `runquill doctor --repo OWNER/REPO`, and `runquill diagnose OWNER/REPO` from the manager environment. For a remote coding agent, use a [read-only MCP connection](docs/mcp.md) or the [remote CLI](docs/remote-cli.md) with a `fleet:read` token. The queue volume contains persisted settings and leases. Keep it attached during service recreation and upgrades.
