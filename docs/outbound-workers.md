# Outbound Docker workers

[Runquill README](../README.md) · [한국어](outbound-workers.ko.md)

A worker polls the Runquill manager over HTTPS and runs jobs on its own Docker engine. It needs outbound access to the manager and GitHub. Each worker requires Linux amd64 or arm64, rootful Docker with cgroup v2, a locally preloaded Runquill runner image, and the Runquill executable. Use a dedicated engine for trusted workflows, as with the Docker backend.

## Register from the panel

Preconfigure a slot for the worker and set the manager origin that serves `/worker/v1/*` over HTTPS. This can differ from the panel origin. Keep the existing `local` and `ssh://` targets if you use them.

```dotenv
RUNQUILL_DOCKER_HOSTS=local,agent://worker-a
RUNQUILL_DOCKER_HOST_LIMITS=agent://worker-a=1
RUNQUILL_WORKER_ENROLLMENT_URL=https://workers.example.com
```

For Compose, apply `compose.yaml` and `compose.panel.yaml`, plus any overlays used by your current installation. The worker auth overlay is needed for `workers.json` registrations. Configure the trusted proxy as described under [Connect](#connect).

As a panel administrator, open **Hosts & pools**, choose **Enroll worker** on the slot, and run the displayed command on the worker. The command registers its Docker engine, creates a private token file in the current directory, and starts the worker using the image configured for that host. Preload that image before starting. The code expires after ten minutes, works once, and is replaced if you issue a new one. Share it only with the operator of the selected machine.

Keep the token file and use the displayed restart command from the same directory in your service manager. If the enrollment request loses its reply, the restart command can still connect with the saved token if the manager accepted the code. A configured slot awaiting its first credentials stays unavailable without reserving unused capacity.

Worker credentials are checked against their Docker engine. The manager stores token hashes with the queue, and queue backups include those bindings. Pending enrollment codes are excluded from backups. Keep worker token files separately. Removing `RUNQUILL_WORKER_ENROLLMENT_URL` disables code issuance while registered workers keep using their existing credentials.

## Register manually

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

`--name`, `--manager` and `--token-file` default to `RUNQUILL_WORKER_NAME`, `RUNQUILL_WORKER_URL` and `RUNQUILL_WORKER_TOKEN_FILE`. The worker validates the server certificate. Literal loopback HTTP is available for local testing. Keep the worker process running with your service manager. It locks a file beside its token file to prevent two processes from using the same credentials concurrently. Use a unique token and name per worker.

Run the same Runquill release on the worker as on the manager. A worker in a pool with `privileged: false` or `cache: true` must be upgraded with the manager, because older workers reject runner tasks that carry those settings. The cache volume lives on the worker's own engine.

Use `runquill status` or the panel to view the worker. Existing reserve, slot, pool, and cordon behavior applies. After a disconnect, Runquill waits for fresh inventory before acting on that worker's previous runner leases. A restarted worker can take over its previous session after up to 45 seconds. After a clean job, the manager removes the worker-owned container by its verified Docker ID.

The Docker integration test exercises panel code issuance, CLI enrollment, signed fixture webhooks, job execution in a real container, and removal. It uses a local GitHub API fixture and runs on both Linux architectures in CI.

## Rotation and replacement

Cordon `agent://worker-a` and wait for its jobs to drain before rotating credentials or replacing the engine. Stop the worker, generate a fresh token, update the manager's digest in `workers.json` and restart the manager, then restart and uncordon the worker. A manual credential for a panel-enrolled host replaces its saved binding on manager restart. Keep the same name and engine ID for token rotation. For an engine replacement, update the engine ID after draining. If a worker is permanently decommissioned with unresolved leases, those leases remain reserved until the manager can reconcile that worker. Review `runquill gc --dry-run` before deleting any unfamiliar runner locally. `runquill gc --remove-orphans` skips outbound workers.
