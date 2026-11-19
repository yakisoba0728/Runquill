package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testWorkerName  = "builder"
	testWorkerHost  = workerHostPrefix + testWorkerName
	testWorkerToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testWorkerImage = "runner:local"
	testWorkerJob   = "runquill-55-0123456789ab"
)

func workerTestFleet(t *testing.T) (*dockerFleet, *queue) {
	t.Helper()
	dir := t.TempDir()
	auth := filepath.Join(dir, "workers.json")
	sum := sha256.Sum256([]byte(testWorkerToken))
	if err := os.WriteFile(auth, fmt.Appendf(nil, `{"builder":{"engine_id":"worker-engine","token_sha256":"%x"}}`, sum), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNQUILL_WORKER_AUTH_FILE", auth)
	dbFile := filepath.Join(dir, "queue.sqlite")
	t.Setenv("RUNQUILL_DB", dbFile)
	q, err := openQueue(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	return workerTestManager(t, q), q
}

func workerTestManager(t *testing.T, q *queue) *dockerFleet {
	t.Helper()
	f, err := newDockerFleet(testWorkerImage, testWorkerHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.workers.attach(t.Context(), q.DB); err != nil {
		t.Fatal(err)
	}
	return f
}

func workerTestDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("WORKER_TEST_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(`#!/bin/sh
set -eu
cd "$WORKER_TEST_DIR"
printf '%s\n' "$*" >> commands
case "$1" in
    info) echo worker-engine ;;
    image)
        if [ "${5:-}" = '`+runnerCacheImageFormat+`' ]; then
            printf '%s\n' '`+cacheFixtureImageID+`|linux|amd64'
        else
            echo image-present
        fi ;;
    ps)
        [ -f runner ] || exit 0
        case "$*" in
            *io.runquill.runner=1*io.runquill.worker=builder*) ;;
            *) exit 4 ;;
        esac
        case "$*" in
            *--no-trunc*)
                printf '%s|0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef|1|%s\n' "$(cat runner)" "${WORKER_TEST_OWNER:-builder}" ;;
            *) printf '%s\t%s\n' "$(cat runner)" "$(cat state)" ;;
        esac ;;
    create) printf '%s\n' "$3" > runner; echo created > state ;;
    cp) cat > jit.tar ;;
    start) echo running > state ;;
    rm)
        [ "$4" = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" ]
        rm runner state ;;
    *) exit 3 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOutboundWorkerRunsAndCleansUpAcrossManagerRestart(t *testing.T) {
	fleet, q := workerTestFleet(t)
	dir := workerTestDocker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	worker := &workerClient{token: testWorkerToken, engine: "worker-engine", session: rand.Text(),
		target: docker{image: testWorkerImage, worker: testWorkerName}, client: &http.Client{
			Timeout: 6 * time.Second,
		}}
	serve := func(f *dockerFleet) func() {
		server := httptest.NewServer(f.workers.route(http.NotFoundHandler()))
		worker.endpoint = server.URL + "/worker/v1/" + testWorkerName + "/poll"
		active, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- worker.run(active) }()
		return func() {
			stop()
			server.Close()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	}
	ready := func(runtime *dockerFleet, state string) {
		t.Helper()
		for {
			snapshots, err := runtime.snapshot(ctx)
			if err == nil && snapshots[0].Err == nil && snapshots[0].Containers[testWorkerJob] == state {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("worker state %q not observed: %+v (%v)", state, snapshots, err)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}

	stop := serve(fleet)
	ready(fleet, "")
	if err := fleet.startOn(ctx, testWorkerHost, testWorkerJob, "test-jit"); err != nil {
		t.Error(err)
		stop()
		t.FailNow()
	}
	stop()

	restarted := workerTestManager(t, q)
	report, err := restarted.snapshot(ctx)
	if err != nil || report[0].Containers != nil {
		t.Fatalf("restarted manager reused unconfirmed inventory: %+v (%v)", report, err)
	}
	worker.session = rand.Text()
	stop = serve(restarted)
	defer stop()
	ready(restarted, "running")
	operator, err := newDockerFleet(testWorkerImage, testWorkerHost)
	if err != nil {
		t.Fatal(err)
	}
	ready(operator, "running")

	if err := restarted.removeOn(ctx, testWorkerHost, "runquill-56-0123456789ab"); err != nil {
		t.Fatal(err)
	}
	ready(restarted, "running")
	if err := restarted.removeOn(ctx, testWorkerHost, testWorkerJob); err != nil {
		t.Fatal(err)
	}
	ready(operator, "")
	log, err := os.ReadFile(filepath.Join(dir, "commands"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "create --name ") != 1 || strings.Count(string(log), "rm -f -v ") != 1 {
		t.Fatalf("unexpected Docker mutations: %s", log)
	}
}

func TestWorkerClaimIsRetainedAfterCanceledDispatch(t *testing.T) {
	fleet, _ := workerTestFleet(t)
	f := fleet.workers
	server := httptest.NewServer(f.route(http.NotFoundHandler()))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h := f.hosts[testWorkerName]
	session := rand.Text()
	h.mu.Lock()
	h.seen, h.session = time.Now(), session
	h.snapshot = workerObservation{Image: testWorkerImage, ImageReady: true, Containers: map[string]string{}}
	h.mu.Unlock()

	report := workerReport{Engine: "worker-engine", Session: session, Observation: h.snapshot}
	poll := func(ctx context.Context, token string, report workerReport) *http.Response {
		t.Helper()
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			server.URL+"/worker/v1/"+testWorkerName+"/poll", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	assertStatus := func(token string, report workerReport, code int) {
		t.Helper()
		res := poll(ctx, token, report)
		res.Body.Close()
		if res.StatusCode != code {
			t.Fatalf("wanted HTTP %d, got %d", code, res.StatusCode)
		}
	}
	assertStatus(strings.Repeat("a", 64), report, http.StatusUnauthorized)
	wrong := report
	wrong.Engine = "other-engine"
	assertStatus(testWorkerToken, wrong, http.StatusForbidden)
	wrong = report
	wrong.Session = rand.Text()
	assertStatus(testWorkerToken, wrong, http.StatusConflict)

	start, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	result := make(chan error, 1)
	go func() { result <- fleet.startOn(start, testWorkerHost, testWorkerJob, "test-jit") }()
	response := poll(ctx, testWorkerToken, report)
	var task workerTask
	err := json.NewDecoder(response.Body).Decode(&task)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || task.Name != testWorkerJob ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("dispatch failed with HTTP %d: %v", response.StatusCode, err)
	}
	cancelStart()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected canceled dispatch result: %v", err)
	}
	snapshot, err := fleet.snapshot(ctx)
	if err != nil || snapshot[0].Containers != nil {
		t.Fatalf("unconfirmed runner exposed schedulable capacity: %+v (%v)", snapshot, err)
	}
	report.Result = &workerResult{ID: task.ID, OK: true}
	report.Observation.Containers = map[string]string{testWorkerJob: "running"}
	assertStatus(testWorkerToken, report, http.StatusNoContent)
	snapshot, err = fleet.snapshot(ctx)
	if err != nil || snapshot[0].Containers[testWorkerJob] != "running" {
		t.Fatalf("late acknowledgement lost runner observation: %+v (%v)", snapshot, err)
	}
}

func TestOutboundWorkerCarriesPoolRuntimeOptions(t *testing.T) {
	fleet, _ := workerTestFleet(t)
	dir := workerTestDocker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	fleet.hosts[0].unprivileged, fleet.hosts[0].cache = true, true
	if err := fleet.startOn(ctx, testWorkerHost, testWorkerJob, "test-jit"); err == nil {
		t.Fatal("dispatched a cached runner without a repository")
	}
	server := httptest.NewServer(fleet.workers.route(http.NotFoundHandler()))
	defer server.Close()
	worker := &workerClient{token: testWorkerToken, engine: "worker-engine", session: rand.Text(),
		endpoint: server.URL + "/worker/v1/" + testWorkerName + "/poll",
		target:   docker{image: testWorkerImage, worker: testWorkerName}, client: &http.Client{Timeout: 6 * time.Second}}
	active, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.run(active) }()
	for snapshots, err := fleet.snapshot(ctx); err != nil || snapshots[0].Err != nil || snapshots[0].Containers == nil; snapshots, err = fleet.snapshot(ctx) {
		select {
		case <-ctx.Done():
			t.Fatalf("worker did not connect: %+v (%v)", snapshots, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	err := fleet.startOn(withRunnerRepository(ctx, 42), testWorkerHost, testWorkerJob, "test-jit")
	stop()
	if runErr := <-done; err != nil || runErr != nil {
		t.Fatalf("start: %v, worker: %v", err, runErr)
	}
	log, err := os.ReadFile(filepath.Join(dir, "commands"))
	if err != nil || !strings.Contains(string(log), "--user 1001:1001") || strings.Contains(string(log), "--privileged") ||
		!strings.Contains(string(log), "type=volume,src=runquill-cache-42,dst=/home/runner/.cache") ||
		!strings.Contains(string(log), "type=volume,src=runquill-cache-42-tools-") ||
		!strings.Contains(string(log), "--env RUNNER_TOOL_CACHE=/home/runner/.cache/tools "+cacheFixtureImageID) {
		t.Fatalf("worker ignored pool runtime options: %s (%v)", log, err)
	}
	for _, task := range []workerTask{
		{ID: "a", Operation: "remove", Name: testWorkerJob, Unprivileged: true, Expires: time.Now()},
		{ID: "a", Operation: "remove", Name: testWorkerJob, CacheRepository: 42, Expires: time.Now()},
		{ID: "a", Operation: "start", Name: testWorkerJob, Image: testWorkerImage, JIT: "x", CacheRepository: -1, Expires: time.Now()},
	} {
		if validWorkerTask(task) {
			t.Errorf("accepted invalid worker task %+v", task)
		}
	}
}

func TestWorkerClientDoesNotReplayDockerMutation(t *testing.T) {
	dir := workerTestDocker(t)
	task := workerTask{ID: rand.Text(), Operation: "start", Name: testWorkerJob, Image: testWorkerImage,
		JIT: "test-jit", Expires: time.Now().Add(15 * time.Second)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(task)
	}))
	defer server.Close()
	c := &workerClient{endpoint: server.URL, token: testWorkerToken, engine: "worker-engine", session: rand.Text(),
		target: docker{image: testWorkerImage, worker: testWorkerName}, client: server.Client()}
	for range 2 {
		if err := c.exchange(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	log, err := os.ReadFile(filepath.Join(dir, "commands"))
	if err != nil || strings.Count(string(log), "create --name ") != 1 || c.last == nil || !c.last.OK {
		t.Fatalf("replayed or failed task: %s (%v)", log, err)
	}
}

func TestWorkerEndpointCredentialsAndTransport(t *testing.T) {
	for _, origin := range []string{"http://workers.example.com", "https://name:pass@example.com", "https://example.com/prefix",
		"https://example.com?token=secret", "http://localhost:8080"} {
		if _, err := workerEndpoint(origin, testWorkerName); err == nil {
			t.Errorf("accepted unsafe manager origin %q", origin)
		}
	}
	endpoint, err := workerEndpoint("http://127.0.0.1:8080", testWorkerName)
	if err != nil || endpoint != "http://127.0.0.1:8080/worker/v1/builder/poll" {
		t.Fatalf("invalid loopback worker target: %q (%v)", endpoint, err)
	}
	file := filepath.Join(t.TempDir(), "worker-token")
	if err := os.WriteFile(file, []byte(testWorkerToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	token, release, err := lockWorkerToken(file)
	if err != nil || token != testWorkerToken {
		t.Fatalf("cannot read private worker token: %v", err)
	}
	defer release()
	if _, closeOther, err := lockWorkerToken(file); err == nil {
		closeOther()
		t.Fatal("the same token file admitted two worker processes")
	}
}

func TestWorkerTrustsOnlyTheConfiguredProxy(t *testing.T) {
	t.Setenv("RUNQUILL_WORKER_PROXY_IP", "192.0.2.44")
	fleet, _ := workerTestFleet(t)
	req := httptest.NewRequest(http.MethodPost, "http://manager.example.com/worker/v1/builder/poll", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.RemoteAddr = "192.0.2.45:43123"
	if fleet.workers.secureRequest(req) {
		t.Fatal("untrusted client spoofed the proxy scheme")
	}
	req.RemoteAddr = "192.0.2.44:43123"
	if !fleet.workers.secureRequest(req) {
		t.Fatal("configured TLS proxy rejected")
	}
	req.Header.Del("X-Forwarded-Proto")
	if fleet.workers.secureRequest(req) {
		t.Fatal("proxy delivered a request without TLS")
	}
}

func TestWorkerRemovalVerifiesLabelsAndContainerID(t *testing.T) {
	dir := workerTestDocker(t)
	t.Setenv("WORKER_TEST_OWNER", "different-worker")
	if err := os.WriteFile(filepath.Join(dir, "runner"), []byte(testWorkerJob), 0600); err != nil {
		t.Fatal(err)
	}
	target := docker{worker: testWorkerName}
	if err := target.removeWorkerRunner(t.Context(), testWorkerJob); err == nil {
		t.Fatal("worker removed a runner assigned to a different worker")
	}
	log, err := os.ReadFile(filepath.Join(dir, "commands"))
	if err != nil || strings.Contains(string(log), "rm -f") {
		t.Fatalf("unexpected container removal: %s (%v)", log, err)
	}
}
