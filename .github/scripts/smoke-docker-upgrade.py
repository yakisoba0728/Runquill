#!/usr/bin/env python3
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


REPOSITORY = "yakisoba0728/Runquill"
IMAGE = f"ghcr.io/{REPOSITORY.lower()}"
TESTDATA = Path(__file__).resolve().parents[1] / "testdata/docker-upgrade"
PANEL_ORIGIN = "http://127.0.0.1:8081"
WEBHOOK_SECRET = "upgrade-fixture-webhook"
PYTHON_IMAGE = "python:3.14-alpine"
WORKER = "upgrade-smoke"
HOST = f"agent://{WORKER}"
FIXTURE_IMAGE = "runquill-upgrade:fixture"
JOB_ID = 7001


def run(*args, capture=True, check=True):
    result = subprocess.run(args, text=True, capture_output=capture, timeout=300)
    if check and result.returncode:
        # Keep command arguments out of the error because enrollment uses a one-time secret.
        raise RuntimeError(f"{args[0]} exited {result.returncode}: {result.stderr or result.stdout or ''}")
    return result.stdout.strip() if capture else result.returncode


def version_key(tag):
    match = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)(?:-[0-9A-Za-z.-]+)?", tag)
    if match is None:
        raise ValueError(f"Invalid version: {tag}")
    return tuple(map(int, match.groups()))


def previous_release(target):
    tags = run("gh", "api", "--paginate", f"repos/{REPOSITORY}/releases?per_page=100", "--jq",
               '.[] | select(.draft == false and .prerelease == false) | .tag_name').splitlines()
    candidates = [tag for tag in tags if re.fullmatch(r"v\d+\.\d+\.\d+", tag)
                  and (0, 3, 4) <= version_key(tag) < version_key(target)]
    if not candidates:
        raise RuntimeError(f"No earlier published stable release supported by this check for {target}")
    return max(candidates, key=version_key)


def wait_for(description, probe, timeout=65):
    deadline = time.monotonic() + timeout
    last_error = None
    while time.monotonic() < deadline:
        try:
            if value := probe():
                return value
        except (URLError, TimeoutError, RuntimeError) as exc:
            last_error = exc
        time.sleep(0.5)
    raise RuntimeError(f"Timed out waiting for {description}: {last_error or 'condition was not met'}")


def request(url, method, body=None, headers=None, status=200):
    req = Request(url, data=body, headers=headers or {}, method=method)
    try:
        with urlopen(req, timeout=5) as response:
            if response.status != status:
                raise RuntimeError(f"{method} {url} returned HTTP {response.status}, expected {status}")
            raw = response.read()
            return json.loads(raw) if raw else None
    except HTTPError as error:
        raise RuntimeError(f"{method} {url} returned HTTP {error.code}: {error.read().decode().strip()}") from error


class UpgradeTest:
    def __init__(self, root):
        self.root = root
        self.install = root / "installed"
        self.project = "runquill-upgrade-" + secrets.token_hex(5)
        self.worker = self.project + "-worker"
        self.session = secrets.token_urlsafe(32)
        self.compose_args = ("docker", "compose", "-p", self.project,
                             "--env-file", str(self.install / ".env"),
                             "-f", str(self.install / "compose.yaml"),
                             "-f", str(self.install / "compose.panel.yaml"),
                             "-f", str(self.install / "compose.fixture.yaml"))

    def compose(self, *args, **kwargs):
        return run(*self.compose_args, *args, **kwargs)

    def install_version(self, tag, *options):
        run("docker", "run", "--rm", "-v", f"{self.root}:/test", "-w", "/test", "alpine:3.23", "sh", "-ec", """
            owner=$1
            shift
            trap 'chown -R "$owner" /test' EXIT
            apk add --no-cache curl
            url="https://github.com/yakisoba0728/Runquill/releases/download/$1"
            for asset in install.sh SHA256SUMS; do
                curl --proto '=https' --proto-redir '=https' --retry 4 --retry-all-errors -fsSL "$url/$asset" -o "$asset"
            done
            awk '$2 == "install.sh"' SHA256SUMS | sha256sum -c -
            sh ./install.sh --version "$@" --dir ./installed
        """, "--", f"{os.getuid()}:{os.getgid()}", tag, *options, capture=False)

    def binary_version(self, path):
        return run("docker", "run", "--rm", "-v", f"{self.install}:/distribution:ro", "alpine:3.23", path, "--version")

    def start_manager(self, tag):
        self.compose("pull", "manager", capture=False)
        self.compose("up", "-d", "--no-build", "manager", "github", capture=False)
        self.webhook = "http://" + self.compose("port", "manager", "8080")
        self.origin = "http://" + self.compose("port", "manager", "8081")
        wait_for("manager health", lambda: request(self.webhook + "/healthz", "GET") is None)
        assert self.compose("exec", "-T", "manager", "runquill", "--version") == tag

    def seed_session(self):
        # Avoid external OAuth. The old manager creates the schema and admin account.
        run("docker", "run", "--rm", "-v", f"{self.project}_queue:/data", PYTHON_IMAGE, "python", "-c", """
import sqlite3, sys, time
with sqlite3.connect('/data/runquill.sqlite3', timeout=10) as db:
    db.execute('INSERT INTO panel_sessions (digest, user_id, expires_at) VALUES (?, 100, ?)',
               (bytes.fromhex(sys.argv[1]), int(time.time()) + 3600))
""", hashlib.sha256(self.session.encode()).hexdigest())

    def panel(self, path, method="GET", body=None, status=200):
        headers = {"Cookie": f"rq_session={self.session}", "Origin": PANEL_ORIGIN,
                   "Host": "127.0.0.1:8081", "Content-Type": "application/json"}
        return request(self.origin + path, method, json.dumps(body).encode() if body is not None else None, headers, status)

    def host(self, name):
        fleet = self.panel("/snapshot")["fleet"] or {}
        return next((host for host in fleet.get("hosts", []) if host["name"] == name), {})

    def job(self):
        jobs = self.panel("/api/jobs")["jobs"] or []
        return next((job for job in jobs if job["id"] == JOB_ID), {})

    def deliver(self, action):
        payload = json.dumps({"action": action, "installation": {"id": 1}, "repository": {
            "id": 2, "full_name": "upgrade-fixture/private-repo", "private": True, "owner": {"login": "upgrade-fixture"},
        }, "workflow_job": {"id": JOB_ID, "run_id": 17, "labels": ["runquill-17-1-upgrade-smoke"]}}).encode()
        signature = hmac.new(WEBHOOK_SECRET.encode(), payload, "sha256").hexdigest()
        request(self.webhook + "/webhook", "POST", payload, {
            "X-GitHub-Event": "workflow_job", "X-Hub-Signature-256": f"sha256={signature}",
        }, 202)

    def volume(self):
        return run("docker", "inspect", "--format", '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}',
                   self.compose("ps", "-q", "manager"))

    def exercise(self, old, new):
        self.install_version(old, "--download-only")
        assert self.binary_version("/distribution/runquill") == old
        for src, dest in [("compose.yaml", "compose.fixture.yaml"), ("github.py", "github.py")]:
            shutil.copyfile(TESTDATA / src, self.install / dest)
        (self.install / "runner").mkdir()
        for name in ("Dockerfile", "run.sh"):
            shutil.copyfile(TESTDATA / name, self.install / "runner" / name)
        run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=api.github.com", "-addext", "subjectAltName=DNS:api.github.com",
            "-keyout", str(self.install / "github.key"), "-out", str(self.install / "github.pem"))
        run("openssl", "genrsa", "-out", str(self.install / "github-app.pem"), "2048")
        env = self.install / ".env"
        env.write_text(f"""RUNQUILL_OWNER=upgrade-fixture
RUNQUILL_APP_ID=123
RUNQUILL_WEBHOOK_SECRET={WEBHOOK_SECRET}
RUNQUILL_POLL_INTERVAL=0
RUNQUILL_KEY_FILE=./github-app.pem
RUNQUILL_PANEL_URL={PANEL_ORIGIN}
RUNQUILL_PANEL_GITHUB_CLIENT_ID=fixture-client
RUNQUILL_PANEL_GITHUB_CLIENT_SECRET=fixture-secret
RUNQUILL_PANEL_BOOTSTRAP_ID=100
RUNQUILL_MANAGER_IMAGE={IMAGE}:{old}
RUNQUILL_RUNNER_IMAGE={IMAGE}-runner:{old}
""")
        self.start_manager(old)
        self.seed_session()
        admin = self.panel("/api/me")
        assert admin["role"] == "admin", admin
        policy = self.panel("/api/settings")["policy"]
        self.panel("/api/settings", "PUT", dict(policy, slot_limit=3))
        saved = self.panel("/api/settings")["policy"]
        assert saved["slot_limit"] == 3 and saved["revision"] > policy["revision"], saved
        wait_for("local Docker host", lambda: self.host("local").get("status") == "UP")
        self.panel("/api/hosts/cordon", "POST", {"host": "local", "cordoned": True}, 204)
        wait_for("cordoned local Docker host", lambda: self.host("local").get("cordoned"))
        self.deliver("queued")
        waiting = wait_for("queued job on the old release", self.job)
        assert waiting["outcome"] == "waiting" and waiting["queued_at"], waiting
        volume = self.volume()
        assert volume == f"{self.project}_queue", volume
        protected = {name: (self.install / name).read_bytes() for name in (".env", "github-app.pem", "compose.fixture.yaml")}
        print(f"{old}: saved panel settings and queued job {JOB_ID}", flush=True)

        self.install_version(new, "--upgrade", "--no-install")
        assert self.binary_version("/distribution/runquill") == new
        assert self.binary_version("/distribution/.previous/runquill") == old
        for name, content in protected.items():
            assert (self.install / name).read_bytes() == content, f"Installer changed {name}"
        env.write_text(env.read_text().replace(f"{IMAGE}:{old}\n", f"{IMAGE}:{new}\n")
                       .replace(f"{IMAGE}-runner:{old}\n", f"{IMAGE}-runner:{new}\n"))
        self.start_manager(new)
        assert self.volume() == volume, "The upgrade replaced the queue volume"
        assert self.panel("/api/me") == admin, "The prior admin session was lost"
        upgraded = self.panel("/api/settings")["policy"]
        assert upgraded["slot_limit"] == saved["slot_limit"] and upgraded["revision"] == saved["revision"], upgraded
        assert upgraded["runner_image"] == f"{IMAGE}-runner:{new}", upgraded
        wait_for("retained cordon", lambda: self.host("local").get("cordoned"))
        pending = self.job()
        for key in ("id", "repository", "runner_label", "outcome", "queued_at"):
            assert pending.get(key) == waiting[key], (key, pending)
        print(f"{new}: confirmed the same volume, settings, cordon, and queued job", flush=True)

        self.run_worker(new, env, waiting)

    def run_worker(self, new, env, waiting):
        self.compose("up", "-d", "engine", capture=False)
        inner = wait_for("worker Docker engine", lambda: self.compose("exec", "-T", "engine", "docker", "info", "--format", "{{.ID}}"))
        assert inner != run("docker", "info", "--format", "{{.ID}}"), "Worker engine must be independent"
        runner = f"{IMAGE}-runner:{new}"
        self.compose("exec", "-T", "engine", "docker", "pull", runner, capture=False)
        self.compose("exec", "-T", "engine", "docker", "build", "--build-arg", f"RUNNER_IMAGE={runner}",
                     "-t", FIXTURE_IMAGE, "/fixture/runner", capture=False)
        pools = {"default": {"hosts": [HOST], "image": FIXTURE_IMAGE, "privileged": False},
                 "maintenance": {"hosts": ["local"], "privileged": False}}
        with env.open("a") as file:
            file.write(f"RUNQUILL_DOCKER_HOSTS=local,{HOST}\nRUNQUILL_WORKER_POOLS='{json.dumps(pools)}'\n"
                       "RUNQUILL_WORKER_ENROLLMENT_URL=http://127.0.0.1:8080\n")
        self.start_manager(new)
        wait_for("worker enrollment in the panel", lambda: self.host(HOST).get("enrollment_available"))
        assert self.job()["outcome"] == "waiting", "Job was assigned before the worker enrolled"
        manager = self.compose("ps", "-q", "manager")
        command = self.panel(f"/api/hosts/{WORKER}/enrollment", "POST", status=201)["command"]
        run("docker", "run", "-d", "--name", self.worker, "--network", f"container:{manager}",
            "-e", "DOCKER_HOST=unix:///worker/docker.sock", "-v", f"{self.project}_worker-socket:/worker",
            "-w", "/tmp", "--entrypoint", "sh", f"{IMAGE}:{new}", "-ec", command)
        wait_for("enrolled worker online", lambda: self.host(HOST).get("status") == "UP")

        def worker_docker(*args):
            return self.compose("exec", "-T", "engine", "docker", *args)

        container = wait_for("runner on the enrolled worker", lambda: worker_docker(
            "ps", "-aq", "--filter", "label=io.runquill.runner=1", "--filter", f"label=io.runquill.worker={WORKER}"))
        assert worker_docker("wait", container) == "0", worker_docker("logs", container)
        assert "runquill-upgrade-job-executed" in worker_docker("logs", container), "Fixture job did not execute"
        self.deliver("completed")

        def finished():
            job = self.job()
            return job if job.get("outcome") == "finished" and job.get("cleaned_at") else None

        result = wait_for("original job completion and cleanup", finished)
        assert result["host"] == HOST and result["queued_at"] == waiting["queued_at"], result
        assert not worker_docker("ps", "-aq", "--filter", "label=io.runquill.runner=1"), "Runner container was not removed"
        assert '"DELETE /repos/upgrade-fixture/private-repo/actions/runners/42 HTTP/1.1" 204' in self.compose("logs", "github")
        print(f"{new}: newly enrolled worker finished the original job {JOB_ID} and cleaned up its runner", flush=True)

    def cleanup(self, failed):
        if not (self.install / ".env").is_file():
            return
        if failed:
            self.compose("logs", "--tail", "40", "manager", "github", "engine", capture=False, check=False)
            run("docker", "logs", "--tail", "40", self.worker, capture=False, check=False)
        run("docker", "rm", "-fv", self.worker, check=False)
        self.compose("down", "--volumes", "--remove-orphans", capture=False, check=not failed)


if __name__ == "__main__":
    if len(sys.argv) not in (2, 3):
        sys.exit(f"Usage: {sys.argv[0]} [previous-release] target-release")
    target = sys.argv[-1]
    old = sys.argv[1] if len(sys.argv) == 3 else previous_release(target)
    if version_key(old) >= version_key(target):
        sys.exit("The baseline release must be older than the target release")
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="runquill-upgrade-") as directory:
        test = UpgradeTest(Path(directory).resolve())
        try:
            test.exercise(old, target)
        finally:
            test.cleanup(failed=sys.exc_info()[0] is not None)
