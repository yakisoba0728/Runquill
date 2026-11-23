package app

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStatusExplainsEligibleCapacity(t *testing.T) {
	ready := hostCapacity{dockerHostSnapshot: dockerHostSnapshot{
		Name: "host-a", Pool: "build", Containers: map[string]string{},
	}, Limit: 1}
	paused, offline, unavailable, busy, other, imagePending := ready, ready, ready, ready, ready, ready
	paused.Cordoned = true
	offline.Containers = nil
	offline.Occupied = 2
	unavailable.Err = errors.New("internal provider data")
	busy.Occupied = 1
	other.Pool = "test"
	imagePending.ImageWait = "waiting for image"

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
		{[]hostCapacity{imagePending}, 2, "runner_image_pending"},
		{[]hostCapacity{ready, offline}, 2, "fleet_full"},
		{[]hostCapacity{ready, other}, 2, "waiting_scheduler"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			j := job{id: 12, repository: "owner/repo", route: "runquill-28-1-pool__build__compile"}
			report := summarizeStatus([]job{j}, tc.hosts, 0, tc.limit, time.Now())
			report.configure("docker", scopeSelected, workerPools{"build": {}}, []job{j}, tc.hosts, jobPlacementDiagnostics{})
			row := report.Jobs[0]
			if row.Diagnostic == nil || row.Diagnostic.Code != tc.code || strings.Contains(row.Diagnostic.Message, "provider data") {
				t.Fatalf("job diagnostic = %+v, want %v", row.Diagnostic, tc.code)
			}
		})
	}
}

func TestStatusUsesRepositoryCapAndReportsItsReasonInHistory(t *testing.T) {
	host := hostCapacity{dockerHostSnapshot: dockerHostSnapshot{
		Name: "host-a", Containers: map[string]string{"test-runner": "running"}, Pool: "build",
	}, Limit: 2, Occupied: 1}
	jobs := []job{
		{id: 1, repository: "owner/project", runnerName: "test-runner", route: "runquill-28-1-pool__build__test", phase: inProgress},
		{id: 2, repository: "owner/project", route: "runquill-29-1-pool__build__test"},
		{id: 3, repository: "owner/other", route: "runquill-30-1-pool__build__test"},
	}
	report := summarizeStatus(jobs, []hostCapacity{host}, 0, 2, time.Now())
	report.configure("docker", scopeSelected, workerPools{"build": {MaxPerRepository: 1}}, jobs, []hostCapacity{host}, jobPlacementDiagnostics{repoCap: 2})
	waiting := report.Jobs[1].Diagnostic
	if waiting == nil || waiting.Code != "repository_cap" || report.Jobs[2].Diagnostic.Code != "waiting_scheduler" {
		t.Fatalf("job reasons = %+v, %+v", waiting, report.Jobs[2].Diagnostic)
	}
	page := panelHistoryPageResponse{Jobs: []panelHistoryJob{{ID: 2, Outcome: "waiting"}, {ID: 3, Outcome: "finished"}}}
	page.addLiveDiagnostics(&report)
	if page.Jobs[0].Diagnostic != waiting || page.Jobs[1].Diagnostic != nil {
		t.Fatalf("history diagnostics = %+v", page.Jobs)
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

func TestStatusReportsWarmRunnersAndPoolTarget(t *testing.T) {
	hosts := []hostCapacity{
		{dockerHostSnapshot: dockerHostSnapshot{Name: "local", Pool: "build", Containers: map[string]string{}, Warm: 2}, Limit: 4},
		{dockerHostSnapshot: dockerHostSnapshot{Name: "agent://edge", Pool: "build", Containers: map[string]string{}}, Limit: 1},
		{dockerHostSnapshot: dockerHostSnapshot{Name: "ssh://runner@b", Containers: map[string]string{}}, Limit: 1},
	}
	report := summarizeStatus(nil, hosts, 0, 4, time.Now())
	report.configure("docker", scopeSelected, workerPools{"build": {Warm: 3}, "default": {Warm: 1}}, nil, hosts, jobPlacementDiagnostics{})
	got := [][2]int{}
	for _, host := range report.Hosts {
		got = append(got, [2]int{host.Warm, host.WarmTarget})
	}
	if want := [][2]int{{2, 3}, {0, 0}, {0, 1}}; !slices.Equal(got, want) {
		t.Fatalf("warm, target per host = %v, want %v", got, want)
	}
}
