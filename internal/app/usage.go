package app

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Usage statistics outlive the seven-day jobs table through a daily rollup.
//
// A job is folded into usage_daily exactly once, when it is completed and its
// runner lease has been released. The scheduler keeps completed jobs until
// usage_rolled records them. This ledger prevents a crash, restart or later
// change to a job from counting it twice. Ledger rows are pruned after their
// jobs are deleted. usage_meta.floor is a scan hint: every job at or below it
// is already folded or deleted. Sequence numbers are reused after the newest
// rows are deleted, so floor_job records the job at the floor and a mismatch
// restarts the scan.
//
// Rollup rows are kept for usageRetentionDays UTC days.
const (
	usageRetentionDays = 400
	usageMaxRangeDays  = 400
	usageMaxGroups     = 500
	usageRollupBatch   = 5000
	// usageRawLimit bounds the unrolled jobs read for one request; the jobs
	// table only holds about a week, so it is a guard rather than a page size.
	usageRawLimit = 50_000
	// usageSummaryMaxWindow is how far back raw jobs reach before deletion.
	usageSummaryMaxWindow = 7 * 24 * time.Hour
)

// usageWaitBounds are the exclusive upper bounds, in seconds, of the queue-wait
// histogram buckets; the last bucket is open. They are a stored format: rows
// written with one set of bounds cannot be read with another, so never change
// them in place.
var usageWaitBounds = []int64{5, 10, 15, 30, 45, 60, 90, 120, 180, 300, 450, 600, 900,
	1200, 1800, 2700, 3600, 5400, 7200, 10800, 14400, 21600, 43200, 86400}

const usageBuckets = 25

var usageWaitColumns = func() []string {
	if len(usageWaitBounds)+1 != usageBuckets {
		panic("usage wait buckets do not match their bounds")
	}
	columns := make([]string, usageBuckets)
	for i := range columns {
		columns[i] = fmt.Sprintf("w%02d", i)
	}
	return columns
}()

func migrateUsage(ctx context.Context, db *sql.DB) error {
	buckets := strings.Join(usageWaitColumns, " INTEGER NOT NULL DEFAULT 0,\n    ") + " INTEGER NOT NULL DEFAULT 0"
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS usage_daily (
    day TEXT NOT NULL,
    repository TEXT NOT NULL,
    pool TEXT NOT NULL,
    host TEXT NOT NULL,
    jobs INTEGER NOT NULL DEFAULT 0,
    failed INTEGER NOT NULL DEFAULT 0,
    runner_seconds INTEGER NOT NULL DEFAULT 0,
    wait_count INTEGER NOT NULL DEFAULT 0,
    wait_seconds INTEGER NOT NULL DEFAULT 0,
    `+buckets+`,
    PRIMARY KEY (day, repository, pool, host)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS usage_daily_repository ON usage_daily(repository, day);
CREATE TABLE IF NOT EXISTS usage_rolled (job_id INTEGER PRIMARY KEY);
CREATE TABLE IF NOT EXISTS usage_meta (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    floor INTEGER NOT NULL,
    floor_job INTEGER NOT NULL,
    swept_day TEXT NOT NULL
);
INSERT OR IGNORE INTO usage_meta (id, floor, floor_job, swept_day) VALUES (1, 0, 0, '');
`+jobResourcesSchema)
	return err
}

// usageTotals aggregates finished jobs. Failed counts jobs Runquill gave up
// on or that were cancelled (the failure column), not GitHub conclusions.
// Runner seconds run from provisioning to runner cleanup. Wait is the
// history view's wait: queued to provisioned, or to the failure for jobs that
// never got a runner; jobs without a known wait are left out of it.
type usageTotals struct {
	Jobs           int64 `json:"jobs"`
	Failed         int64 `json:"failed"`
	RunnerSeconds  int64 `json:"runner_seconds"`
	WaitCount      int64 `json:"wait_count"`
	WaitAvgSeconds int64 `json:"wait_avg_seconds"`
	WaitP50Seconds int64 `json:"wait_p50_seconds"`
	WaitP95Seconds int64 `json:"wait_p95_seconds"`

	waitSum int64
	waits   [usageBuckets]int64
}

type usageGroup struct {
	Key string `json:"key"`
	usageTotals
}

type usageResponse struct {
	From   string       `json:"from"`
	To     string       `json:"to"`
	Group  string       `json:"group"`
	Totals usageTotals  `json:"totals"`
	Series []usageGroup `json:"series"`
	// Truncated means Series was cut to the busiest groups; Totals cover all.
	Truncated bool `json:"truncated,omitempty"`
}

// usageSample is one finished job as the rollup and the open window see it.
type usageSample struct {
	day, repository, pool, host string
	failed                      bool
	runnerSeconds               int64
	wait                        *int64
}

func usageWaitBucket(seconds int64) int {
	index, _ := slices.BinarySearch(usageWaitBounds, seconds+1)
	return index
}

func (t *usageTotals) add(s usageSample) {
	t.Jobs++
	if s.failed {
		t.Failed++
	}
	t.RunnerSeconds += s.runnerSeconds
	if s.wait != nil {
		t.WaitCount++
		t.waitSum += *s.wait
		t.waits[usageWaitBucket(*s.wait)]++
	}
}

func (t *usageTotals) merge(other usageTotals) {
	t.Jobs += other.Jobs
	t.Failed += other.Failed
	t.RunnerSeconds += other.RunnerSeconds
	t.WaitCount += other.WaitCount
	t.waitSum += other.waitSum
	for i, count := range other.waits {
		t.waits[i] += count
	}
}

// finish fills the derived wait fields. Percentiles are interpolated inside
// their bucket, so they are exact to the bucket's width.
func (t *usageTotals) finish() {
	if t.WaitCount == 0 {
		return
	}
	t.WaitAvgSeconds = t.waitSum / t.WaitCount
	t.WaitP50Seconds = t.percentile(0.50)
	t.WaitP95Seconds = t.percentile(0.95)
}

func (t *usageTotals) percentile(p float64) int64 {
	rank := int64(math.Ceil(p*float64(t.WaitCount) - 1e-9))
	rank = max(1, min(rank, t.WaitCount))
	var below int64
	for i, count := range t.waits {
		if below+count < rank {
			below += count
			continue
		}
		var low int64
		if i > 0 {
			low = usageWaitBounds[i-1]
		}
		if i == len(usageWaitBounds) {
			return low
		}
		width := usageWaitBounds[i] - low
		return low + width*(rank-below)/(count+1)
	}
	return 0
}

const usageJobColumns = `sequence, job_id, repository, route, phase, changed_at, coalesce(runner_name, ''),
    coalesce(runner_host, runner_cleaned_host, ''), retry_at, retry_reason, coalesce(failure, ''),
    coalesce(github_created_at, 0), coalesce(discovered_at, 0), coalesce(provisioned_at, 0),
    coalesce(github_started_at, 0), coalesce(runner_cleaned_at, 0)`

type usageRow struct {
	sequence, cleanedAt int64
	job                 job
}

func scanUsageRows(rows *sql.Rows) ([]usageRow, error) {
	defer rows.Close()
	var out []usageRow
	for rows.Next() {
		var r usageRow
		j := &r.job
		if err := rows.Scan(&r.sequence, &j.id, &j.repository, &j.route, &j.phase, &j.changedAt, &j.runnerName,
			&j.runnerHost, &j.retryAt, &j.retryReason, &j.failure, &j.githubCreatedAt, &j.discoveredAt,
			&j.provisionedAt, &j.githubStartedAt, &r.cleanedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// sample derives pool, host, outcome and wait exactly as the job history does.
func (r usageRow) sample(pools workerPools) usageSample {
	j := r.job
	row := panelHistoryRow(j, r.cleanedAt, pools, time.Unix(j.changedAt, 0))
	s := usageSample{day: usageDay(j.changedAt), repository: strings.ToLower(j.repository), pool: row.Pool,
		host: row.Host, failed: row.Outcome == "failed", wait: row.WaitSeconds}
	if j.provisionedAt > 0 {
		end := r.cleanedAt
		if end == 0 {
			end = j.changedAt
		}
		s.runnerSeconds = max(0, end-j.provisionedAt)
	}
	return s
}

func usageDay(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(time.DateOnly)
}

type usageKey struct{ day, repository, pool, host string }

// rollupUsage folds final jobs that are not yet in the ledger into usage_daily.
// It must run before the scheduler deletes old jobs; each call handles at most
// usageRollupBatch jobs and continues on the next call. Once per UTC day it
// rescans from the start (sequence numbers can be reused after deletes), prunes
// the ledger and drops rollup rows past retention.
func rollupUsage(ctx context.Context, q *queue, pools workerPools, now time.Time) error {
	tx, err := q.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	today := usageDay(now.Unix())
	var floor, floorJob, top, atFloor int64
	var swept string
	if err := tx.QueryRowContext(ctx, `SELECT floor, floor_job, swept_day FROM usage_meta WHERE id = 1`).Scan(&floor, &floorJob, &swept); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence), 0),
        coalesce((SELECT job_id FROM jobs WHERE sequence = ?), 0) FROM jobs`, floor).Scan(&top, &atFloor); err != nil {
		return err
	}
	sweep := swept != today
	if sweep || top < floor || atFloor != 0 && atFloor != floorJob {
		floor = 0
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+usageJobColumns+` FROM jobs
        WHERE sequence > ? AND phase = ? AND runner_name IS NULL
            AND NOT EXISTS (SELECT 1 FROM usage_rolled WHERE usage_rolled.job_id = jobs.job_id)
        ORDER BY sequence LIMIT ?`, floor, completed, usageRollupBatch)
	if err != nil {
		return err
	}
	final, err := scanUsageRows(rows)
	if err != nil {
		return err
	}
	groups := make(map[usageKey]*usageTotals)
	for _, r := range final {
		s := r.sample(pools)
		key := usageKey{s.day, s.repository, s.pool, s.host}
		if groups[key] == nil {
			groups[key] = &usageTotals{}
		}
		groups[key].add(s)
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_rolled (job_id) VALUES (?)`, r.job.id); err != nil {
			return err
		}
	}
	if err := upsertUsage(ctx, tx, groups); err != nil {
		return err
	}

	var pending int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(min(sequence), 0) FROM jobs
        WHERE sequence > ? AND (phase < ? OR runner_name IS NOT NULL)`, floor, completed).Scan(&pending); err != nil {
		return err
	}
	next := top
	if pending > 0 {
		next = pending - 1
	}
	if len(final) == usageRollupBatch {
		next = min(next, final[len(final)-1].sequence)
	}
	if sweep {
		cutoff := now.UTC().AddDate(0, 0, -usageRetentionDays+1).Format(time.DateOnly)
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_daily WHERE day < ?`, cutoff); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_rolled
            WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE jobs.job_id = usage_rolled.job_id)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM job_resources
            WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE jobs.job_id = job_resources.job_id)`); err != nil {
			return err
		}
		// A full batch leaves the rescan unfinished; sweep again next call.
		if len(final) < usageRollupBatch {
			swept = today
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_meta SET floor = ?, swept_day = ?,
        floor_job = coalesce((SELECT job_id FROM jobs WHERE sequence = ?), 0) WHERE id = 1`, next, swept, next); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertUsage(ctx context.Context, tx *sql.Tx, groups map[usageKey]*usageTotals) error {
	if len(groups) == 0 {
		return nil
	}
	columns := append([]string{"day", "repository", "pool", "host", "jobs", "failed", "runner_seconds",
		"wait_count", "wait_seconds"}, usageWaitColumns...)
	updates := make([]string, 0, len(columns)-4)
	for _, column := range columns[4:] {
		updates = append(updates, column+" = usage_daily."+column+" + excluded."+column)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO usage_daily (`+strings.Join(columns, ", ")+`)
        VALUES (?`+strings.Repeat(", ?", len(columns)-1)+`)
        ON CONFLICT (day, repository, pool, host) DO UPDATE SET `+strings.Join(updates, ", "))
	if err != nil {
		return err
	}
	defer statement.Close()
	for key, t := range groups {
		args := []any{key.day, key.repository, key.pool, key.host, t.Jobs, t.Failed, t.RunnerSeconds, t.WaitCount, t.waitSum}
		for _, count := range t.waits {
			args = append(args, count)
		}
		if _, err := statement.ExecContext(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

// usageSummary totals jobs that finished in the last window, read from the raw
// jobs table so a rolling 24 hours is exact rather than day-aligned. Raw jobs
// are kept for seven days, which bounds the window.
func usageSummary(ctx context.Context, q *queue, now time.Time, window time.Duration) (usageTotals, error) {
	if window <= 0 || window > usageSummaryMaxWindow {
		return usageTotals{}, fmt.Errorf("usage window must be between 1s and %s", usageSummaryMaxWindow)
	}
	rows, err := q.QueryContext(ctx, `SELECT `+usageJobColumns+` FROM jobs
        WHERE phase = ? AND changed_at > ? AND changed_at <= ? LIMIT ?`,
		completed, now.Add(-window).Unix(), now.Unix(), usageRawLimit)
	if err != nil {
		return usageTotals{}, err
	}
	finished, err := scanUsageRows(rows)
	if err != nil {
		return usageTotals{}, err
	}
	var totals usageTotals
	for _, r := range finished {
		totals.add(r.sample(nil))
	}
	totals.finish()
	return totals, nil
}

type usageQuery struct {
	from, to   time.Time
	group      string
	repository string
}

var errUsageQuery = errors.New("Use dates as YYYY-MM-DD at most 400 days apart, group day, repository, pool or host, and a valid repository.")

// parseUsageQuery reads the GET /api/usage parameters, which the MCP
// usage_summary tool also builds.
func parseUsageQuery(query url.Values, now time.Time) (usageQuery, error) {
	u := usageQuery{group: cmp.Or(query.Get("group"), "day"),
		repository: strings.ToLower(strings.TrimSpace(query.Get("repository")))}
	switch u.group {
	case "day", "repository", "pool", "host":
	default:
		return u, errUsageQuery
	}
	if u.repository != "" && !repositoryName.MatchString(u.repository) {
		return u, errUsageQuery
	}
	today := now.UTC().Truncate(24 * time.Hour)
	var err error
	u.to = today
	if raw := query.Get("to"); raw != "" {
		if u.to, err = time.Parse(time.DateOnly, raw); err != nil {
			return u, errUsageQuery
		}
	}
	u.from = u.to.AddDate(0, 0, -29)
	if raw := query.Get("from"); raw != "" {
		if u.from, err = time.Parse(time.DateOnly, raw); err != nil {
			return u, errUsageQuery
		}
	}
	if u.to.Before(u.from) || u.to.Sub(u.from) >= usageMaxRangeDays*24*time.Hour {
		return u, errUsageQuery
	}
	return u, nil
}

// readUsage merges rolled-up days with finished jobs the rollup has not folded
// yet. Both are read in one transaction, so a concurrent rollup cannot put a
// job in both or neither.
func readUsage(ctx context.Context, db *sql.DB, u usageQuery, pools workerPools) (usageResponse, error) {
	response := usageResponse{From: u.from.Format(time.DateOnly), To: u.to.Format(time.DateOnly), Group: u.group,
		Series: []usageGroup{}}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return response, err
	}
	defer tx.Rollback()
	sums := make([]string, 0, len(usageWaitColumns)+5)
	for _, column := range append([]string{"jobs", "failed", "runner_seconds", "wait_count", "wait_seconds"}, usageWaitColumns...) {
		sums = append(sums, "sum("+column+")")
	}
	filter, args := "", []any{response.From, response.To}
	if u.repository != "" {
		filter, args = " AND repository = ?", append(args, u.repository)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+u.group+`, `+strings.Join(sums, ", ")+` FROM usage_daily
        WHERE day >= ? AND day <= ?`+filter+` GROUP BY `+u.group, args...)
	if err != nil {
		return response, err
	}
	groups := make(map[string]*usageTotals)
	for rows.Next() {
		var key string
		t := &usageTotals{}
		target := []any{&key, &t.Jobs, &t.Failed, &t.RunnerSeconds, &t.WaitCount, &t.waitSum}
		for i := range t.waits {
			target = append(target, &t.waits[i])
		}
		if err := rows.Scan(target...); err != nil {
			rows.Close()
			return response, err
		}
		groups[key] = t
	}
	if err := rows.Close(); err != nil {
		return response, err
	}
	if err := rows.Err(); err != nil {
		return response, err
	}

	filter, args = "", []any{completed, u.from.Unix(), u.to.AddDate(0, 0, 1).Unix()}
	if u.repository != "" {
		filter, args = " AND repository = ? COLLATE NOCASE", append(args, u.repository)
	}
	rows, err = tx.QueryContext(ctx, `SELECT `+usageJobColumns+` FROM jobs
        WHERE phase = ? AND changed_at >= ? AND changed_at < ?`+filter+`
            AND NOT EXISTS (SELECT 1 FROM usage_rolled WHERE usage_rolled.job_id = jobs.job_id)
        LIMIT ?`, append(args, usageRawLimit)...)
	if err != nil {
		return response, err
	}
	open, err := scanUsageRows(rows)
	if err != nil {
		return response, err
	}
	if err := tx.Commit(); err != nil {
		return response, err
	}
	for _, r := range open {
		s := r.sample(pools)
		key := map[string]string{"day": s.day, "repository": s.repository, "pool": s.pool, "host": s.host}[u.group]
		if groups[key] == nil {
			groups[key] = &usageTotals{}
		}
		groups[key].add(s)
	}
	if u.group == "day" {
		for day := u.from; !day.After(u.to); day = day.AddDate(0, 0, 1) {
			if key := day.Format(time.DateOnly); groups[key] == nil {
				groups[key] = &usageTotals{}
			}
		}
	}
	for key, t := range groups {
		response.Totals.merge(*t)
		t.finish()
		response.Series = append(response.Series, usageGroup{Key: key, usageTotals: *t})
	}
	response.Totals.finish()
	if u.group == "day" {
		slices.SortFunc(response.Series, func(a, b usageGroup) int { return strings.Compare(a.Key, b.Key) })
	} else {
		slices.SortFunc(response.Series, func(a, b usageGroup) int {
			return cmp.Or(cmp.Compare(b.Jobs, a.Jobs), strings.Compare(a.Key, b.Key))
		})
	}
	if len(response.Series) > usageMaxGroups {
		response.Series, response.Truncated = response.Series[:usageMaxGroups], true
	}
	return response, nil
}

// usagePools is the live worker pool configuration, as the job history uses.
func (p *panelState) usagePools() workerPools {
	if p == nil || p.operations == nil {
		return nil
	}
	return p.operations.current.Load().runners.pools
}

func registerUsageRoutes(mux *http.ServeMux, a *panelAccess, pools func() workerPools) {
	mux.HandleFunc("GET /api/usage", a.require("fleet:read", func(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
		u, err := parseUsageQuery(r.URL.Query(), time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		response, err := readUsage(ctx, a.db, u, pools())
		if err != nil {
			http.Error(w, "Could not read usage statistics.", http.StatusServiceUnavailable)
			return
		}
		panelJSON(w, http.StatusOK, response)
	}))
	registerRightsizingRoute(mux, a, pools)
}
