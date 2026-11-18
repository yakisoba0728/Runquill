package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
)

const (
	retryDelay      = 15 * time.Second
	idleTimeout     = 5 * time.Minute
	completionGrace = 30 * time.Second
)

type runnerRuntime interface {
	containers(context.Context) (map[string]string, error)
	start(context.Context, string, string) error
	remove(context.Context, string) error
}

type scheduler struct {
	queue   *queue
	github  *github
	runtime runnerRuntime
	limit   int
}

func (s *scheduler) tick(ctx context.Context, now time.Time) error {
	containers, err := s.runtime.containers(ctx)
	if err != nil {
		return err
	}
	active, err := s.queue.jobs(ctx, "WHERE runner_name IS NOT NULL")
	if err != nil {
		return err
	}
	for _, j := range active {
		if j.retryAt > now.Unix() {
			continue
		}
		if err := s.reconcile(ctx, j, containers, now); err != nil {
			if err := s.retry(ctx, j, now, "reconcile runner", err); err != nil {
				return err
			}
		}
	}

	_, err = s.queue.ExecContext(ctx, `UPDATE jobs SET phase = ?, changed_at = ?
        WHERE runner_name IS NULL AND phase = ? AND changed_at <= ?`,
		completed, now.Unix(), queued, now.Add(-24*time.Hour).Unix())
	if err != nil {
		return err
	}
	_, err = s.queue.ExecContext(ctx, "DELETE FROM jobs WHERE runner_name IS NULL AND changed_at < ?",
		now.Add(-7*24*time.Hour).Unix())
	if err != nil {
		return err
	}

	var occupied int
	// Charge Docker capacity for containers that can run, even if the queue volume was replaced.
	for _, state := range containers {
		if runnerOccupiesSlot(state) {
			occupied++
		}
	}
	if occupied >= s.limit {
		return nil
	}
	waiting, err := s.queue.jobs(ctx, `
        WHERE phase = 0 AND runner_name IS NULL AND retry_at <= ?
        ORDER BY sequence LIMIT ?`, now.Unix(), s.limit-occupied)
	if err != nil {
		return err
	}
	for _, j := range waiting {
		if err := s.launch(ctx, j, now); err != nil {
			if err := s.retry(ctx, j, now, "provision runner", err); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scheduler) retry(ctx context.Context, j job, now time.Time, operation string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	slog.Warn(operation, "repository", j.repository, "job", j.id, "error", cause)
	retryAt := now.Add(retryDelay)
	var apiErr *githubError
	if errors.As(cause, &apiErr) && apiErr.retryAt.After(retryAt) {
		retryAt = apiErr.retryAt
	}
	_, err := s.queue.ExecContext(ctx, "UPDATE jobs SET retry_at = ?, retry_reason = ? WHERE job_id = ?",
		retryAt.Unix(), retryMessage(operation, cause), j.id)
	return err
}

func retryMessage(operation string, cause error) string {
	text := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, cause.Error())
	detail := []rune(strings.Join(strings.Fields(text), " "))
	if len(detail) > 240 {
		detail = append(detail[:240], '…')
	}
	return operation + ": " + string(detail)
}

func (s *scheduler) launch(ctx context.Context, j job, now time.Time) error {
	token, err := s.github.token(ctx, j)
	if err != nil {
		return err
	}
	eligible, err := s.github.eligible(ctx, j, token)
	if err != nil {
		return err
	}
	if !eligible {
		return s.queue.advance(ctx, j.id, completed, now)
	}
	status, err := s.github.jobStatus(ctx, j, token)
	if err != nil {
		return err
	}
	if status != "queued" {
		if p, known := phases[status]; known {
			return s.queue.advance(ctx, j.id, p, now)
		}
		return fmt.Errorf("waiting for GitHub to queue job, status %q", status)
	}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := fmt.Sprintf("runquill-%d-%s", j.id, hex.EncodeToString(nonce[:]))
	claim, err := s.queue.ExecContext(ctx, `
        UPDATE jobs SET runner_name = ?, launched_at = ?
        WHERE job_id = ? AND phase = ? AND runner_name IS NULL`, name, now.Unix(), j.id, queued)
	if err != nil {
		return err
	}
	count, err := claim.RowsAffected()
	if err != nil || count == 0 {
		return err
	}
	runnerID, jit, err := s.github.provision(ctx, j, name, token)
	if err != nil {
		return err
	}
	_, err = s.queue.ExecContext(ctx, "UPDATE jobs SET runner_id = ? WHERE job_id = ?", runnerID, j.id)
	if err != nil {
		return err
	}
	var current phase
	if err := s.queue.QueryRowContext(ctx, "SELECT phase FROM jobs WHERE job_id = ?", j.id).Scan(&current); err != nil {
		return err
	}
	if current == queued {
		if err := s.runtime.start(ctx, name, jit); err != nil {
			return err
		}
		slog.Info("runner started", "runner", name, "repository", j.repository, "job", j.id)
		_, err = s.queue.ExecContext(ctx, "UPDATE jobs SET retry_at = 0, retry_reason = '' WHERE job_id = ?", j.id)
		return err
	}
	return nil
}

func (s *scheduler) reconcile(ctx context.Context, j job, containers map[string]string, now time.Time) error {
	state := containers[j.runnerName]
	if state == "running" || state == "paused" || state == "restarting" {
		switch j.phase {
		case completed:
			if now.Unix() < j.changedAt+int64(completionGrace.Seconds()) {
				return nil
			}
		default:
			lastSeen := max(j.launchedAt, j.changedAt)
			if now.Unix() < lastSeen+int64(idleTimeout.Seconds()) {
				return nil
			}
			token, err := s.github.token(ctx, j)
			if err != nil {
				return err
			}
			status, err := s.github.jobStatus(ctx, j, token)
			if githubNotFound(err) {
				status = "completed"
			} else if err != nil {
				return err
			}
			if status == "completed" {
				return s.queue.advance(ctx, j.id, completed, now)
			}
			if status == "in_progress" || j.phase == inProgress {
				// Recheck periodically when GitHub misses a lifecycle webhook.
				_, err := s.queue.ExecContext(ctx, `UPDATE jobs SET phase = ?, changed_at = ?
                    WHERE job_id = ? AND phase < ?`, inProgress, now.Unix(), j.id, completed)
				return err
			}
		}
	}
	if state != "" {
		if err := s.runtime.remove(ctx, j.runnerName); err != nil {
			return err
		}
		delete(containers, j.runnerName)
	}
	if j.phase != completed {
		// A runner can exit before GitHub has sent its final job event, including in polling-only mode.
		token, err := s.github.token(ctx, j)
		var status string
		if err == nil {
			status, err = s.github.jobStatus(ctx, j, token)
		}
		if err != nil && !githubNotFound(err) {
			return err
		}
		if status == "completed" || githubNotFound(err) {
			if err := s.queue.advance(ctx, j.id, completed, now); err != nil {
				return err
			}
		} else if status == "in_progress" {
			_, err := s.queue.ExecContext(ctx, `UPDATE jobs SET retry_at = ?,
                retry_reason = 'waiting for GitHub to finish the job after the runner exited'
                WHERE job_id = ? AND phase < ?`, now.Add(retryDelay).Unix(), j.id, completed)
			return err
		}
	}
	if err := s.github.remove(ctx, j); err != nil {
		return err
	}
	_, err := s.queue.ExecContext(ctx, `UPDATE jobs SET runner_name = NULL, runner_id = NULL,
        launched_at = NULL, retry_at = ?,
        retry_reason = CASE WHEN phase = ? THEN coalesce(nullif(retry_reason, ''),
            'runner became unavailable while the job was queued') ELSE '' END
        WHERE job_id = ?`, now.Add(retryDelay).Unix(), queued, j.id)
	if err != nil {
		return err
	}
	slog.Info("runner removed", "runner", j.runnerName, "repository", j.repository, "job", j.id)
	return nil
}
