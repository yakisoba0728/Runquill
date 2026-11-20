package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGCReportsUntrackedContainersAndRetainsLeases(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	for id := int64(1); id <= 3; id++ {
		j := job{id: id, repository: "owner/repo", repositoryID: 7, installationID: 10, route: "runquill-42-1-build"}
		if _, err := q.record(ctx, j, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	_, err = q.ExecContext(ctx, `UPDATE jobs SET runner_name = 'runquill-1-0123456789ab', runner_host = 'local' WHERE job_id = 1;
        UPDATE jobs SET runner_name = 'runquill-3-0123456789ab', runner_host = NULL WHERE job_id = 3`)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openQueueDB(path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hosts := []dockerHostSnapshot{
		{Name: "local", Containers: map[string]string{
			"runquill-1-0123456789ab": "running",
			"runquill-2-0123456789ab": "exited",
			"runquill-3-0123456789ab": "running",
			"runquill-4-0123456789ab": "paused",
		}},
		{Name: "ssh://offline", Err: errors.New("connection refused")},
	}
	var lookedUp []int64
	lookup := func(_ context.Context, j job) (string, error) {
		lookedUp = append(lookedUp, j.id)
		if j.repository != "owner/repo" || j.repositoryID != 7 {
			t.Errorf("unexpected GitHub lookup %+v", j)
		}
		return "completed", nil
	}
	var out bytes.Buffer
	if err := writeGCInventory(ctx, &out, db, hosts, lookup); err == nil || !strings.Contains(err.Error(), "ssh://offline") {
		t.Fatalf("expected incomplete inspection for an offline host: %v", err)
	}
	got := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(got, "local runquill-2-0123456789ab exited untracked completed https://github.com/owner/repo/actions/runs/42/job/2") ||
		!strings.Contains(got, "local runquill-4-0123456789ab paused untracked unknown") ||
		strings.Contains(got, "runquill-1-") || strings.Contains(got, "runquill-3-") ||
		!slices.Equal(lookedUp, []int64{2}) {
		t.Fatalf("unexpected dry-run inventory:\n%s", out.String())
	}
	leases, err := readRunnerLeases(ctx, q.DB)
	if err != nil || len(leases) != 2 || leases["runquill-1-0123456789ab"] != "local" {
		t.Fatalf("inventory altered leases: %+v, %v", leases, err)
	}
}

func TestGCMissingQueueReportsUnknownAndCreatesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite3")
	db, err := openQueueDB(path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out bytes.Buffer
	hosts := []dockerHostSnapshot{{Name: "local", Containers: map[string]string{"runquill-2-0123456789ab": "created"}}}
	lookup := func(context.Context, job) (string, error) {
		t.Error("unexpected GitHub lookup for a runner without queue metadata")
		return "", nil
	}
	if err := writeGCInventory(t.Context(), &out, db, hosts, lookup); err == nil {
		t.Fatal("expected the missing queue to make inspection incomplete")
	}
	got := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(got, "Queue unavailable") || !strings.Contains(got, "runquill-2-0123456789ab created unknown unknown") {
		t.Fatalf("unexpected incomplete dry-run inventory:\n%s", out.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created a database: %v", err)
	}
}

func TestGCReadOnlyGitHubJobStatus(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/10/access_tokens":
			var body struct {
				Repositories []int64 `json:"repository_ids"`
				Permissions  map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
				!slices.Equal(body.Repositories, []int64{7}) || len(body.Permissions) != 1 ||
				body.Permissions["actions"] != "read" {
				t.Errorf("dry-run lookup must stay read-only and repository-scoped: %+v, %v", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"token":"fixture"}`)
		case "GET /repos/owner/repo":
			fmt.Fprint(w, `{"id":7,"private":true,"owner":{"login":"owner"}}`)
		case "GET /repos/owner/repo/actions/jobs/2":
			fmt.Fprint(w, `{"status":"completed"}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	api := &github{client: server.Client(), base: server.URL, key: key, appID: "1", owner: "owner"}
	j := job{id: 2, repository: "owner/repo", repositoryID: 7, installationID: 10}
	if observed, err := api.readRunnerJobStatus(t.Context(), j); err != nil || observed != "completed" {
		t.Fatalf("unexpected GitHub job status %q, %v", observed, err)
	}
}
