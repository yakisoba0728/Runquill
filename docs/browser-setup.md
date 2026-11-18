# Browser setup

On a Linux host with the release files, start the browser wizard.

```sh
sh install.sh --wizard --dir ./runquill
```

With downloaded release files, run `./runquill wizard` in the installation directory. A source-built binary can run from the source checkout using its local Docker images. For a remote host, forward the printed port over SSH. Keep the setup URL private. The wizard listens on loopback for one hour.

1. Select Docker Compose or a K3s context, personal or organization ownership, and repository access.
2. Approve registration and installation on GitHub. The wizard saves the generated key and environment locally and detects the installation.
3. Register a panel OAuth App using the homepage and callback shown in the wizard. Enter its credentials and the first administrator's GitHub user ID.
4. Review the target and deploy. For K3s, place the matching release’s `runquill-k3s.yaml` in the installation directory and select a context authorized to create the bundled namespaces, RBAC, deployment and Secrets. The wizard checks for existing Runquill namespaces before a fresh deployment.
5. Download the sample workflow, add it on GitHub and choose **Run workflow**. Follow discovery and runner cleanup in the operations panel.

English and Korean are available in both screens. Administrator browser sessions can check repository access and download a queue-only backup. Restore uses an offline manager and the [recovery procedure](../README.md#back-up-and-recover-the-queue).

## Tailscale

Sign into the Compose host's Tailscale client and enable MagicDNS and HTTPS. Give the host account running Runquill permission to configure the local Tailscale client.

| Option | Access | Listener | Target |
| --- | --- | --- | --- |
| Serve panel | Authorized tailnet peers | HTTPS 9443 | `127.0.0.1:8081` |
| Funnel webhook | GitHub and the internet | HTTPS 8443, `/webhook` | `127.0.0.1:8080/webhook` |

Funnel also requires tailnet policy approval. The wizard fills in the GitHub URLs, activates the routes during deployment, and can pause or resume them from the completion page. A conflicting listener stops configuration. GitHub webhook signatures and panel OAuth still apply.

For K3s, use a port-forward for a loopback panel or supply an existing HTTPS ingress. Managed Tailscale routes use the Compose host's published ports. [Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve) · [Tailscale Funnel](https://tailscale.com/docs/features/tailscale-funnel)
