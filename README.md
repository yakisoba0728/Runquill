# Runquill

English · [한국어](README.ko.md)

Runquill runs GitHub Actions jobs from private repositories on your own Linux servers. Public repositories are an [opt-in](docs/workflows.md#run-jobs-from-public-repositories) that refuses workflow runs from forks and requires approval for all external contributors. A GitHub App receives jobs by webhook, polling, or both. Runquill starts a disposable runner for each matching job and removes it when the job finishes.

[Quickstart](#quickstart) · [First job](#verify-your-first-job) · [Sample panel](https://yakisoba0728.github.io/Runquill/) · [Releases](https://github.com/yakisoba0728/Runquill/releases)

## Quickstart

### Install a public release

On an amd64 or arm64 Linux server with cgroup v2, run:

```sh
cd ~ && curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh && sh install.sh --wizard
```

Open the printed URL. Over SSH, first run the printed forwarding command on your computer. In the wizard, connect GitHub, install the App on **All repositories** or **Only select repositories**, then deploy. Keep the setup URL private. Add `--no-tailscale` to skip automatic Tailscale setup when using polling or your own tunnel.

[Installation guide](docs/install.md) · [Browser wizard](docs/browser-setup.md) · [Cloudflare Tunnel](docs/cloudflare-tunnel.md)

### Verify your first job

Copy [the first-job workflow](examples/runquill-first-job.yml) into `.github/workflows/` in a private repository covered by the App. Commit it to the default branch and select **Actions → Runquill first job → Run workflow**. Check that **Show the runner** prints a `runquill-` name and that the panel or manager logs record runner cleanup.

For an existing workflow, change `runs-on` for each job you want Runquill to execute:

```yaml
runs-on: ${{ format('runquill-{0}-{1}-build', github.run_id, github.run_attempt) }}
```

Use a distinct suffix such as `build` or `test` per job. Add `strategy.job-index` for matrix jobs. For a private repository under the same account with **All repositories** access, the workflow needs no extra Runquill secrets or webhook.

[Workflow guide and matrix examples](docs/workflows.md) · [Workflow migration](docs/workflow-migration.md)

## Choose a backend

| Backend | Runner |
| --- | --- |
| Docker Compose | One container per job. Docker builds, service containers and job containers work in privileged pools. |
| K3s `plain` | One restricted Pod per job for steps that run directly in the Pod. |
| K3s `dind` | One privileged Docker-in-Docker Pod per job for Docker builds, service containers and job containers. |

Run one manager per fleet and keep its SQLite queue on persistent storage. Use a dedicated host for trusted jobs. The manager can control the Docker daemon, and the GitHub App key grants access to the repositories covered by the installation.

[Docker workers and pools](docs/workers.md) · [K3s deployment](deploy/k3s/README.md)

## Documentation

| Task | Guide |
| --- | --- |
| Install, choose repository access, or register an organization App | [Installation](docs/install.md) |
| Publish the panel and receive signed webhooks over HTTPS | [Cloudflare Tunnel](docs/cloudflare-tunnel.md), [Tailscale and other proxies](docs/install.md#use-an-existing-github-app-or-configure-webhooks) |
| Run a job, migrate a workflow, or choose an OS | [Workflows](docs/workflows.md), [Migration](docs/workflow-migration.md), [Runner images and cache](docs/runner-images.md) |
| Scale to more hosts or tune capacity | [Workers and pools](docs/workers.md), [Outbound workers](docs/outbound-workers.md), [Container engines](docs/container-engines.md), [Elastic limits](docs/elastic-limits.md) |
| Inspect, troubleshoot, upgrade, or back up a deployment | [Operations](docs/operations.md), [Web panel](docs/panel.md), [Backups](docs/backups.md), [Metrics](docs/metrics.md) |
| Operate via scripts or a coding agent | [Remote CLI](docs/remote-cli.md), [MCP](docs/mcp.md), [Panel API](api/openapi.yaml) |
| Work on the repository | [AGENTS.md](AGENTS.md), [Development and release](docs/development.md) |

[Apache License 2.0](LICENSE)
