package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type phase int

const (
	queued phase = iota
	inProgress
	completed
)

var phases = map[string]phase{"queued": queued, "in_progress": inProgress, "completed": completed}

type job struct {
	id, repositoryID, installationID int64
	repository, route                string
	phase                            phase
	changedAt, launchedAt            int64
	runnerName                       string
	runnerHost                       string
	runnerID                         int64
	retryAt                          int64
	retryReason                      string
	githubCreatedAt, discoveredAt    int64
	provisionedAt, githubStartedAt   int64
	discoverySource                  string
}

type queue struct {
	*sql.DB
	lock *os.File
}

// Keep the existing SQLite schema so an upgrade can resume pending jobs and runner leases.
const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    sequence INTEGER PRIMARY KEY,
    job_id INTEGER NOT NULL UNIQUE,
    repository TEXT NOT NULL,
    repository_id INTEGER NOT NULL,
    installation_id INTEGER NOT NULL,
    route TEXT NOT NULL,
    phase INTEGER NOT NULL,
    changed_at INTEGER NOT NULL,
    runner_name TEXT UNIQUE,
    runner_id INTEGER,
    launched_at INTEGER,
    retry_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS jobs_waiting ON jobs(sequence)
    WHERE phase = 0 AND runner_name IS NULL;
CREATE INDEX IF NOT EXISTS jobs_leased ON jobs(runner_name)
    WHERE runner_name IS NOT NULL;`

func openQueue(path string) (*queue, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("lock queue %s: %w", path, err)
	}
	db, err := openQueueDB(path, "rwc")
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	q := &queue{DB: db, lock: lock}
	if _, err := q.Exec(schema); err != nil {
		q.close()
		return nil, fmt.Errorf("initialize queue: %w", err)
	}
	if err := q.migrate(); err != nil {
		q.close()
		return nil, fmt.Errorf("migrate queue: %w", err)
	}
	return q, nil
}

func (q *queue) migrate() error {
	for _, column := range []struct{ name, definition string }{
		{"retry_reason", "TEXT NOT NULL DEFAULT ''"},
		{"runner_host", "TEXT"},
		{"github_created_at", "INTEGER"},
		{"discovered_at", "INTEGER"},
		{"provisioned_at", "INTEGER"},
		{"github_started_at", "INTEGER"},
		{"discovery_source", "TEXT"},
		{"runner_cleaned_at", "INTEGER"},
		{"runner_cleaned_host", "TEXT"},
	} {
		present, err := hasJobColumn(context.Background(), q.DB, column.name)
		if err != nil {
			return err
		}
		if !present {
			if _, err := q.Exec("ALTER TABLE jobs ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	_, err := q.Exec(`CREATE TABLE IF NOT EXISTS docker_hosts (
        host TEXT PRIMARY KEY NOT NULL,
        cordoned INTEGER NOT NULL DEFAULT 0,
        untracked_slots INTEGER CHECK (untracked_slots >= 0)
    );
    CREATE TABLE IF NOT EXISTS repository_polls (
        installation_id INTEGER NOT NULL,
        repository_id INTEGER NOT NULL,
        checked_at INTEGER NOT NULL,
        full_at INTEGER NOT NULL,
        active_at INTEGER NOT NULL,
        last_error TEXT NOT NULL,
        PRIMARY KEY (installation_id, repository_id)
    );
    CREATE INDEX IF NOT EXISTS jobs_recent ON jobs(installation_id, changed_at);
    CREATE INDEX IF NOT EXISTS jobs_cleaned ON jobs(runner_cleaned_at) WHERE runner_cleaned_at IS NOT NULL;
    CREATE INDEX IF NOT EXISTS jobs_first_workflow ON jobs(runner_cleaned_at)
        WHERE route GLOB '*-first-job'`)
	return err
}

func openQueueDB(path, mode string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)&mode=" + mode}
	if mode == "rwc" {
		dsn.RawQuery += "&_pragma=journal_mode(WAL)"
	}
	return sql.Open("sqlite", dsn.String())
}

func hasRetryReason(ctx context.Context, db *sql.DB) (bool, error) {
	return hasJobColumn(ctx, db, "retry_reason")
}

func hasJobColumn(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var present bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (
        SELECT 1 FROM pragma_table_info('jobs') WHERE name = ?
    )`, name).Scan(&present)
	return present, err
}

func (q *queue) close() {
	q.Close()
	q.lock.Close()
}

func (q *queue) record(ctx context.Context, j job, now time.Time) (bool, error) {
	var discoveredAt int64
	var source string
	if j.phase == queued && j.discoverySource != "" {
		discoveredAt, source = now.Unix(), j.discoverySource
	}
	result, err := q.ExecContext(ctx, `
        INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at,
                          github_created_at, discovered_at, discovery_source, github_started_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, nullif(?, 0), nullif(?, 0), nullif(?, ''), nullif(?, 0))
        ON CONFLICT (job_id) DO UPDATE SET phase = excluded.phase,
            changed_at = excluded.changed_at, repository = excluded.repository,
            github_created_at = coalesce(jobs.github_created_at, excluded.github_created_at),
            github_started_at = coalesce(jobs.github_started_at, excluded.github_started_at)
        WHERE excluded.phase > jobs.phase`,
		j.id, j.repository, j.repositoryID, j.installationID, j.route, j.phase, now.Unix(),
		j.githubCreatedAt, discoveredAt, source, j.githubStartedAt)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func (q *queue) jobs(ctx context.Context, condition string, args ...any) ([]job, error) {
	rows, err := q.QueryContext(ctx, `
        SELECT job_id, repository, repository_id, installation_id, route, phase, changed_at,
               coalesce(runner_name, ''), coalesce(runner_host, ''), coalesce(runner_id, 0),
               coalesce(launched_at, 0), retry_at
        FROM jobs `+condition, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		var j job
		err := rows.Scan(&j.id, &j.repository, &j.repositoryID, &j.installationID, &j.route, &j.phase,
			&j.changedAt, &j.runnerName, &j.runnerHost, &j.runnerID, &j.launchedAt, &j.retryAt)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (q *queue) advance(ctx context.Context, id int64, p phase, now time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE jobs SET phase = ?, changed_at = ?
        WHERE job_id = ? AND phase < ?`, p, now.Unix(), id, p)
	return err
}

func (q *queue) advanceGitHubJob(ctx context.Context, id int64, p phase, now time.Time, observed workflowJob) error {
	_, err := q.ExecContext(ctx, `UPDATE jobs SET phase = max(phase, ?),
        changed_at = CASE WHEN phase < ? THEN ? ELSE changed_at END,
        github_created_at = coalesce(github_created_at, nullif(?, 0)),
        github_started_at = coalesce(github_started_at, nullif(?, 0))
        WHERE job_id = ?`, p, p, now.Unix(), githubTimestamp(observed.CreatedAt),
		githubTimestamp(observed.StartedAt), id)
	return err
}
