# Workflow migration and checks

[Runquill README](../README.md) · [한국어](workflow-migration.ko.md)

Runquill can move existing jobs from GitHub-hosted labels such as `ubuntu-latest` to Runquill labels by opening a pull request, and it can check workflow files offline before you commit them.

## Permissions

Opening a migration pull request needs **Contents**, **Pull requests** and **Workflows** read and write access on the GitHub App. Runquill does not request them by default.

- For a new App, run `runquill setup --migrate`. Setup adds the three permissions to the App manifest. The installer and the browser wizard do not offer this option.
- For an existing App, add the three permissions in the App's settings on GitHub, then approve the new permissions on its installation.

The panel's **Migrate** screen shows which permissions are missing. A preview needs only read access to contents.

## Panel flow

Migration is available to administrators on the panel's **Migrate** screen. API tokens cannot use it.

1. **Permissions** checks the installation.
2. **Repository and labels** takes a private repository owned by the configured account and up to 50 mappings. Each mapping lists one to ten `runs-on` labels, compared as a set without regard to case or order, and an optional target pool.
3. **Preview** reads every `.yml` and `.yaml` file directly under `.github/workflows` on the default branch (at most 50 files of up to 512 KiB) and shows a diff for each file and the outcome for each job. Nothing is written.
4. **Open pull request** repeats the preview with a write token and continues only if the result still matches the reviewed preview. It creates the branch `runquill/migrate-runs-on-<hash>` from the default branch, commits each changed file, and opens one pull request that lists the changed jobs and the jobs left alone.

The request, the opened pull request or the failure are recorded in the audit log. Merge the pull request yourself once the target pools are ready.

## What changes and what never changes

For each matched job, only the `runs-on` value is replaced with a per-job Runquill label, with `strategy.job-index` added for matrix jobs and a `pool__NAME__` prefix for a pool other than `default`. When the job's labels include exactly one GitHub-hosted Ubuntu version label, such as `ubuntu-22.04`, and the manager's [runner image catalogue](runner-images.md#the-runner-image-catalogue) has that entry, the label also gets `os__ubuntu2204__` so the job keeps its OS. `ubuntu-latest` gets no `os__` selector because it is the default image. No other line changes, and the rewritten file must parse back to the same jobs before it is accepted.

Runquill never merges, never pushes to an existing branch, never writes outside `.github/workflows`, and never touches public repositories, repositories outside the App installation or repositories of another owner. If a commit or the pull request fails, it deletes the branch it created when nothing else has moved it.

A job is left unchanged, with the reason in the preview, when:

- no mapping matches its labels;
- `runs-on` is an expression, uses `group` or `labels`, already targets self-hosted or Runquill runners, or is missing;
- the job calls a reusable workflow, or the file itself is a reusable workflow (`workflow_call`);
- the job uses YAML anchors or a form the scanner does not read reliably;
- its job ID cannot form a Runquill label, starts with `pool__` or `os__`, or differs from another job ID only in letter case.

A whole file is left unchanged when it is too large, has several YAML documents, is indented with tabs, contains a stray carriage return, or is not valid UTF-8.

The request is refused when the App lacks permissions, the repository has more than 50 workflow files, the files changed since the preview, nothing matches, a migration branch for the same change already exists, or the installation's repository access does not match `RUNQUILL_REPOSITORY_SCOPE`.

The rewriter is covered by unit tests and the GitHub calls by tests against a fake GitHub API. Migration has not yet been exercised against real GitHub, so review the diff of the first pull request carefully.

## Check workflows offline

`runquill workflow check` reads local workflow files and reports problems. It makes no network calls.

```sh
runquill workflow check                 # .github/workflows in the current directory
runquill workflow check --json path/to/workflow.yml
```

| Code | Severity | Finding |
| --- | --- | --- |
| `untrusted-trigger` | high | A self-hosted job runs on `pull_request_target` or `workflow_run`. |
| `fork-trigger` | warning | A self-hosted job runs on `pull_request` or review events, which can run fork code. |
| `runs-on` | warning | A Runquill label is malformed: not the only label, not built with `format()`, a matrix job (one whose `strategy` has a `matrix` key) without `strategy.job-index`, an invalid pool or `os__` suffix or a label that can exceed 100 characters. Also flags self-hosted jobs that Runquill will not serve. |
| `duplicate-suffix` | warning | Two jobs in the same file build the same Runquill label, so either job can take the other's runner. The whole lowercase suffix is compared, including `pool__` and `os__` selectors, and a matrix suffix with `strategy.job-index` also matches a literal suffix such as `test-1`. |
| `pool` | warning | The label names a pool missing from the configured worker pools. |
| `runner-image` | warning | The label selects an `os__NAME__` image missing from the runner image catalogue, or one its pool's `images` list does not allow. |
| `docker` | warning | The job seems to need Docker (by a heuristic), but its pool sets `privileged: false`. |
| `permissions` | warning | The workflow has no top-level `permissions` block. |
| `timeout` | warning | A job has no `timeout-minutes`. |
| `unpinned-action` | info | An action outside `actions/` and `github/` is not pinned to a full commit SHA. |
| `unparsed` | warning or info | A file or job could not be checked. |

The pool checks use the worker pools the manager would apply. When `RUNQUILL_DB` points to an existing queue database with operation settings saved in the panel, the command opens it read-only, uses those pools, and prints a note on standard error. Run it in the manager environment to get this. Otherwise, or when the database cannot be read, the pool checks run only when `RUNQUILL_WORKER_POOLS` is set in the environment of the command. The `runner-image` check uses the catalogue built into the binary that runs the command plus `RUNQUILL_RUNNER_IMAGES` in its environment. Release builds know every published variant. If the manager sets `RUNQUILL_RUNNER_IMAGES`, set the same value for the command. [Runner images](runner-images.md)

The command exits with status 1 when a high-severity finding exists, or for any finding with `--strict`, and with status 2 for a missing directory, an unreadable file or a file larger than 1 MiB. Otherwise it exits with 0. `--json` prints `{"files": N, "findings": [...]}`, where each finding has `file`, `line`, optional `job`, `severity`, `code` and `message`.
