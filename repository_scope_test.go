package main

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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectedRepositoryLifecycle(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const repo = `{"id":9,"full_name":"owner/approved","private":true,"owner":{"login":"owner"}}`
	var allowed, broadened atomic.Bool
	var jobID atomic.Int64
	var pollTokens, runnerTokens, registered, removed atomic.Int32
	jobID.Store(41)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := scopeSelected
		if broadened.Load() {
			scope = scopeAll
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /users/owner/installation", "GET /repos/owner/approved/installation":
			if r.URL.Path != "/users/owner/installation" && !allowed.Load() {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"id":7,"account":{"login":"owner","type":"User"},"repository_selection":%q,"permissions":{"actions":"read","administration":"write"}}`, scope)
		case "POST /app/installations/7/access_tokens":
			var body struct {
				Permissions   map[string]string
				RepositoryIDs []int64 `json:"repository_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode token request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body.Permissions != nil {
				if body.Permissions["actions"] != "read" || len(body.Permissions) != 1 || len(body.RepositoryIDs) != 0 {
					t.Errorf("unexpected discovery token request: %+v", body)
				}
				pollTokens.Add(1)
				token := "before-approval"
				if allowed.Load() {
					token = "with-approval"
				}
				fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, time.Now().Add(time.Hour).Format(time.RFC3339))
				return
			}
			runnerTokens.Add(1)
			if !slices.Equal(body.RepositoryIDs, []int64{9}) {
				t.Errorf("runner token must be scoped to the approved repository: %+v", body)
			}
			if !allowed.Load() {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			fmt.Fprint(w, `{"token":"repo-only"}`)
		case "GET /installation/repositories":
			if r.Header.Get("Authorization") == "Bearer with-approval" {
				fmt.Fprintf(w, `{"repositories":[%s]}`, repo)
			} else {
				fmt.Fprint(w, `{"repositories":[]}`)
			}
		case "GET /repos/owner/approved/actions/runs":
			if r.Header.Get("Authorization") != "Bearer with-approval" || !allowed.Load() {
				t.Error("poll scanned a repository before it was approved")
			}
			if r.URL.Query().Get("status") == "queued" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":17}]}`)
			} else {
				fmt.Fprint(w, `{"workflow_runs":[]}`)
			}
		case "GET /repos/owner/approved/actions/runs/17/jobs":
			fmt.Fprintf(w, `{"jobs":[{"id":%d,"run_id":17,"status":"queued","labels":["runquill-17-1-ci"]}]}`, jobID.Load())
		case "GET /repos/owner/approved":
			if r.Header.Get("Authorization") != "Bearer repo-only" {
				t.Error("repository check must use the scoped token")
			}
			fmt.Fprint(w, repo)
		case "GET /repos/owner/approved/actions/jobs/41", "GET /repos/owner/approved/actions/jobs/42":
			fmt.Fprint(w, `{"status":"queued"}`)
		case "POST /repos/owner/approved/actions/runners/generate-jitconfig":
			if !allowed.Load() || r.Header.Get("Authorization") != "Bearer repo-only" {
				t.Error("JIT configuration without approved repository scope")
			}
			registered.Add(1)
			fmt.Fprint(w, `{"runner":{"id":55},"encoded_jit_config":"test-config"}`)
		case "DELETE /repos/owner/approved/actions/runners/55":
			removed.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)

	ctx := t.Context()
	q, err := openQueue(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	api := &github{client: mock.Client(), base: mock.URL, appID: "7", key: key, owner: "owner", repositoryScope: scopeSelected}
	poll := &poller{github: api, queue: q, cache: make(map[string]pollPage)}
	containers := map[string]string{}
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: containers}}}
	sched := &scheduler{github: api, queue: q, runtime: runtime, limit: 1}
	secret := []byte("test-secret")
	hook := webhook(q, api, secret, nil)
	send := func(action string, want int) {
		t.Helper()
		body := fmt.Sprintf(`{"action":%q,"installation":{"id":7},"repository":%s,"workflow_job":{"id":%d,"run_id":17,"labels":["runquill-17-1-ci"]}}`,
			action, repo, jobID.Load())
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(body))
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-GitHub-Event", "workflow_job")
		res := httptest.NewRecorder()
		hook.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("webhook status: %d, want %d", res.Code, want)
		}
	}
	jobs := func(condition string) []job {
		t.Helper()
		jobs, err := q.jobs(ctx, condition)
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	tick := func(now time.Time) {
		t.Helper()
		if err := sched.tick(ctx, now); err != nil {
			t.Fatal(err)
		}
	}

	send("queued", http.StatusAccepted)
	if err := poll.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := jobs(""); len(got) != 0 || runnerTokens.Load() != 0 {
		t.Fatalf("excluded repository entered queue: %+v", got)
	}

	allowed.Store(true)
	broadened.Store(true)
	send("queued", http.StatusServiceUnavailable)
	if err := poll.sweep(ctx); err == nil || !strings.Contains(err.Error(), "Only select repositories") {
		t.Fatalf("poll must reject broadened GitHub scope, got %v", err)
	}
	if got := jobs(""); len(got) != 0 || pollTokens.Load() != 1 {
		t.Fatalf("broadened installation fetched jobs: %+v", got)
	}

	broadened.Store(false)
	if err := poll.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	send("queued", http.StatusAccepted)
	if got := jobs(""); len(got) != 1 || got[0].id != 41 || pollTokens.Load() != 2 {
		t.Fatalf("approval must refresh discovery token and enqueue once: %+v", got)
	}
	start := time.Now()
	tick(start)
	if got := jobs("WHERE runner_name IS NOT NULL"); len(got) != 1 || registered.Load() != 1 || len(runtime.started) != 1 {
		t.Fatalf("approved job was not launched once: %+v", got)
	}
	send("completed", http.StatusAccepted)
	tick(start.Add(completionGrace + time.Second))
	if got := jobs("WHERE runner_name IS NOT NULL"); len(got) != 0 || removed.Load() != 1 || len(runtime.removed) != 1 {
		t.Fatal("completed approved job did not clean up the runtime and GitHub registration")
	}

	jobID.Store(42)
	send("queued", http.StatusAccepted)
	allowed.Store(false)
	tick(time.Now())
	if got := jobs("WHERE job_id = 42"); len(got) != 0 || registered.Load() != 1 {
		t.Fatalf("revoked repository left a queued job or launched a runner: %+v", got)
	}
	allowed.Store(true)
	send("queued", http.StatusAccepted)
	start = time.Now()
	tick(start)
	if got := jobs("WHERE runner_name IS NOT NULL"); len(got) != 1 || registered.Load() != 2 {
		t.Fatalf("same queued job was not re-discovered after approval: %+v", got)
	}
	allowed.Store(false)
	tick(start.Add(idleTimeout + time.Second))
	if got := jobs("WHERE job_id = 42"); len(got) != 0 || len(runtime.removed) != 2 || removed.Load() != 1 {
		t.Fatalf("revoked repository should release its local runner without remote access: %+v", got)
	}
	if err := api.checkRepositoryAccess(ctx, job{repository: "owner/approved", installationID: 7}); !errors.Is(err, errRepositoryOutsideInstallation) {
		t.Fatalf("expected explicit revoked access, got %v", err)
	}
}
