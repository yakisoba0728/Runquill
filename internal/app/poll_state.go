package app

import (
	"context"
	"database/sql"
)

type repositoryPoll struct {
	repositoryID                int64
	checkedAt, fullAt, activeAt int64
	lastError                   string
}

func readRepositoryPolls(ctx context.Context, db *sql.DB, installationID int64) (map[int64]repositoryPoll, error) {
	rows, err := db.QueryContext(ctx, `SELECT repository_id, checked_at, full_at, active_at, last_error
        FROM repository_polls WHERE installation_id = ?`, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make(map[int64]repositoryPoll)
	for rows.Next() {
		var result repositoryPoll
		if err := rows.Scan(&result.repositoryID, &result.checkedAt, &result.fullAt, &result.activeAt, &result.lastError); err != nil {
			return nil, err
		}
		results[result.repositoryID] = result
	}
	return results, rows.Err()
}

func (q *queue) saveRepositoryPolls(ctx context.Context, installationID int64, results []repositoryPoll) error {
	tx, err := q.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO repository_polls
        (installation_id, repository_id, checked_at, full_at, active_at, last_error) VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT (installation_id, repository_id) DO UPDATE SET checked_at = excluded.checked_at,
        full_at = excluded.full_at, active_at = excluded.active_at, last_error = excluded.last_error`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, result := range results {
		if _, err := stmt.ExecContext(ctx, installationID, result.repositoryID, result.checkedAt, result.fullAt, result.activeAt, result.lastError); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (q *queue) recentRepositories(ctx context.Context, installationID, since int64) (map[int64]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT repository_id FROM jobs
        WHERE installation_id = ? AND changed_at >= ?`, installationID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		results[id] = true
	}
	return results, rows.Err()
}
