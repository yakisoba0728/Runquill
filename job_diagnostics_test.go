package main

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusExplainsEligibleCapacity(t *testing.T) {
	ready := hostCapacity{dockerHostSnapshot: dockerHostSnapshot{
		Name: "host-a", Pool: "build", Containers: map[string]string{},
	}, Limit: 1}
	paused, offline, unavailable, busy, other := ready, ready, ready, ready, ready
	paused.Cordoned = true
	offline.Containers = nil
	offline.Occupied = 2
	unavailable.Err = errors.New("internal provider data")
	busy.Occupied = 1
	other.Pool = "test"

	for _, tc := range []struct {
		hosts []hostCapacity
		limit int
		code  string
	}{
		{nil, 2, "fleet_status_unknown"},
		{[]hostCapacity{other}, 2, "pool_no_workers"},
		{[]hostCapacity{paused, other}, 2, "workers_cordoned"},
		{[]hostCapacity{offline, other}, 2, "workers_offline"},
		{[]hostCapacity{unavailable, offline}, 2, "workers_unavailable"},
		{[]hostCapacity{busy, other}, 2, "workers_full"},
		{[]hostCapacity{ready, offline}, 2, "fleet_full"},
		{[]hostCapacity{ready, other}, 2, "waiting_scheduler"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			j := job{id: 12, repository: "owner/repo", route: "runquill-28-1-pool__build__compile"}
			report := summarizeStatus([]job{j}, tc.hosts, 0, tc.limit, time.Now())
			report.configure("docker", scopeSelected, workerPools{"build": {}}, []job{j}, tc.hosts)
			row := report.Jobs[0]
			if row.Diagnostic == nil || row.Diagnostic.Code != tc.code || strings.Contains(row.Diagnostic.Message, "provider data") {
				t.Fatalf("job diagnostic = %+v, want %v", row.Diagnostic, tc.code)
			}
		})
	}
}

func TestSchedulerPreservesKubernetesFailureAfterRemovingPod(t *testing.T) {
	const name = "runquill-12-012345abcdef"
	now := time.Now()
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	ctx := t.Context()
	j := job{id: 12, repository: "owner/repo", route: "runquill-28-1-test", repositoryID: 4, installationID: 7}
	if _, err := q.record(ctx, j, now); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ExecContext(ctx, `UPDATE jobs SET runner_name = ?, runner_host = ?, runner_id = 17 WHERE job_id = ?`, name, kubeHost, j.id); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/7/access_tokens":
			fmt.Fprint(w, `{"token":"fixture"}`)
		case "GET /repos/owner/repo/actions/jobs/12":
			fmt.Fprint(w, `{"status":"queued"}`)
		case "DELETE /repos/owner/repo/actions/runners/17":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	cause := jobDiagnostic{"pod_init_failed", "Test runner initialization failed."}
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{
		Name: kubeHost, Containers: map[string]string{name: "exited"},
		RunnerDiagnostics: map[string]jobDiagnostic{name: cause},
	}}}
	s := &scheduler{queue: q, runtime: runtime, limit: 1, github: &github{
		base: api.URL, client: api.Client(), owner: "owner", appID: "3", key: key,
	}}
	if err := s.tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	jobs, err := readStatusJobs(ctx, q.DB)
	if err != nil || len(jobs) != 1 || jobs[0].runnerName != "" ||
		jobs[0].retryReason != "Previous runner · "+cause.Message || len(runtime.removed) != 1 {
		t.Fatalf("retry evidence after cleanup: jobs=%+v removed=%v err=%v", jobs, runtime.removed, err)
	}
}
