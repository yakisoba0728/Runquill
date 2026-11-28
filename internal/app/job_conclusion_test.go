package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sendWorkflowJob(t *testing.T, handler http.Handler, action string, id int64, extra string) {
	t.Helper()
	body := fmt.Sprintf(`{"action":%q,"installation":{"id":1},
		"repository":{"id":2,"full_name":"owner/repo","private":true,"owner":{"login":"owner"}},
		"workflow_job":{"id":%d,"run_id":17,"labels":["runquill-17-1-ci"],"status":%q,
		"workflow_name":"CI","head_branch":"main"%s}}`, action, id, action, extra)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	signWebhookFixture(req, []byte("fixture-secret"), body)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("%s webhook for job %d: status %d", action, id, res.Code)
	}
}

func TestWebhookRecordsGitHubConclusion(t *testing.T) {
	q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
	t.Cleanup(q.close)
	handler := webhook(q, &github{owner: "owner"}, []byte("fixture-secret"), nil)
	now := time.Now()

	sendWorkflowJob(t, handler, "queued", 9, "")
	sendWorkflowJob(t, handler, "completed", 9, `,"conclusion":"failure","completed_at":"2026-11-29T00:00:00Z"`)
	row, err := readPanelJob(t.Context(), q.DB, 9, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if row.Outcome != "finished" || row.GitHubConclusion != "failure" || row.WorkflowName != "CI" || row.HeadBranch != "main" {
		t.Fatalf("completed job row %+v", row)
	}
	// A redelivered or later event never replaces the recorded result.
	sendWorkflowJob(t, handler, "completed", 9, `,"conclusion":"success"`)
	if row, _ := readPanelJob(t.Context(), q.DB, 9, nil, now); row.GitHubConclusion != "failure" {
		t.Fatalf("conclusion changed to %q", row.GitHubConclusion)
	}

	// A job Runquill already gave up on still takes GitHub's later result, and nothing else changes.
	sendWorkflowJob(t, handler, "queued", 10, "")
	if _, err := q.fail(t.Context(), 10, now.Add(-time.Hour), "image pull failed"); err != nil {
		t.Fatal(err)
	}
	before := readJob(t, q, 10)
	sendWorkflowJob(t, handler, "completed", 10, `,"conclusion":"cancelled"`)
	after := readJob(t, q, 10)
	row, err = readPanelJob(t.Context(), q.DB, 10, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if row.Outcome != "failed" || row.Failure != "image pull failed" || row.GitHubConclusion != "cancelled" ||
		after.changedAt != before.changedAt || after.installationID != before.installationID {
		t.Fatalf("failed job row %+v, before %+v, after %+v", row, before, after)
	}
}

func TestPollerAndReconcileRecordGitHubConclusion(t *testing.T) {
	q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
	t.Cleanup(q.close)
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo/actions/jobs/5":
			fmt.Fprint(w, `{"id":5,"status":"completed","conclusion":"success","completed_at":"2026-11-29T00:05:00Z",
				"workflow_name":"CI","head_branch":"main"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// The poller records queued jobs from the same GitHub job object.
	candidate := workflowJob{ID: 5, RunID: 17, Labels: []string{"runquill-17-1-ci"}, Status: "queued",
		WorkflowName: "CI", HeadBranch: strings.Repeat("b", 300)}
	repo := repository{ID: 2, FullName: "owner/repo", Private: true}
	repo.Owner.Login = "owner"
	j, eligible, err := candidate.queueJob("owner", false, repo, 1, candidate.Status)
	if err != nil || !eligible {
		t.Fatalf("queueJob: %v, eligible %v", err, eligible)
	}
	j.discoverySource = "poll"
	now := time.Unix(1_800_000_000, 0)
	if _, err := q.record(t.Context(), j, now); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(`UPDATE jobs SET phase = ?, runner_name = 'runner-5', runner_host = 'h1' WHERE job_id = 5`, inProgress); err != nil {
		t.Fatal(err)
	}
	// The runner exited before GitHub's completed webhook: reconcile asks GitHub for the result.
	s := &scheduler{queue: q, github: gh, runtime: &testRuntime{}, metrics: &fleetMetrics{backend: "docker"}}
	if _, err := s.reconcile(t.Context(), readJob(t, q, 5), "", jobDiagnostic{}, now); err != nil {
		t.Fatal(err)
	}
	row, err := readPanelJob(t.Context(), q.DB, 5, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if row.Outcome != "finished" || row.GitHubConclusion != "success" || row.WorkflowName != "CI" ||
		row.HeadBranch != strings.Repeat("b", 255) {
		t.Fatalf("reconciled job row %+v", row)
	}
}

func TestUsageCountsGitHubConclusions(t *testing.T) {
	q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
	t.Cleanup(q.close)
	insert := func(id int64, conclusion string) {
		insertUsageJob(t, q, usageFinishedJob(id, "owner/app", "h1", usageNow.Add(-26*time.Hour), time.Minute, time.Minute))
		if _, err := q.Exec(`UPDATE jobs SET conclusion = nullif(?, '') WHERE job_id = ?`, conclusion, id); err != nil {
			t.Fatal(err)
		}
	}
	// Jobs 1 to 3 are rolled up; jobs 4 to 6 are still read from the open window.
	insert(1, "success")
	insert(2, "success")
	insert(3, "failure")
	if err := rollupUsage(t.Context(), q, nil, usageNow); err != nil {
		t.Fatal(err)
	}
	insert(4, "timed_out")
	insert(5, "cancelled")
	insert(6, "")
	u, err := parseUsageQuery(map[string][]string{"group": {"repository"}}, usageNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := readUsage(t.Context(), q.DB, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Jobs != 6 || report.Totals.Failed != 0 || report.Totals.GitHubSucceeded != 2 || report.Totals.GitHubFailed != 2 ||
		len(report.Series) != 1 || report.Series[0].GitHubSucceeded != 2 || report.Series[0].GitHubFailed != 2 {
		t.Fatalf("usage report %+v", report)
	}
	summary, err := usageSummary(t.Context(), q, usageNow, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if summary.GitHubSucceeded != 2 || summary.GitHubFailed != 2 {
		t.Fatalf("usage summary %+v", summary)
	}
}

func TestGitHubResultColumnsMigrateOldQueue(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	db, err := openQueueDB(path, "rwc")
	if err != nil {
		t.Fatal(err)
	}
	// A queue from before GitHub results were recorded, with a rolled-up day.
	_, err = db.ExecContext(ctx, schema+`
        INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at)
        VALUES (42, 'owner/repo', 1, 1, 'runquill-1-1-ci', 2, 100);
        CREATE TABLE usage_daily (day TEXT NOT NULL, repository TEXT NOT NULL, pool TEXT NOT NULL, host TEXT NOT NULL,
            jobs INTEGER NOT NULL DEFAULT 0, failed INTEGER NOT NULL DEFAULT 0, runner_seconds INTEGER NOT NULL DEFAULT 0,
            wait_count INTEGER NOT NULL DEFAULT 0, wait_seconds INTEGER NOT NULL DEFAULT 0,
            `+strings.Join(usageWaitColumns, " INTEGER NOT NULL DEFAULT 0, ")+` INTEGER NOT NULL DEFAULT 0,
            PRIMARY KEY (day, repository, pool, host)) WITHOUT ROWID;
        INSERT INTO usage_daily (day, repository, pool, host, jobs, failed) VALUES ('2026-11-18', 'owner/repo', '', '', 4, 1);`)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("create old queue: exec=%v close=%v", err, closeErr)
	}
	q := openUsageQueue(t, path)
	t.Cleanup(q.close)
	if err := migrateUsage(ctx, q.DB); err != nil {
		t.Fatalf("repeat usage migration: %v", err)
	}
	row, err := readPanelJob(ctx, q.DB, 42, nil, usageNow)
	if err != nil || row.GitHubConclusion != "" || row.WorkflowName != "" {
		t.Fatalf("old job row %+v, %v", row, err)
	}
	if err := q.advanceGitHubJob(ctx, 42, completed, usageNow, workflowJob{Status: "completed", Conclusion: "failure"}); err != nil {
		t.Fatal(err)
	}
	if row, _ := readPanelJob(ctx, q.DB, 42, nil, usageNow); row.GitHubConclusion != "failure" {
		t.Fatalf("migrated job conclusion %q", row.GitHubConclusion)
	}
	u, err := parseUsageQuery(map[string][]string{"from": {"2026-11-18"}, "to": {"2026-11-18"}}, usageNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := readUsage(ctx, q.DB, u, nil)
	if err != nil || report.Totals.Jobs != 4 || report.Totals.Failed != 1 || report.Totals.GitHubSucceeded != 0 || report.Totals.GitHubFailed != 0 {
		t.Fatalf("old usage %+v, %v", report.Totals, err)
	}
}
