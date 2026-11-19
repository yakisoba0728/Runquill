# Runquill on K3s

Run one manager Deployment and let Kubernetes schedule a fresh Pod for each GitHub Actions job. SQLite lives on a persistent volume. The manager uses the same GitHub App, webhooks, polling, and `runs-on` labels as a Docker deployment. Kubernetes mode presents a single runner pool called `kubernetes` in status and in the panel.

## Install

Use a K3s cluster with a default StorageClass and nodes that can pull the release images and reach GitHub. Keep these namespaces dedicated to Runquill. The manifest places the manager and runner Pods in separate namespaces enforced by restricted Pod Security. A job receives no Kubernetes API credentials.

1. Download the release with `sh install.sh --download-only --dir runquill`. From that directory, run `./runquill setup --k3s` and install the GitHub App. The default is **All repositories**. For [selected repositories](../../README.md#connect-selected-repositories), use `./runquill setup --k3s --repository-scope selected`.
2. To enable the panel, use `./runquill setup --k3s --panel` with a release that supports guided panel setup. For v0.2.0 and manual installs, add the [panel settings](../../README.md#web-panel-and-team-access) to `.env` and set `RUNQUILL_PANEL_ADDR=0.0.0.0:8081` in that file.
3. Apply the manager manifest and create the configuration Secrets from the installation files.

```sh
kubectl apply -f runquill-k3s.yaml
kubectl -n runquill-system create secret generic runquill-config --from-env-file=.env
kubectl -n runquill-system create secret generic runquill-github-key --from-file=github-app.pem
kubectl -n runquill-system rollout status deployment/runquill
kubectl -n runquill-system exec deployment/runquill -- runquill doctor --repo OWNER/REPO
```

From a Git checkout, use `kubectl apply -f deploy/k3s/runquill.yaml` and match both image references to your version. For configuration updates, update the Secrets and restart the Deployment to read the new values.

Run the [first job](../../README.md#verify-your-first-job). Watch it with these commands.

```sh
kubectl -n runquill-runners get pods --watch
kubectl -n runquill-system exec deployment/runquill -- runquill status --json
kubectl -n runquill-system logs -f deployment/runquill
kubectl -n runquill-system port-forward svc/runquill 8080:8080 8081:8081
```

For polling, the manager only needs outbound GitHub access. To receive webhooks, route public HTTPS `/webhook` to the Service's port 8080. For a shared team panel, configure a separate HTTPS origin on port 8081 and preserve the public Host header through the proxy. For a loopback panel, register `http://127.0.0.1:8081/auth/github/callback` and use the port-forward above. Use your cluster's secret encryption and restrict access to the manager namespace, volumes, and GitHub App key.

Pending runner Pods can show scheduling, image pull, volume mount and container startup causes in the panel and `runquill status --json`. Apply the current manifest to let Runquill list warning Events in its runner namespace for mount diagnostics. Full Kubernetes details are available through `kubectl describe pod -n runquill-runners POD_NAME`.

## Runner settings

| Setting | Default | Effect |
| --- | --- | --- |
| `RUNQUILL_MAX_RUNNERS` | `2` | Global limit across cluster nodes. Pending and terminating Pods consume slots. |
| `RUNQUILL_K8S_IMAGE_PULL_SECRET` | unset | Optional registry Secret in `runquill-runners` for a custom runner image. |
| `RUNQUILL_K8S_NODE_SELECTOR` | unset | Optional JSON map of node label names and values. |
| `RUNQUILL_K8S_JOB_CPU` | unset | Optional CPU request and limit, for example `2`. |
| `RUNQUILL_K8S_JOB_MEMORY` | unset | Optional memory request and limit, for example `4Gi`. |
| `RUNQUILL_K8S_RUNNER_MODE` | `plain` | `plain` executes steps directly under UID 1001. `dind` adds the privileged Docker daemon for Docker builds and container-based jobs, actions, and services. |

For DinD, use a dedicated cluster with trusted workflows. Label the approved worker nodes, set `RUNQUILL_K8S_NODE_SELECTOR` to select them, and allow privileged Pods in the runner namespace only. Keep the manager namespace restricted. Leave `RUNQUILL_DOCKER_HOSTS`, `RUNQUILL_DOCKER_HOST_LIMITS`, and `RUNQUILL_WORKER_POOLS` empty for this backend. `RUNQUILL_MAX_PER_REPOSITORY`, `RUNQUILL_SCHEDULER_WORKERS`, the `RUNQUILL_LOG_*` and `RUNQUILL_NOTIFY_*` settings apply here as well. Add them to `.env`, recreate `runquill-config`, and restart the Deployment. Use `runquill host cordon kubernetes` to pause new assignments without interrupting active jobs.

The default K3s local-path volume persists manager restarts on its storage node. Use cluster-appropriate persistent storage and backups for recovery from node loss. Keep the manager at one replica. Drain existing jobs before switching the runner backend.

The bundled manifest probes `/healthz`, which checks only the queue database. `GET /readyz` on port 8080 also reports a stalled scheduler or failing GitHub App authentication if you want to probe or alert on it. `runquill gc --remove-orphans` supports Docker hosts only and skips this backend.

## Upgrade and uninstall

With a release that supports it, run `sh install.sh --upgrade --dir runquill` to replace the release files, including `runquill-k3s.yaml`, then apply the new manifest. The Secrets and the queue PVC are kept.

```sh
kubectl apply -f runquill-k3s.yaml
kubectl -n runquill-system rollout status deployment/runquill
```

To uninstall, delete the manifest's objects. This removes both namespaces with the manager, runner Pods, Secrets and the queue PVC. Back up the queue first if you might need it, then delete the GitHub App as described in the [README](../../README.md#uninstall).

```sh
kubectl delete -f runquill-k3s.yaml
```
