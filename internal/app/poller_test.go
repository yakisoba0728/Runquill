package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollerRejectsInvalidPagesBeforeCaching(t *testing.T) {
	const path = "/repos/owner/repo/actions/runs"
	const valid = `{"workflow_runs":[]}`
	for _, tt := range []struct {
		name, body string
	}{
		{"trailing JSON", valid + " {}"},
		{"oversize page", valid + strings.Repeat(" ", maxPollPageBytes+1-len(valid))},
		{"null list", `{"workflow_runs":null}`},
		{"malformed list entry", `{"workflow_runs":[{"id":"not an integer"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"invalid"`)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			p := &poller{github: &github{base: server.URL, client: server.Client()}, cache: make(map[string]pollPage)}
			if _, err := polledPages[workflowRun](t.Context(), p, path, "workflow_runs"); err == nil {
				t.Fatal("expected invalid page to be rejected")
			}
			if _, cached := p.cache[path]; cached {
				t.Fatal("invalid page was cached")
			}
		})
	}
}

func TestPollerFindsQueuedJobsInActiveRunsAndDeduplicatesWebhooks(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)

	var activeRunHasQueuedJob atomic.Bool
	var conditionalRequests, mintedTokens atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/owner/installation":
			fmt.Fprint(w, `{"id":7,"account":{"login":"owner","type":"User"}}`)
		case "/app/installations/7/access_tokens":
			mintedTokens.Add(1)
			var out struct{ Permissions map[string]string }
			if err := json.NewDecoder(r.Body).Decode(&out); err != nil ||
				len(out.Permissions) != 1 || out.Permissions["actions"] != "read" {
				t.Error("discovery token must request Actions read only")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"token":"test-poll-token","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "/installation/repositories":
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `{"repositories":[{"id":12,"full_name":"Owner/Repo","private":true,"owner":{"login":"owner"}}]}`)
				return
			}
			w.Header().Set("Link", `<`+server.URL+`/installation/repositories?per_page=100&page=2>; rel="next"`)
			fmt.Fprint(w, `{"repositories":[{"id":10,"full_name":"owner/public","private":false,"owner":{"login":"owner"}},
				{"id":11,"full_name":"other/private","private":true,"owner":{"login":"other"}}]}`)
		case "/repos/owner/repo/actions/runs":
			if r.URL.Query().Get("status") != "in_progress" {
				fmt.Fprint(w, `{"workflow_runs":[]}`)
				return
			}
			if r.Header.Get("If-None-Match") == `"same-run"` {
				conditionalRequests.Add(1)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"same-run"`)
			fmt.Fprint(w, `{"workflow_runs":[{"id":90}]}`)
		case "/repos/owner/repo/actions/runs/90/jobs":
			if r.URL.Query().Get("filter") != "latest" {
				t.Error("polling must use the current run attempt")
			}
			if !activeRunHasQueuedJob.Load() {
				fmt.Fprint(w, `{"jobs":[]}`)
				return
			}
			fmt.Fprint(w, `{"jobs":[
				{"id":42,"run_id":90,"labels":["runquill-90-1-build"],"status":"queued"},
				{"id":43,"run_id":90,"labels":["ubuntu-latest"],"status":"queued"},
				{"id":44,"run_id":90,"labels":["runquill-90-1-other"],"status":"in_progress"}]}`)
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	api := &github{client: server.Client(), base: server.URL, appID: "9", key: key, owner: "owner"}
	p := &poller{github: api, queue: q, cache: make(map[string]pollPage)}
	if err := p.sweep(t.Context()); err != nil {
		t.Fatal(err)
	}

	activeRunHasQueuedJob.Store(true)
	body := `{"action":"queued","installation":{"id":7},
		"repository":{"id":12,"full_name":"owner/repo","private":true,"owner":{"login":"owner"}},
		"workflow_job":{"id":42,"run_id":90,"labels":["runquill-90-1-build"]}}`
	secret := []byte("hook-secret")
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	res := httptest.NewRecorder()
	pollResult := make(chan error, 1)
	go func() { pollResult <- p.sweep(t.Context()) }()
	webhook(q, api, secret, nil).ServeHTTP(res, req)
	if err := <-pollResult; err != nil || res.Code != http.StatusAccepted {
		t.Fatalf("poll error: %v, webhook status: %d", err, res.Code)
	}
	jobs, err := q.jobs(t.Context(), "ORDER BY sequence")
	if err != nil || len(jobs) != 1 || jobs[0].id != 42 || jobs[0].phase != queued {
		t.Fatalf("unexpected jobs: %+v, error %v", jobs, err)
	}
	if conditionalRequests.Load() != 1 || mintedTokens.Load() != 1 {
		t.Fatalf("conditional responses=%d, minted tokens=%d", conditionalRequests.Load(), mintedTokens.Load())
	}
}

func TestPollerFindsNewReposAndRotatesFullScansAcrossRestarts(t *testing.T) {
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	var includeNew atomic.Bool
	var fullChecks [6]atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch path {
		case "/installation/repositories":
			count := 4
			if includeNew.Load() {
				count++
			}
			repos := make([]repository, count)
			for i := range repos {
				repos[i] = repository{ID: int64(i + 1), FullName: fmt.Sprintf("owner/repo%d", i+1), Private: true}
				repos[i].Owner.Login = "owner"
			}
			json.NewEncoder(w).Encode(map[string][]repository{"repositories": repos})
		case "/repos/owner/repo4/actions/runs/40/jobs":
			fmt.Fprint(w, `{"jobs":[{"id":400,"run_id":40,"status":"queued","labels":["runquill-40-1-test"]}]}`)
		case "/repos/owner/repo5/actions/runs/50/jobs":
			fmt.Fprint(w, `{"jobs":[{"id":500,"run_id":50,"status":"queued","labels":["runquill-50-1-test"]}]}`)
		default:
			if !strings.HasSuffix(path, "/actions/runs") {
				http.NotFound(w, r)
				return
			}
			var repoID int
			if _, err := fmt.Sscanf(path, "/repos/owner/repo%d/actions/runs", &repoID); err != nil || repoID < 1 || repoID > 5 {
				t.Errorf("unexpected repository path %q", path)
				http.NotFound(w, r)
				return
			}
			switch r.URL.Query().Get("status") {
			case "pending":
				fullChecks[repoID].Add(1)
				if repoID == 4 {
					fmt.Fprint(w, `{"workflow_runs":[{"id":40}]}`)
					return
				}
			case "queued":
				if repoID == 5 {
					fmt.Fprint(w, `{"workflow_runs":[{"id":50}]}`)
					return
				}
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		}
	}))
	t.Cleanup(server.Close)
	for sweep := range 4 {
		// Recreate the poller to ensure a manager restart doesn't reset the rotation.
		p := &poller{github: &github{client: server.Client(), base: server.URL, owner: "owner"},
			queue: q, installationID: 1, cache: make(map[string]pollPage),
			token: installationToken{Token: "fixture", ExpiresAt: time.Now().Add(time.Hour)},
		}
		if sweep == 1 {
			includeNew.Store(true)
		}
		if err := p.sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
		if sweep == 1 {
			rows, err := q.jobs(t.Context(), "WHERE job_id = ?", 500)
			if err != nil || len(rows) != 1 {
				t.Fatalf("new repository's queued run was missed in its first sweep: %v, %v", rows, err)
			}
		}
	}
	for id := 1; id <= 5; id++ {
		if fullChecks[id].Load() == 0 {
			t.Errorf("repository %d never received a full scan", id)
		}
	}
	rows, err := q.jobs(t.Context(), "WHERE job_id = ?", 400)
	if err != nil || len(rows) != 1 {
		t.Fatalf("quiet repository's pending run was missed: %v, %v", rows, err)
	}
}

func TestPollerStopsOnRateLimitAndRecordsFailedScan(t *testing.T) {
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/installation/repositories":
			fmt.Fprint(w, `{"repositories":[
				{"id":1,"full_name":"owner/first","private":true,"owner":{"login":"owner"}},
				{"id":2,"full_name":"owner/second","private":true,"owner":{"login":"owner"}}]}`)
		case "/repos/owner/first/actions/runs":
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			t.Errorf("GitHub call after a rate-limit response: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	p := &poller{github: &github{client: server.Client(), base: server.URL, owner: "owner"},
		queue: q, installationID: 1, cache: make(map[string]pollPage),
		token: installationToken{Token: "fixture", ExpiresAt: time.Now().Add(time.Hour)},
	}
	var apiErr *githubError
	if err := p.sweep(t.Context()); !errors.As(err, &apiErr) || time.Until(apiErr.retryAt) < time.Second {
		t.Fatalf("expected a rate limit with a retry time, got %v", err)
	}
	results, err := readRepositoryPolls(t.Context(), q.DB, 1)
	if err != nil || len(results) != 1 || !strings.Contains(results[1].lastError, "HTTP 429") {
		t.Fatalf("unexpected recorded poll results: %+v, %v", results, err)
	}
}
