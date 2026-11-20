package app

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDiagnoseReportsQueuedJobsFromGitHubAndLocalEvidence(t *testing.T) {
	testDockerFleet(t)
	t.Setenv("RUNQUILL_DOCKER_HOSTS", "local")
	t.Setenv("RUNQUILL_MAX_RUNNERS", "1")
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	if err := q.saveRepositoryPolls(ctx, 7, []repositoryPoll{
		{repositoryID: 9, checkedAt: time.Now().UnixNano(), lastError: "scan: GitHub returned HTTP 429"},
	}); err != nil {
		t.Fatal(err)
	}
	local := job{id: 3, repository: "owner/repo", repositoryID: 9, installationID: 7, route: "runquill-10-1-waiting"}
	if _, err := q.record(ctx, local, time.Now()); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.URL.Path != "/app/installations/7/access_tokens" {
			t.Errorf("unexpected GitHub write %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/users/owner/installation":
			fmt.Fprint(w, `{"id":7,"account":{"login":"owner","type":"User"},"repository_selection":"all","permissions":{"actions":"read","administration":"write"}}`)
		case "/repos/owner/repo/installation":
			fmt.Fprint(w, `{"id":7}`)
		case "/app/installations/7/access_tokens":
			var body struct {
				Repositories []string
				Permissions  map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
				!slices.Equal(body.Repositories, []string{"repo"}) || len(body.Permissions) != 1 ||
				body.Permissions["actions"] != "read" {
				t.Errorf("diagnostics token must be read-only and repository-scoped: %+v, %v", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"token":"fixture"}`)
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"id":9,"full_name":"Owner/Repo","private":true,"owner":{"login":"owner"}}`)
		case "/repos/owner/repo/actions/runs":
			if r.URL.Query().Get("status") == "queued" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":10}]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[]}`)
		case "/repos/owner/repo/actions/runs/10/jobs":
			fmt.Fprint(w, `{"jobs":[
                {"id":1,"run_id":10,"status":"queued","labels":["runquill-invalid"]},
                {"id":2,"run_id":10,"status":"queued","labels":["runquill-10-1-missing"]},
                {"id":3,"run_id":10,"status":"queued","labels":["runquill-10-1-waiting"]},
                {"id":4,"run_id":10,"status":"queued","labels":["ubuntu-latest"]}]}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	db, err := openQueueDB(path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	api := &github{client: server.Client(), base: server.URL, key: key, appID: "1", owner: "owner"}
	var out bytes.Buffer
	if err := diagnoseRepository(ctx, &out, api, db, "Owner/Repo", true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GitHub App installation covers owner/repo",
		"Last repository poll: scan: GitHub returned HTTP 429",
		"Job 1 (run 10): invalid Runquill labels",
		"Job 2 (run 10): label is valid and the job is absent from the local queue. The last repository poll failed",
		"Job 3 (run 10): The fleet has reached its runner slot limit. Running and reserved slots both count.",
		"Job 4 (run 10): uses labels outside Runquill",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in diagnostics:\n%s", want, out.String())
		}
	}
	jobs, err := q.jobs(ctx, "ORDER BY sequence")
	if err != nil || len(jobs) != 1 || jobs[0].id != local.id {
		t.Fatalf("diagnostics changed local queue: %+v, %v", jobs, err)
	}
}
