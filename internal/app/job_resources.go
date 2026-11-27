package app

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Job resources are recorded per job in job_resources, created by migrateUsage
// and pruned with the jobs they belong to. The elastic loop's docker stats pass
// adds one sample per running runner each interval on the local Docker host;
// runner removal records whether the engine killed the runner for memory. Other
// hosts leave the usage columns NULL, which the API reports as unavailable.
const jobResourcesSchema = `CREATE TABLE IF NOT EXISTS job_resources (
    job_id INTEGER PRIMARY KEY,
    samples INTEGER NOT NULL DEFAULT 0,
    cpu_limit REAL,
    memory_limit_mib INTEGER,
    cpu_limit_max REAL,
    memory_limit_max_mib INTEGER,
    cpu_peak REAL,
    cpu_sum REAL NOT NULL DEFAULT 0,
    memory_peak_mib INTEGER,
    oom_killed INTEGER
);`

// jobResources is one job's sampled usage. Nil fields were not measured. A
// zero limit is unlimited. Peaks are the highest sample, not a cgroup high-water mark.
type jobResources struct {
	Samples        int64    `json:"samples"`
	CPULimit       *float64 `json:"cpu_limit,omitempty"`
	MemoryLimitMiB *int64   `json:"memory_limit_mib,omitempty"`
	// The max limits are the highest effective limits seen, above the guarantee after an elastic raise.
	CPULimitMax       *float64 `json:"cpu_limit_max,omitempty"`
	MemoryLimitMaxMiB *int64   `json:"memory_limit_max_mib,omitempty"`
	CPUPeak           *float64 `json:"cpu_peak,omitempty"`
	CPUAverage        *float64 `json:"cpu_average,omitempty"`
	MemoryPeakMiB     *int64   `json:"memory_peak_mib,omitempty"`
	OOMKilled         *bool    `json:"oom_killed,omitempty"`
}

const jobResourceColumns = `samples, cpu_limit, memory_limit_mib, cpu_peak, cpu_sum, memory_peak_mib, oom_killed,
    cpu_limit_max, memory_limit_max_mib`

func scanJobResources(scan func(...any) error) (jobResources, error) {
	var r jobResources
	var cpuSum float64
	var oom sql.NullBool
	if err := scan(&r.Samples, &r.CPULimit, &r.MemoryLimitMiB, &r.CPUPeak, &cpuSum, &r.MemoryPeakMiB, &oom,
		&r.CPULimitMax, &r.MemoryLimitMaxMiB); err != nil {
		return r, err
	}
	if r.Samples > 0 {
		average := math.Round(cpuSum/float64(r.Samples)*100) / 100
		r.CPUAverage = &average
	}
	if oom.Valid {
		r.OOMKilled = &oom.Bool
	}
	return r, nil
}

// readJobResources returns nil when nothing was recorded for the job.
func readJobResources(ctx context.Context, db *sql.DB, id int64) (*jobResources, error) {
	r, err := scanJobResources(db.QueryRowContext(ctx, `SELECT `+jobResourceColumns+` FROM job_resources WHERE job_id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// recordJobSamples folds one sampling pass into job_resources. The limits are
// the runner's guarantee as first seen, so an elastic raise does not hide a
// pool that is too small. Runners without a job, such as a starting runner
// whose lease is not written yet, are skipped.
func recordJobSamples(ctx context.Context, db *sql.DB, runners []elasticRunner) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO job_resources
            (job_id, samples, cpu_limit, memory_limit_mib, cpu_peak, cpu_sum, memory_peak_mib, cpu_limit_max, memory_limit_max_mib)
        SELECT job_id, 1, ?, ?, ?, ?, ?, ?, ? FROM jobs WHERE runner_name = ?
        ON CONFLICT (job_id) DO UPDATE SET samples = samples + 1,
            cpu_limit = coalesce(cpu_limit, excluded.cpu_limit),
            memory_limit_mib = coalesce(memory_limit_mib, excluded.memory_limit_mib),
            cpu_limit_max = max(coalesce(cpu_limit_max, 0), excluded.cpu_limit_max),
            memory_limit_max_mib = max(coalesce(memory_limit_max_mib, 0), excluded.memory_limit_max_mib),
            cpu_peak = max(coalesce(cpu_peak, 0), excluded.cpu_peak),
            cpu_sum = cpu_sum + excluded.cpu_sum,
            memory_peak_mib = max(coalesce(memory_peak_mib, 0), excluded.memory_peak_mib)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, r := range runners {
		if !r.Sampled || r.Name == "" {
			continue
		}
		cpu := math.Round(r.CPUUsage*100) / 100
		if _, err := statement.ExecContext(ctx, cpuBase(r), memoryBase(r), cpu, cpu,
			int64(math.Ceil(r.MemoryUsageMiB)), r.CPULimit, r.MemoryLimitMiB, r.Name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// jobSampleRecorder is the elastic loop's hook; a failed write only loses one sample.
func jobSampleRecorder(db *sql.DB) func(context.Context, []elasticRunner) {
	return func(ctx context.Context, runners []elasticRunner) {
		if err := recordJobSamples(ctx, db, runners); err != nil && ctx.Err() == nil {
			slog.Warn("record job resource samples", "error", err)
		}
	}
}

// recordJobOOM notes whether a job's exited runner was killed for memory; a
// kill seen on any runner of the job is kept.
func recordJobOOM(ctx context.Context, db *sql.DB, id int64, killed bool) {
	_, err := db.ExecContext(ctx, `INSERT INTO job_resources (job_id, oom_killed) VALUES (?, ?)
        ON CONFLICT (job_id) DO UPDATE SET oom_killed = max(coalesce(oom_killed, 0), excluded.oom_killed)`, id, killed)
	if err != nil && ctx.Err() == nil {
		slog.Warn("record runner memory kill", "job", id, "error", err)
	}
}

// exitedOOM inspects exited runners on a Docker host in one call and reports
// which the engine killed for memory. Runners it cannot inspect stay unknown;
// a failure never takes the host out of scheduling.
func (d docker) exitedOOM(ctx context.Context, containers map[string]string) (map[string]bool, map[string]jobDiagnostic) {
	var names []string
	for name, state := range containers {
		if state == "exited" || state == "dead" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	slices.Sort(names)
	output, err := d.command(ctx, nil, append([]string{"inspect", "--type", "container", "--format", "{{.Name}} {{.State.OOMKilled}}"}, names...)...)
	if err != nil {
		slog.Debug("inspect exited runners", "host", d.label(), "error", err)
		return nil, nil
	}
	killed, diagnostics := parseExitedOOM(output)
	missing := 0
	for _, name := range names {
		if _, ok := killed[name]; !ok {
			missing++
		}
	}
	if missing != 0 {
		slog.Debug("missing memory kill status for exited runners", "host", d.label(), "runners", missing)
	}
	return killed, diagnostics
}

func parseExitedOOM(output string) (map[string]bool, map[string]jobDiagnostic) {
	killed := make(map[string]bool)
	diagnostics := make(map[string]jobDiagnostic)
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		name = strings.TrimPrefix(name, "/")
		oom, err := strconv.ParseBool(value)
		if !ok || name == "" || err != nil {
			continue
		}
		killed[name] = oom
		if oom {
			diagnostics[name] = jobDiagnostic{"runner_memory_limit", "The container engine reported an out-of-memory kill. Check the worker pool memory limit."}
		}
	}
	return killed, diagnostics
}

// Rightsizing compares each finished job's sampled peaks with its limits.
const (
	rightsizingLow        = 0.25 // a group whose largest peak stays below this share is oversized
	rightsizingHigh       = 0.80 // a group with a peak above this share, or any memory kill, is tight
	rightsizingMinJobs    = 3    // jobs with samples needed before a limit is called oversized
	rightsizingMaxGroups  = 500
	rightsizingDefaultAge = 7
)

type rightsizingResource struct {
	// Limit is the newest sampled job's limit (cores or MiB); 0 is unlimited.
	Limit   float64 `json:"limit"`
	PeakMax float64 `json:"peak_max"`
	// PeakRatio is the largest peak divided by its own job's limit, over jobs with a limit.
	PeakRatio *float64 `json:"peak_ratio,omitempty"`
	Verdict   string   `json:"verdict"` // oversized, tight, ok, too_few_jobs or unlimited
	limited   int64
	latest    int64
}

type rightsizingGroup struct {
	Key         string               `json:"key"`
	Jobs        int64                `json:"jobs"`
	SampledJobs int64                `json:"sampled_jobs"`
	OOMKills    int64                `json:"oom_kills"`
	CPU         *rightsizingResource `json:"cpu,omitempty"`
	Memory      *rightsizingResource `json:"memory,omitempty"`
}

type rightsizingResponse struct {
	Group     string             `json:"group"`
	Days      int                `json:"days"`
	Groups    []rightsizingGroup `json:"groups"`
	Truncated bool               `json:"truncated,omitempty"`
}

func (r *rightsizingResource) add(limit, peak float64, sequence int64) {
	r.PeakMax = max(r.PeakMax, peak)
	if sequence >= r.latest {
		r.Limit, r.latest = limit, sequence
	}
	if limit > 0 {
		ratio := peak / limit
		if r.PeakRatio == nil || ratio > *r.PeakRatio {
			r.PeakRatio = &ratio
		}
		r.limited++
	}
}

func (r *rightsizingResource) finish(kills int64) {
	switch {
	case kills > 0 || r.PeakRatio != nil && *r.PeakRatio > rightsizingHigh:
		r.Verdict = "tight"
	case r.PeakRatio == nil:
		r.Verdict = "unlimited"
	case r.limited < rightsizingMinJobs:
		r.Verdict = "too_few_jobs"
	case *r.PeakRatio < rightsizingLow:
		r.Verdict = "oversized"
	default:
		r.Verdict = "ok"
	}
	if r.PeakRatio != nil {
		ratio := math.Round(*r.PeakRatio*100) / 100
		r.PeakRatio = &ratio
	}
}

func parseRightsizingQuery(query url.Values) (group string, days int, ok bool) {
	group = cmp.Or(query.Get("group"), "pool")
	days = rightsizingDefaultAge
	if raw := query.Get("days"); raw != "" {
		var err error
		if days, err = strconv.Atoi(raw); err != nil {
			return group, days, false
		}
	}
	return group, days, (group == "pool" || group == "job") && days >= 1 && days <= 7
}

// readRightsizing groups the recorded jobs that finished in the last days by
// pool, or by repository and runner label (one label per workflow job).
func readRightsizing(ctx context.Context, db *sql.DB, group string, days int, pools workerPools, now time.Time) (rightsizingResponse, error) {
	response := rightsizingResponse{Group: group, Days: days, Groups: []rightsizingGroup{}}
	rows, err := db.QueryContext(ctx, `SELECT j.sequence, j.job_id, j.repository, j.route,
        samples, cpu_limit, memory_limit_mib, cpu_peak, cpu_sum, memory_peak_mib, oom_killed
        FROM jobs j JOIN job_resources USING (job_id)
        WHERE j.phase = ? AND j.changed_at > ? LIMIT ?`,
		completed, now.Add(-time.Duration(days)*24*time.Hour).Unix(), usageRawLimit)
	if err != nil {
		return response, err
	}
	defer rows.Close()
	groups := make(map[string]*rightsizingGroup)
	for rows.Next() {
		var sequence int64
		var j job
		var cpuSum float64
		var samples int64
		var cpuLimit, cpuPeak sql.NullFloat64
		var memoryLimit, memoryPeak sql.NullInt64
		var oom sql.NullBool
		if err := rows.Scan(&sequence, &j.id, &j.repository, &j.route, &samples, &cpuLimit, &memoryLimit, &cpuPeak,
			&cpuSum, &memoryPeak, &oom); err != nil {
			return response, err
		}
		key := strings.ToLower(j.repository) + " " + j.route
		if group == "pool" {
			key, _ = pools.forJob(j)
		}
		g := groups[key]
		if g == nil {
			g = &rightsizingGroup{Key: key, CPU: &rightsizingResource{}, Memory: &rightsizingResource{}}
			groups[key] = g
		}
		g.Jobs++
		if oom.Bool {
			g.OOMKills++
		}
		if samples > 0 {
			g.SampledJobs++
			g.CPU.add(cpuLimit.Float64, cpuPeak.Float64, sequence)
			g.Memory.add(float64(memoryLimit.Int64), float64(memoryPeak.Int64), sequence)
		}
	}
	if err := rows.Err(); err != nil {
		return response, err
	}
	for _, g := range groups {
		if g.SampledJobs == 0 {
			g.CPU, g.Memory = nil, nil
		} else {
			g.CPU.finish(0)
			g.Memory.finish(g.OOMKills)
		}
		if g.Memory == nil && g.OOMKills > 0 {
			g.Memory = &rightsizingResource{Verdict: "tight"}
		}
		response.Groups = append(response.Groups, *g)
	}
	slices.SortFunc(response.Groups, func(a, b rightsizingGroup) int {
		return cmp.Or(cmp.Compare(b.Jobs, a.Jobs), strings.Compare(a.Key, b.Key))
	})
	if len(response.Groups) > rightsizingMaxGroups {
		response.Groups, response.Truncated = response.Groups[:rightsizingMaxGroups], true
	}
	return response, nil
}

var errRightsizingQuery = errors.New("Use group pool or job and days from 1 to 7.")

func registerRightsizingRoute(mux *http.ServeMux, a *panelAccess, pools func() workerPools) {
	mux.HandleFunc("GET /api/usage/rightsizing", a.require("fleet:read", func(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
		group, days, ok := parseRightsizingQuery(r.URL.Query())
		if !ok {
			http.Error(w, errRightsizingQuery.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		response, err := readRightsizing(ctx, a.db, group, days, pools(), time.Now())
		if err != nil {
			if r.Context().Err() == nil {
				slog.Warn("read job resource rightsizing", "error", err)
			}
			http.Error(w, "Could not read job resources.", http.StatusServiceUnavailable)
			return
		}
		panelJSON(w, http.StatusOK, response)
	}))
}
