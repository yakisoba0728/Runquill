package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testGitHub(t *testing.T, handler http.HandlerFunc) *github {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	return &github{base: api.URL, client: api.Client(), key: key, appID: "123", owner: "owner"}
}

func testQueue(t *testing.T) *queue {
	t.Helper()
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	return q
}

func recordTestJob(t *testing.T, q *queue, id int64, route string, now time.Time) job {
	t.Helper()
	j := job{id: id, repository: "owner/repo", repositoryID: 2, installationID: 1, route: route}
	if _, err := q.record(t.Context(), j, now); err != nil {
		t.Fatal(err)
	}
	return j
}

func readJob(t *testing.T, q *queue, id int64) job {
	t.Helper()
	jobs, err := q.jobs(t.Context(), "WHERE job_id = ?", id)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("read job %d: %+v, %v", id, jobs, err)
	}
	return jobs[0]
}

func TestSchedulerRetryHonoursGitHubRetryAt(t *testing.T) {
	q := testQueue(t)
	now := time.Unix(1_800_000_000, 0)
	j := recordTestJob(t, q, 1, "runquill-1-1-ci", now)
	s := &scheduler{queue: q}
	for _, test := range []struct {
		cause error
		want  time.Time
	}{
		{errors.New("docker unavailable"), now.Add(retryDelay)},
		{fmt.Errorf("provision: %w", &githubError{status: 403, retryAt: now.Add(10 * time.Minute)}), now.Add(10 * time.Minute)},
		{&githubError{status: 429, retryAt: now.Add(time.Second)}, now.Add(retryDelay)},
	} {
		if err := s.retry(t.Context(), j, now, "provision runner", test.cause); err != nil {
			t.Fatal(err)
		}
		got := readJob(t, q, j.id)
		if got.retryAt != test.want.Unix() || got.phase != queued || got.failure != "" ||
			got.retryReason != retryMessage("provision runner", test.cause) {
			t.Errorf("retry after %v: %+v, want retry at %d", test.cause, got, test.want.Unix())
		}
	}
}

func TestGitHubRemoveFindsRunnerByNameWithoutID(t *testing.T) {
	var deleted []string
	var mu sync.Mutex
	const name = "runquill-5-0123456789ab"
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo/actions/runners":
			if r.URL.Query().Get("name") == name {
				fmt.Fprintf(w, `{"runners":[{"id":3,"name":"runquill-5-other"},{"id":77,"name":%q}]}`, name)
				return
			}
			fmt.Fprint(w, `{"runners":[]}`)
		default:
			if r.Method == http.MethodDelete {
				mu.Lock()
				deleted = append(deleted, r.URL.Path)
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
				return
			}
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	j := job{id: 5, repository: "owner/repo", repositoryID: 2, installationID: 1, runnerName: name}
	if err := gh.remove(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	j.runnerName = "runquill-5-ffffffffffff"
	if err := gh.remove(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "/repos/owner/repo/actions/runners/77" {
		t.Fatalf("deleted %v, want only the runner named %s", deleted, name)
	}
}

type failingStartRuntime struct {
	*testRuntime
	err error
}

func (r *failingStartRuntime) startOn(ctx context.Context, host, name, jit string) error {
	if r.err != nil {
		return r.err
	}
	return r.testRuntime.startOn(ctx, host, name, jit)
}

func TestSchedulerReleasesRunnerWhenStartFailsAfterProvisioning(t *testing.T) {
	var deleted atomic.Int32
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo":
			fmt.Fprint(w, `{"id":2,"private":true,"owner":{"login":"owner"}}`)
		case "GET /repos/owner/repo/actions/jobs/9":
			fmt.Fprint(w, `{"status":"queued"}`)
		case "POST /repos/owner/repo/actions/runners/generate-jitconfig":
			fmt.Fprint(w, `{"runner":{"id":55},"encoded_jit_config":"test-config"}`)
		case "DELETE /repos/owner/repo/actions/runners/55":
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	q := testQueue(t)
	now := time.Now()
	recordTestJob(t, q, 9, "runquill-1-1-ci", now)
	runtime := &failingStartRuntime{&testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}},
		errors.New("docker create failed")}
	s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 1}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	leased := readJob(t, q, 9)
	if leased.runnerName == "" || leased.runnerID != 55 || leased.phase != queued ||
		leased.retryAt != now.Add(retryDelay).Unix() || !strings.Contains(leased.retryReason, "docker create failed") {
		t.Fatalf("failed start must keep the lease for cleanup and retry later: %+v", leased)
	}
	later := now.Add(retryDelay + time.Second)
	if err := s.tick(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	released := readJob(t, q, 9)
	if released.runnerName != "" || released.runnerID != 0 || deleted.Load() != 1 || released.phase != queued ||
		released.retryAt != later.Add(retryDelay).Unix() {
		t.Fatalf("missing container must release the lease and GitHub registration: %+v, deletes=%d", released, deleted.Load())
	}
	runtime.err = nil
	if err := s.tick(t.Context(), later.Add(retryDelay)); err != nil {
		t.Fatal(err)
	}
	if started := readJob(t, q, 9); len(runtime.started) != 1 || started.runnerName != runtime.started[0] || started.retryReason != "" {
		t.Fatalf("job was not relaunched after the start failure: %+v, started %v", started, runtime.started)
	}
	rows, err := readStatusJobs(t.Context(), q.DB)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read status after successful retry: %v, %v", rows, err)
	}
	row := rows[0]
	if row.jitRequestedMS == 0 || row.jitRegisteredMS < row.jitRequestedMS || row.runnerLaunchMS < row.jitRegisteredMS {
		t.Fatalf("successful retry did not record ordered registration and launch: %+v", row)
	}
}

func TestSchedulerRecordsJITRequestWhenRegistrationFails(t *testing.T) {
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo":
			fmt.Fprint(w, `{"id":2,"private":true,"owner":{"login":"owner"}}`)
		case "GET /repos/owner/repo/actions/jobs/9":
			fmt.Fprint(w, `{"status":"queued"}`)
		case "POST /repos/owner/repo/actions/runners/generate-jitconfig":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	q := testQueue(t)
	now := time.Now()
	recordTestJob(t, q, 9, "runquill-1-1-ci", now)
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}
	s := scheduler{queue: q, github: gh, runtime: runtime, limit: 1}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	rows, err := readStatusJobs(t.Context(), q.DB)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read status after failed registration: %v, %v", rows, err)
	}
	row := rows[0]
	if row.jitRequestedMS == 0 || row.jitRegisteredMS != 0 || row.runnerLaunchMS != 0 || len(runtime.started) != 0 {
		t.Fatalf("failed registration should record only the attempted stage: %+v", row)
	}
}

func TestSchedulerFailsJobsThatCannotRun(t *testing.T) {
	q := testQueue(t)
	now := time.Now()
	recordTestJob(t, q, 1, "runquill-1-1-pool__missing__build", now)
	recordTestJob(t, q, 2, "runquill-1-1-old", now.Add(-queueExpiry-time.Minute))
	if _, err := q.ExecContext(t.Context(), "UPDATE jobs SET retry_reason = 'provision runner: boom', retry_at = ? WHERE job_id = 2",
		now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	recordTestJob(t, q, 3, "runquill-1-1-waiting", now)
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Pool: defaultWorkerPool, Containers: map[string]string{}}}}
	// The default pool has no capacity so only selection and expiry run.
	s := &scheduler{queue: q, github: &github{}, runtime: runtime, limit: 1, hostLimits: map[string]int{"local": 1},
		pools: workerPools{defaultWorkerPool: {}}}
	runtime.hosts[0].Containers["busy"] = "running"
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 2); j.phase != completed || j.failure != "not started within 24 hours; last error: provision runner: boom" {
		t.Errorf("expired job: %+v", j)
	}
	if j := readJob(t, q, 3); j.phase != queued || j.failure != "" {
		t.Errorf("waiting job must stay queued: %+v", j)
	}
	delete(runtime.hosts[0].Containers, "busy")
	if _, err := q.ExecContext(t.Context(), "UPDATE jobs SET retry_at = ? WHERE job_id = 3", now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	// Fixing the pool settings must be able to start the job, so it waits instead of failing.
	if j := readJob(t, q, 1); j.phase != queued || j.failure != "" ||
		j.retryReason != `select worker pool: worker pool "missing" is not configured` {
		t.Errorf("unconfigured pool must keep waiting with a reason: %+v", j)
	}
	// A late webhook cannot reopen or clear a failed job.
	if _, err := q.record(t.Context(), job{id: 2, repository: "owner/repo", repositoryID: 2, installationID: 1,
		route: "runquill-1-1-old", phase: completed}, now); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 2); j.phase != completed || j.failure == "" {
		t.Errorf("webhook cleared the failure: %+v", j)
	}
}

func TestGitHubTokenCache(t *testing.T) {
	var mints, unauthorized atomic.Int32
	var lifetime atomic.Int64
	lifetime.Store(int64(time.Hour))
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			expires := time.Now().Add(time.Duration(lifetime.Load()))
			fmt.Fprintf(w, `{"token":"token-%d","expires_at":%q}`, mints.Add(1), expires.Format(time.RFC3339))
		case "GET /repos/owner/repo":
			if unauthorized.Load() > 0 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"id":2}`)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
		}
	})
	gh.ready = &readiness{}
	j := job{id: 1, repository: "owner/repo", repositoryID: 2, installationID: 1}
	for range 3 {
		if token, err := gh.token(t.Context(), j); err != nil || token != "token-1" {
			t.Fatalf("token %q, %v", token, err)
		}
	}
	other := j
	other.repositoryID = 3
	if token, err := gh.token(t.Context(), other); err != nil || token != "token-2" || mints.Load() != 2 {
		t.Fatalf("tokens for different repositories must not be shared: %q, %v", token, err)
	}
	unauthorized.Store(1)
	if _, _, err := gh.eligible(t.Context(), j, "token-1"); err == nil {
		t.Fatal("expected 401")
	}
	if token, err := gh.token(t.Context(), j); err != nil || token != "token-3" {
		t.Fatalf("a rejected token must be replaced: %q, %v", token, err)
	}
	lifetime.Store(int64(tokenRefreshMargin - time.Second))
	gh.tokens = nil
	for range 2 {
		if _, err := gh.token(t.Context(), j); err != nil {
			t.Fatal(err)
		}
	}
	if mints.Load() != 5 {
		t.Fatalf("tokens near expiry must be refreshed: %d mints", mints.Load())
	}
	lifetime.Store(int64(time.Hour))
	gh.repositoryScope = scopeSelected
	for range 2 {
		if _, err := gh.token(t.Context(), j); err != nil {
			t.Fatal(err)
		}
	}
	if mints.Load() != 7 {
		t.Fatalf("selected installations must mint to notice removed repositories: %d mints", mints.Load())
	}
	if report := gh.ready.report(time.Now()); report.GitHubAuth != "ok" {
		t.Fatalf("successful token mint not recorded: %+v", report)
	}
}

func TestSchedulerStopsCallingGitHubAfterRateLimit(t *testing.T) {
	reset := time.Now().Add(10 * time.Minute).Unix()
	var calls atomic.Int32
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprintf(w, `{"token":"stub","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "GET /repos/owner/repo":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
		}
	})
	gh.ready = &readiness{}
	q := testQueue(t)
	now := time.Now()
	for id := range int64(3) {
		recordTestJob(t, q, id+1, "runquill-1-1-ci", now)
	}
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}
	s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 3}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("scheduler kept calling GitHub after a rate limit: %d calls", calls.Load())
	}
	if j := readJob(t, q, 1); j.retryAt != reset+1 {
		t.Errorf("limited job retry at %d, want %d", j.retryAt, reset+1)
	}
	for _, id := range []int64{2, 3} {
		if j := readJob(t, q, id); j.retryAt != 0 || j.retryReason != "" {
			t.Errorf("job %d must wait untouched: %+v", id, j)
		}
	}
	err := gh.request(withRateGate(t.Context()), http.MethodGet, "/repos/owner/repo", "stub", nil, nil)
	var apiErr *githubError
	if !errors.As(err, &apiErr) || !apiErr.deferred || apiErr.retryAt.Unix() != reset+1 || calls.Load() != 2 {
		t.Fatalf("scheduler request during backoff reached GitHub or lost its retry time: %v", err)
	}
	// Webhook verification and the poller keep their own handling of GitHub's answer.
	if err := gh.request(t.Context(), http.MethodGet, "/repos/owner/repo", "stub", nil, nil); !errors.As(err, &apiErr) ||
		apiErr.deferred || calls.Load() != 3 {
		t.Fatalf("ungated request was deferred: %v", err)
	}
	if report := gh.ready.report(time.Now()); !report.Ready || report.Scheduler != "ok" {
		t.Fatalf("a rate-limited tick is still a successful tick: %+v", report)
	}
}

func TestSchedulerFailsJobsOfIneligibleRepositories(t *testing.T) {
	for _, test := range []struct {
		organization bool
		repo, reason string
	}{
		{false, `{"id":2,"private":false,"owner":{"login":"owner"}}`, "repository is public"},
		{false, `{"id":2,"private":true,"archived":true,"owner":{"login":"owner"}}`, "repository is archived"},
		{false, `{"id":2,"private":true,"disabled":true,"owner":{"login":"owner"}}`, "repository is disabled"},
		{false, `{"id":3,"private":true,"owner":{"login":"owner"}}`, "GitHub repository ID is 3, not 2"},
		{true, `{"id":2,"private":true,"visibility":"internal","owner":{"login":"owner"}}`, "repository is internal"},
	} {
		gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method + " " + r.URL.Path {
			case "GET /repos/owner/repo/installation":
				fmt.Fprint(w, `{"id":1,"account":{"login":"owner","type":"Organization"},"repository_selection":"all"}`)
			case "POST /app/installations/1/access_tokens":
				fmt.Fprint(w, `{"token":"stub"}`)
			case "GET /repos/owner/repo":
				fmt.Fprint(w, test.repo)
			default:
				t.Errorf("unexpected API request %s %s", r.Method, r.URL)
				w.WriteHeader(http.StatusNotFound)
			}
		})
		gh.organization = test.organization
		q := testQueue(t)
		now := time.Now()
		recordTestJob(t, q, 9, "runquill-1-1-ci", now)
		runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}
		s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 1}
		if err := s.tick(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		// The panel must show why the job never ran instead of a plain completion.
		want := "repository is no longer eligible: " + test.reason
		if j := readJob(t, q, 9); j.phase != completed || j.failure != want || j.runnerName != "" || len(runtime.started) != 0 {
			t.Errorf("ineligible repository %s: %+v, want failure %q", test.repo, j, want)
		}
	}
}
