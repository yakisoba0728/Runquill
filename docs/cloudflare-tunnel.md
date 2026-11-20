# Runquill behind Cloudflare Tunnel

[Runquill README](../README.md) · [한국어](cloudflare-tunnel.ko.md)

This guide publishes the Docker Compose panel and GitHub App webhook under one HTTPS hostname. Cloudflare Tunnel connects outbound from your Linux server. Runquill's panel and webhook stay bound to loopback on the server.

## Create the tunnel

Use a domain managed by Cloudflare and a hostname with a valid Cloudflare edge certificate. The example uses `runner-ci.example.com`. Install `cloudflared` from the [Cloudflare package repository](https://pkg.cloudflare.com/) for your distribution. Run the following as the user who will manage the tunnel. Open the login URL and approve the zone in your browser.

```sh
cloudflared tunnel login
cloudflared tunnel create runquill
cloudflared tunnel route dns runquill runner-ci.example.com
```

Copy the UUID from the create command into `TUNNEL_ID`. Keep `~/.cloudflared/cert.pem` private. It authorizes tunnel management for the account. Keep the tunnel credentials JSON private as well.

```sh
TUNNEL_ID='YOUR_TUNNEL_UUID'
sudo install -d -m 0755 /etc/cloudflared
sudo install -m 0600 "$HOME/.cloudflared/$TUNNEL_ID.json" "/etc/cloudflared/$TUNNEL_ID.json"
sudoedit /etc/cloudflared/config.yml
```

Use these ingress rules in the config file. Replace `YOUR_TUNNEL_UUID` in both places and change the hostname in both rules. Cloudflared evaluates rules in order, so the exact `/webhook` match comes first.

```yaml
tunnel: YOUR_TUNNEL_UUID
credentials-file: /etc/cloudflared/YOUR_TUNNEL_UUID.json
ingress:
  - hostname: runner-ci.example.com
    path: ^/webhook$
    service: http://127.0.0.1:8080
  - hostname: runner-ci.example.com
    service: http://127.0.0.1:8081
  - service: http_status:404
```

Validate the rules and install the systemd service. If a cloudflared service already runs on this host, add the routes to its existing configuration, validate that configuration, then restart that service.

```sh
sudo cloudflared tunnel --config /etc/cloudflared/config.yml ingress validate
sudo cloudflared tunnel --config /etc/cloudflared/config.yml ingress rule https://runner-ci.example.com/webhook
sudo cloudflared --config /etc/cloudflared/config.yml service install
systemctl is-enabled cloudflared.service
systemctl is-active cloudflared.service
```

[Cloudflare locally managed tunnels](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/create-local-tunnel/)

## Configure Runquill and its GitHub App

For a new install, run the [browser wizard](install.md#install-a-public-release) with `--no-tailscale`. Under **Advanced**, set **Panel URL** to `https://runner-ci.example.com` and **Public webhook URL** to `https://runner-ci.example.com/webhook`. Complete the GitHub App creation and installation through the wizard.

For an existing installation, keep the same App and queue. Add `https://runner-ci.example.com/auth/github/callback` as a callback on the App used for panel sign-in. On the runner App, enable the webhook, set its URL with `/webhook`, and subscribe to **Workflow job**. Use the same webhook secret in GitHub and the manager. If you already configured one, reuse it.

Update these values in the installation directory's `.env`:

```dotenv
RUNQUILL_PANEL_URL=https://runner-ci.example.com
RUNQUILL_WEBHOOK_URL=https://runner-ci.example.com/webhook
RUNQUILL_POLL_INTERVAL=30s
```

Set `RUNQUILL_WEBHOOK_SECRET` in that same file. If the panel already has a saved polling interval, change **Settings** in the panel to override it. Recreate the manager from the same installation directory with every Compose file used by the deployment:

```sh
cd "$HOME/runquill"
docker compose -f compose.yaml -f compose.panel.yaml up -d --no-build
```

The panel uses GitHub OAuth. The webhook validates GitHub's signature. If you put Cloudflare Access in front of the panel, leave the exact `/webhook` path outside the interactive login policy so GitHub can deliver events.

[Configure a GitHub App webhook](https://docs.github.com/en/webhooks/using-webhooks/creating-webhooks)

## Verify the public address and a real job

From a client outside the server:

```sh
curl -fsS -o /dev/null -w '%{http_code}\n' https://runner-ci.example.com/
curl -sS -o /dev/null -w '%{http_code}\n' -d '{}' https://runner-ci.example.com/webhook
```

Expect `200` for the panel and `401` for the unsigned webhook. Sign in to the panel with your enrolled GitHub account. Then [run the first-job workflow](workflows.md#verify-your-first-job) from a private repository covered by the App. Check **Advanced → Recent deliveries** in GitHub App settings for HTTP `202` on the job's `workflow_job` deliveries. In the manager log, match the same job ID against `queued job received` with `source=webhook`, `runner started`, and `runner removed`. Verify the job succeeded in GitHub and the runner has been removed.

Run the [reboot checks](operations.md#verify-a-reboot) to confirm automatic startup. For a webhook-only test, use [the polling check](operations.md#verify-webhook-only-delivery).
