package app

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueueMigrationAddsCompletionTimeToOldQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	old, err := openQueueDB(path, "rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at, runner_name)
        VALUES (4, 'owner/repo', 2, 1, 'runquill-1-1-ci', ?, 10, 'runquill-4-a')`, inProgress); err != nil {
		t.Fatal(err)
	}
	old.Close()
	for range 2 { // the second open must find the column and change nothing
		q, err := openQueue(path)
		if err != nil {
			t.Fatal(err)
		}
		present, err := hasJobColumn(t.Context(), q.DB, "github_completed_at")
		if err != nil || !present {
			t.Fatalf("github_completed_at present = %v, %v", present, err)
		}
		if j := readJob(t, q, 4); j.runnerName != "runquill-4-a" || j.phase != inProgress {
			t.Fatalf("leased job after migration: %+v", j)
		}
		q.close()
	}
}

func TestJobCompletionTimeIsRecordedOnce(t *testing.T) {
	q := testQueue(t)
	repo := repository{ID: 2, FullName: "owner/repo", Private: true}
	repo.Owner.Login = "owner"
	event := workflowJob{ID: 8, RunID: 1, Labels: []string{"runquill-1-1-ci"}, CreatedAt: "2027-01-15T08:00:00Z",
		StartedAt: "2027-01-15T08:01:00Z", CompletedAt: "2027-01-15T08:06:00Z"}
	completedAt := func() int64 {
		t.Helper()
		var at int64
		if err := q.QueryRow("SELECT coalesce(github_completed_at, 0) FROM jobs WHERE job_id = 8").Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	now := time.Unix(1_800_000_000, 0)
	for _, status := range []string{"queued", "in_progress"} {
		j, ok, err := event.queueJob("owner", false, repo, 1, status)
		if err != nil || !ok {
			t.Fatalf("%s event: %v, %v", status, ok, err)
		}
		if _, err := q.record(t.Context(), j, now); err != nil {
			t.Fatal(err)
		}
		if at := completedAt(); at != 0 {
			t.Fatalf("%s event recorded completion at %d", status, at)
		}
	}
	j, _, _ := event.queueJob("owner", false, repo, 1, "completed")
	if _, err := q.record(t.Context(), j, now); err != nil {
		t.Fatal(err)
	}
	want := githubTimestamp(event.CompletedAt)
	if at := completedAt(); at != want {
		t.Fatalf("completed at %d, want %d", at, want)
	}
	// A later observation, such as a reconcile poll, keeps the first completion time.
	later := event
	later.CompletedAt = "2027-01-15T09:00:00Z"
	if err := q.advanceGitHubJob(t.Context(), 8, completed, now, later); err != nil {
		t.Fatal(err)
	}
	if at := completedAt(); at != want {
		t.Fatalf("completion time changed to %d, want %d", at, want)
	}
	row, err := readPanelJob(t.Context(), q.DB, 8, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if row.Timing == nil || row.Timing.ExecutionSeconds == nil || *row.Timing.ExecutionSeconds != 300 ||
		row.Timing.GitHubCompletedAt == nil || row.Timing.GitHubCompletedAt.Unix() != want {
		t.Fatalf("job timing %+v, want 300 s of execution", row.Timing)
	}
	if text := row.Timing.text(); !strings.Contains(text, "execution 300s") {
		t.Fatalf("timing text %q has no execution stage", text)
	}
}

func TestReleasedJobFeedsStageHistogramsOnce(t *testing.T) {
	q := testQueue(t)
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo/actions/jobs/2":
			fmt.Fprint(w, `{"id":2,"status":"queued"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	start := int64(1_800_000_000)
	insert := func(id int64, p phase) {
		t.Helper()
		_, err := q.Exec(`INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at,
            runner_name, runner_host, runner_id, discovered_at, jit_requested_ms, jit_registered_ms, runner_launch_ms,
            github_started_at, github_completed_at)
            VALUES (?, 'owner/repo', 2, 1, 'runquill-1-1-pool__big__ci', ?, ?, ?, 'h1', 77, ?, ?, ?, ?, ?, ?)`,
			id, p, start+312, "runquill-"+string(rune('0'+id))+"-a", start, start*1000+3000, start*1000+3500,
			start*1000+8500, start+12, start+312)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(1, completed)
	insert(2, queued) // a runner that never took its job is not a finished job
	metrics := &fleetMetrics{backend: "docker"}
	s := &scheduler{queue: q, github: gh, runtime: &testRuntime{}, metrics: metrics,
		pools: workerPools{"big": {}}}
	now := time.Unix(start+400, 0)
	for _, id := range []int64{1, 2} {
		j := readJob(t, q, id)
		for range 2 { // the second release finds no lease and must not count the job again
			if _, err := s.reconcile(t.Context(), j, "", jobDiagnostic{}, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	var out bytes.Buffer
	metrics.writePrometheus(&out)
	text := out.String()
	labels := `{pool="big",backend="docker",`
	for _, want := range []string{
		"# TYPE runquill_job_scheduler_wait_seconds histogram",
		"runquill_job_scheduler_wait_seconds_bucket" + labels + `le="2"} 0`,
		"runquill_job_scheduler_wait_seconds_bucket" + labels + `le="5"} 1`,
		"runquill_job_scheduler_wait_seconds_count{pool=\"big\",backend=\"docker\"} 1",
		"runquill_job_jit_registration_seconds_bucket" + labels + `le="0.5"} 1`,
		"runquill_job_runtime_launch_seconds_bucket" + labels + `le="5"} 1`,
		"runquill_job_runtime_launch_seconds_sum{pool=\"big\",backend=\"docker\"} 5",
		"runquill_job_github_assign_seconds_bucket" + labels + `le="2"} 0`,
		"runquill_job_github_assign_seconds_bucket" + labels + `le="5"} 1`,
		"runquill_job_execution_seconds_bucket" + labels + `le="120"} 0`,
		"runquill_job_execution_seconds_bucket" + labels + `le="300"} 1`,
		"runquill_job_execution_seconds_bucket" + labels + `le="+Inf"} 1`,
		"runquill_job_execution_seconds_count{pool=\"big\",backend=\"docker\"} 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q\n%s", want, text)
		}
	}
	if got := strings.Count(text, "# TYPE runquill_job_execution_seconds histogram"); got != 1 {
		t.Errorf("execution histogram declared %d times", got)
	}
}
