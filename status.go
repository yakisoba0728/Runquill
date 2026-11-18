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
	db, err := openQueueDB(path, "ro")
	if err != nil {
		return err
	}
	defer db.Close()
	jobs, err := readStatusJobs(ctx, db)
	if err != nil {
		return fmt.Errorf("read queue %s (start the manager to initialize the queue): %w", path, err)
	}
	states, err := readHostStates(ctx, db)
	if err != nil {
		return fmt.Errorf("read Docker host states: %w", err)
	}
	runtime, err := newDockerFleet(env("RUNQUILL_RUNNER_IMAGE", "runquill-runner:local"), env("RUNQUILL_DOCKER_HOSTS", "local"))
	if err != nil {
		return err
	}
	limits, err := runtime.limits(env("RUNQUILL_DOCKER_HOST_LIMITS", ""))
	if err != nil {
		return err
	}
	dockerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snapshots, dockerErr := runtime.snapshot(dockerCtx)
	if dockerErr != nil {
		return errors.Join(writeStatus(out, jobs, nil, 0, limit, time.Now()), fmt.Errorf("list runner containers: %w", dockerErr))
	}
	hosts, unassigned := fleetCapacity(snapshots, states, jobs, limits, limit)
	var hostErrors []error
	for _, host := range hosts {
		if host.Containers != nil {
			hostErrors = nil
			break
		}
		hostErrors = append(hostErrors, host.Err)
	}
	return errors.Join(writeStatus(out, jobs, hosts, unassigned, limit, time.Now()), errors.Join(hostErrors...))
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
	hasHost, err := hasJobColumn(ctx, db, "runner_host")
	if err != nil {
		return nil, err
	}
	host := "''"
	if hasHost {
		host = "coalesce(runner_host, '')"
	}
	rows, err := db.QueryContext(ctx, `
        SELECT job_id, repository, route, phase, coalesce(runner_name, ''), retry_at, `+reason+`, `+host+`
        FROM jobs WHERE phase = ? OR runner_name IS NOT NULL ORDER BY sequence`, queued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.repository, &j.route, &j.phase, &j.runnerName, &j.retryAt, &j.retryReason, &j.runnerHost); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func writeStatus(out io.Writer, jobs []job, hosts []hostCapacity, unassigned, limit int, now time.Time) error {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if len(hosts) == 0 {
		fmt.Fprintln(table, "Runners: unavailable (Docker query failed)")
	} else {
		occupied := unassigned
		var running, offline int
		for _, host := range hosts {
			occupied += host.Occupied
			if host.Containers == nil {
				offline++
			}
			for _, state := range host.Containers {
				if state == "running" {
					running++
				}
			}
		}
		if offline > 0 {
			fmt.Fprintf(table, "Runners: %d known running, %d/%d slots occupied or reserved\n", running, occupied, limit)
		} else {
			fmt.Fprintf(table, "Runners: %d running, %d/%d slots occupied\n", running, occupied, limit)
		}
		if unassigned > 0 {
			fmt.Fprintf(table, "Slots reserved without a known host: %d\n", unassigned)
		}
		fmt.Fprintln(table, "\nHOST\tSTATUS\tMODE\tSLOTS\tDETAIL")
		for _, host := range hosts {
			health, mode, detail := "UP", "ACTIVE", ""
			slots := fmt.Sprintf("%d/%d", host.Occupied, host.Limit)
			if host.Err != nil {
				health = "UNAVAILABLE"
				detail = strings.Join(strings.Fields(host.Err.Error()), " ")
			}
			if host.Containers == nil {
				health = "OFFLINE"
				slots += " reserved"
			}
			if host.Cordoned {
				mode = "CORDONED"
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", host.Name, health, mode, slots, detail)
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	if len(jobs) == 0 {
		fmt.Fprintln(table, "\nNo queued jobs or runner leases.")
		return table.Flush()
	}
	fmt.Fprintln(table, "\nSTATE\tPOSITION\tJOB\tDETAIL")
	position := 0
	for _, j := range jobs {
		state, detail := statusRow(j, hosts, now)
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

func statusRow(j job, hosts []hostCapacity, now time.Time) (string, string) {
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
	detail := j.runnerName
	if j.runnerHost != "" {
		detail += " on " + j.runnerHost
	}
	known := len(hosts) > 0 && j.runnerHost == ""
	var containerState string
	for _, host := range hosts {
		if j.runnerHost != "" {
			if host.Name != j.runnerHost {
				continue
			}
			if host.Containers == nil {
				return "OFFLINE", detail
			}
			known = true
		}
		if host.Containers == nil {
			known = false
		}
		if state, exists := host.Containers[j.runnerName]; exists {
			known, containerState = true, state
			break
		}
	}
	if !known {
		return "UNKNOWN", detail
	}
	if j.phase == completed {
		return "CLEANUP", detail
	}
	switch containerState {
	case "running":
		return "RUNNING", detail
	case "paused":
		return "PAUSED", detail
	case "exited", "dead":
		return "CLEANUP", detail
	default:
		return "STARTING", detail
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
