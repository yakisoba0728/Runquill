package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func status(ctx context.Context, out io.Writer) error {
	limit, err := runnerLimit()
	if err != nil {
		return err
	}
	path := env("RUNQUILL_DB", "runquill.sqlite3")
	db, err := openQueueDB(path, true)
	if err != nil {
		return err
	}
	defer db.Close()
	jobs, err := readStatusJobs(ctx, db)
	if err != nil {
		return fmt.Errorf("read queue %s (start the manager to initialize the queue): %w", path, err)
	}
	runtime, err := newDockerFleet("", env("RUNQUILL_DOCKER_HOSTS", "local"))
	if err != nil {
		return err
	}
	dockerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	containers, dockerErr := runtime.containers(dockerCtx)
	if dockerErr != nil {
		dockerErr = fmt.Errorf("list runner containers: %w", dockerErr)
	}
	return errors.Join(writeStatus(out, jobs, containers, limit, time.Now()), dockerErr)
}

func readStatusJobs(ctx context.Context, db *sql.DB) ([]job, error) {
	hasReason, err := hasRetryReason(ctx, db)
	if err != nil {
		return nil, err
	}
	reason := "''"
	if hasReason {
		reason = "retry_reason"
	}
	rows, err := db.QueryContext(ctx, `
        SELECT job_id, repository, route, phase, coalesce(runner_name, ''), retry_at, `+reason+`
        FROM jobs WHERE phase = ? OR runner_name IS NOT NULL ORDER BY sequence`, queued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.repository, &j.route, &j.phase, &j.runnerName, &j.retryAt, &j.retryReason); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func writeStatus(out io.Writer, jobs []job, containers map[string]string, limit int, now time.Time) error {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if containers == nil {
		fmt.Fprintln(table, "Runners: unavailable (Docker query failed)")
	} else {
		var running, occupied int
		for _, state := range containers {
			if state == "running" {
				running++
			}
			if runnerOccupiesSlot(state) {
				occupied++
			}
		}
		fmt.Fprintf(table, "Runners: %d running, %d/%d slots occupied\n", running, occupied, limit)
	}
	if len(jobs) == 0 {
		fmt.Fprintln(table, "No queued jobs or runner leases.")
		return table.Flush()
	}
	fmt.Fprintln(table, "\nSTATE\tPOSITION\tJOB\tDETAIL")
	position := 0
	for _, j := range jobs {
		state, detail := statusRow(j, containers, now)
		rank := "-"
		if state == "WAITING" {
			position++
			rank = strconv.Itoa(position)
		}
		fmt.Fprintf(table, "%s\t%s\t%d\t%s\n", state, rank, j.id, detail)
		if link := jobURL(j); link != "" {
			fmt.Fprintf(table, "\t\t\t%s\n", link)
		}
	}
	return table.Flush()
}

func statusRow(j job, containers map[string]string, now time.Time) (string, string) {
	remaining := j.retryAt - now.Unix()
	if remaining > 0 || j.runnerName != "" && j.retryAt > 0 && j.retryReason != "" {
		detail := "due now"
		if remaining > 0 {
			detail = "in " + strconv.FormatInt(remaining, 10) + "s"
		}
		if j.retryReason != "" {
			detail += " — " + j.retryReason
		}
		return "RETRY", detail
	}
	if j.runnerName == "" {
		detail := j.repository
		if j.retryReason != "" {
			detail = "last retry — " + j.retryReason
		}
		return "WAITING", detail
	}
	if j.phase == completed {
		return "CLEANUP", j.runnerName
	}
	if containers == nil {
		return "UNKNOWN", j.runnerName
	}
	switch containers[j.runnerName] {
	case "running":
		return "RUNNING", j.runnerName
	case "paused":
		return "PAUSED", j.runnerName
	case "exited", "dead":
		return "CLEANUP", j.runnerName
	default:
		return "STARTING", j.runnerName
	}
}

func jobURL(j job) string {
	// The run ID is already stored as part of every validated Runquill route.
	route, ok := strings.CutPrefix(j.route, "runquill-")
	runID, _, hasSuffix := strings.Cut(route, "-")
	id, err := strconv.ParseUint(runID, 10, 63)
	if !ok || !hasSuffix || err != nil || id == 0 || j.id <= 0 || !repositoryName.MatchString(j.repository) {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/actions/runs/%s/job/%d", j.repository, runID, j.id)
}
