# Prometheus metrics

[Runquill README](../README.md) · [한국어](metrics.ko.md)

Runquill can expose aggregate fleet metrics on a dedicated HTTP listener. Set `RUNQUILL_METRICS_ADDR` to enable it. A local listener works without credentials. Use a literal IP and port.

```sh
RUNQUILL_METRICS_ADDR=127.0.0.1:9090 ./runquill
curl --fail http://127.0.0.1:9090/metrics
```

Use the manager's usual GitHub App and runtime environment when starting it. The metrics listener is independent of the panel and webhook.

A non-loopback bind requires `RUNQUILL_METRICS_TOKEN_FILE`. Put a dedicated random token in that file and send it as a bearer token when scraping. Restrict the port to your collector and terminate TLS on a trusted reverse proxy if requests cross a network. Restart the manager after rotating the token.

For Compose, create the token file beside `compose.yaml`.

```sh
(umask 077; set -C; openssl rand -hex 32 > metrics-token)
```

Save this overlay as `compose.metrics.yaml`. Docker publishes the port on the host's loopback interface.

```yaml
services:
  manager:
    environment:
      RUNQUILL_METRICS_ADDR: 0.0.0.0:9090
      RUNQUILL_METRICS_TOKEN_FILE: /run/secrets/metrics-token
    secrets:
      - metrics-token
    ports:
      - "127.0.0.1:9090:9090"

secrets:
  metrics-token:
    file: ./metrics-token
```

Apply it with `docker compose -f compose.yaml -f compose.metrics.yaml up -d`, keeping any SSH or panel overlays you already use. For a published-image installation, add `--no-build`.

Configure a Prometheus scraper running on the host. The file path must be readable by Prometheus.

```yaml
scrape_configs:
  - job_name: runquill
    static_configs:
      - targets: ["127.0.0.1:9090"]
    authorization:
      credentials_file: /path/to/runquill/metrics-token
```

For K3s, add `RUNQUILL_METRICS_ADDR=127.0.0.1:9090` to the manager configuration and restart it. Use a collector in the same Pod or `kubectl -n runquill-system port-forward deployment/runquill 9090:9090` for local inspection. To scrape a non-loopback address, mount the token as a Kubernetes Secret and restrict network access to the collector.

Metric values describe the fleet as a whole. Samples carry no repository, job or runner identifiers, and only the elastic gauges below carry a host label.

| Gauge | Meaning |
| --- | --- |
| `runquill_queue_ready_jobs`, `runquill_queue_retry_jobs` | Queued jobs without a runner, split by whether their retry delay has elapsed. |
| `runquill_queue_oldest_job_age_seconds` | Age of the oldest queued job without a runner. |
| `runquill_runner_slot_limit`, `runquill_runner_occupied_slots`, `runquill_runner_reserved_slots` | Fleet limit, slots observed occupied, and slots held for offline hosts or unassigned leases. |
| `runquill_runners_running`, `runquill_hosts_offline` | Runner runtimes observed running and hosts whose inventory could not be read. |
| `runquill_fleet_snapshot_timestamp_seconds` | Timestamp of the last successful sample. Gauges are absent until that sample is ready. |

Counters `runquill_runner_starts_total`, `runquill_runner_launch_errors_total`, `runquill_runner_lease_releases_total`, and `runquill_scheduler_tick_errors_total` reset on manager restart. Lease releases include cleanup before a retry and can include jobs that did not finish successfully.

With [notifications](operations.md#send-notifications) configured, `runquill_notifications_sent_total`, `runquill_notifications_failed_total` and `runquill_notifications_dropped_total` count events delivered to the webhook, events not delivered after all attempts, and events discarded because the in-memory queue was full or the manager was stopping. These counters are absent without `RUNQUILL_NOTIFY_URL` and also reset on manager restart.

The [elastic limits](elastic-limits.md) loop adds `runquill_elastic_changes_total{applied="true"|"false"}` for applied and observed-only decisions and `runquill_elastic_update_errors_total` for changes Docker rejected. With `RUNQUILL_ELASTIC` set to `observe` or `apply`, the gauges `runquill_elastic_surplus_cpus`, `runquill_elastic_lent_cpus`, `runquill_elastic_surplus_memory_bytes`, `runquill_elastic_extra_memory_bytes` and `runquill_elastic_held_slots` report each managed local host, labelled `host`, when its pool sets `cpus` or `memory_mib`. The counters reset on manager restart.

For example, this query finds managers with ready jobs and all configured slots occupied or reserved. Use `time() - runquill_fleet_snapshot_timestamp_seconds` to detect stale values.

```promql
runquill_queue_ready_jobs > 0
and (runquill_runner_occupied_slots + runquill_runner_reserved_slots >= runquill_runner_slot_limit)
```

[Prometheus scrape configuration](https://prometheus.io/docs/prometheus/latest/configuration/configuration/)
