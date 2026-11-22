# Use Runquill in GitHub Actions

[Runquill README](../README.md) · [한국어](workflows.ko.md)

## Connect a repository

- With **All repositories**, use the workflows below in other private repositories owned by the same account or organization. Public repositories need the [opt-in below](#run-jobs-from-public-repositories). Runquill credentials and the webhook belong to the App installation, so each repository needs no extra Runquill secrets or webhook.
- With **Only select repositories**, select the target repository in the GitHub App installation settings.
- For work owned by a different organization, set up a [separate installation for that organization](install.md#connect-a-github-organization).

## Verify your first job

From the root of a private Git repository checkout, generate the [first-job workflow](../examples/runquill-first-job.yml). Use the path to your Runquill installation if it differs from the quickstart.

```sh
"$HOME/runquill/runquill" workflow init
```

The command creates `.github/workflows/runquill-first-job.yml` and protects any existing file at that path. If your installed version predates `workflow init` or the binary is on a different machine, copy the linked YAML into `.github/workflows/` yourself.

Commit and push the workflow to the repository's default branch or merge it through your usual review process. In GitHub, open **Actions → Runquill first job → Run workflow**. The **Show the runner** step prints a name beginning with `runquill-`. This workflow needs only a shell, so it checks runner assignment independently of application dependencies. [GitHub manual workflow requirements](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow)

On the server, use the Compose files from the original deployment to check App coverage and follow runner removal. These commands assume the panel overlay:

```sh
docker compose -f compose.yaml -f compose.panel.yaml exec -T manager runquill doctor --repo OWNER/REPO
docker compose -f compose.yaml -f compose.panel.yaml logs -f manager
```

`doctor` checks the local configuration, Docker Engine, runner image, GitHub App installation and permissions, and webhook settings if configured. It exits with status 1 if a check fails. After a successful job, look for `runner removed` and confirm the runner has disappeared from the repository's **Settings → Actions → Runners**. Run the [Docker and PostgreSQL smoke workflow](../examples/runquill-smoke.yml) when you need to verify service containers and Docker builds.

## Choose a runner

The [smoke workflow](../examples/runquill-smoke.yml) runs on Runquill by default. On **Run workflow**, select `github_hosted` to use `ubuntu-latest`. The [parallel jobs example](../examples/runquill-parallel.yml) applies the same switch to one standalone job and two matrix jobs. Copy it into `.github/workflows/` and run it to see the runner name in each job's log.

Build each label from the run ID, the run attempt, and a suffix unique to the job within that run. Write the job ID literally in the suffix. For example, the `check` job uses:

```yaml
runs-on: ${{ format('runquill-{0}-{1}-check', github.run_id, github.run_attempt) }}
```

Append `strategy.job-index` to the matrix job's own suffix. GitHub starts the index at zero, so the `test` matrix produces `test-0` and `test-1`. A rerun uses a new `run_attempt`.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-test-{2}', github.run_id, github.run_attempt, strategy.job-index) }}
```

Pass the expression as the job's single `runs-on` label. Start the suffix with a letter or digit, then use letters, digits, hyphens, or underscores. Keep the whole label within 100 characters. Runquill registers each ephemeral runner with its job's label so GitHub can match parallel jobs to their respective runners. [GitHub Actions contexts](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts)

In an existing workflow, replace `runs-on` only on the jobs you intend to move and test the workflow. To target a pool, follow the [worker pool rules](workers.md#route-jobs-to-worker-pools). For a ready-to-copy YAML file, use the [first-job example](../examples/runquill-first-job.yml).

### Choose the runner OS

Select an OS variant inside the same per-job label with `os__NAME__`, after an optional pool selector. Write the name without dots, such as `ubuntu2204`, `ubuntu2604`, `debian12` or `debian13`. A job without it runs its host's image.

```yaml
runs-on: ${{ format('runquill-{0}-{1}-os__ubuntu2204__build', github.run_id, github.run_attempt) }}
```

Names come from the runner image catalogue. Release builds include every published variant, and `RUNQUILL_RUNNER_IMAGES` adds or replaces entries. A bare `runs-on: ubuntu-22.04` is not supported, because Runquill pairs each runner with one job through its unique label. [Runner images](runner-images.md)

### Reuse Docker build layers across jobs

Copy the [cached build example](../examples/runquill-build-cache.yml) into `.github/workflows/` in a repository with a root `Dockerfile`. It builds the committed Git revision on the default branch and keeps the resulting image available for later steps in the same job. A new runner can restore layers through the GitHub Actions cache. Choose a distinct `scope` for each image and use it in both cache settings. Cache export failures leave the build usable. [Docker GitHub Actions cache](https://docs.docker.com/build/cache/backends/gha/)

The example pins both actions to exact commits, uses read-only repository permissions, and writes cache from the default branch. Pass build secrets through BuildKit secret mounts so they stay out of cached image layers. [Docker build secrets](https://docs.docker.com/build/building/secrets/)

### Move existing workflows

Administrators can open a pull request that moves jobs from GitHub-hosted labels to Runquill labels from the panel's **Migrate** screen. It needs Contents, Pull requests and Workflows write access, which only `runquill setup --migrate` requests for a new App. Runquill previews the diff first, changes only `runs-on`, leaves jobs it cannot rewrite safely alone with a reason, and never merges. A GitHub-hosted Ubuntu version label such as `ubuntu-22.04` becomes an `os__` selector when the manager's catalogue has that OS. Migration has not yet been exercised against real GitHub. [Workflow migration](workflow-migration.md)

`runquill workflow check` checks local workflow files for malformed Runquill labels, suffixes shared by several jobs in one file, unknown pools and runner OS names, risky triggers on self-hosted runners, missing permissions and timeouts, and unpinned actions. It exits with status 1 for a high-severity finding, or for any finding with `--strict`, and `--json` prints the findings. [Check codes](workflow-migration.md#check-workflows-offline)

## Run jobs from public repositories

Runquill ignores jobs from public repositories unless the deployment sets `RUNQUILL_ALLOW_PUBLIC_REPOSITORIES=true`. Set it in `.env` and pass it through a [Compose overlay](operations.md#add-manager-settings), or add it to the K3s `runquill-config` Secret. The panel's Settings screen cannot change it, and the panel overview, `runquill status --json` and `runquill doctor` show it when it is on. Internal organization repositories stay excluded.

When it is on, Runquill takes jobs from public repositories owned by `RUNQUILL_OWNER` and keeps every other check: owner, repository ID, archived and disabled state, and App installation scope. Anyone can fork a public repository, change `runs-on` to a Runquill label in a pull request, and run code on your server. Before registering a runner for a public repository, Runquill reads the job's workflow run from GitHub and fails the job with a reason in the panel when the run's head repository is a fork or no longer exists. If GitHub cannot answer, the job waits and Runquill checks again. Private repositories skip this extra API call.

A workflow run that GitHub starts in the base repository still runs, even when a fork triggered it. `pull_request_target`, `workflow_run` and `issue_comment` workflows run with base repository credentials, so never check out or build pull request code in a job that uses a Runquill label. The offline `fork-trigger` check flags risky triggers, but it cannot tell whether a repository is public.

Also:

- In the repository or organization **Settings → Actions → General**, require approval for fork pull request workflows from all external contributors.
- Run public repository jobs on a dedicated host or [worker pool](workers.md#route-jobs-to-worker-pools) with `repositories` limited to them, and prefer unprivileged pools without Docker-in-Docker.
- With an [organization runner group](install.md#connect-a-github-organization), allow public repositories in the group. GitHub does not assign a public repository's jobs to a group that disallows them, and Runquill reports a group that disallows public repositories but contains one.

[GitHub security hardening for self-hosted runners](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions#hardening-for-self-hosted-runners)
