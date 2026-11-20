# Elastic runner limits

A [worker pool](workers.md#route-jobs-to-worker-pools) with `cpus` or `memory_mib` gives each runner fixed Docker limits. Elastic limits let a busy runner borrow host capacity that no other runner or queued job has been promised, and give it back when it goes idle.

Elastic limits work only on the manager's `local` Docker host, for runners whose pool sets `cpus`, `memory_mib` or both. `ssh://` hosts, outbound `agent://` workers and Kubernetes are left alone, and the manager logs which hosts it skips.

## Modes

| `RUNQUILL_ELASTIC` | Behaviour |
| --- | --- |
| `off` (default) | No sampling. |
| `observe` | Samples runners and records the decisions it would make, without changing any container. |
| `apply` | Records the decisions and applies them with `docker update`. |

Start with `observe` and review the decisions on the panel's **Hosts & pools** screen, in `GET /api/elastic`, or through the MCP `elastic_decisions` tool.

## Settings

| Variable | Default | Range | Effect |
| --- | --- | --- | --- |
| `RUNQUILL_ELASTIC_INTERVAL` | `10s` | `1s`–`1h` | How often runners are sampled. |
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
```

The planning rules are covered by unit tests and the Docker calls by tests with a fake Docker command. Elastic limits have not yet been exercised on a real Docker host under load.
