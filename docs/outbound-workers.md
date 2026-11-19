# Outbound Docker workers

A worker polls the Runquill manager over HTTPS and runs jobs on its own Docker engine. It needs outbound access to the manager and GitHub. Each worker requires Linux amd64 or arm64, rootful Docker with cgroup v2, a locally preloaded Runquill runner image, and the Runquill executable. Use a dedicated engine for trusted workflows, as with the Docker backend.

## Register a worker

On the worker, create a private token file and obtain the Docker engine ID and token digest. Send the engine ID and digest to the manager administrator.

```sh
(umask 077 && openssl rand -hex 32 >"$PWD/worker-a.token")
docker info --format '{{.ID}}'
tr -d '\r\n' <"$PWD/worker-a.token" | sha256sum
```

On the manager, create `workers.json` using the exact values. The raw token stays on the worker.

```json
{
  "worker-a": {
    "engine_id": "WORKER_ENGINE_ID",
    "token_sha256": "WORKER_TOKEN_SHA256"
  }
}
```

List the worker in the manager's configuration. Keep `local` or an existing SSH target in the list if you want to continue using it. Each target needs its own engine. The per-host limit bounds the capacity reserved when a worker is offline.

```dotenv
RUNQUILL_DOCKER_HOSTS=agent://worker-a
RUNQUILL_DOCKER_HOST_LIMITS=agent://worker-a=1
RUNQUILL_WORKER_AUTH_FILE=./workers.json
```

For Compose, enable the outbound-worker overlay.

```sh
docker compose -f compose.yaml -f compose.worker.yaml up -d
```

## Connect

Use a trusted HTTPS reverse proxy for `/worker/v1/*` to the manager's HTTP listener. Keep the manager's HTTP port bound to loopback or accessible only to that proxy. The proxy must overwrite `X-Forwarded-Proto` with the actual external scheme. Runquill accepts that header from a loopback peer or a single explicitly configured `RUNQUILL_WORKER_PROXY_IP`. Set this to the proxy's source IP as seen by the manager, which can be the Docker bridge gateway when proxying to a Compose port on the host. Restrict access to the backend listener to prevent other clients from impersonating that peer. Keep authorization headers and response bodies out of proxy logs.

On the worker, start Runquill with the worker name, HTTPS manager origin, and private token file. Use the same image configured for this host in the manager or its pool. Load or pull that image through your usual trusted image supply first. The manager pulls runner images only on the `local` host and `ssh://` hosts, never on an outbound worker. A worker starts only this image, so it never takes a job whose `os__NAME__` label selects a different [catalogue image](runner-images.md), and the manager's [cleanup of unused images and cache volumes](runner-images.md#cleanup-of-unused-images-and-cache-volumes) skips it.

```sh
RUNQUILL_RUNNER_IMAGE=runquill-runner:local \
  runquill worker --name worker-a --manager https://runquill.example.com \
  --token-file "$PWD/worker-a.token"
```

The worker validates the server certificate. Literal loopback HTTP is available for local testing. Keep the worker process running with your service manager. It locks a file beside its token file to prevent two processes from using the same credentials concurrently. Use a unique token and name per worker.

Run the same Runquill release on the worker as on the manager. A worker in a pool with `privileged: false` or `cache: true` must be upgraded with the manager, because older workers reject runner tasks that carry those settings. The cache volume lives on the worker's own engine.

Use `runquill status` or the panel to view the worker. Existing reserve, slot, pool, and cordon behavior applies. After a disconnect, Runquill waits for fresh inventory before acting on that worker's previous runner leases. A restarted worker can take over its previous session after up to 45 seconds. After a clean job, the manager removes the worker-owned container by its verified Docker ID.

## Rotation and replacement

Cordon `agent://worker-a` and wait for its jobs to drain before rotating credentials or replacing the engine. Stop the worker, generate a fresh token, update the manager's digest and restart the manager, then restart and uncordon the worker. Keep the same name and engine ID for token rotation. For an engine replacement, update the engine ID after draining. If a worker is permanently decommissioned with unresolved leases, those leases remain reserved until the manager can reconcile that worker. Review `runquill gc --dry-run` before deleting any unfamiliar runner locally. `runquill gc --remove-orphans` skips outbound workers.
