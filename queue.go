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
	runnerID                         int64
	retryAt                          int64
	retryReason                      string
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
	db, err := openQueueDB(path, false)
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
	hasReason, err := hasRetryReason(context.Background(), q.DB)
	if err == nil && !hasReason {
		_, err = q.Exec(`ALTER TABLE jobs ADD COLUMN retry_reason TEXT NOT NULL DEFAULT ''`)
	}
	if err != nil {
		q.close()
		return nil, fmt.Errorf("migrate queue: %w", err)
	}
	return q, nil
}

func openQueueDB(path string, readOnly bool) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)"}
	if readOnly {
		dsn.RawQuery += "&mode=ro"
	} else {
		dsn.RawQuery += "&_pragma=journal_mode(WAL)"
	}
	return sql.Open("sqlite", dsn.String())
}

func hasRetryReason(ctx context.Context, db *sql.DB) (bool, error) {
	var present bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (
        SELECT 1 FROM pragma_table_info('jobs') WHERE name = 'retry_reason'
    )`).Scan(&present)
	return present, err
}

func (q *queue) close() {
	q.Close()
	q.lock.Close()
}

func (q *queue) record(ctx context.Context, j job, now time.Time) (bool, error) {
	result, err := q.ExecContext(ctx, `
        INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT (job_id) DO UPDATE SET phase = excluded.phase,
            changed_at = excluded.changed_at, repository = excluded.repository
        WHERE excluded.phase > jobs.phase`,
		j.id, j.repository, j.repositoryID, j.installationID, j.route, j.phase, now.Unix())
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func (q *queue) jobs(ctx context.Context, condition string, args ...any) ([]job, error) {
	rows, err := q.QueryContext(ctx, `
        SELECT job_id, repository, repository_id, installation_id, route, phase, changed_at,
               coalesce(runner_name, ''), coalesce(runner_id, 0), coalesce(launched_at, 0), retry_at
        FROM jobs `+condition, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		var j job
		err := rows.Scan(&j.id, &j.repository, &j.repositoryID, &j.installationID, &j.route, &j.phase,
			&j.changedAt, &j.runnerName, &j.runnerID, &j.launchedAt, &j.retryAt)
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
