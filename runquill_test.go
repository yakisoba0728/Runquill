package main

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhook(t *testing.T) {
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	handler := webhook(q, "owner", []byte("secret"), nil)

	for _, test := range []struct {
		id      int64
		action  string
		label   string
		private bool
		key     string
		status  int
	}{
		{1, "queued", "runquill-17-1-ci", true, "wrong", 401},
		{2, "completed", "runquill-17-1-ci", true, "secret", 202},
		{2, "queued", "runquill-17-1-ci", true, "secret", 202},
		{3, "queued", "runquill-17-1-ci", false, "secret", 202},
		{4, "queued", "ubuntu-latest", true, "secret", 202},
		{5, "queued", "runquill-17-1-ci", true, "secret", 202},
		{5, "queued", "runquill-17-1-ci", true, "secret", 202},
	} {
		payload := fmt.Sprintf(`{"action":%q,"installation":{"id":1},
            "repository":{"id":2,"full_name":"Owner/repo","private":%t,"owner":{"login":"Owner"}},
            "workflow_job":{"id":%d,"run_id":17,"labels":[%q]}}`,
			test.action, test.private, test.id, test.label)
		mac := hmac.New(sha256.New, []byte(test.key))
		mac.Write([]byte(payload))
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-GitHub-Event", "workflow_job")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != test.status {
			t.Fatalf("job %d: status %d, want %d", test.id, res.Code, test.status)
		}
	}
	jobs, err := q.jobs(t.Context(), "ORDER BY sequence")
	if err != nil || len(jobs) != 2 || jobs[0].id != 2 || jobs[0].phase != completed ||
		jobs[1].id != 5 || jobs[1].phase != queued {
		t.Fatalf("unexpected jobs: %+v, error %v", jobs, err)
	}
}

type testRuntime struct {
	hosts   []dockerHostSnapshot
	started []string
	removed []string
}

func (r *testRuntime) snapshot(context.Context) ([]dockerHostSnapshot, error) {
	hosts := slices.Clone(r.hosts)
	for i := range hosts {
		hosts[i].Containers = maps.Clone(hosts[i].Containers)
	}
	return hosts, nil
}

func (r *testRuntime) startOn(_ context.Context, host, name, jit string) error {
	if jit != "test-config" {
		return fmt.Errorf("unexpected test runner config")
	}
	for _, target := range r.hosts {
		if target.Name == host && target.Containers != nil && target.Err == nil {
			r.started = append(r.started, name)
			target.Containers[name] = "running"
			return nil
		}
	}
	return fmt.Errorf("start on unavailable test host %s", host)
}

func (r *testRuntime) removeOn(_ context.Context, host, name string) error {
	for _, target := range r.hosts {
		if target.Name == host && target.Containers[name] != "" {
			r.removed = append(r.removed, name)
			delete(target.Containers, name)
			return nil
		}
	}
	return fmt.Errorf("remove runner %s on unavailable test host %s", name, host)
}

func TestSchedulerRestoresLeasesAndMaintainsFIFO(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var deleted atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			signatureAt := strings.LastIndexByte(auth, '.')
			if signatureAt < 0 {
				t.Error("missing JWT signature")
				w.WriteHeader(401)
				return
			}
			signature, err := base64.RawURLEncoding.DecodeString(auth[signatureAt+1:])
			digest := sha256.Sum256([]byte(auth[:signatureAt]))
			if err != nil || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
				t.Error("invalid JWT signature")
				w.WriteHeader(401)
				return
			}
			var body struct {
				RepositoryIDs []int64 `json:"repository_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !slices.Equal(body.RepositoryIDs, []int64{2}) {
				t.Error("installation token must be scoped to the requested repository")
				w.WriteHeader(400)
				return
			}
			fmt.Fprint(w, `{"token":"stub-installation-token"}`)
		case "GET /repos/owner/repo":
			fmt.Fprint(w, `{"id":2,"private":true,"owner":{"login":"owner"}}`)
		case "POST /repos/owner/repo/actions/runners/generate-jitconfig":
			var body struct {
				Name   string   `json:"name"`
				Labels []string `json:"labels"`
				Group  int      `json:"runner_group_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Labels) != 1 ||
				!strings.HasPrefix(body.Labels[0], "runquill-17-1-ci-") || body.Group != 1 ||
				auth != "stub-installation-token" {
				t.Error("unexpected JIT request")
				w.WriteHeader(400)
				return
			}
			fmt.Fprint(w, `{"runner":{"id":55},"encoded_jit_config":"test-config"}`)
		case "DELETE /repos/owner/repo/actions/runners/55":
			if deleted.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(204)
		default:
			if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/owner/repo/actions/jobs/") {
				fmt.Fprint(w, `{"status":"queued"}`)
				return
			}
			t.Errorf("unexpected API request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(api.Close)

	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, now := t.Context(), time.Now()
	const worker = "ssh://runner@worker"
	localRunners, workerRunners := make(map[string]string), make(map[string]string)
	runtime := &testRuntime{hosts: []dockerHostSnapshot{
		{Name: "local", Containers: localRunners},
		{Name: worker, Containers: workerRunners},
	}}
	gh := &github{base: api.URL, client: api.Client(), key: key, appID: "123", owner: "owner"}
	panel := newPanel(config{})
	s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 2, panel: panel}
	for _, id := range []int64{70, 20, 40} {
		j := job{id: id, repository: "owner/repo", repositoryID: 2, installationID: 1,
			route: fmt.Sprintf("runquill-17-1-ci-%d", id)}
		if _, err := q.record(ctx, j, now); err != nil {
			q.close()
			t.Fatal(err)
		}
	}
	err = s.tick(ctx, now)
	q.close()
	if err != nil || len(runtime.started) != 2 ||
		!strings.HasPrefix(runtime.started[0], "runquill-70-") || !strings.HasPrefix(runtime.started[1], "runquill-20-") ||
		localRunners[runtime.started[0]] != "running" || workerRunners[runtime.started[1]] != "running" {
		t.Fatalf("first tick: started %v, error %v", runtime.started, err)
	}
	q, err = openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	s.queue = q
	runtime.hosts[1].Containers = nil
	runtime.hosts[1].Err = fmt.Errorf("worker disconnected")
	leases, err := q.jobs(ctx, "WHERE runner_name IS NOT NULL ORDER BY sequence")
	if err != nil || len(leases) != 2 || leases[0].runnerHost != "local" || leases[1].runnerHost != worker {
		t.Fatalf("restored host leases: %+v, error %v", leases, err)
	}
	if err := q.advance(ctx, 20, completed, now); err != nil {
		t.Fatal(err)
	}
	if err := s.tick(ctx, now); err != nil || len(runtime.started) != 2 {
		t.Fatalf("after restart with offline worker: started %v, error %v", runtime.started, err)
	}
	report := panel.snapshot.Fleet
	if report == nil || report.Runners == nil || report.Runners.ReservedSlots != 1 || report.Runners.OfflineHosts != 1 {
		t.Fatalf("panel failed to report the reserved offline slot: %+v", report)
	}
	if err := q.advance(ctx, 70, completed, now); err != nil {
		t.Fatal(err)
	}
	finished := runtime.started[0]
	localRunners[finished] = "exited"
	if err := s.tick(ctx, now.Add(time.Second)); err != nil || deleted.Load() != 1 ||
		!slices.Equal(runtime.removed, []string{finished}) || len(runtime.started) != 3 ||
		!strings.HasPrefix(runtime.started[2], "runquill-40-") {
		t.Fatalf("cleanup: started %v, removed %v, deleted %v, error %v",
			runtime.started, runtime.removed, deleted.Load(), err)
	}
	if err := s.tick(ctx, now.Add(time.Second+retryDelay)); err != nil || deleted.Load() != 2 || len(runtime.removed) != 1 {
		t.Fatalf("runner registration cleanup retry: deletes=%d, error=%v", deleted.Load(), err)
	}
	runtime.hosts[1].Containers = workerRunners
	runtime.hosts[1].Err = nil
	if err := s.tick(ctx, now.Add(completionGrace)); err != nil || deleted.Load() != 3 ||
		!slices.Equal(runtime.removed, []string{finished, runtime.started[1]}) {
		t.Fatalf("cleanup after reconnection: removed %v, deletes=%d, error=%v", runtime.removed, deleted.Load(), err)
	}
	recent := panel.snapshot.Recent
	if len(recent) != 2 || recent[0].ID != 20 || recent[0].RunnerHost != worker || recent[0].CleanedAt == nil {
		t.Fatalf("panel failed to preserve the worker cleanup: %+v", recent)
	}

	t.Setenv("RUNQUILL_DB", path)
	t.Setenv("RUNQUILL_DOCKER_HOSTS", "local,"+worker)
	s.hostLimits = map[string]int{"local": 1}
	if err := setHostCordon(ctx, io.Discard, worker, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{50, 60} {
		j := job{id: id, repository: "owner/repo", repositoryID: 2, installationID: 1,
			route: fmt.Sprintf("runquill-17-1-ci-%d", id)}
		if _, err := q.record(ctx, j, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.tick(ctx, now.Add(completionGrace)); err != nil || len(runtime.started) != 3 {
		t.Fatalf("host at limit and worker cordoned: started %v, error %v", runtime.started, err)
	}
	if err := setHostCordon(ctx, io.Discard, worker, false); err != nil {
		t.Fatal(err)
	}
	if err := s.tick(ctx, now.Add(completionGrace)); err != nil || len(runtime.started) != 4 ||
		!strings.HasPrefix(runtime.started[3], "runquill-50-") || workerRunners[runtime.started[3]] != "running" {
		t.Fatalf("worker uncordoned: started %v, error %v", runtime.started, err)
	}
}

func TestDockerRunner(t *testing.T) {
	image := os.Getenv("RUNQUILL_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set RUNQUILL_TEST_DOCKER_IMAGE to a locally built runner image")
	}
	dir := t.TempDir()
	tag := fmt.Sprintf("runquill-smoke:%d", time.Now().UnixNano())
	name := strings.ReplaceAll(tag, ":", "-")
	for file, content := range map[string]string{
		"Dockerfile": "FROM " + image + "\nCOPY --chmod=755 run.sh /home/runner/run.sh\n",
		"run.sh": `#!/bin/bash
set -euo pipefail
test "$(id -un)" = runner
test "$1" = --jitconfig
test "$2" = test-config
test ! -e /run/runquill-jit
test "$(cat /proc/1/cgroup)" = '0::/'
printf 'FROM alpine:3.23\nRUN echo runquill > /result\n' | docker build -t runquill-built -
test "$(docker run --rm runquill-built cat /result)" = runquill
docker network create smoke
docker run -d --name postgres --network smoke -e POSTGRES_PASSWORD=smoke-test \
  -p 5432/tcp --health-cmd pg_isready --health-interval 2s postgres:17-alpine
for i in {1..30}; do
  if [ "$(docker inspect -f '{{.State.Health.Status}}' postgres)" = healthy ]; then break; fi
  sleep 1
done
port=$(docker port postgres 5432/tcp | head -n1 | cut -d: -f2)
docker run --rm --network host -e PGPASSWORD=smoke-test postgres:17-alpine \
  psql -h 127.0.0.1 -p "$port" -U postgres -v ON_ERROR_STOP=1 -c 'SELECT 1'
docker run --rm --network smoke -v "$PWD:/work:ro" -e PGPASSWORD=smoke-test postgres:17-alpine \
  sh -ec 'test -f /work/run.sh; psql -h postgres -U postgres -v ON_ERROR_STOP=1 -c "SELECT 1"'
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "build", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("build smoke image: %v\n%s", err, output)
	}
	d := docker{image: tag, cpus: 1, memoryMiB: 2048}
	t.Cleanup(func() {
		d.remove(context.Background(), name)
		dockerCommand(context.Background(), nil, "image", "rm", tag)
	})
	if err := d.start(ctx, name, "test-config"); err != nil {
		t.Fatal(err)
	}
	limits, err := dockerCommand(ctx, nil, "inspect", "--format",
		"{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}", name)
	if err != nil || strings.TrimSpace(limits) != "1000000000 2147483648 2147483648" {
		t.Fatalf("runner resource limits: %q, %v", limits, err)
	}
	exit, err := exec.CommandContext(ctx, "docker", "wait", name).CombinedOutput()
	if err != nil || strings.TrimSpace(string(exit)) != "0" {
		logs, _ := exec.Command("docker", "logs", "--tail", "100", name).CombinedOutput()
		t.Fatalf("runner exit=%s, error=%v\n%s", exit, err, logs)
	}
	if err := d.remove(ctx, name); err != nil {
		t.Fatal(err)
	}
	states, err := d.containers(ctx)
	if _, exists := states[name]; err != nil || exists {
		t.Fatalf("runner still present: error=%v present=%t", err, exists)
	}
}
