package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueueMigrationQueryPlans(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	db, err := openQueueDB(path, "rwc")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, schema+`
        INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at, runner_name)
        VALUES (42, 'owner/repo', 1, 1, 'build', 2, 100, 'active-runner');`)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("create pre-migration queue: exec=%v close=%v", err, closeErr)
	}
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	if err := q.migrate(); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	if j := readJob(t, q, 42); j.runnerName != "active-runner" || j.phase != completed {
		t.Fatalf("migration changed persisted lease: %+v", j)
	}
	if err := migrateUsage(ctx, q.DB); err != nil {
		t.Fatal(err)
	}

	for _, check := range []struct {
		name, query, plan string
		args              []any
	}{
		{"expiry", "SELECT retry_reason FROM jobs " + expiredJobsCondition,
			"SEARCH jobs USING INDEX jobs_phase_changed", []any{queued, 100}},
		{"prune", pruneJobsQuery + ` AND (phase <> ? OR EXISTS (SELECT 1 FROM usage_rolled WHERE usage_rolled.job_id = jobs.job_id))`,
			"SEARCH jobs USING INDEX jobs_unleased_changed", []any{100, completed}},
		{"status", "SELECT route FROM jobs WHERE phase = 0 OR runner_name IS NOT NULL ORDER BY sequence",
			"SCAN jobs USING INDEX jobs_status", nil},
		{"usage", "SELECT " + usageJobColumns + " FROM jobs WHERE phase = ? AND changed_at > ? AND changed_at <= ? LIMIT ?",
			"SEARCH jobs USING INDEX jobs_phase_changed", []any{completed, 90, 110, usageRawLimit}},
		{"rightsizing", `SELECT j.sequence, j.repository, samples FROM jobs j JOIN job_resources USING (job_id)
            WHERE j.phase = ? AND j.changed_at > ? LIMIT ?`,
			"SEARCH j USING INDEX jobs_phase_changed", []any{completed, 90, usageRawLimit}},
	} {
		t.Run(check.name, func(t *testing.T) {
			rows, err := q.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+check.query, check.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(plan, "\n"), check.plan) {
				t.Fatalf("plan %q does not contain %q", plan, check.plan)
			}
		})
	}
}

func TestQueueRecordMovesQueuedJobToNewInstallation(t *testing.T) {
	q := testQueue(t)
	first := time.Unix(1_800_000_000, 0)
	recordTestJob(t, q, 9, "runquill-1-1-ci", first)
	record := func(id, installation int64, p phase, at time.Time) bool {
		t.Helper()
		changed, err := q.record(t.Context(), job{id: id, repository: "owner/repo", repositoryID: 2, installationID: installation,
			route: "runquill-1-1-ci", phase: p, discoverySource: "poll"}, at)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}

	// The App was reinstalled: the same queued job is seen from the new installation.
	if !record(9, 5, queued, first.Add(time.Hour)) {
		t.Fatal("re-observation from a new installation reported no change")
	}
	if j := readJob(t, q, 9); j.installationID != 5 || j.changedAt != first.Unix() || j.phase != queued {
		t.Fatalf("queued job after reinstall: %+v, want installation 5 and its original age", j)
	}
	if record(9, 5, queued, first.Add(2*time.Hour)) {
		t.Fatal("a repeated observation changed the job")
	}

	// A leased job keeps the installation that registered its runner.
	if _, err := q.ExecContext(t.Context(), "UPDATE jobs SET runner_name = 'runquill-9-a' WHERE job_id = 9"); err != nil {
		t.Fatal(err)
	}
	if record(9, 6, queued, first.Add(3*time.Hour)) {
		t.Fatal("a leased job moved to another installation")
	}
	if j := readJob(t, q, 9); j.installationID != 5 {
		t.Fatalf("leased job changed installation: %+v", j)
	}

	// A phase change keeps the installation and still updates the phase.
	if !record(9, 7, inProgress, first.Add(4*time.Hour)) {
		t.Fatal("phase change was not recorded")
	}
	if j := readJob(t, q, 9); j.installationID != 5 || j.phase != inProgress || j.changedAt != first.Add(4*time.Hour).Unix() {
		t.Fatalf("job after phase change: %+v", j)
	}

	// A different repository ID under the same job ID is not the same job.
	recordTestJob(t, q, 10, "runquill-1-1-ci", first)
	if _, err := q.record(t.Context(), job{id: 10, repository: "owner/repo", repositoryID: 3, installationID: 5,
		route: "runquill-1-1-ci", phase: queued}, first); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 10); j.installationID != 1 || j.repositoryID != 2 {
		t.Fatalf("job moved for another repository: %+v", j)
	}
}

func TestQueueRecordKeepsJobPhaseAndLeaseOnRedelivery(t *testing.T) {
	for _, tt := range []struct {
		name      string
		initial   phase
		incoming  phase
		wantPhase phase
		changed   bool
	}{
		{"duplicate queued", queued, queued, queued, false},
		{"late queued", inProgress, queued, inProgress, false},
		{"late in progress", completed, inProgress, completed, false},
		{"duplicate completed", completed, completed, completed, false},
		{"new completion", inProgress, completed, completed, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := testQueue(t)
			ctx := t.Context()
			first := time.Unix(1_800_000_000, 0)
			j := recordTestJob(t, q, 9, "runquill-1-1-ci", first)
			if _, err := q.ExecContext(ctx, `UPDATE jobs SET phase = ?, runner_name = 'runner',
				runner_host = 'local', runner_id = 55, launched_at = ? WHERE job_id = ?`, tt.initial, first.Unix(), j.id); err != nil {
				t.Fatal(err)
			}
			want := readJob(t, q, j.id)
			later := first.Add(time.Minute)
			j.phase = tt.incoming
			changed, err := q.record(ctx, j, later)
			if err != nil || changed != tt.changed {
				t.Fatalf("record returned (%v, %v), want (%v, nil)", changed, err, tt.changed)
			}
			want.phase = tt.wantPhase
			if tt.changed {
				want.changedAt = later.Unix()
			}
			if got := readJob(t, q, j.id); got != want {
				t.Fatalf("stored job = %+v, want %+v", got, want)
			}
		})
	}
}
