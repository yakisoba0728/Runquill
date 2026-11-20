package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func enrollmentTestFleet(t *testing.T, fixture *panelFixture, hosts string) *dockerFleet {
	t.Helper()
	f, err := newDockerFleet(testWorkerImage, hosts)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.workers.attach(t.Context(), fixture.q.DB); err != nil {
		t.Fatal(err)
	}
	panel := newPanel(config{})
	panel.operations = &operations{}
	panel.operations.current.Store(&operationState{runners: runnerConfiguration{runtime: f}})
	f.workers.operations = panel.operations
	fixture.handler = panelHandler(panel, fixture.access)
	return f
}

func createTestWorkerEnrollment(t *testing.T, fixture *panelFixture, name string) (workerEnrollmentCommand, string) {
	t.Helper()
	rec := fixture.call(http.MethodPost, "/api/hosts/"+name+"/enrollment", "", "admin", true)
	fixture.expect(rec, http.StatusCreated)
	var result workerEnrollmentCommand
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	_, arguments, ok := strings.Cut(result.Command, " --code '")
	code, _, end := strings.Cut(arguments, "' && ")
	if !ok || !end || !validHex32(code) || !result.ExpiresAt.After(time.Now()) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("administrator did not receive a private, expiring enrollment command")
	}
	return result, code
}

func TestWorkerEnrollmentIsHostScopedAndSingleUse(t *testing.T) {
	t.Setenv("RUNQUILL_WORKER_AUTH_FILE", "")
	t.Setenv("RUNQUILL_WORKER_ENROLLMENT_URL", "https://workers.example.test")
	fixture := newPanelFixture(t)
	f := enrollmentTestFleet(t, fixture, "agent://builder,agent://other")
	f.hosts[0].image = "pool/runner:tag"
	observed, err := f.snapshot(t.Context())
	if err != nil || len(observed) != 2 || !observed[0].EnrollmentAvailable || observed[0].Containers != nil {
		t.Fatal("new worker slot is not awaiting authenticated enrollment", err)
	}
	capacity, _ := fleetCapacity(observed, nil, nil, nil, 2)
	if capacity[0].Occupied != 0 || capacity[1].Occupied != 0 {
		t.Fatal("unregistered agents took scheduler capacity")
	}
	const path = "/api/hosts/builder/enrollment"
	fixture.expect(fixture.call(http.MethodPost, path, "", "", true), http.StatusUnauthorized)
	fixture.expect(fixture.call(http.MethodPost, path, "", "operator", true), http.StatusForbidden)
	fixture.expect(fixture.call(http.MethodPost, path, "", "admin", false), http.StatusForbidden)
	fixture.expect(fixture.call(http.MethodPost, "/api/hosts/unknown/enrollment", "", "admin", true), http.StatusBadRequest)
	if _, err := fixture.q.Exec("INSERT INTO docker_hosts (host, untracked_slots) VALUES ('agent://builder', 1)"); err != nil {
		t.Fatal(err)
	}
	fixture.expect(fixture.call(http.MethodPost, path, "", "admin", true), http.StatusConflict)
	if _, err := fixture.q.Exec("DELETE FROM docker_hosts WHERE host = 'agent://builder'"); err != nil {
		t.Fatal(err)
	}

	handler := f.workers.route(http.NotFoundHandler())
	digest := workerDigest(testWorkerToken)
	post := func(host, code, engine, tokenDigest, peer string) int {
		body, _ := json.Marshal(workerAuthority{Engine: engine, Digest: tokenDigest})
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/worker/v1/"+host+"/enroll", bytes.NewReader(body))
		req.RemoteAddr = peer + ":1234"
		req.Header.Set("Authorization", "Bearer "+code)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	const loopback = "127.0.0.1"
	check := func(want int, host, code, engine, tokenDigest, peer string) {
		t.Helper()
		if status := post(host, code, engine, tokenDigest, peer); status != want {
			t.Fatalf("enroll %s: HTTP %d, want %d", host, status, want)
		}
	}
	_, expired := createTestWorkerEnrollment(t, fixture, "builder")
	if _, err := fixture.q.Exec("UPDATE worker_enrollment_codes SET expires_at = 1"); err != nil {
		t.Fatal(err)
	}
	check(http.StatusUnauthorized, "builder", expired, "engine", digest, loopback)
	_, previous := createTestWorkerEnrollment(t, fixture, "builder")
	result, code := createTestWorkerEnrollment(t, fixture, "builder")
	if !strings.Contains(result.StartCommand, "RUNQUILL_RUNNER_IMAGE='pool/runner:tag'") || strings.Contains(result.StartCommand, code) {
		t.Fatal("start command lacks the pool image or contains the enrollment code")
	}
	check(http.StatusForbidden, "builder", code, "engine", digest, "192.0.2.1")
	check(http.StatusUnauthorized, "builder", previous, "engine", digest, loopback)
	check(http.StatusUnauthorized, "other", code, "engine", digest, loopback)
	var savedCode string
	if err := fixture.q.QueryRow("SELECT code_digest FROM worker_enrollment_codes WHERE host = 'builder'").Scan(&savedCode); err != nil || savedCode != workerDigest(code) {
		t.Fatal("the manager did not persist a digest of the code", err)
	}

	results := make(chan int, 2)
	for range 2 {
		go func() { results <- post("builder", code, "engine", digest, loopback) }()
	}
	first, second := <-results, <-results
	if first > second {
		first, second = second, first
	}
	if first != http.StatusNoContent || second != http.StatusUnauthorized {
		t.Fatalf("concurrent enrollments answered %d, %d", first, second)
	}
	fixture.expect(fixture.call(http.MethodPost, path, "", "admin", true), http.StatusConflict)
	_, otherCode := createTestWorkerEnrollment(t, fixture, "other")
	check(http.StatusConflict, "other", otherCode, "engine", workerDigest(workerSecret()), loopback)
	check(http.StatusConflict, "other", otherCode, "other-engine", digest, loopback)
	check(http.StatusNoContent, "other", otherCode, "other-engine", workerDigest(workerSecret()), loopback)
}

func TestWorkerEnrollmentRejectsAnEngineUsedByTheManager(t *testing.T) {
	workerTestDocker(t)
	t.Setenv("RUNQUILL_WORKER_AUTH_FILE", "")
	t.Setenv("RUNQUILL_WORKER_ENROLLMENT_URL", "https://workers.example.test")
	fixture := newPanelFixture(t)
	f := enrollmentTestFleet(t, fixture, "local,agent://builder")
	_, code := createTestWorkerEnrollment(t, fixture, "builder")
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/worker/v1/builder/enroll",
		strings.NewReader(fmt.Sprintf(`{"engine_id":"worker-engine","token_sha256":%q}`, workerDigest(testWorkerToken))))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+code)
	rec := httptest.NewRecorder()
	f.workers.route(http.NotFoundHandler()).ServeHTTP(rec, req)
	fixture.expect(rec, http.StatusConflict)
}

func TestWorkerEnrollmentSavesCredentialBeforeReplyAndRecoversOnRestart(t *testing.T) {
	workerTestDocker(t)
	t.Setenv("RUNQUILL_WORKER_AUTH_FILE", "")
	t.Setenv("RUNQUILL_WORKER_ENROLLMENT_URL", "https://workers.example.test")
	fixture := newPanelFixture(t)
	f := enrollmentTestFleet(t, fixture, testWorkerHost)
	_, code := createTestWorkerEnrollment(t, fixture, testWorkerName)
	worker := f.workers.route(http.NotFoundHandler())
	lostReply := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		worker.ServeHTTP(rec, r)
		if rec.Code != http.StatusNoContent {
			w.WriteHeader(rec.Code)
			return
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer lostReply.Close()
	filename := filepath.Join(t.TempDir(), "worker.token")
	err := workerCommand(t.Context(), []string{"enroll", "--name", testWorkerName, "--manager", lostReply.URL, "--token-file", filename, "--code", code})
	if err == nil || !strings.Contains(err.Error(), "saved token file") || strings.Contains(err.Error(), code) {
		t.Fatalf("lost reply did not explain how to recover safely: %v", err)
	}
	token, release, err := lockWorkerToken(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var saved string
	err = fixture.q.QueryRow("SELECT token_digest FROM worker_authorities WHERE host = ?", testWorkerName).Scan(&saved)
	if err != nil || saved != workerDigest(token) {
		t.Fatal("the worker token was not persisted safely on both sides", err)
	}
	t.Setenv("RUNQUILL_WORKER_ENROLLMENT_URL", "")
	restarted := enrollmentTestFleet(t, fixture, testWorkerHost)
	server := httptest.NewServer(restarted.workers.route(http.NotFoundHandler()))
	defer server.Close()
	client := workerClient{endpoint: server.URL + "/worker/v1/" + testWorkerName + "/poll",
		token: token, engine: "worker-engine", session: rand.Text(), target: docker{worker: testWorkerName, image: testWorkerImage}, client: server.Client()}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.exchange(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	deadline := time.After(3 * time.Second)
	for {
		snapshot, err := restarted.snapshot(t.Context())
		if err == nil && snapshot[0].Containers != nil && snapshot[0].Err == nil && !snapshot[0].EnrollmentAvailable {
			return
		}
		select {
		case <-deadline:
			t.Fatal("worker did not connect to restarted manager")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestWorkerWithoutCredentialsRequiresEnrollmentConfiguration(t *testing.T) {
	t.Setenv("RUNQUILL_WORKER_AUTH_FILE", "")
	t.Setenv("RUNQUILL_WORKER_ENROLLMENT_URL", "")
	f, err := newDockerFleet(testWorkerImage, testWorkerHost)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newPanelFixture(t)
	if err := f.workers.attach(t.Context(), fixture.q.DB); err == nil {
		t.Fatal("started an unauthenticated worker fleet with enrollment disabled")
	}
}

func TestManualWorkerCredentialsReplaceManagedEnrollment(t *testing.T) {
	fixture := newPanelFixture(t)
	managed := workerDigest(testWorkerToken)
	manual := workerDigest(workerSecret())
	if _, err := fixture.q.Exec("INSERT INTO worker_authorities VALUES (?, ?, ?)", testWorkerName, "engine", managed); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(t.TempDir(), "workers.json")
	if err := os.WriteFile(authFile, fmt.Appendf(nil, `{"builder":{"engine_id":"engine","token_sha256":%q}}`, manual), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNQUILL_WORKER_AUTH_FILE", authFile)
	t.Setenv("RUNQUILL_WORKER_ENROLLMENT_URL", "")
	f := enrollmentTestFleet(t, fixture, testWorkerHost)
	authority := f.workers.hosts[testWorkerName].authority.Load()
	if authority == nil || authority.Digest != manual {
		t.Fatal("the manual credentials did not replace managed enrollment")
	}
	var managedRows int
	if err := fixture.q.QueryRow("SELECT count(*) FROM worker_authorities").Scan(&managedRows); err != nil || managedRows != 0 {
		t.Fatal("revoked credentials could become active if the manual file were removed", err)
	}
}

func TestPrepareWorkerTokenKeepsExistingCredential(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "worker.token")
	if err := os.WriteFile(filename, []byte(testWorkerToken), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkerToken(filename); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(filename)
	if err != nil || string(value) != testWorkerToken {
		t.Fatal("worker enrollment replaced an existing token file", err)
	}
}

func TestOutboundWorkerEnrollmentOnDocker(t *testing.T) {
	image := os.Getenv("RUNQUILL_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set RUNQUILL_TEST_DOCKER_IMAGE to a locally built runner image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	now := time.Now()
	name := fmt.Sprintf("enrollment-%d", now.UnixNano())
	host := workerHostPrefix + name
	tag := "runquill-worker-smoke:" + name
	buildDockerTestRunner(t, ctx, image, tag, `#!/bin/bash
set -euo pipefail
test "$(id -u)" = 1001
test "$1" = --jitconfig
test "$2" = worker-fixture-jit
test ! -e "$RUNQUILL_JIT_FILE"
test -w "$RUNNER_TOOL_CACHE"
echo outbound-worker-job-ok
`)
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	t.Setenv("RUNQUILL_RUNNER_IMAGE", tag)
	t.Setenv("RUNQUILL_WORKER_AUTH_FILE", "")
	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	t.Setenv("RUNQUILL_WORKER_ENROLLMENT_URL", "http://"+server.Listener.Addr().String())
	fixture := newPanelFixture(t)
	q := fixture.q
	if err := migrateUsage(ctx, q.DB); err != nil {
		t.Fatal(err)
	}
	fleet := enrollmentTestFleet(t, fixture, host)
	fleet.hosts[0].image = tag
	pools, err := fleet.configurePools(`{"default":{"privileged":false,"cache":true}}`)
	if err != nil {
		t.Fatal(err)
	}

	const jobID, runnerID = 7001, 42
	repositoryID := 6_000_000_000 + now.UnixNano()%1_000_000_000
	target := docker{image: tag}
	cache := fmt.Sprintf("runquill-cache-%d", repositoryID)
	_, tools, err := target.toolCache(ctx, cache)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if jobs, err := q.jobs(cleanup, "WHERE job_id = ?", jobID); err == nil && len(jobs) == 1 && jobs[0].runnerName != "" {
			target.remove(cleanup, jobs[0].runnerName)
		}
		dockerCommand(cleanup, nil, "volume", "rm", tools, cache)
	})

	var deleted atomic.Bool
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"fixture-token"}`)
		case "GET /repos/owner/repo":
			fmt.Fprintf(w, `{"id":%d,"private":true,"owner":{"login":"owner"}}`, repositoryID)
		case "GET /repos/owner/repo/actions/jobs/7001":
			fmt.Fprint(w, `{"status":"queued"}`)
		case "POST /repos/owner/repo/actions/runners/generate-jitconfig":
			fmt.Fprintf(w, `{"runner":{"id":%d},"encoded_jit_config":"worker-fixture-jit"}`, runnerID)
		case "DELETE /repos/owner/repo/actions/runners/42":
			deleted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected fixture API request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	const webhookSecret = "worker-fixture-webhook"
	server.Config.Handler = fleet.workers.route(webhook(q, gh, []byte(webhookSecret), nil))
	server.Start()
	_, code := createTestWorkerEnrollment(t, fixture, name)
	args := []string{"--name", name, "--manager", server.URL, "--token-file", filepath.Join(t.TempDir(), "worker.token")}
	if err := workerCommand(ctx, append([]string{"enroll", "--code", code}, args...)); err != nil {
		t.Fatal(err)
	}
	active, stopWorker := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- workerCommand(active, args) }()
	defer func() {
		stopWorker()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()

	waitFor := func(runner, state string) {
		t.Helper()
		for {
			snapshots, err := fleet.snapshot(ctx)
			if err == nil && snapshots[0].Containers != nil && snapshots[0].Err == nil && snapshots[0].Containers[runner] == state {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("worker did not report %q in state %q: %+v (%v)", runner, state, snapshots, err)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	deliver := func(action string) {
		t.Helper()
		payload := fmt.Appendf(nil, `{"action":%q,"installation":{"id":1},
            "repository":{"id":%d,"full_name":"owner/repo","private":true,"owner":{"login":"owner"}},
            "workflow_job":{"id":%d,"run_id":17,"labels":["runquill-17-1-worker-smoke"]}}`, action, repositoryID, jobID)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/webhook", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, []byte(webhookSecret))
		mac.Write(payload)
		req.Header.Set("X-Hub-Signature-256", fmt.Sprintf("sha256=%x", mac.Sum(nil)))
		req.Header.Set("X-GitHub-Event", "workflow_job")
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusAccepted {
			t.Fatalf("%s delivery returned HTTP %d", action, res.StatusCode)
		}
	}

	waitFor("", "")
	t.Log("panel enrollment succeeded and the Docker worker reported ready")
	deliver("queued")
	s := &scheduler{queue: q, github: gh, runtime: fleet, pools: pools, limit: 1}
	if err := s.tick(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	job := readJob(t, q, jobID)
	if job.runnerHost != host || job.runnerID != runnerID || job.runnerName == "" || job.retryReason != "" {
		t.Fatalf("outbound assignment did not succeed: %+v", job)
	}
	exit, err := dockerCommand(ctx, nil, "wait", job.runnerName)
	logs, logErr := dockerCommand(ctx, nil, "logs", job.runnerName)
	if err != nil || strings.TrimSpace(exit) != "0" || logErr != nil || !strings.Contains(logs, "outbound-worker-job-ok") {
		t.Fatalf("worker job exit=%q (%v), logs=%q (%v)", exit, err, logs, logErr)
	}
	t.Log("the worker's runner executed the fixture job in Docker")
	waitFor(job.runnerName, "exited")
	deliver("completed")
	if err := s.tick(ctx, time.Now().Add(completionGrace)); err != nil {
		t.Fatal(err)
	}
	waitFor(job.runnerName, "")
	if completedJob := readJob(t, q, jobID); completedJob.phase != completed || completedJob.runnerName != "" || !deleted.Load() {
		t.Fatalf("worker runner or GitHub registration remained after completion: %+v", completedJob)
	}
	t.Log("the worker removed the runner container and the fixture registration was deleted")
}
