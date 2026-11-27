# Install Runquill

[Runquill README](../README.md) · [한국어](install.ko.md)

## Install a public release

Use an amd64 or arm64 Linux server with cgroup v2. Download the installer and run it on that server. It prepares the host, downloads the release and opens the setup wizard:

```sh
cd ~ && curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh && sh install.sh --wizard
```

Run one manager per group of Docker hosts. Outbound HTTPS to GitHub is enough for polling, even behind NAT.

The command installs in `$HOME/runquill`. For an existing deployment, follow [Upgrade](operations.md#upgrade). Add `--no-tailscale` to skip Tailscale setup. Use the [Cloudflare Tunnel guide](cloudflare-tunnel.md) to publish the panel and webhook through your own HTTPS hostname.

| Access method | Setup |
| --- | --- |
| Polling with a local or SSH-forwarded panel | Run `--wizard --no-tailscale` and leave the public webhook URL empty. |
| Tailscale Serve and Funnel | Use the default wizard and check your tailnet's HTTPS and Funnel permissions. |
| Public HTTPS on your own domain | Configure [Cloudflare Tunnel](cloudflare-tunnel.md) or an [HTTPS proxy](#use-an-existing-github-app-or-configure-webhooks), then enter the URLs in the wizard. |

### Prepare the host

The installer checks Linux, cgroup v2, Docker Compose, and Docker access. When the host needs changes, it shows the commands and asks once. It can install missing Docker packages on Ubuntu, Debian, Fedora, RHEL and derivatives, or Alpine. On a system without systemd or OpenRC, start `dockerd` yourself. Use `--prepare-only` to check connectivity to GitHub and GHCR in advance.

The installer verifies Docker's signing key before adding its repository. If it adds your user to the `docker` group, which grants root-equivalent access, the current installation resumes with the new group active.

| Flag | Effect |
| --- | --- |
| `--yes`, `-y` | Make the listed changes without asking, for unattended installs |
| `--no-install` | Never change the system. Fail and list what is missing |
| `--prepare-only` | Prepare the host and stop |
| `--no-docker` | Skip the Docker checks, for example on a K3s host |
| `--no-tailscale` | Run the browser setup without automatic Tailscale configuration |
| `--download-only` | Skip the host checks. Download and verify the release only |

### Prepare the host manually

To prepare the host yourself, install Docker Engine and the Compose plugin with [Docker's guide](https://docs.docker.com/engine/install/). Then start Docker and give your user access:

```sh
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"
```

Log out and back in. Then `sh install.sh --prepare-only --no-install` checks the host without changing it.

### Set up in a browser

Run `sh install.sh --dir "$HOME/runquill" --wizard`. Accept host preparation if prompted, open the printed setup link, connect GitHub, and deploy. The wizard fills in runner limits for the host. Use **Advanced** for organizations, selected repositories, K3s, or custom addresses.

The installer can prepare Tailscale and start its browser sign-in. On Docker Compose, the wizard uses Serve for private panel access and Funnel for GitHub webhooks when the tailnet allows them. Polling remains available. [Browser setup](browser-setup.md)

The wizard listens on `127.0.0.1` and prints a forwarding command when you connect over SSH:

```text
ssh -L 18080:127.0.0.1:18080 alice@203.0.113.10
```

Keep that session open and open the printed `http://127.0.0.1:18080/?key=…` URL in your local browser. Use the same port number on both sides. The `key` is a secret for this setup session. If you reach the server under another name or through NAT, use that address in the `ssh` command.

When the wizard reports **Deployment complete** and the live panel opens, press `Ctrl+C` in the terminal running the wizard. The manager container keeps running in the background. If you use SSH forwarding for the panel, keep the panel forwarding session open while using it.

### Set up from the terminal

To set up from the terminal instead of the browser, leave out `--wizard`:

```sh
curl -fsSLO https://github.com/yakisoba0728/Runquill/releases/latest/download/install.sh
sh install.sh --dir "$HOME/runquill"
```

1. Open the browser link from the installer. Create the private GitHub App and install it on your personal account with **All repositories** access.
2. Wait for the installer to start Compose and report `Preflight passed.`
3. [Run your first job](workflows.md#verify-your-first-job) from a private repository you own.

The installer verifies release checksums and uses images from the same release. Use `--version v0.4.0` to pin a published release, or `--download-only` to fetch the binary and deployment files before setup.

### Create and install the GitHub App

Run `setup` from the directory containing `compose.yaml`. To use a source checkout, build the setup binary with Go 1.26.11 or later and use the latest patch for your Go version.

```sh
CGO_ENABLED=0 go build -o runquill .
./runquill setup
```

For a published release, download `compose.yaml` from its tag and the matching Linux executable and `SHA256SUMS` from the release assets into one directory. Run these commands there on amd64 hardware. On arm64, use `runquill-linux-arm64`.

```sh
sha256sum --check --ignore-missing SHA256SUMS
chmod +x ./runquill-linux-amd64
./runquill-linux-amd64 setup
```

1. Open the browser link printed by `setup`. If your browser is on another machine, use the SSH port-forwarding instructions printed by the command.
2. In GitHub, approve creation of the private App. `setup` saves `.env` and `github-app.pem` with permissions `0600` and prints an installation link.
3. Follow the link and install the App on your personal account with **All repositories** access so Runquill can discover new private repositories.

Rerun `setup` to resume App installation once the files have been saved. The generated configuration uses GitHub API polling and works without a public HTTPS address.

[GitHub App manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest) · [Installing a GitHub App you created](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app)

### Connect selected repositories

For a new installation, use the same setup with the selected repository scope.

```sh
sh install.sh --dir "$HOME/runquill" --repository-scope selected
# With a standalone binary in the installation directory:
./runquill setup --repository-scope selected
```

On GitHub, choose **Only select repositories** and select the private repositories you want to use. GitHub stores that list and the setup saves `RUNQUILL_REPOSITORY_SCOPE=selected` in `.env`. Add or remove repositories through the GitHub App installation settings. With polling enabled, the next sweep can discover jobs in newly approved repositories without restarting the manager. With webhook-only configuration, jobs are discovered through deliveries from GitHub. Run `runquill doctor --repo OWNER/REPO` in the manager environment to check coverage before your first run. [Change GitHub App repository access](https://docs.github.com/en/apps/using-github-apps/reviewing-and-modifying-installed-github-apps)

To switch an existing installation between **All repositories** and **Only select repositories**, change its GitHub settings and set `RUNQUILL_REPOSITORY_SCOPE` to the corresponding `all` or `selected` in the manager configuration. Recreate the Compose manager or update the K3s Secret and restart the Deployment. A configuration without this setting defaults to `all`.

### Connect a GitHub organization

Run setup as an organization owner and install the generated App on that organization. Each manager handles one GitHub account. Use a fresh directory and queue when switching to a different account or runner registration level.

```sh
./runquill setup --organization YOUR_ORG --repository-scope selected
```

Pass the same options to the public installer for a Compose deployment. For K3s, add `--k3s` to `setup` as usual. By default, Runquill registers each runner on the repository that requested it, with repository **Actions → Read-only** and **Administration → Read and write** permissions. To use an existing App, set `RUNQUILL_OWNER=YOUR_ORG` and `RUNQUILL_OWNER_TYPE=organization` in `.env`. For panel setup in an organization, enter the GitHub user ID of the person who will administer the panel. [GitHub App manifest registration](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)

To register runners in an organization runner group, create an organization-owned group with **Selected repositories** access and public repository access disabled. Choose a group without GitHub's workflow-level restrictions. Add only private repositories covered by the App and pass `--runner-group-id ID` to setup or the installer. With [`RUNQUILL_ALLOW_PUBLIC_REPOSITORIES=true`](workflows.md#run-jobs-from-public-repositories), use a separate group that allows public repositories and contains only them. For an existing App, set `RUNQUILL_RUNNER_GROUP_ID=ID` in `.env` and grant **Self-hosted runners → Read and write** under organization permissions. Runquill checks group policy and repository access before registering each runner. Use the configured group name if your workflow specifies a runner group, and keep the job-specific label. GitHub routes organization runners by group access and matching labels, so share a group only among repositories that trust each other's workflows. [GitHub runner groups](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access) · [Organization runner API](https://docs.github.com/en/rest/actions/self-hosted-runners#create-configuration-for-a-just-in-time-runner-for-an-organization)

`runquill doctor --repo YOUR_ORG/REPO` checks App coverage and group membership. Use `runquill diagnose YOUR_ORG/REPO` for queued jobs.

### Start the manager

From a source checkout, build the runner image and start the manager.

```sh
docker build --pull -t runquill-runner:local runner
docker compose up -d --build
```

With a release binary, `setup` writes both image names pinned to its release tag. The manager pulls a missing runner image onto the `local` host and `ssh://` hosts itself, so the runner image pull command printed by `setup` is optional and only gets the image ready before the first job. Pull and start the manager.

```sh
docker compose pull manager
docker compose up -d --no-build
```

### Use an existing GitHub App or configure webhooks

For a manually configured App, select **Only on this account**, grant **Actions → Read-only** and **Administration → Read and write**, and create a private key. Install the App on your personal account. Copy [.env.example](../.env.example) to `.env`, fill in `RUNQUILL_OWNER`, `RUNQUILL_APP_ID` and the PEM path in `RUNQUILL_KEY_FILE`, and set both files to mode `0600`. Set `RUNQUILL_REPOSITORY_SCOPE=selected` if you chose **Only select repositories**. For polling, disable webhooks in the App settings and leave `RUNQUILL_WEBHOOK_SECRET` empty. [GitHub App installation](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app) · [Repository JIT runner API](https://docs.github.com/en/rest/actions/self-hosted-runners#create-configuration-for-a-just-in-time-runner-for-a-repository)

To use published images with a manual `.env`, set the manager and runner image names from the example to the same release tag. The manager pulls the runner image when a host lacks it.

For an existing App, subscribe to **Workflow job**, set its public HTTPS webhook URL, and set the same secret in GitHub and `RUNQUILL_WEBHOOK_SECRET`. Generate the secret with `openssl rand -hex 32`. Forward `/webhook` to the manager on port 8080. With Caddy on the same server, use the following configuration.

```caddyfile
runquill.example.com {
    reverse_proxy /webhook 127.0.0.1:8080
}
```

### Connect an HTTPS webhook

For a fresh App, use `sh install.sh --webhook-url https://runquill.example.com/webhook` or pass the same option to `runquill setup`. The App subscribes to `workflow_job`, and setup stores GitHub's webhook secret and sets `RUNQUILL_POLL_INTERVAL=30s` to recover missed deliveries. Forward `/webhook` to the local manager through your HTTPS proxy as shown above. For webhook-only operation, use [the polling settings](operations.md#verify-webhook-only-delivery).

### Use Tailscale Funnel for the webhook

Funnel gives GitHub a public HTTPS address for a manager behind NAT. Run it on the host that publishes the Compose manager at `127.0.0.1:8080`. Enable MagicDNS and HTTPS for your tailnet and allow this node to use Funnel in its policy. The example uses HTTPS port 443. [Tailscale Funnel requirements](https://tailscale.com/docs/features/tailscale-funnel)

```sh
tailscale funnel status
tailscale funnel --bg --https=443 http://127.0.0.1:8080
```

Append `/webhook` to the HTTPS address printed by Funnel. Pass that URL to the installer you downloaded above or to `./runquill setup`. For an existing App, update its webhook URL in GitHub and use its configured webhook secret.

```sh
sh install.sh --webhook-url https://runquill.example-tailnet.ts.net/webhook
```

After Compose starts, request `https://runquill.example-tailnet.ts.net/healthz` from outside the tailnet and expect HTTP 200. Run the smoke workflow and check for HTTP 202 on its queued event in the App's **Advanced → Recent deliveries**. If a delivery times out, use **Redeliver** there. On an existing installation with polling disabled, set `RUNQUILL_POLL_INTERVAL=30s` in `.env` and apply it with your usual Compose command to recover missed jobs automatically. [GitHub App webhook redelivery](https://docs.github.com/en/webhooks/testing-and-troubleshooting-webhooks/redelivering-webhooks#redelivering-github-app-webhooks)

Use the command below to remove this Funnel listener. Funnel exposes the manager's HTTP endpoints over the internet. Keep the setup browser endpoint and Docker sockets on their local or private networks.

```sh
tailscale funnel --https=443 off
tailscale funnel status
```

Workers on your tailnet can use their MagicDNS names in `ssh://` entries if the manager container can resolve those names and reach SSH. [Tailscale Funnel CLI](https://tailscale.com/docs/reference/tailscale-cli/funnel)
