package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	panelHistoryPage    = 50
	panelHistoryMaxPage = 100
)

// panelHistoryJob is one row of the job history. The outcome is derived only
// from the phase, the failure reason and the runner lease: completed rows keep
// stale retry columns that say nothing about how the job ended.
type panelHistoryJob struct {
	ID          int64      `json:"id"`
	Repository  string     `json:"repository"`
	RunnerLabel string     `json:"runner_label"`
	Pool        string     `json:"pool,omitempty"`
	RunnerOS    string     `json:"runner_os,omitempty"`
	Host        string     `json:"host,omitempty"`
	Outcome     string     `json:"outcome"`
	Failure     string     `json:"failure,omitempty"`
	RetryReason string     `json:"retry_reason,omitempty"`
	RetryAt     *time.Time `json:"retry_at,omitempty"`
	QueuedAt    *time.Time `json:"queued_at,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	CleanedAt   *time.Time `json:"cleaned_at,omitempty"`
	WaitSeconds *int64     `json:"wait_seconds,omitempty"`
	URL         string     `json:"url,omitempty"`
}

type panelHistoryPageResponse struct {
	Jobs []panelHistoryJob `json:"jobs"`
	// Next is the cursor for the following (older) page, or zero on the last page.
	Next int64 `json:"next,omitempty"`
}

type panelHistoryFilter struct {
	repository   string
	outcome      string
	since, until int64
	before       int64
	limit        int
}

var errPanelJobNotWaiting = errors.New("This job is no longer waiting for a runner.")
var errPanelJobNoDelay = errors.New("This job has no retry delay to clear.")

func parsePanelHistoryFilter(query url.Values) (panelHistoryFilter, bool) {
	filter := panelHistoryFilter{limit: panelHistoryPage, repository: strings.ToLower(strings.TrimSpace(query.Get("repository"))),
		outcome: query.Get("outcome")}
	switch filter.outcome {
	case "", "waiting", "running", "finished", "failed":
	default:
		return filter, false
	}
	if filter.repository != "" && !repositoryName.MatchString(filter.repository) {
		return filter, false
	}
	for _, field := range []struct {
		name   string
		target *int64
	}{{"since", &filter.since}, {"until", &filter.until}, {"before", &filter.before}} {
		if raw := query.Get(field.name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				return filter, false
			}
			*field.target = value
		}
	}
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return filter, false
		}
		filter.limit = min(value, panelHistoryMaxPage)
	}
	return filter, true
}

func (p *panelState) jobHistory(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	filter, ok := parsePanelHistoryFilter(r.URL.Query())
	if !ok {
		http.Error(w, "Use a valid repository, outcome, time range, and page.", http.StatusBadRequest)
		return
	}
	var pools workerPools
	if p.operations != nil {
		pools = p.operations.current.Load().runners.pools
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	page, err := readPanelHistory(ctx, db, filter, pools, time.Now())
	if err != nil {
		http.Error(w, "Could not read the job history.", http.StatusServiceUnavailable)
		return
	}
	panelJSON(w, http.StatusOK, page)
}

// readPanelHistory pages newest first by the primary key, so each page reads at
// most the rows it skips past plus one. The scheduler deletes finished rows
// after seven days, which bounds the scan.
func readPanelHistory(ctx context.Context, db *sql.DB, filter panelHistoryFilter, pools workerPools, now time.Time) (panelHistoryPageResponse, error) {
	conditions := []string{"1 = 1"}
	var args []any
	if filter.before > 0 {
		conditions = append(conditions, "sequence < ?")
		args = append(args, filter.before)
	}
	if filter.repository != "" {
		conditions = append(conditions, "repository = ? COLLATE NOCASE")
		args = append(args, filter.repository)
	}
	switch filter.outcome {
	case "waiting":
		conditions = append(conditions, "phase = ? AND runner_name IS NULL")
		args = append(args, queued)
	case "running":
		conditions = append(conditions, "phase < ? AND (runner_name IS NOT NULL OR phase = ?)")
		args = append(args, completed, inProgress)
	case "finished":
		conditions = append(conditions, "phase = ? AND failure IS NULL")
		args = append(args, completed)
	case "failed":
		conditions = append(conditions, "phase = ? AND failure IS NOT NULL")
		args = append(args, completed)
	}
	queuedAt := "coalesce(discovered_at, github_created_at, changed_at)"
	if filter.since > 0 {
		conditions = append(conditions, queuedAt+" >= ?")
		args = append(args, filter.since)
	}
	if filter.until > 0 {
		conditions = append(conditions, queuedAt+" < ?")
		args = append(args, filter.until)
	}
	args = append(args, filter.limit+1)
	rows, err := db.QueryContext(ctx, `SELECT sequence, job_id, repository, route, phase, changed_at,
        coalesce(runner_name, ''), coalesce(runner_host, runner_cleaned_host, ''), retry_at, retry_reason,
        coalesce(failure, ''), coalesce(github_created_at, 0), coalesce(discovered_at, 0),
        coalesce(provisioned_at, 0), coalesce(github_started_at, 0), coalesce(runner_cleaned_at, 0)
        FROM jobs WHERE `+strings.Join(conditions, " AND ")+` ORDER BY sequence DESC LIMIT ?`, args...)
	if err != nil {
		return panelHistoryPageResponse{}, err
	}
	defer rows.Close()
	page := panelHistoryPageResponse{Jobs: make([]panelHistoryJob, 0, filter.limit)}
	var last int64
	for rows.Next() {
		var j job
		var sequence, cleanedAt int64
		if err := rows.Scan(&sequence, &j.id, &j.repository, &j.route, &j.phase, &j.changedAt, &j.runnerName,
			&j.runnerHost, &j.retryAt, &j.retryReason, &j.failure, &j.githubCreatedAt, &j.discoveredAt,
			&j.provisionedAt, &j.githubStartedAt, &cleanedAt); err != nil {
			return panelHistoryPageResponse{}, err
		}
		if len(page.Jobs) == filter.limit {
			page.Next = last
			break
		}
		last = sequence
		page.Jobs = append(page.Jobs, panelHistoryRow(j, cleanedAt, pools, now))
	}
	return page, rows.Err()
}

func panelHistoryRow(j job, cleanedAt int64, pools workerPools, now time.Time) panelHistoryJob {
	pool, _ := pools.forJob(j)
	osName, _ := routeOS(j.route)
	row := panelHistoryJob{ID: j.id, Repository: j.repository, RunnerLabel: j.route, Pool: pool, RunnerOS: osName, Host: j.runnerHost,
		URL: jobURL(j), StartedAt: statusTime(j.githubStartedAt), CleanedAt: statusTime(cleanedAt)}
	queuedAt := j.discoveredAt
	if queuedAt == 0 {
		queuedAt = j.githubCreatedAt
	}
	row.QueuedAt = statusTime(queuedAt)
	switch {
	case j.phase == completed && j.failure != "":
		row.Outcome, row.Failure = "failed", j.failure
	case j.phase == completed:
		row.Outcome = "finished"
	case j.runnerName != "" || j.phase == inProgress:
		row.Outcome = "running"
	default:
		row.Outcome = "waiting"
		row.RetryReason = j.retryReason
		if j.retryAt > now.Unix() {
			row.RetryAt = statusTime(j.retryAt)
		}
	}
	if j.phase == completed {
		row.FinishedAt = statusTime(j.changedAt)
	}
	waitEnd := j.provisionedAt
	if waitEnd == 0 && row.Outcome == "waiting" {
		waitEnd = now.Unix()
	}
	if waitEnd == 0 && row.Outcome == "failed" {
		waitEnd = j.changedAt
	}
	row.WaitSeconds = elapsedSeconds(queuedAt, waitEnd)
	return row
}

func panelJobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// cancelJob fails a waiting job in one guarded UPDATE. The scheduler claims a
// job with the same phase and lease guard, so whichever statement runs first
// wins and the other changes nothing.
func (a *panelAccess) cancelJob(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	id, ok := panelJobID(w, r)
	if !ok {
		return
	}
	actor := "GitHub ID " + strconv.FormatInt(user.GitHubID, 10)
	if user.Login != "" {
		actor = "@" + user.Login
	}
	ctx := r.Context()
	err := a.change(ctx, user, "job.cancel", strconv.FormatInt(id, 10), func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE jobs SET phase = ?, changed_at = ?, retry_at = 0, failure = ?
            WHERE job_id = ? AND phase = ? AND runner_name IS NULL`,
			completed, time.Now().Unix(), "cancelled by "+actor, id, queued)
		return panelJobChanged(result, err)
	})
	a.finishJobChange(w, err)
}

// retryJob clears a waiting job's retry delay so the next scheduler pass can try it.
func (a *panelAccess) retryJob(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	id, ok := panelJobID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	err := a.change(ctx, user, "job.retry", strconv.FormatInt(id, 10), func(tx *sql.Tx) error {
		var waiting bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE job_id = ? AND phase = ? AND runner_name IS NULL)`,
			id, queued).Scan(&waiting)
		if err != nil {
			return err
		}
		if !waiting {
			return errPanelJobNotWaiting
		}
		result, err := tx.ExecContext(ctx, `UPDATE jobs SET retry_at = 0
            WHERE job_id = ? AND phase = ? AND runner_name IS NULL AND retry_at > ?`, id, queued, time.Now().Unix())
		err = panelJobChanged(result, err)
		if errors.Is(err, errPanelJobNotWaiting) {
			return errPanelJobNoDelay
		}
		return err
	})
	a.finishJobChange(w, err)
}

func panelJobChanged(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return errPanelJobNotWaiting
	}
	return err
}

func (a *panelAccess) finishJobChange(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPanelJobNotWaiting):
		http.Error(w, "This job is no longer waiting for a runner.", http.StatusConflict)
	case errors.Is(err, errPanelJobNoDelay):
		http.Error(w, "This job has no retry delay to clear.", http.StatusConflict)
	case errors.Is(err, errPanelUnauthorized):
		http.Error(w, "Your panel permissions changed. Reload and try again.", http.StatusForbidden)
	case err != nil:
		http.Error(w, "Could not update the job.", http.StatusServiceUnavailable)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
