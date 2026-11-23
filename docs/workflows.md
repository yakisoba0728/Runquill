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

In a [worker pool](workers.md#route-jobs-to-worker-pools) with `cache: true` and the default `privileged: true`, BuildKit can also keep its cache in a directory under `/home/runner/.cache`. That directory lives in the repository's cache volume on the host, so the next job of the same repository on the same host reads it without a download. Jobs on other hosts start cold, and the manager removes a cache volume no runner has used for 30 days.

```yaml
concurrency:
  group: buildx-cache-runquill-app

jobs:
  build:
    runs-on: ${{ format('runquill-{0}-{1}-pool__build__docker-build', github.run_id, github.run_attempt) }}
    timeout-minutes: 15
    steps:
      - uses: docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069 # v4.4.1
      - uses: docker/build-push-action@c3c9e263c25d99ce0380d002d59b67737d91b0dc # v7.4.0
        with:
          load: true
          tags: runquill-app:ci
          cache-from: type=local,src=/home/runner/.cache/buildx/runquill-app
          cache-to: type=local,dest=/home/runner/.cache/buildx/runquill-app,mode=max,reset=true
```

- `reset=true` deletes blobs that no cache tag references any more. Without it, every export leaves the previous blobs behind and the directory grows until the volume fills the host's disk. It needs Buildx 0.35.0 or later. With an older Buildx, export to a new directory with `dest=` and replace the old directory with it in a later step.
- The local cache backend needs a builder other than the default `docker` driver, unless that daemon uses the containerd image store. `docker/setup-buildx-action` creates a `docker-container` builder by default.
- Jobs of one repository that run at the same time on one host write the same directory. Give each image its own directory and a workflow `concurrency` group, as above. GitHub then runs one build at a time and keeps only the newest waiting run, cancelling older pending ones.
- Jobs from one repository share the volume, so any of them can change cached layers that later builds use. Do not use this for workflows that build untrusted code.

[Docker local cache](https://docs.docker.com/build/cache/backends/local/) · [Local cache in GitHub Actions](https://docs.docker.com/build/ci/github-actions/cache/)

### Move existing workflows

Administrators can open a pull request that moves jobs from GitHub-hosted labels to Runquill labels from the panel's **Migrate** screen. It needs Contents, Pull requests and Workflows write access, which only `runquill setup --migrate` requests for a new App. Runquill previews the diff first, changes only `runs-on`, leaves jobs it cannot rewrite safely alone with a reason, and never merges. A GitHub-hosted Ubuntu version label such as `ubuntu-22.04` becomes an `os__` selector when the manager's catalogue has that OS. Migration has not yet been exercised against real GitHub. [Workflow migration](workflow-migration.md)

`runquill workflow check` checks local workflow files for malformed Runquill labels, suffixes shared by several jobs in one file, unknown pools and runner OS names, risky triggers on self-hosted runners, missing permissions and timeouts, and unpinned actions. It exits with status 1 for a high-severity finding, or for any finding with `--strict`, and `--json` prints the findings. [Check codes](workflow-migration.md#check-workflows-offline)

## Run jobs from public repositories

Runquill ignores jobs from public repositories unless the deployment sets `RUNQUILL_ALLOW_PUBLIC_REPOSITORIES=true`. Set it in `.env` and pass it through a [Compose overlay](operations.md#add-manager-settings), or add it to the K3s `runquill-config` Secret. The panel's Settings screen cannot change it, and the panel overview, `runquill status --json` and `runquill doctor` show it when it is on. Internal organization repositories stay excluded.

When it is on, Runquill takes jobs from public repositories owned by `RUNQUILL_OWNER` and keeps every other check: owner, repository ID, archived and disabled state, and App installation scope.

### Require approval for all external contributors

Before it starts a runner for a public repository, Runquill reads the repository's fork pull request approval policy. It must be `all_external_contributors`: in the repository's **Settings → Actions → General**, under approval for fork pull request workflows, require approval for all external contributors. Until it is, or while the GitHub App cannot read the policy, the job waits with that reason in the panel and retries with a growing delay. Reading the policy needs repository **Administration** read access. Apps that register runners on repositories already have it. An App registered for an organization runner group needs it added. `runquill diagnose OWNER/REPO` reports the result for a public repository.

### How Runquill keeps fork runs off your runners

Anyone can fork a public repository and open a pull request that changes `runs-on` to a Runquill label, and labels are predictable from the workflow file and the run ID. GitHub gives a Runquill runner to any queued job in the repository with the same label, so a fork job could aim at the label of a job Runquill accepted. Runquill guards against both:

- It fails a job whose own workflow run comes from a fork or from a deleted fork, with the reason in the panel.
- Before registering each runner, it lists the repository's queued, in-progress and waiting workflow runs. While a run from a fork has an unfinished job with a `runquill-` label, the job waits with the fork run's number in the reason. Cancel that run to continue.

This costs one approval read and three run listings per launch, plus a job listing for each unfinished fork run. Private repositories make none of these calls. Some risk remains. If an administrator approves a fork pull request whose workflow uses a Runquill label, public repository jobs wait until that run finishes or is cancelled. Check `runs-on` changes before approving. A fork job approved in the seconds between the check and the runner taking its job could still win the runner. Runquill does not detect a runner that took a different job.

### Workflows that run in the base repository

A workflow run that GitHub starts in the base repository still runs, even when someone outside the repository triggered it. `pull_request_target`, `workflow_run`, `issue_comment`, `issues`, `pull_request_review`, `pull_request_review_comment`, `discussion`, `discussion_comment`, `fork` and `watch` let anyone with a GitHub account start a base repository workflow, which can read repository secrets. Never check out, build or run pull request code in a job that uses a Runquill label. Never put event fields such as an issue or pull request title or body directly into a `run:` script, because they are script injection. Pass them through an `env:` variable instead. The offline `fork-trigger` check flags some risky triggers, but it cannot tell whether a repository is public.

### Isolate public repository jobs

- Run them on a dedicated host in an unprivileged [worker pool](workers.md#route-jobs-to-worker-pools) with `privileged: false` and with `repositories` limited to the public repositories. If the pool sets `cache: true`, public repositories default to [`cache_writes: default_branch`](workers.md#protect-the-shared-cache): only push, schedule and manual runs on the default branch write the shared cache, and other jobs get an empty one. Setting `cache_writes` to `all` lets one job poison the next.
- With an [organization runner group](install.md#connect-a-github-organization), use a group only for public repositories and allow public repositories in it. Do not mix private repositories into that group. GitHub does not assign a public repository's jobs to a group that disallows them, and Runquill reports a group that disallows public repositories but contains one.

[GitHub security hardening for self-hosted runners](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions#hardening-for-self-hosted-runners) · [Script injection](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions#understanding-the-risk-of-script-injections)
