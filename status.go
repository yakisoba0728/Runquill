package main

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func status(ctx context.Context, out io.Writer, jsonOutput bool) error {
	path := env("RUNQUILL_DB", "runquill.sqlite3")
	db, err := openQueueDB(path, "ro")
	if err != nil {
		return err
	}
	defer db.Close()
	report, err := inspectStatus(ctx, db)
	if report == nil {
		return fmt.Errorf("read queue %s: %w", path, err)
	}
	if jsonOutput {
		return errors.Join(json.NewEncoder(out).Encode(report), err)
	}
	return errors.Join(report.writeText(out), err)
}

func inspectStatus(ctx context.Context, db *sql.DB) (*statusReport, error) {
	limit, err := runnerLimit()
	if err != nil {
		return nil, err
	}
	jobs, err := readStatusJobs(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("start the manager to initialize the queue: %w", err)
	}
	states, err := readHostStates(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("read runner host states: %w", err)
	}
	state, err := appliedRunnerSettings(ctx, db, env("RUNQUILL_RUNNER_IMAGE", "runquill-runner:local"), limit)
	if err != nil {
		return nil, err
	}
	runners, limit := state.runners, state.policy.Limit
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snapshots, probeErr := runners.runtime.snapshot(probeCtx)
	var hosts []hostCapacity
	var unassigned int
	if probeErr == nil {
		hosts, unassigned = fleetCapacity(snapshots, states, jobs, runners.hostLimits, limit)
		var hostErrors []error
		for _, host := range hosts {
			if host.Containers != nil {
				hostErrors = nil
				break
			}
			hostErrors = append(hostErrors, host.Err)
		}
		probeErr = errors.Join(hostErrors...)
	} else {
		probeErr = fmt.Errorf("list runners: %w", probeErr)
	}
	report := summarizeStatus(jobs, hosts, unassigned, limit, time.Now())
	// Bad GitHub configuration should not hide the local queue.
	scope, _ := parseRepositoryScope(os.Getenv(repositoryScopeEnv))
	report.configure(runners.kind, scope, runners.pools, jobs, hosts)
	report.describeImages(runners.images, jobs)
	if probeErr != nil {
		report.DockerError = strings.Join(strings.Fields(probeErr.Error()), " ")
	}
	return &report, probeErr
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
	hasTiming, err := hasJobColumn(ctx, db, "discovery_source")
	if err != nil {
		return nil, err
	}
	timing := "0, 0, '', 0, 0"
	if hasTiming {
		timing = "coalesce(github_created_at, 0), coalesce(discovered_at, 0), coalesce(discovery_source, ''), coalesce(provisioned_at, 0), coalesce(github_started_at, 0)"
	}
	rows, err := db.QueryContext(ctx, `
        SELECT job_id, repository, route, phase, coalesce(runner_name, ''), retry_at, `+reason+`, `+host+`, `+timing+`
        FROM jobs WHERE phase = ? OR runner_name IS NOT NULL ORDER BY sequence`, queued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.repository, &j.route, &j.phase, &j.runnerName, &j.retryAt, &j.retryReason, &j.runnerHost,
			&j.githubCreatedAt, &j.discoveredAt, &j.discoverySource, &j.provisionedAt, &j.githubStartedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

type statusReport struct {
	Backend         string         `json:"backend,omitempty"`
	RepositoryScope string         `json:"repository_scope,omitempty"`
	SchemaVersion   int            `json:"schema_version"`
	SlotLimit       int            `json:"slot_limit"`
	Runners         *statusRunners `json:"runners"`
	Hosts           []statusHost   `json:"hosts"`
	Jobs            []statusJob    `json:"jobs"`
	DockerError     string         `json:"docker_error,omitempty"`
	pools           workerPools
}

type statusRunners struct {
	KnownRunning            int `json:"known_running"`
	OccupiedSlots           int `json:"occupied_slots"`
	ReservedSlots           int `json:"reserved_slots"`
	UnassignedReservedSlots int `json:"unassigned_reserved_slots"`
	OfflineHosts            int `json:"offline_hosts"`
}

type statusHost struct {
	Name          string `json:"name"`
	Pool          string `json:"pool,omitempty"`
	Status        string `json:"status"`
	Cordoned      bool   `json:"cordoned"`
	SlotLimit     int    `json:"slot_limit"`
	OccupiedSlots int    `json:"occupied_slots"`
	ReservedSlots int    `json:"reserved_slots"`
	Error         string `json:"error,omitempty"`
	// ImageStatus is ready, pulling, missing or failed while Runquill pulls runner images on this host.
	ImageStatus string `json:"image_status,omitempty"`
}

type statusJob struct {
	ID            int64            `json:"id"`
	Repository    string           `json:"repository"`
	Status        string           `json:"status"`
	QueuePosition int              `json:"queue_position,omitempty"`
	RetryAt       *time.Time       `json:"retry_at,omitempty"`
	RetryReason   string           `json:"retry_reason,omitempty"`
	Runner        string           `json:"runner,omitempty"`
	RunnerLabel   string           `json:"runner_label,omitempty"`
	Diagnostic    *jobDiagnostic   `json:"diagnostic,omitempty"`
	RunnerHost    string           `json:"runner_host,omitempty"`
	Pool          string           `json:"pool,omitempty"`
	PoolError     string           `json:"pool_error,omitempty"`
	RunnerOS      string           `json:"runner_os,omitempty"`    // the os__NAME__ selector in the label
	RunnerImage   string           `json:"runner_image,omitempty"` // the image the job's runner starts from
	URL           string           `json:"url,omitempty"`
	Timing        *statusJobTiming `json:"timing,omitempty"`
	detail        string
}

type statusJobTiming struct {
	GitHubCreatedAt   *time.Time `json:"github_created_at,omitempty"`
	DiscoveredAt      *time.Time `json:"discovered_at,omitempty"`
	DiscoverySource   string     `json:"discovery_source,omitempty"`
	ProvisionedAt     *time.Time `json:"provisioned_at,omitempty"`
	GitHubStartedAt   *time.Time `json:"github_started_at,omitempty"`
	DiscoverySeconds  *int64     `json:"creation_to_discovery_seconds,omitempty"`
	QueueSeconds      *int64     `json:"runquill_queue_seconds,omitempty"`
	AssignmentSeconds *int64     `json:"provision_to_start_seconds,omitempty"`
}

func jobTiming(j job, now time.Time) *statusJobTiming {
	if j.githubCreatedAt == 0 && j.discoveredAt == 0 && j.provisionedAt == 0 && j.githubStartedAt == 0 {
		return nil
	}
	queueEnd := j.provisionedAt
	if queueEnd == 0 && j.phase == queued {
		queueEnd = now.Unix()
	}
	return &statusJobTiming{
		GitHubCreatedAt:   statusTime(j.githubCreatedAt),
		DiscoveredAt:      statusTime(j.discoveredAt),
		DiscoverySource:   j.discoverySource,
		ProvisionedAt:     statusTime(j.provisionedAt),
		GitHubStartedAt:   statusTime(j.githubStartedAt),
		DiscoverySeconds:  elapsedSeconds(j.githubCreatedAt, j.discoveredAt),
		QueueSeconds:      elapsedSeconds(j.discoveredAt, queueEnd),
		AssignmentSeconds: elapsedSeconds(j.provisionedAt, j.githubStartedAt),
	}
}

func statusTime(timestamp int64) *time.Time {
	if timestamp == 0 {
		return nil
	}
	at := time.Unix(timestamp, 0).UTC()
	return &at
}

func elapsedSeconds(start, end int64) *int64 {
	if start == 0 || end < start {
		return nil
	}
	seconds := end - start
	return &seconds
}

func (t statusJobTiming) text() string {
	var stages []string
	if t.DiscoverySource != "" {
		stages = append(stages, "via "+t.DiscoverySource)
	}
	for _, stage := range []struct {
		label string
		value *int64
	}{
		{"creation to discovery", t.DiscoverySeconds},
		{"Runquill queue", t.QueueSeconds},
		{"provision to start", t.AssignmentSeconds},
	} {
		if stage.value != nil {
			stages = append(stages, fmt.Sprintf("%s %ds", stage.label, *stage.value))
		}
	}
	return strings.Join(stages, " · ")
}

func summarizeStatus(jobs []job, hosts []hostCapacity, unassigned, limit int, now time.Time) statusReport {
	report := statusReport{
		SchemaVersion: 1,
		SlotLimit:     limit,
		Hosts:         make([]statusHost, 0, len(hosts)),
		Jobs:          make([]statusJob, 0, len(jobs)),
	}
	if len(hosts) > 0 {
		runners := &statusRunners{ReservedSlots: unassigned, UnassignedReservedSlots: unassigned}
		for _, host := range hosts {
			row := statusHost{Name: host.Name, Pool: host.Pool, Status: "UP", Cordoned: host.Cordoned, SlotLimit: host.Limit,
				ImageStatus: host.ImageStatus}
			if host.Err != nil {
				row.Status = "UNAVAILABLE"
				row.Error = strings.Join(strings.Fields(host.Err.Error()), " ")
			} else if host.ImageWait != "" {
				row.Status, row.Error = "UNAVAILABLE", host.ImageWait
			}
			if host.Containers == nil {
				row.Status = "OFFLINE"
				row.ReservedSlots = host.Occupied
				runners.OfflineHosts++
				runners.ReservedSlots += host.Occupied
			} else {
				row.OccupiedSlots = host.Occupied
				runners.OccupiedSlots += host.Occupied
				for _, state := range host.Containers {
					if state == "running" {
						runners.KnownRunning++
					}
				}
			}
			report.Hosts = append(report.Hosts, row)
		}
		report.Runners = runners
	}
	position := 0
	for _, j := range jobs {
		state, detail := statusRow(j, hosts, now)
		row := statusJob{
			ID: j.id, Repository: j.repository, Status: state, RetryReason: j.retryReason,
			Runner: j.runnerName, RunnerLabel: j.route, RunnerHost: j.runnerHost, URL: jobURL(j), Timing: jobTiming(j, now), detail: detail,
		}
		if state == "WAITING" {
			position++
			row.QueuePosition = position
		}
		if state == "RETRY" {
			retryAt := time.Unix(j.retryAt, 0).UTC()
			row.RetryAt = &retryAt
		}
		report.Jobs = append(report.Jobs, row)
	}
	return report
}

func (r *statusReport) configure(backend, repositoryScope string, pools workerPools, jobs []job, hosts []hostCapacity) {
	r.pools = pools
	r.Backend = backend
	r.RepositoryScope = repositoryScope
	for i, j := range jobs {
		pool, err := pools.forJob(j)
		r.Jobs[i].Pool = pool
		if err != nil {
			r.Jobs[i].PoolError = err.Error()
		}
		r.Jobs[i].Diagnostic = r.explainJob(r.Jobs[i], j.phase, hosts)
	}
}

func (r statusReport) writeText(out io.Writer) error {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if r.Runners == nil {
		fmt.Fprintln(table, "Runners: unavailable (runner query failed)")
	} else {
		counts := r.Runners
		total := counts.OccupiedSlots + counts.ReservedSlots
		if counts.OfflineHosts > 0 {
			fmt.Fprintf(table, "Runners: %d known running, %d/%d slots occupied or reserved\n", counts.KnownRunning, total, r.SlotLimit)
		} else if counts.ReservedSlots > 0 {
			fmt.Fprintf(table, "Runners: %d running, %d/%d slots occupied or reserved\n", counts.KnownRunning, total, r.SlotLimit)
		} else {
			fmt.Fprintf(table, "Runners: %d running, %d/%d slots occupied\n", counts.KnownRunning, total, r.SlotLimit)
		}
		if counts.UnassignedReservedSlots > 0 {
			fmt.Fprintf(table, "Slots reserved without a known host: %d\n", counts.UnassignedReservedSlots)
		}
		fmt.Fprintln(table, "\nHOST\tSTATUS\tMODE\tSLOTS\tDETAIL")
		for _, host := range r.Hosts {
			mode := "ACTIVE"
			slots := fmt.Sprintf("%d/%d", host.OccupiedSlots+host.ReservedSlots, host.SlotLimit)
			if host.Status == "OFFLINE" {
				slots += " reserved"
			}
			if host.Cordoned {
				mode = "CORDONED"
			}
			detail := host.Error
			if host.Pool != "" {
				detail = strings.TrimSpace("pool=" + host.Pool + " " + detail)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", host.Name, host.Status, mode, slots, detail)
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	if len(r.Jobs) == 0 {
		fmt.Fprintln(table, "\nNo queued jobs or runner leases.")
		return table.Flush()
	}
	fmt.Fprintln(table, "\nSTATE\tPOSITION\tJOB\tDETAIL")
	for _, j := range r.Jobs {
		rank := "-"
		if j.QueuePosition > 0 {
			rank = strconv.Itoa(j.QueuePosition)
		}
		fmt.Fprintf(table, "%s\t%s\t%d\t%s\n", j.Status, rank, j.ID, j.detail)
		if j.URL != "" {
			fmt.Fprintf(table, "\t\t\t%s\n", j.URL)
		}
		if j.PoolError != "" {
			fmt.Fprintf(table, "\t\t\t%s\n", j.PoolError)
		} else if j.Pool != "" {
			fmt.Fprintf(table, "\t\t\tpool=%s\n", j.Pool)
		}
		if j.RunnerOS != "" {
			fmt.Fprintf(table, "\t\t\tos=%s image=%s\n", j.RunnerOS, cmp.Or(j.RunnerImage, "unknown"))
		}
		if j.Diagnostic != nil && j.Diagnostic.Message != j.PoolError {
			fmt.Fprintf(table, "\t\t\t%s\n", j.Diagnostic.Message)
		}
		if j.Timing != nil {
			if timing := j.Timing.text(); timing != "" {
				fmt.Fprintf(table, "\t\t\t%s\n", timing)
			}
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
