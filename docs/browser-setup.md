# Browser setup

On your Linux host, run the installer and open the printed setup link.

```sh
sh install.sh --wizard --dir ./runquill
```

1. Choose the repository access, then select **Connect GitHub**. Approve and install the GitHub App using the access shown by the wizard. For a personal account, the wizard uses the installation owner as the first panel administrator.
2. Choose **Deploy and check installation**. After the manager starts, open the panel or download the first workflow.

The default setup uses local Docker Compose, a personal GitHub account, and the host's recommended runner limits. Open **Advanced** for a GitHub organization, a K3s context, custom addresses, or different limits. For K3s, use the matching release's `runquill-k3s.yaml` and a context authorized to create the resources in it. Use a port-forward for a local panel or supply an existing HTTPS ingress.

The K3s wizard reads the `kube-system` namespace UID to identify the cluster. To resume an interrupted deployment, reopen the wizard from the same installation directory and select the context for the same cluster and GitHub App. The wizard verifies its setup ID on the namespaces before continuing. If an older wizard created the namespaces without a setup ID, complete the deployment using the manual K3s instructions. [K3s installation](../deploy/k3s/README.md#install)

Run `./runquill wizard` from the installation directory if you already have the release files. For an existing Compose installation, `./runquill wizard --check-compose` runs its read-only preflight. Over SSH, use the printed forwarding command and keep the setup link private.

## Tailscale

For the Compose wizard, host preparation can install Tailscale on supported Linux distributions, start the daemon, and configure the local operator. If prompted, open Tailscale's sign-in link to join your tailnet. MagicDNS and HTTPS must be enabled in the tailnet. Funnel also needs the node's `funnel` permission.

The wizard uses these routes when available. Polling stays enabled every 30 seconds and also works without Tailscale.

| Option | Access | Listener | Target |
| --- | --- | --- | --- |
| Serve panel | Authorized tailnet peers | HTTPS 9443 | `127.0.0.1:8081` |
| Funnel webhook | GitHub and the internet | HTTPS 8443, `/webhook` | `127.0.0.1:8080/webhook` |

Use `install.sh --wizard --no-tailscale` to skip automatic configuration, or use **Advanced** for custom panel and webhook addresses. The wizard keeps existing listeners and uses GitHub webhook signatures and panel OAuth. [Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve) · [Tailscale Funnel](https://tailscale.com/docs/features/tailscale-funnel)
