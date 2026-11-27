# Elastic runner limits

[Runquill README](../README.md) · [한국어](elastic-limits.ko.md)

A [worker pool](workers.md#route-jobs-to-worker-pools) with `cpus` or `memory_mib` gives each runner fixed Docker limits. Elastic limits let a busy runner borrow host capacity that no other runner or queued job has been promised, and give it back when it goes idle.

Elastic limits work only on the manager's `local` Docker host, for runners whose pool sets `cpus`, `memory_mib` or both. `ssh://` hosts, outbound `agent://` workers and Kubernetes are left alone, and the manager logs which hosts it skips.

## Modes

| `RUNQUILL_ELASTIC` | Behaviour |
| --- | --- |
| `off` (default) | No limit changes. Runners are still sampled for [job resources](#job-resources) unless `RUNQUILL_JOB_RESOURCES=off`. |
| `observe` | Samples runners and records the decisions it would make, without changing any container. |
| `apply` | Records the decisions and applies them with `docker update`. |

Start with `observe` and review the decisions on the panel's **Hosts** screen, in `GET /api/elastic`, or through the MCP `elastic_decisions` tool.

## Settings

| Variable | Default | Range | Effect |
| --- | --- | --- | --- |
| `RUNQUILL_ELASTIC_INTERVAL` | `10s` | `1s`–`1h` | How often runners are sampled, also for job resources. |
| `RUNQUILL_ELASTIC_COOLDOWN` | three intervals | `1s`–`1h`, at least the interval | Minimum time between two changes to one runner's CPU or memory. |
| `RUNQUILL_ELASTIC_HOST_RESERVE_CPUS` | `1` | `0` or more | CPUs never lent to runners. |
| `RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB` | `512` | whole MiB | Memory never lent to runners. |
| `RUNQUILL_ELASTIC_MEMORY_TRIGGER` | `0.9` | `0.5`–`0.99` | Share of its memory limit a runner must use before it gets more. |
| `RUNQUILL_ELASTIC_MAX_MULTIPLE` | `2` | `1`–`16` | No limit grows beyond this multiple of the pool's guarantee. |

An invalid value stops the manager at startup.

## Rules

The pool's `cpus` and `memory_mib` are each runner's guarantee. On every sample the manager computes the host surplus: the host's CPUs and memory minus the reserve, the guarantees of every runner, and the guarantees of the host's free slots, which stay reserved for jobs that could still start there.

- A runner using at least 90% of its CPU limit borrows up to half its guarantee more, from the surplus not already lent.
- A runner below half its CPU guarantee returns what it borrowed.
- If the surplus shrinks below what is lent, the largest borrowers give CPU back at once, without waiting for the cooldown.
- A runner past the memory trigger gets a quarter of its guarantee more, at least 64 MiB, from the surplus not already granted. Memory is never lowered while the runner lives. Swap always matches the memory limit.
- Limits never drop below the guarantee and never grow beyond `RUNQUILL_ELASTIC_MAX_MULTIPLE` times the guarantee.
- Only runner containers this manager labelled are changed. The manager always uses the `docker` CLI here, even with `RUNQUILL_CONTAINER_ENGINE=podman`.

The panel keeps the last 200 decisions in memory. With [metrics](metrics.md) enabled, per-host gauges and decision counters are exported.

## Compose

The manager reaches the `local` host through the mounted Docker socket. Pass the variables through an [overlay](operations.md#add-manager-settings).

```yaml
services:
  manager:
    environment:
      RUNQUILL_ELASTIC: ${RUNQUILL_ELASTIC:-off}
      RUNQUILL_ELASTIC_INTERVAL: ${RUNQUILL_ELASTIC_INTERVAL:-}
      RUNQUILL_ELASTIC_COOLDOWN: ${RUNQUILL_ELASTIC_COOLDOWN:-}
      RUNQUILL_ELASTIC_HOST_RESERVE_CPUS: ${RUNQUILL_ELASTIC_HOST_RESERVE_CPUS:-}
      RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB: ${RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB:-}
      RUNQUILL_ELASTIC_MEMORY_TRIGGER: ${RUNQUILL_ELASTIC_MEMORY_TRIGGER:-}
      RUNQUILL_ELASTIC_MAX_MULTIPLE: ${RUNQUILL_ELASTIC_MAX_MULTIPLE:-}
      RUNQUILL_JOB_RESOURCES: ${RUNQUILL_JOB_RESOURCES:-}
```

## Job resources

The same sampling pass records each job's CPU and memory, so you can see which pools are too small or too large. It runs by default, even with `RUNQUILL_ELASTIC=off`, and covers every runner on the `local` Docker host, including pools without `cpus` or `memory_mib`. Set `RUNQUILL_JOB_RESOURCES=off` to stop it; any other value than `on` or `off` stops the manager at startup. The pass always runs the `docker` CLI, so with `RUNQUILL_CONTAINER_ENGINE=podman` it stays off unless you set `RUNQUILL_JOB_RESOURCES=on` and the `docker` command reaches the Podman socket. `off` stops only the sampling: exited runners are still checked for memory kills.

| Recorded | Where |
| --- | --- |
| Sample count, peak and average CPU in cores, peak memory in MiB | `local` Docker host only |
| CPU and memory limits | The pool's guarantee as first sampled, so an elastic raise does not hide a pool that is too small, and the highest effective limit sampled. `0` is unlimited. |
| Killed for memory | `local` and `ssh://` Docker hosts from `State.OOMKilled` of the exited runner, and Kubernetes from the runner container's `OOMKilled` reason. Not on outbound `agent://` workers. |

Fields a host does not record are left out of the output rather than shown as zero.

With elastic limits off, each pass runs `docker ps`, `docker inspect` and, while runners run, one `docker stats --no-stream` for all of them. It skips `docker info` and never changes a container. Each `docker stats` call keeps the daemon busy for a second or two, so raise `RUNQUILL_ELASTIC_INTERVAL` on a busy host if that matters.

The average CPU covers the runner's whole life, including the idle time before it picks up the job. Peaks are the highest of these snapshots, not a cgroup high-water mark: a spike between two samples, or a fast out-of-memory kill, can show a lower peak than the job reached. The out-of-memory flag is the engine's own report. Docker 29 sets it when any process in the runner was killed for memory, even if the runner itself then exited normally; other engines and versions have not been checked.

Read one job with `runquill jobs show JOB_ID`, `GET /api/jobs/{id}` or the MCP `get_job` tool. Compare recent jobs with their limits with `runquill usage --rightsizing`, `GET /api/usage/rightsizing` or the MCP `job_rightsizing` tool. They group finished jobs from the last seven days by pool, or with `--group job` by repository and runner label, and give each resource a verdict:

| Verdict | Meaning |
| --- | --- |
| `tight` | A peak passed 80% of its limit, or, for memory, a runner was killed for memory. |
| `oversized` | At least three jobs were sampled and every peak stayed below 25% of its limit. |
| `ok` | Neither. |
| `too_few_jobs` | Fewer than three sampled jobs with a limit, and nothing tight. |
| `unlimited` | No sampled job had a limit. |

Records are kept as long as their jobs, seven days, and are not included in queue backups.

The planning rules are covered by unit tests and the Docker calls by tests with a fake Docker command. Elastic limits have not yet been exercised on a real Docker host under load.
