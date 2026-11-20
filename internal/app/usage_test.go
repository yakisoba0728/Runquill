package app

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

var usageNow = time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)

func openUsageQueue(t *testing.T, path string) *queue {
	t.Helper()
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateUsage(t.Context(), q.DB); err != nil {
		t.Fatal(err)
	}
	return q
}

type usageJob struct {
	id                   int64
	repository, route    string
	host                 string
	queued, provisioned  time.Time
	finished, cleaned    time.Time
	failure              string
	leased, notCompleted bool
}

func insertUsageJob(t *testing.T, q *queue, j usageJob) {
	t.Helper()
	unix := func(at time.Time) any {
		if at.IsZero() {
			return nil
		}
		return at.Unix()
	}
	route := j.route
	if route == "" {
		route = "runquill-1-1-build"
	}
	phase, runner, host, cleanedHost := completed, any(nil), any(nil), any(nil)
	if j.notCompleted {
		phase = inProgress
	}
	if j.leased || j.notCompleted {
		runner, host = fmt.Sprintf("runner-%d", j.id), j.host
	} else if j.host != "" {
		cleanedHost = j.host
	}
	var failure any
	if j.failure != "" {
		failure = j.failure
	}
	_, err := q.Exec(`INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at,
        runner_name, runner_host, runner_cleaned_host, discovered_at, provisioned_at, runner_cleaned_at, failure)
        VALUES (?, ?, 1, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, j.id, j.repository, route, phase, j.finished.Unix(),
		runner, host, cleanedHost, unix(j.queued), unix(j.provisioned), unix(j.cleaned), failure)
	if err != nil {
		t.Fatal(err)
	}
}

// usageFinishedJob waited wait seconds, ran for run seconds and finished at end.
func usageFinishedJob(id int64, repository, host string, end time.Time, wait, run time.Duration) usageJob {
	provisioned := end.Add(-run)
	return usageJob{id: id, repository: repository, host: host, queued: provisioned.Add(-wait),
		provisioned: provisioned, finished: end, cleaned: end}
}

func usageRows(t *testing.T, q *queue) map[string][4]int64 {
	t.Helper()
	rows, err := q.Query(`SELECT day || ' ' || repository || ' ' || pool || ' ' || host, jobs, failed, runner_seconds, wait_count
        FROM usage_daily`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][4]int64{}
	for rows.Next() {
		var key string
		var v [4]int64
		if err := rows.Scan(&key, &v[0], &v[1], &v[2], &v[3]); err != nil {
			t.Fatal(err)
		}
		out[key] = v
	}
	return out
}

func TestUsageRollupIsIdempotentAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	q := openUsageQueue(t, path)
	ctx := t.Context()
	insertUsageJob(t, q, usageFinishedJob(1, "Owner/App", "h1", usageNow.Add(-26*time.Hour), time.Minute, 10*time.Minute))
	insertUsageJob(t, q, usageFinishedJob(2, "owner/app", "h1", usageNow.Add(-25*time.Hour), 2*time.Minute, 5*time.Minute))
	failed := usageJob{id: 3, repository: "owner/app", queued: usageNow.Add(-3 * time.Hour), finished: usageNow.Add(-time.Hour), failure: "cancelled"}
	insertUsageJob(t, q, failed)
	leased := usageFinishedJob(4, "owner/app", "h2", usageNow.Add(-time.Hour), time.Minute, time.Minute)
	leased.leased, leased.cleaned = true, time.Time{}
	insertUsageJob(t, q, leased)
	running := usageFinishedJob(5, "owner/app", "h2", usageNow, time.Minute, time.Minute)
	running.notCompleted = true
	insertUsageJob(t, q, running)

	for range 3 {
		if err := rollupUsage(ctx, q, nil, usageNow); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string][4]int64{
		"2026-11-18 owner/app  h1": {2, 0, 900, 2},
		"2026-11-19 owner/app  ":   {1, 1, 0, 1},
	}
	if got := usageRows(t, q); !maps.Equal(got, want) {
		t.Fatalf("rollup = %v, want %v", got, want)
	}

	// Simulated restart: the ledger and floor live in the database.
	q.close()
	q = openUsageQueue(t, path)
	t.Cleanup(q.close)
	// A row changing after its rollup does not count again.
	if _, err := q.Exec(`UPDATE jobs SET changed_at = ?, failure = 'late' WHERE job_id = 1`, usageNow.Unix()); err != nil {
		t.Fatal(err)
	}
	// The leased job is released and the running one finishes.
	if _, err := q.Exec(`UPDATE jobs SET runner_name = NULL, runner_host = NULL, runner_cleaned_host = 'h2',
        runner_cleaned_at = changed_at + 30 WHERE job_id = 4`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(`UPDATE jobs SET phase = ?, runner_name = NULL, runner_host = NULL, runner_cleaned_host = 'h2',
        runner_cleaned_at = changed_at WHERE job_id = 5`, completed); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := rollupUsage(ctx, q, nil, usageNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	want["2026-11-19 owner/app  h2"] = [4]int64{2, 0, 90 + 60, 2}
	if got := usageRows(t, q); !maps.Equal(got, want) {
		t.Fatalf("after restart = %v, want %v", got, want)
	}
	var floor, top int64
	q.QueryRow(`SELECT floor FROM usage_meta`).Scan(&floor)
	q.QueryRow(`SELECT max(sequence) FROM jobs`).Scan(&top)
	if floor != top {
		t.Fatalf("floor = %d, want %d once every job is final", floor, top)
	}
}

func TestUsageRollupContinuesPastBatchAndPrunes(t *testing.T) {
	q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
	t.Cleanup(q.close)
	ctx := t.Context()
	pending := usageFinishedJob(1, "owner/app", "h1", usageNow, time.Minute, time.Minute)
	pending.notCompleted = true
	insertUsageJob(t, q, pending)
	tx, err := q.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for id := int64(2); id <= usageRollupBatch+10; id++ {
		if _, err := tx.Exec(`INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at)
            VALUES (?, 'owner/app', 1, 1, 'runquill-1-1-build', ?, ?)`, id, completed, usageNow.Add(-time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// An old rollup row past retention and a ledger entry for a deleted job.
	if _, err := q.Exec(`INSERT INTO usage_daily (day, repository, pool, host, jobs) VALUES ('2025-10-01', 'owner/old', '', '', 1),
        ('2025-10-16', 'owner/kept', '', '', 1); INSERT INTO usage_rolled (job_id) VALUES (999999)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := rollupUsage(ctx, q, nil, usageNow); err != nil {
			t.Fatal(err)
		}
	}
	var jobs, rolled, old, kept int64
	q.QueryRow(`SELECT sum(jobs) FROM usage_daily WHERE repository = 'owner/app'`).Scan(&jobs)
	q.QueryRow(`SELECT count(*) FROM usage_rolled`).Scan(&rolled)
	q.QueryRow(`SELECT count(*) FROM usage_daily WHERE repository = 'owner/old'`).Scan(&old)
	q.QueryRow(`SELECT count(*) FROM usage_daily WHERE repository = 'owner/kept'`).Scan(&kept)
	if jobs != usageRollupBatch+9 || rolled != usageRollupBatch+9 || old != 0 || kept != 1 {
		t.Fatalf("jobs=%d rolled=%d old=%d kept=%d", jobs, rolled, old, kept)
	}
	var floor int64
	q.QueryRow(`SELECT floor FROM usage_meta`).Scan(&floor)
	if floor != 0 {
		t.Fatalf("floor = %d, want 0 below the pending job", floor)
	}
}

func TestUsageRollupRescansReusedSequences(t *testing.T) {
	q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
	t.Cleanup(q.close)
	ctx := t.Context()
	insertUsageJob(t, q, usageFinishedJob(1, "owner/app", "", usageNow.Add(-time.Hour), time.Minute, time.Minute))
	insertUsageJob(t, q, usageFinishedJob(2, "owner/app", "", usageNow.Add(-time.Hour), time.Minute, time.Minute))
	if err := rollupUsage(ctx, q, nil, usageNow); err != nil {
		t.Fatal(err)
	}
	// Deleting the top row lets SQLite hand its sequence to the next job.
	if _, err := q.Exec(`DELETE FROM jobs WHERE job_id = 2`); err != nil {
		t.Fatal(err)
	}
	insertUsageJob(t, q, usageFinishedJob(3, "owner/app", "", usageNow.Add(-time.Minute), time.Minute, time.Minute))
	if err := rollupUsage(ctx, q, nil, usageNow); err != nil {
		t.Fatal(err)
	}
	var jobs int64
	q.QueryRow(`SELECT sum(jobs) FROM usage_daily`).Scan(&jobs)
	if jobs != 3 {
		t.Fatalf("jobs = %d, want 3", jobs)
	}
}

func TestSchedulerPrunesCompletedJobsAfterUsageRollup(t *testing.T) {
	checkCounts := func(t *testing.T, q *queue, wantRemaining, wantRolled int) {
		t.Helper()
		var remaining, rolled int
		err := q.QueryRow(`SELECT (SELECT count(*) FROM jobs),
            (SELECT coalesce(sum(jobs), 0) FROM usage_daily)`).Scan(&remaining, &rolled)
		if err != nil {
			t.Fatal(err)
		}
		if remaining != wantRemaining || rolled != wantRolled {
			t.Fatalf("remaining jobs = %d, rolled jobs = %d; want %d, %d", remaining, rolled, wantRemaining, wantRolled)
		}
	}

	t.Run("batch backlog", func(t *testing.T) {
		q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
		t.Cleanup(q.close)
		if _, err := q.Exec(`WITH RECURSIVE ids(id) AS (
            VALUES (1) UNION ALL SELECT id + 1 FROM ids WHERE id < ?
        )
        INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at)
        SELECT id, 'owner/app', 1, 1, 'runquill-1-1-build', ?, ? FROM ids`,
			usageRollupBatch+1, completed, usageNow.Add(-8*24*time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
		s := scheduler{queue: q, runtime: &testRuntime{}, limit: 1}
		if err := s.tick(t.Context(), usageNow); err != nil {
			t.Fatal(err)
		}
		checkCounts(t, q, 1, usageRollupBatch)
		if err := s.tick(t.Context(), usageNow.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		checkCounts(t, q, 0, usageRollupBatch+1)
	})

	t.Run("failed rollup", func(t *testing.T) {
		q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
		t.Cleanup(q.close)
		old := usageNow.Add(-8 * 24 * time.Hour)
		insertUsageJob(t, q, usageFinishedJob(1, "owner/app", "h1", old, time.Minute, time.Minute))
		// Webhooks can record an in-progress job without a local lease.
		if _, err := q.Exec(`INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at)
            VALUES (2, 'owner/app', 1, 1, 'runquill-1-1-build', ?, ?)`, inProgress, old.Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Exec(`CREATE TRIGGER fail_usage_rollup BEFORE INSERT ON usage_daily
            BEGIN SELECT RAISE(ABORT, 'simulated rollup failure'); END;`); err != nil {
			t.Fatal(err)
		}
		s := scheduler{queue: q, runtime: &testRuntime{}, limit: 1}
		if err := s.tick(t.Context(), usageNow); err != nil {
			t.Fatal(err)
		}
		checkCounts(t, q, 1, 0)
		if _, err := q.Exec("DROP TRIGGER fail_usage_rollup"); err != nil {
			t.Fatal(err)
		}
		if err := s.tick(t.Context(), usageNow.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		checkCounts(t, q, 0, 1)
	})
}

func TestUsagePercentileMergeWithinBucket(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var parts [4]usageTotals
	var all []int64
	for i := range 4000 {
		wait := int64(rng.ExpFloat64() * 400)
		all = append(all, wait)
		parts[i%4].add(usageSample{wait: &wait})
	}
	var merged usageTotals
	for _, part := range parts {
		merged.merge(part)
	}
	merged.finish()
	slices.Sort(all)
	for _, c := range []struct {
		p   float64
		got int64
	}{{0.50, merged.WaitP50Seconds}, {0.95, merged.WaitP95Seconds}} {
		exact := all[int(math.Ceil(c.p*float64(len(all))-1e-9))-1]
		if usageWaitBucket(c.got) != usageWaitBucket(exact) {
			t.Errorf("p%.0f = %d, exact %d is in another bucket", c.p*100, c.got, exact)
		}
	}
	if merged.Jobs != 4000 || merged.WaitCount != 4000 {
		t.Fatalf("merged %+v", merged)
	}
	huge := int64(200000)
	var open usageTotals
	open.add(usageSample{wait: &huge})
	open.finish()
	if open.WaitP95Seconds != 86400 {
		t.Fatalf("open bucket p95 = %d", open.WaitP95Seconds)
	}
}

func usageServer(t *testing.T, q *queue, pools workerPools) (*panelAccess, http.Handler, map[string]*http.Cookie) {
	t.Helper()
	access := panelAccessForTest(t, q)
	mux := http.NewServeMux()
	registerUsageRoutes(mux, access, func() workerPools { return pools })
	key, err := access.startSession(t.Context(), panelIdentity{GitHubID: 101, Login: "fleet-admin"})
	if err != nil {
		t.Fatal(err)
	}
	return access, mux, map[string]*http.Cookie{"admin": {Name: access.cookieName("session"), Value: key}}
}

func usageGet(t *testing.T, access *panelAccess, handler http.Handler, cookie *http.Cookie, query string) (*httptest.ResponseRecorder, usageResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, access.cfg.origin+"/api/usage"+query, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var response usageResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return rec, response
}

func TestUsageEndpointValidatesAndGroups(t *testing.T) {
	q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
	t.Cleanup(q.close)
	pools := workerPools{"default": {}, "gpu": {Hosts: []string{"g1"}}}
	access, handler, cookies := usageServer(t, q, pools)
	today := time.Now().UTC()
	day := func(offset int) string { return today.AddDate(0, 0, offset).Format(time.DateOnly) }

	if rec, _ := usageGet(t, access, handler, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous usage = %d", rec.Code)
	}
	for _, query := range []string{"?group=week", "?from=2026-13-01", "?from=" + day(-400) + "&to=" + day(0),
		"?from=" + day(0) + "&to=" + day(-1), "?repository=bad", "?to=yesterday"} {
		if rec, _ := usageGet(t, access, handler, cookies["admin"], query); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", query, rec.Code)
		}
	}

	insertUsageJob(t, q, usageFinishedJob(1, "owner/app", "h1", today.Add(-48*time.Hour), time.Minute, time.Minute))
	gpu := usageFinishedJob(2, "owner/ml", "g1", today.Add(-48*time.Hour), 10*time.Minute, time.Hour)
	gpu.route = "runquill-1-1-pool__gpu__train"
	insertUsageJob(t, q, gpu)
	if err := rollupUsage(t.Context(), q, pools, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Finished today but not rolled up yet, and not yet released by its runner.
	open := usageFinishedJob(3, "owner/app", "h2", today, 30*time.Second, time.Minute)
	open.leased, open.cleaned = true, time.Time{}
	insertUsageJob(t, q, open)

	rec, byDay := usageGet(t, access, handler, cookies["admin"], "?from="+day(-6)+"&to="+day(0))
	if rec.Code != http.StatusOK {
		t.Fatalf("usage = %d %s", rec.Code, rec.Body)
	}
	if len(byDay.Series) != 7 || byDay.Totals.Jobs != 3 || byDay.Totals.RunnerSeconds != 60+3600+60 {
		t.Fatalf("by day = %+v", byDay)
	}
	if last := byDay.Series[6]; last.Key != day(0) || last.Jobs != 1 {
		t.Fatalf("today = %+v, want the unrolled job", last)
	}

	_, byPool := usageGet(t, access, handler, cookies["admin"], "?group=pool&from="+day(-6))
	if len(byPool.Series) != 2 || byPool.Series[0].Key != "default" || byPool.Series[0].Jobs != 2 ||
		byPool.Series[1].Key != "gpu" || byPool.Series[1].WaitP50Seconds < 600 || byPool.Series[1].WaitP50Seconds >= 900 {
		t.Fatalf("by pool = %+v", byPool.Series)
	}
	_, byHost := usageGet(t, access, handler, cookies["admin"], "?group=host&repository=Owner/App")
	if len(byHost.Series) != 2 || byHost.Totals.Jobs != 2 {
		t.Fatalf("by host = %+v", byHost)
	}
	_, byRepo := usageGet(t, access, handler, cookies["admin"], "?group=repository")
	if len(byRepo.Series) != 2 || byRepo.Series[0].Key != "owner/app" || byRepo.Series[0].Jobs != 2 {
		t.Fatalf("by repository = %+v", byRepo.Series)
	}

	// Rolling the open job up does not change what the endpoint reports.
	if _, err := q.Exec(`UPDATE jobs SET runner_name = NULL, runner_host = NULL, runner_cleaned_host = 'h2',
        runner_cleaned_at = changed_at WHERE job_id = 3`); err != nil {
		t.Fatal(err)
	}
	_, before := usageGet(t, access, handler, cookies["admin"], "?group=host")
	if err := rollupUsage(t.Context(), q, pools, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, after := usageGet(t, access, handler, cookies["admin"], "?group=host")
	if before.Totals != after.Totals || len(after.Series) != 3 {
		t.Fatalf("before %+v after %+v", before, after)
	}
}

func TestUsageSummaryWindows(t *testing.T) {
	q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
	t.Cleanup(q.close)
	insertUsageJob(t, q, usageFinishedJob(1, "owner/app", "h1", usageNow.Add(-2*time.Hour), time.Minute, time.Minute))
	insertUsageJob(t, q, usageFinishedJob(2, "owner/app", "h1", usageNow.Add(-3*24*time.Hour), time.Minute, 2*time.Minute))
	failed := usageJob{id: 3, repository: "owner/app", queued: usageNow.Add(-2 * time.Hour), finished: usageNow.Add(-time.Hour), failure: "x"}
	insertUsageJob(t, q, failed)
	if err := rollupUsage(t.Context(), q, nil, usageNow); err != nil {
		t.Fatal(err)
	}
	day, err := usageSummary(t.Context(), q, usageNow, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	week, err := usageSummary(t.Context(), q, usageNow, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if day.Jobs != 2 || day.Failed != 1 || day.RunnerSeconds != 60 || week.Jobs != 3 || week.RunnerSeconds != 180 {
		t.Fatalf("day %+v week %+v", day, week)
	}
	if _, err := usageSummary(t.Context(), q, usageNow, 8*24*time.Hour); err == nil {
		t.Fatal("a window beyond raw retention was accepted")
	}
}

func TestUsageSurvivesJobDeletionAndBoundsGroups(t *testing.T) {
	q := openUsageQueue(t, filepath.Join(t.TempDir(), "queue.sqlite3"))
	t.Cleanup(q.close)
	ctx := t.Context()
	insertUsageJob(t, q, usageFinishedJob(1, "owner/app", "h1", usageNow.Add(-8*24*time.Hour), time.Minute, time.Minute))
	if err := rollupUsage(ctx, q, nil, usageNow); err != nil {
		t.Fatal(err)
	}
	before := usageRows(t, q)
	// The scheduler's retention delete, then a rollup on the next day.
	s := scheduler{queue: q, runtime: &testRuntime{}, limit: 1}
	if err := s.tick(ctx, usageNow); err != nil {
		t.Fatal(err)
	}
	if err := rollupUsage(ctx, q, nil, usageNow.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var ledger int64
	q.QueryRow(`SELECT count(*) FROM usage_rolled`).Scan(&ledger)
	if after := usageRows(t, q); !maps.Equal(before, after) || len(after) != 1 || ledger != 0 {
		t.Fatalf("before %v after %v ledger %d", before, after, ledger)
	}

	day := time.Now().UTC().Format(time.DateOnly)
	for i := range usageMaxGroups + 1 {
		if _, err := q.Exec(`INSERT INTO usage_daily (day, repository, pool, host, jobs) VALUES (?, ?, '', '', 1)`,
			day, fmt.Sprintf("owner/r%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	access, handler, cookies := usageServer(t, q, nil)
	rec, response := usageGet(t, access, handler, cookies["admin"], "?group=repository&from="+day+"&to="+day)
	if rec.Code != http.StatusOK || len(response.Series) != usageMaxGroups || !response.Truncated ||
		response.Totals.Jobs != usageMaxGroups+1 {
		t.Fatalf("groups = %d truncated=%v totals=%+v", len(response.Series), response.Truncated, response.Totals)
	}
}
