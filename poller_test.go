package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
	webhook(q, "owner", secret).ServeHTTP(res, req)
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
