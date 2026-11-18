package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

func diagnose(ctx context.Context, out io.Writer, name string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	api, err := cfg.github()
	if err != nil {
		return err
	}
	db, err := openQueueDB(env("RUNQUILL_DB", "runquill.sqlite3"), "ro")
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return diagnoseRepository(ctx, out, api, db, name, cfg.pollInterval != 0)
}

func diagnoseRepository(ctx context.Context, out io.Writer, api *github, db *sql.DB, name string, polling bool) error {
	name, valid := ownedRepositoryPath(name, api.owner)
	if !valid {
		return errors.New("use OWNER/REPO for a repository owned by RUNQUILL_OWNER")
	}
	jwt, err := api.jwt(time.Now())
	if err != nil {
		return err
	}
	inst, err := api.installation(ctx, jwt)
	if err != nil {
		return err
	}
	var target struct{ ID int64 }
	if err := api.request(ctx, http.MethodGet, "/repos/"+name+"/installation", jwt, nil, &target); err != nil {
		return fmt.Errorf("check GitHub App access to %s: %w", name, err)
	}
	if target.ID != inst.ID {
		return fmt.Errorf("%s does not belong to the configured GitHub App installation", name)
	}
	fmt.Fprintf(out, "GitHub App installation covers %s\n", name)
	scopeErr, permissionErr := inst.accessErrors()
	if scopeErr != nil {
		fmt.Fprintf(out, "Repository access: %s\n", scopeErr)
	}
	if permissionErr != nil {
		fmt.Fprintf(out, "App permissions: %s\n", strings.Join(strings.Fields(permissionErr.Error()), " "))
	}
	_, slug, _ := strings.Cut(name, "/")
	token, err := api.mintToken(ctx, jwt, inst.ID, map[string]any{
		"repositories": []string{slug},
		"permissions":  map[string]string{"actions": "read"},
	})
	if err != nil {
		return errors.Join(permissionErr, err)
	}
	var repo repository
	if err := api.request(ctx, http.MethodGet, "/repos/"+name, token.Token, nil, &repo); err != nil {
		return err
	}
	if !repo.privateFor(api.owner) || repo.Archived || repo.Disabled || repo.ID <= 0 {
		return errors.New("Runquill requires an active private repository owned by RUNQUILL_OWNER")
	}
	poll, err := latestRepositoryPoll(ctx, db, inst.ID, repo.ID)
	if err != nil {
		return err
	}
	if !polling {
		fmt.Fprintln(out, "Polling is disabled for this configuration.")
	} else if poll.checkedAt == 0 {
		fmt.Fprintln(out, "No completed poll attempt recorded for this repository.")
	} else {
		checked := time.Unix(0, poll.checkedAt).UTC().Format(time.RFC3339)
		result := "passed"
		if poll.lastError != "" {
			result = poll.lastError
		}
		fmt.Fprintf(out, "Last repository poll: %s (%s)\n", result, checked)
	}

	report, err := inspectStatus(ctx, db)
	if err != nil {
		fmt.Fprintf(out, "Local queue or worker state: %s\n", strings.Join(strings.Fields(err.Error()), " "))
	}
	byID := make(map[int64]statusJob)
	if report != nil {
		for _, local := range report.Jobs {
			byID[local.ID] = local
		}
	}
	p := &poller{github: api, token: token, cache: make(map[string]pollPage)}
	seen := make(map[int64]bool)
	_, scanErr := p.walkWorkflowJobs(ctx, name, true, func(candidate workflowJob) error {
		if candidate.Status != "queued" || seen[candidate.ID] {
			return nil
		}
		seen[candidate.ID] = true
		routed, eligible, err := candidate.queueJob(api.owner, repo, inst.ID, candidate.Status)
		if err != nil {
			return err
		}
		local, found := byID[candidate.ID]
		reason := missingJobReason(candidate, poll, polling, report != nil)
		var poolErr error
		if eligible && report != nil {
			_, poolErr = report.pools.forJob(routed)
		}
		switch {
		case !eligible:
			reason = "uses labels outside Runquill"
			if slices.ContainsFunc(candidate.Labels, func(label string) bool {
				return strings.HasPrefix(strings.ToLower(label), "runquill")
			}) {
				reason = fmt.Sprintf("invalid Runquill labels %q (use a single runquill-%d-ATTEMPT-JOB label)", candidate.Labels, candidate.RunID)
			}
		case permissionErr != nil:
			reason = "required GitHub App permissions are missing"
		case poolErr != nil:
			reason = poolErr.Error()
		case found:
			reason = localJobReason(local, report)
		}
		_, err = fmt.Fprintf(out, "Job %d (run %d): %s\n", candidate.ID, candidate.RunID, reason)
		return err
	})
	if len(seen) == 0 && scanErr == nil {
		fmt.Fprintln(out, "GitHub did not return any queued jobs in the scanned workflow runs.")
	}
	return errors.Join(permissionErr, scanErr, err)
}

func latestRepositoryPoll(ctx context.Context, db *sql.DB, installation, repo int64) (repositoryPoll, error) {
	var result repositoryPoll
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (
        SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'repository_polls'
    )`).Scan(&exists); err != nil || !exists {
		return result, err
	}
	err := db.QueryRowContext(ctx, `SELECT checked_at, last_error FROM repository_polls
        WHERE installation_id = ? AND repository_id = ?`, installation, repo).Scan(&result.checkedAt, &result.lastError)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return result, err
}

func missingJobReason(candidate workflowJob, poll repositoryPoll, polling, localKnown bool) string {
	switch {
	case !localKnown:
		return "label is valid. Local queue state is unavailable"
	case !polling:
		return "label is valid and the job is absent from the local queue. Polling is disabled. Check the webhook delivery"
	case poll.checkedAt == 0:
		return "label is valid and the job is absent from the local queue. No poll attempt recorded yet"
	case poll.lastError != "":
		return "label is valid and the job is absent from the local queue. The last repository poll failed"
	case githubTimestamp(candidate.CreatedAt) > time.Unix(0, poll.checkedAt).Unix():
		return "label is valid and the job was created after the last recorded repository poll"
	default:
		return "label is valid and the job is absent from the local queue. Check webhook delivery or the next poll"
	}
}

func localJobReason(j statusJob, report *statusReport) string {
	switch j.Status {
	case "RETRY":
		return "waiting for a Runquill retry (" + j.detail + ")"
	case "WAITING":
		if report.Runners != nil && report.Runners.OccupiedSlots+report.Runners.ReservedSlots >= report.SlotLimit {
			return "waiting for capacity (manager runner limit reached)"
		}
		if !slices.ContainsFunc(report.Hosts, func(host statusHost) bool {
			return (j.Pool == "" || j.Pool == host.Pool) && host.Status == "UP" && !host.Cordoned && host.OccupiedSlots < host.SlotLimit
		}) {
			if j.Pool != "" {
				return fmt.Sprintf("waiting for an available slot in worker pool %q", j.Pool)
			}
			return "waiting for a worker with an available slot"
		}
		return fmt.Sprintf("in the Runquill queue at position %d", j.QueuePosition)
	case "RUNNING":
		return "runner container is running. GitHub still reports the job as queued"
	default:
		return fmt.Sprintf("local state %s (%s)", j.Status, j.detail)
	}
}
