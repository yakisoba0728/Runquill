#!/usr/bin/env python3
import base64
from contextlib import contextmanager
import hashlib
from http.client import HTTPConnection, HTTPException
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import signal
import subprocess
import sys
import tempfile
import threading
import time
from datetime import datetime, timezone
from urllib.request import Request, urlopen


MANAGER_IMAGE = "runquill-live-manager:ci"
RUNNER_IMAGE = "ghcr.io/yakisoba0728/runquill-runner:v0.4.0"
CLOUDFLARED = "cloudflare/cloudflared@sha256:e613aadbc2d9511785d2e67a86b0c8ac18dd63a3f9af0d06faf04cbfdcb35f2d"
JOB_WORKFLOW = "runquill-webhook-job.yml"
MAX_PAYLOAD = 2 * 1024 * 1024
STATE = Path(os.environ.get("RUNNER_TEMP", tempfile.gettempdir())) / "runquill-webhook-target.json"


def docker(*args, check=True, timeout=180):
    result = subprocess.run(("docker", *args), capture_output=True, text=True, timeout=timeout)
    if check and result.returncode:
        # The Docker command may include credential file paths or environment variables.
        raise RuntimeError(f"docker {args[0]} failed: {result.stderr.strip()}")
    return (result.stdout + result.stderr).strip()


def github(token, path, method="GET", body=None):
    request = Request(f"https://api.github.com{path}", method=method,
                      data=json.dumps(body).encode() if body is not None else None,
                      headers={"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json",
                               "X-GitHub-Api-Version": "2026-03-10", "Content-Type": "application/json"})
    with urlopen(request, timeout=12) as response:
        raw = response.read()
    return json.loads(raw) if raw else None


def wait_for(description, check, seconds=75):
    deadline = time.monotonic() + seconds
    error = ""
    while time.monotonic() < deadline:
        try:
            if value := check():
                return value
        except (OSError, RuntimeError) as exc:
            error = f" ({exc})"
        time.sleep(2)
    raise RuntimeError(f"Timed out waiting for {description}{error}")


def jwt(app_id, key):
    def encoded(raw):
        return base64.urlsafe_b64encode(raw).decode().rstrip("=")

    now = int(time.time())
    claim = encoded(b'{"alg":"RS256","typ":"JWT"}') + "." + encoded(json.dumps(
        {"iss": app_id, "iat": now - 45, "exp": now + 480}).encode())
    signature = subprocess.check_output(("openssl", "dgst", "-sha256", "-sign", str(key)), input=claim.encode())
    return f"{claim}.{encoded(signature)}"


def restore_webhook(app_token):
    if not STATE.is_file():
        return
    state = json.loads(STATE.read_text())
    current = github(app_token(), "/app/hook/config")["url"]
    if current == state["temporary"]:
        github(app_token(), "/app/hook/config", "PATCH", {"url": state["original"]})
        assert github(app_token(), "/app/hook/config")["url"] == state["original"], "Webhook restoration failed"
    elif current != state["original"]:
        raise RuntimeError("App webhook URL was changed externally during the smoke test")
    STATE.unlink()
    print("Original GitHub App webhook URL restored", flush=True)


@contextmanager
def relay(manager_port, repo, installation_id, nonce):
    deliveries = {}

    class WebhookRelay(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def reply(self, code):
            self.send_response(code)
            self.end_headers()

        def do_GET(self):
            self.reply(200 if self.path == f"/healthz/{nonce}" else 404)

        def do_POST(self):
            if self.path != "/webhook" or self.headers.get("X-GitHub-Event") != "workflow_job":
                return self.reply(404)
            signature = self.headers.get("X-Hub-Signature-256", "")
            guid = self.headers.get("X-GitHub-Delivery")
            if not guid or not re.fullmatch(r"sha256=[a-f0-9]{64}", signature):
                return self.reply(403)
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= MAX_PAYLOAD:
                    return self.reply(413)
                self.connection.settimeout(8)
                raw = self.rfile.read(length)
                payload = json.loads(raw)
                repo_data, installation, job = (payload[key] for key in ("repository", "installation", "workflow_job"))
                labels = job["labels"]
                if not isinstance(labels, list) or len(labels) != 1 or not isinstance(labels[0], str):
                    return self.reply(400)
                if (repo_data["full_name"] != repo or installation["id"] != installation_id
                        or not labels[0].endswith(f"-webhook-{nonce}")):
                    return self.reply(403)
            except (ValueError, OSError, TypeError, KeyError):
                return self.reply(400)
            conn = HTTPConnection("127.0.0.1", manager_port, timeout=8)
            try:
                conn.request("POST", "/webhook", raw, {"X-GitHub-Event": "workflow_job",
                             "X-GitHub-Delivery": guid, "X-Hub-Signature-256": signature,
                             "Content-Type": "application/json"})
                with conn.getresponse() as response:
                    code = response.status
            except (OSError, HTTPException):
                return self.reply(502)
            finally:
                conn.close()
            deliveries[(job.get("id"), payload.get("action"))] = (guid, code)
            self.reply(code)

    with ThreadingHTTPServer(("127.0.0.1", 0), WebhookRelay) as server:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield server.server_port, deliveries
        finally:
            server.shutdown()
            thread.join()


def smoke(app_token, directory):
    repo = os.environ["GITHUB_REPOSITORY"]
    owner = repo.split("/", 1)[0]
    token = os.environ["GITHUB_TOKEN"]
    webhook_secret = os.environ["RUNQUILL_WEBHOOK_SECRET"]
    assert webhook_secret, "Set the test App's webhook secret"
    repo_path = f"/repos/{repo}"
    info = github(token, repo_path)
    assert info["private"] and os.environ["GITHUB_REF"] == f"refs/heads/{info['default_branch']}"
    app = github(app_token(), "/app")
    assert app["owner"]["login"] == owner and "workflow_job" in app["events"], "Use the dedicated webhook App"
    install = github(app_token(), f"/users/{owner}/installation")
    assert install["repository_selection"] == "selected", "Install the test App on the fixture repository only"
    app_access = github(app_token(), f"/app/installations/{install['id']}/access_tokens", "POST",
                        {"permissions": {"actions": "read", "administration": "read"}})["token"]
    project = f"runquill-webhook-{secrets.token_hex(5)}"
    manager, worker, engine, tunnel, socket = (f"{project}-{part}" for part in
                                               ("manager", "worker", "engine", "tunnel", "socket"))
    nonce = secrets.token_hex(6)
    run_id = None
    started = datetime.now(timezone.utc).isoformat()

    def inner(*args):
        return docker("exec", engine, "docker", *args)

    def runners():
        return github(app_access, f"{repo_path}/actions/runners?per_page=100")["runners"]

    try:
        repos = github(app_access, "/installation/repositories?per_page=100")
        assert repos["total_count"] == 1 and repos["repositories"][0]["full_name"] == repo, "Test App scope differs"
        controller = os.environ.get("RUNNER_NAME")
        assert not any(runner["name"] != controller for runner in runners()), "Fixture repository already has a competing runner"
        docker("run", "-d", "--name", engine, "--privileged", "-e", "DOCKER_HOST=unix:///worker/docker.sock",
               "-v", f"{socket}:/worker", "docker:29-dind", "dockerd", "--host=unix:///worker/docker.sock",
               "--storage-driver=overlay2")
        engine_id = wait_for("standalone Docker worker engine", lambda: inner("info", "--format", "{{.ID}}"))
        assert engine_id != docker("info", "--format", "{{.ID}}")
        docker("exec", engine, "docker", "pull", RUNNER_IMAGE, timeout=260)
        worker_name = "live-webhook"
        worker_host = f"agent://{worker_name}"
        worker_token = secrets.token_hex(32)
        (directory / "worker.token").write_text(worker_token + "\n")
        (directory / "workers.json").write_text(json.dumps({
            worker_name: {"engine_id": engine_id, "token_sha256": hashlib.sha256(worker_token.encode()).hexdigest()},
        }))
        manager_settings = {
            "RUNQUILL_OWNER": owner, "RUNQUILL_APP_ID": str(app["id"]), "RUNQUILL_REPOSITORY_SCOPE": "selected",
            "RUNQUILL_KEY_FILE": "/auth/app.pem", "RUNQUILL_POLL_INTERVAL": "0", "RUNQUILL_WEBHOOK_SECRET": webhook_secret,
            "RUNQUILL_RUNNER_IMAGE": RUNNER_IMAGE, "RUNQUILL_DOCKER_HOSTS": worker_host, "RUNQUILL_MAX_RUNNERS": "1",
            "RUNQUILL_WORKER_AUTH_FILE": "/auth/workers.json", "RUNQUILL_DB": "/data/runquill.sqlite3",
            "RUNQUILL_BIND": "0.0.0.0",
            "RUNQUILL_WORKER_POOLS": json.dumps({"default": {"hosts": [worker_host], "privileged": False,
                                                             "repositories": [repo]}}),
        }
        # Pass the webhook secret by environment file so it never appears in the command line.
        env_file = directory / "manager.env"
        env_file.write_text("".join(f"{k}={v}\n" for k, v in manager_settings.items()))
        docker("run", "-d", "--name", manager, "--env-file", str(env_file), "-p", "127.0.0.1::8080",
               "-v", f"{directory / 'app.pem'}:/auth/app.pem:ro", "-v", f"{directory / 'workers.json'}:/auth/workers.json:ro",
               "-v", "/data", MANAGER_IMAGE)
        manager_port = int(docker("port", manager, "8080/tcp").rsplit(":", 1)[1])
        docker("run", "-d", "--name", worker, "--network", f"container:{manager}", "-v", f"{socket}:/worker",
               "-v", f"{directory / 'worker.token'}:/auth/worker.token", "-e", "DOCKER_HOST=unix:///worker/docker.sock",
               "-e", f"RUNQUILL_RUNNER_IMAGE={RUNNER_IMAGE}", MANAGER_IMAGE, "worker", "--name", worker_name,
               "--manager", "http://127.0.0.1:8080", "--token-file", "/auth/worker.token")
        wait_for("worker online", lambda: any(h["name"] == worker_host and h["status"] == "UP" for h in
                 json.loads(docker("exec", manager, "runquill", "status", "--json"))["hosts"]))
        assert "Preflight passed" in docker("exec", manager, "runquill", "doctor", "--repo", repo)
        assert any("webhook=true" in line and "poll_interval=0s" in line for line in docker("logs", manager).splitlines())
        print("Runquill ready with poll_interval=0s and a separate Docker worker", flush=True)

        with relay(manager_port, repo, install["id"], nonce) as (port, inbound):
            docker("run", "-d", "--network", "host", "--name", tunnel, CLOUDFLARED, "tunnel", "--no-autoupdate",
                   "--protocol", "http2", "--url", f"http://127.0.0.1:{port}")
            tunnel_url = wait_for("temporary HTTPS URL", lambda: next(iter(re.findall(
                r"https://[a-z0-9-]+\.trycloudflare\.com", docker("logs", tunnel))), None))
            health = f"{tunnel_url}/healthz/{nonce}"

            def reachable():
                # The tunnel hostname is new and the local resolver can cache a negative lookup.
                return subprocess.run(("curl", "--fail", "--silent", "--max-time", "8", "--output", "/dev/null",
                                       "--doh-url", "https://cloudflare-dns.com/dns-query", health),
                                      timeout=10).returncode == 0

            wait_for("public HTTPS relay", reachable, seconds=130)
            existing = github(app_token(), "/app/hook/config")
            assert existing["url"] and existing["content_type"] == "json" and str(existing["insecure_ssl"]) == "0"
            temporary = f"{tunnel_url}/webhook"
            STATE.write_text(json.dumps({"original": existing["url"], "temporary": temporary}))
            github(app_token(), "/app/hook/config", "PATCH", {"url": temporary})
            assert github(app_token(), "/app/hook/config")["url"] == temporary

            dispatch = github(token, f"{repo_path}/actions/workflows/{JOB_WORKFLOW}/dispatches", "POST",
                              {"ref": info["default_branch"], "inputs": {"nonce": nonce}, "return_run_details": True})
            run_id = dispatch["workflow_run_id"]
            print(f"Dispatched fixture job {dispatch['html_url']}", flush=True)
            jobs_path = f"{repo_path}/actions/runs/{run_id}/jobs"
            first = wait_for("fixture job in GitHub", lambda: next(iter(github(token, jobs_path)["jobs"]), None))
            job_id = first["id"]
            wait_for("GitHub queued webhook accepted", lambda: inbound.get((job_id, "queued"), (None, 0))[1] == 202)

            def finished():
                job = github(token, f"{repo_path}/actions/jobs/{job_id}")
                return job if job["status"] == "completed" else None

            job = wait_for("GitHub job to finish", finished, seconds=180)
            assert job["conclusion"] == "success", f"Fixture job concluded with {job['conclusion']}"
            runner = job["runner_name"]
            assert runner and job["runner_id"], "GitHub did not record a runner"
            wait_for("GitHub completed webhook accepted", lambda: inbound.get((job_id, "completed"), (None, 0))[1] == 202)
            wait_for("Runquill to clean the runner", lambda: any("runner removed" in line and f"job={job_id}" in line
                     for line in docker("logs", manager).splitlines()))
            lines = docker("logs", manager).splitlines()
            assert any("source=webhook" in line and f"job={job_id}" in line for line in lines)
            assert any("runner started" in line and f"runner_host={worker_host}" in line and runner in line for line in lines), (
                "The job was not executed by the smoke-test worker. Check for another Runquill manager watching this repository."
            )
            events = [json.loads(line) for line in inner("events", "--since", started, "--until",
                      datetime.now(timezone.utc).isoformat(), "--filter", "type=container",
                      "--filter", f"label=io.runquill.worker={worker_name}", "--format", "{{json .}}").splitlines()]
            stages = [(e["Action"], e["Actor"]["Attributes"].get("exitCode")) for e in events
                      if e["Actor"]["Attributes"].get("name") == runner and e["Action"] in ("start", "die", "destroy")]
            assert stages == [("start", None), ("die", "0"), ("destroy", None)], stages
            assert not inner("ps", "-aq", "--filter", f"label=io.runquill.worker={worker_name}")
            assert not any(r["name"] == runner or r["id"] == job["runner_id"] for r in runners())
            assert len(github(token, jobs_path)["jobs"]) == 1

            def app_deliveries():
                by_guid = {record["guid"]: record for record in github(app_token(), "/app/hook/deliveries?per_page=100")}
                rows = [by_guid.get(inbound[(job_id, action)][0]) for action in ("queued", "completed")]
                return rows if all(row and row["event"] == "workflow_job" and row["status_code"] == 202 for row in rows) else None

            records = wait_for("signed deliveries in the GitHub App history", app_deliveries)
            report = (f"[Fixture job]({job['html_url']}) succeeded with discovery polling disabled. "
                      f"GitHub App deliveries {records[0]['id']} and {records[1]['id']} returned HTTP 202. "
                      "The Docker worker exited with code 0 and removed the runner. The GitHub registration is gone.")
            print(report, flush=True)
            if summary := os.getenv("GITHUB_STEP_SUMMARY"):
                with Path(summary).open("a") as file:
                    print(report, file=file)
    finally:
        try:
            if run_id and github(token, f"{repo_path}/actions/runs/{run_id}")["status"] != "completed":
                github(token, f"{repo_path}/actions/runs/{run_id}/cancel", "POST")
        finally:
            try:
                restore_webhook(app_token)
            finally:
                if sys.exc_info()[0] is not None:
                    for name in (manager, worker, tunnel):
                        print(docker("logs", "--tail", "30", name, check=False), flush=True)
                docker("rm", "-fv", worker, manager, tunnel, engine, check=False)
                docker("volume", "rm", socket, check=False)
                github(app_access, "/installation/token", "DELETE")


def interrupt(signum, _frame):
    raise InterruptedError(f"Received signal {signum}")


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupt)
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="runquill-live-webhook-") as scratch:
        directory = Path(scratch)
        key = directory / "app.pem"
        key.write_text(os.environ["RUNQUILL_APP_PRIVATE_KEY"].rstrip() + "\n")
        app_id = os.environ["RUNQUILL_APP_ID"]
        assert app_id.isdecimal(), "Set RUNQUILL_APP_ID to the dedicated GitHub App ID"

        def app_token():
            return jwt(app_id, key)

        if sys.argv[1:] == ["--restore-webhook"]:
            restore_webhook(app_token)
        else:
            smoke(app_token, directory)
