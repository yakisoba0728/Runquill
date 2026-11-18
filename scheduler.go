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
	snapshot(context.Context) ([]dockerHostSnapshot, error)
	startOn(context.Context, string, string, string) error
	removeOn(context.Context, string, string) error
}

type scheduler struct {
	queue      *queue
	github     *github
	runtime    runnerRuntime
	limit      int
	hostLimits map[string]int
}

func (s *scheduler) tick(ctx context.Context, now time.Time) error {
	snapshots, err := s.runtime.snapshot(ctx)
	if err != nil {
		return err
	}
	active, err := s.queue.jobs(ctx, "WHERE runner_name IS NOT NULL")
	if err != nil {
		return err
	}
	states, err := readHostStates(ctx, s.queue.DB)
	if err != nil {
		return err
	}
	if err := s.reconcileFleet(ctx, active, snapshots, states, now); err != nil {
		return err
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

	hosts, occupied := fleetCapacity(snapshots, states, active, s.hostLimits, s.limit)
	for _, host := range hosts {
		occupied += host.Occupied
	}
	if occupied >= s.limit || availableHost(hosts) == nil {
		return nil
	}
	waiting, err := s.queue.jobs(ctx, `
        WHERE phase = 0 AND runner_name IS NULL AND retry_at <= ?
        ORDER BY sequence LIMIT ?`, now.Unix(), s.limit-occupied)
	if err != nil {
		return err
	}
	for _, j := range waiting {
		host := availableHost(hosts)
		if host == nil {
			break
		}
		claimed, err := s.launch(ctx, j, host.Name, now)
		if claimed {
			host.Occupied++
		}
		if err != nil {
			if err := s.retry(ctx, j, now, "provision runner", err); err != nil {
				return err
			}
		}
	}
	return nil
}

func availableHost(hosts []hostCapacity) *hostCapacity {
	var best *hostCapacity
	for i := range hosts {
		host := &hosts[i]
		if host.Containers == nil || host.Err != nil || host.Cordoned || host.Occupied >= host.Limit {
			continue
		}
		if best == nil || host.Occupied < best.Occupied {
			best = host
		}
	}
	return best
}

func (s *scheduler) reconcileFleet(ctx context.Context, active []job, snapshots []dockerHostSnapshot, states map[string]hostState, now time.Time) error {
	containers := make(map[string]map[string]string, len(snapshots))
	seenOn := make(map[string]string)
	allObserved := true
	for _, host := range snapshots {
		containers[host.Name] = host.Containers
		allObserved = allObserved && host.Containers != nil
		for name := range host.Containers {
			seenOn[name] = host.Name
		}
	}
	for name, state := range states {
		if _, configured := containers[name]; !configured && state.untrackedSlots.Int64 > 0 {
			allObserved = false
		}
	}
	for _, j := range active {
		if _, configured := containers[j.runnerHost]; j.runnerHost != "" && !configured {
			allObserved = false
		}
	}

	owners := make(map[string]string, len(active))
	for i := range active {
		j := &active[i]
		seen := seenOn[j.runnerName]
		if j.runnerHost == "" && seen != "" {
			// Adopt runners that predate persisted Docker host ownership.
			_, err := s.queue.ExecContext(ctx, "UPDATE jobs SET runner_host = ? WHERE job_id = ?", seen, j.id)
			if err != nil {
				return err
			}
			j.runnerHost = seen
		}
		owners[j.runnerName] = j.runnerHost
		if j.retryAt > now.Unix() || (seen != "" && j.runnerHost != seen) {
			continue
		}
		hostContainers := containers[j.runnerHost]
		if hostContainers == nil && (j.runnerHost != "" || !allObserved) {
			continue
		}
		if err := s.reconcile(ctx, *j, hostContainers, now); err != nil {
			if err := s.retry(ctx, *j, now, "reconcile runner", err); err != nil {
				return err
			}
		}
	}

	for _, host := range snapshots {
		if host.Containers == nil {
			continue
		}
		var untracked int
		for name, state := range host.Containers {
			if owners[name] != host.Name && runnerOccupiesSlot(state) {
				untracked++
			}
		}
		if previous := states[host.Name].untrackedSlots; !previous.Valid || previous.Int64 != int64(untracked) {
			if err := s.queue.observeHostSlots(ctx, host.Name, untracked); err != nil {
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

func (s *scheduler) launch(ctx context.Context, j job, host string, now time.Time) (bool, error) {
	token, err := s.github.token(ctx, j)
	if err != nil {
		return false, err
	}
	eligible, err := s.github.eligible(ctx, j, token)
	if err != nil {
		return false, err
	}
	if !eligible {
		return false, s.queue.advance(ctx, j.id, completed, now)
	}
	observed, err := s.github.jobDetails(ctx, j, token)
	if err != nil {
		return false, err
	}
	if observed.Status != "queued" {
		if p, known := phases[observed.Status]; known {
			return false, s.queue.advanceGitHubJob(ctx, j.id, p, now, observed)
		}
		return false, fmt.Errorf("waiting for GitHub to queue job, status %q", observed.Status)
	}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return false, err
	}
	name := fmt.Sprintf("runquill-%d-%s", j.id, hex.EncodeToString(nonce[:]))
	claim, err := s.queue.ExecContext(ctx, `
        UPDATE jobs SET runner_name = ?, runner_host = ?, launched_at = ?
        WHERE job_id = ? AND phase = ? AND runner_name IS NULL
        AND NOT EXISTS (SELECT 1 FROM docker_hosts WHERE host = ? AND cordoned = 1)`,
		name, host, now.Unix(), j.id, queued, host)
	if err != nil {
		return false, err
	}
	count, err := claim.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	runnerID, jit, err := s.github.provision(ctx, j, name, token)
	if err != nil {
		return true, err
	}
	_, err = s.queue.ExecContext(ctx, `UPDATE jobs SET runner_id = ?,
        provisioned_at = coalesce(provisioned_at, ?),
        github_created_at = coalesce(github_created_at, nullif(?, 0)) WHERE job_id = ?`,
		runnerID, time.Now().Unix(), githubTimestamp(observed.CreatedAt), j.id)
	if err != nil {
		return true, err
	}
	var current phase
	if err := s.queue.QueryRowContext(ctx, "SELECT phase FROM jobs WHERE job_id = ?", j.id).Scan(&current); err != nil {
		return true, err
	}
	if current == queued {
		if err := s.runtime.startOn(ctx, host, name, jit); err != nil {
			return true, err
		}
		slog.Info("runner started", "runner", name, "docker_host", host, "repository", j.repository, "job", j.id)
		_, err = s.queue.ExecContext(ctx, "UPDATE jobs SET retry_at = 0, retry_reason = '' WHERE job_id = ?", j.id)
		return true, err
	}
	return true, nil
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
			observed, err := s.github.jobDetails(ctx, j, token)
			if githubNotFound(err) {
				return s.queue.advance(ctx, j.id, completed, now)
			}
			if err != nil {
				return err
			}
			if observed.Status == "completed" {
				return s.queue.advanceGitHubJob(ctx, j.id, completed, now, observed)
			}
			if observed.Status == "in_progress" || j.phase == inProgress {
				// Recheck periodically when GitHub misses a lifecycle webhook.
				_, err := s.queue.ExecContext(ctx, `UPDATE jobs SET phase = ?, changed_at = ?,
                    github_created_at = coalesce(github_created_at, nullif(?, 0)),
                    github_started_at = coalesce(github_started_at, nullif(?, 0))
                    WHERE job_id = ? AND phase < ?`, inProgress, now.Unix(),
					githubTimestamp(observed.CreatedAt), githubTimestamp(observed.StartedAt), j.id, completed)
				return err
			}
		}
	}
	if state != "" {
		if err := s.runtime.removeOn(ctx, j.runnerHost, j.runnerName); err != nil {
			return err
		}
		delete(containers, j.runnerName)
	}
	if j.phase != completed {
		// A runner can exit before GitHub has sent its final job event, including in polling-only mode.
		token, err := s.github.token(ctx, j)
		var observed workflowJob
		if err == nil {
			observed, err = s.github.jobDetails(ctx, j, token)
		}
		if err != nil && !githubNotFound(err) {
			return err
		}
		if observed.Status == "completed" || githubNotFound(err) {
			if err := s.queue.advanceGitHubJob(ctx, j.id, completed, now, observed); err != nil {
				return err
			}
		} else if observed.Status == "in_progress" {
			if err := s.queue.advanceGitHubJob(ctx, j.id, inProgress, now, observed); err != nil {
				return err
			}
			_, err := s.queue.ExecContext(ctx, `UPDATE jobs SET retry_at = ?,
                retry_reason = 'waiting for GitHub to finish the job after the runner exited'
                WHERE job_id = ? AND phase < ?`, now.Add(retryDelay).Unix(), j.id, completed)
			return err
		}
	}
	if err := s.github.remove(ctx, j); err != nil {
		return err
	}
	_, err := s.queue.ExecContext(ctx, `UPDATE jobs SET runner_name = NULL, runner_host = NULL, runner_id = NULL,
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
