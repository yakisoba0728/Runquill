package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func jobResourcesQueue(t *testing.T) *queue {
	t.Helper()
	q := testQueue(t)
	// The migration is idempotent and runs on every start.
	for range 2 {
		if err := migrateUsage(t.Context(), q.DB); err != nil {
			t.Fatal(err)
		}
	}
	return q
}

func leaseTestRunner(t *testing.T, q *queue, id int64, runner string) {
	t.Helper()
	if _, err := q.Exec(`UPDATE jobs SET runner_name = ?, runner_host = 'local' WHERE job_id = ?`, runner, id); err != nil {
		t.Fatal(err)
	}
}

func TestRecordJobSamplesKeepsPeaksAndFirstLimits(t *testing.T) {
	q := jobResourcesQueue(t)
	now := time.Unix(1_800_000_000, 0)
	recordTestJob(t, q, 1, "runquill-1-1-ci", now)
	leaseTestRunner(t, q, 1, "runquill-1-a")
	passes := [][]elasticRunner{
		{{Name: "runquill-1-a", Sampled: true, GuaranteeCPUs: 2, CPULimit: 2, GuaranteeMemoryMiB: 1024, MemoryLimitMiB: 1024, CPUUsage: 1.5, MemoryUsageMiB: 300.2},
			{Name: "runquill-warm", Sampled: true, CPUUsage: 9, MemoryUsageMiB: 9000}, // no job
			{Name: "runquill-1-a", Sampled: false, CPUUsage: 99}},                     // not sampled this pass
		// An elastic raise does not replace the guarantee; zero guarantees fall back to the container limit.
		{{Name: "runquill-1-a", Sampled: true, GuaranteeCPUs: 2, CPULimit: 3, GuaranteeMemoryMiB: 1024, MemoryLimitMiB: 1280, CPUUsage: 0.5, MemoryUsageMiB: 900}},
	}
	for _, pass := range passes {
		if err := recordJobSamples(t.Context(), q.DB, pass); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readJobResources(t.Context(), q.DB, 1)
	if err != nil || got == nil {
		t.Fatalf("read: %+v, %v", got, err)
	}
	if got.Samples != 2 || *got.CPULimit != 2 || *got.MemoryLimitMiB != 1024 || *got.CPUPeak != 1.5 || *got.CPUAverage != 1 ||
		*got.MemoryPeakMiB != 900 || got.OOMKilled != nil || *got.CPULimitMax != 3 || *got.MemoryLimitMaxMiB != 1280 {
		t.Fatalf("resources %s", mustJSON(t, got))
	}
	var rows int
	if err := q.QueryRow(`SELECT count(*) FROM job_resources`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows %d, %v: a runner without a job was recorded", rows, err)
	}
	if missing, err := readJobResources(t.Context(), q.DB, 2); missing != nil || err != nil {
		t.Fatalf("unrecorded job: %+v, %v", missing, err)
	}

	// A memory kill seen on any runner of the job is kept.
	for _, killed := range []bool{false, true, false} {
		recordJobOOM(t.Context(), q.DB, 1, killed)
	}
	recordJobOOM(t.Context(), q.DB, 3, false)
	if got, _ := readJobResources(t.Context(), q.DB, 1); got.OOMKilled == nil || !*got.OOMKilled || got.Samples != 2 {
		t.Fatalf("after kill: %s", mustJSON(t, got))
	}
	if got, _ := readJobResources(t.Context(), q.DB, 3); got.OOMKilled == nil || *got.OOMKilled || got.Samples != 0 || got.CPUPeak != nil {
		t.Fatalf("checked exit without samples: %s", mustJSON(t, got))
	}

	// The daily sweep drops rows whose jobs are gone.
	if err := rollupUsage(t.Context(), q, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRow(`SELECT count(*) FROM job_resources`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows after sweep %d, %v", rows, err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseExitedOOM(t *testing.T) {
	killed, diagnostics := parseExitedOOM("/runquill-1 true\nrunquill-2 false\n\ngarbage\n/runquill-3 maybe\n/ true\n")
	if len(killed) != 2 || !killed["runquill-1"] || killed["runquill-2"] {
		t.Fatalf("killed %v", killed)
	}
	if len(diagnostics) != 1 || diagnostics["runquill-1"].Code != "runner_memory_limit" {
		t.Fatalf("diagnostics %v", diagnostics)
	}
}

func TestJobResourceInspectLogsMissingStatus(t *testing.T) {
	fleet, _ := testDockerFleet(t)
	t.Setenv("FLEET_TEST_INSPECT_OUTPUT", "/runner-1 true\n/runner-2 maybe\n")
	logs := captureLogs(t)
	killed, _ := fleet.hosts[0].exitedOOM(t.Context(), map[string]string{"runner-1": "exited", "runner-2": "dead"})
	if len(killed) != 1 || !killed["runner-1"] || !strings.Contains(logs.String(), "missing memory kill status for exited runners") {
		t.Fatalf("memory kill status %v, logs %s", killed, logs)
	}
}

// The scheduler records the exit check before it removes the runner.
func TestReconcileRecordsMemoryKill(t *testing.T) {
	q := jobResourcesQueue(t)
	now := time.Unix(1_800_000_000, 0)
	for id := range int64(3) {
		recordTestJob(t, q, id+1, fmt.Sprintf("runquill-%d-1-ci", id+1), now.Add(-time.Hour))
		leaseTestRunner(t, q, id+1, fmt.Sprintf("runquill-%d-a", id+1))
	}
	if _, err := q.Exec(`UPDATE jobs SET phase = ?, changed_at = ?`, completed, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo/actions/runners":
			fmt.Fprint(w, `{"runners":[]}`)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local",
		Containers: map[string]string{"runquill-1-a": "exited", "runquill-2-a": "exited", "runquill-3-a": "exited"},
		OOMKilled:  map[string]bool{"runquill-1-a": true, "runquill-2-a": false}}}}
	snapshots, _ := runtime.snapshot(t.Context())
	active, err := q.jobs(t.Context(), "WHERE runner_name IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	s := &scheduler{queue: q, github: gh, runtime: runtime}
	if err := s.reconcileFleet(t.Context(), active, snapshots, nil, now); err != nil {
		t.Fatal(err)
	}
	if len(runtime.removed) != 3 {
		t.Fatalf("removed %v", runtime.removed)
	}
	for id, want := range map[int64]*bool{1: new(true), 2: new(false), 3: nil} {
		got, err := readJobResources(t.Context(), q.DB, id)
		switch {
		case err != nil:
			t.Fatal(err)
		case want == nil && got != nil:
			t.Errorf("job %d: unchecked exit recorded %s", id, mustJSON(t, got))
		case want != nil && (got == nil || got.OOMKilled == nil || *got.OOMKilled != *want):
			t.Errorf("job %d: %s, want oom_killed %v", id, mustJSON(t, got), *want)
		}
	}
}

// With elastic limits off, the sampler skips docker info, changes nothing and
// still records unlimited pools.
func TestElasticOffStillRecordsJobSamples(t *testing.T) {
	f := fleetDocker(nil)
	deps := elasticTestDeps(elasticOff, f)
	deps.Hosts = func() []elasticHostConfig { return []elasticHostConfig{{Name: "local", Local: true, SlotLimit: 5}} }
	var recorded []elasticRunner
	deps.Record = func(_ context.Context, runners []elasticRunner) { recorded = append(recorded, runners...) }
	deps.tick(t.Context())
	for _, call := range f.calls {
		if call[0] == "info" || call[0] == "update" {
			t.Fatalf("record-only pass ran docker %v", call)
		}
	}
	names := []string{}
	for _, r := range recorded {
		if r.Sampled {
			names = append(names, r.Name)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"runquill-busy", "runquill-idle", "runquill-mem"}) {
		t.Fatalf("recorded %v", names)
	}
	if report := deps.State.report(); len(report.Hosts) != 0 || len(report.Decisions) != 0 {
		t.Fatalf("record-only pass changed the elastic report: %+v", report)
	}

	// Recording does not change what apply mode decides, and sees limits before the change.
	f = fleetDocker(nil)
	deps = elasticTestDeps(elasticApply, f)
	recorded = nil
	deps.Record = func(_ context.Context, runners []elasticRunner) { recorded = append(recorded, runners...) }
	deps.tick(t.Context())
	if len(f.updates()) == 0 || len(recorded) == 0 || recorded[0].CPULimit != 1 {
		t.Fatalf("apply with recording: updates %v, recorded %+v", f.updates(), recorded)
	}

	cfg, err := loadElasticConfig(func(string) string { return "" })
	if err != nil || !cfg.JobResources {
		t.Fatalf("default: %+v, %v", cfg, err)
	}
	cfg, err = loadElasticConfig(func(name string) string { return map[string]string{"RUNQUILL_JOB_RESOURCES": "off"}[name] })
	if err != nil || cfg.JobResources {
		t.Fatalf("off: %+v, %v", cfg, err)
	}
	if _, err := loadElasticConfig(func(name string) string { return map[string]string{"RUNQUILL_JOB_RESOURCES": "yes"}[name] }); err == nil {
		t.Fatal("invalid RUNQUILL_JOB_RESOURCES accepted")
	}
	// The sampler runs the docker CLI, so Podman deployments record only when asked to.
	podman := map[string]string{"RUNQUILL_CONTAINER_ENGINE": "podman"}
	if cfg, err = loadElasticConfig(func(name string) string { return podman[name] }); err != nil || cfg.JobResources {
		t.Fatalf("podman default: %+v, %v", cfg, err)
	}
	podman["RUNQUILL_JOB_RESOURCES"] = "on"
	if cfg, err = loadElasticConfig(func(name string) string { return podman[name] }); err != nil || !cfg.JobResources {
		t.Fatalf("podman on: %+v, %v", cfg, err)
	}
}

func TestRightsizingThresholds(t *testing.T) {
	q := jobResourcesQueue(t)
	now := time.Now()
	type sample struct {
		route         string
		cpuLimit, cpu float64
		memLimit, mem int64
		oom           *bool
		samples       int
	}
	var id int64
	add := func(s sample) {
		id++
		recordTestJob(t, q, id, s.route, now.Add(-time.Hour))
		if _, err := q.Exec(`UPDATE jobs SET phase = ?, changed_at = ? WHERE job_id = ?`, completed, now.Add(-time.Minute).Unix(), id); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Exec(`INSERT INTO job_resources (job_id, samples, cpu_limit, memory_limit_mib, cpu_peak, cpu_sum, memory_peak_mib, oom_killed)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, s.samples, s.cpuLimit, s.memLimit, s.cpu, s.cpu*float64(s.samples), s.mem, s.oom); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		// Exactly 25% and 80% are ok; just below 25% is oversized, just above 80% tight.
		add(sample{route: "runquill-1-1-pool__edge__a", cpuLimit: 4, cpu: 1, memLimit: 1000, mem: 800, samples: 2})
		add(sample{route: "runquill-1-1-pool__big__b", cpuLimit: 4, cpu: 0.96, memLimit: 1000, mem: 801, samples: 2})
	}
	add(sample{route: "runquill-1-1-pool__few__c", cpuLimit: 4, cpu: 0.1, memLimit: 1000, mem: 10, samples: 2})
	add(sample{route: "runquill-1-1-pool__free__d", cpuLimit: 0, cpu: 3, memLimit: 0, mem: 500, samples: 2})
	// A kill makes memory tight even below 80%; a kill without samples still counts.
	add(sample{route: "runquill-1-1-pool__kill__e", cpuLimit: 2, cpu: 0.1, memLimit: 1000, mem: 100, samples: 2, oom: new(true)})
	add(sample{route: "runquill-1-1-pool__kill__e", oom: new(true)})
	add(sample{route: "runquill-1-1-pool__k3s__f", oom: new(false)})
	pools := workerPools{}
	for _, name := range []string{"edge", "big", "few", "free", "kill", "k3s"} {
		pools[name] = workerPool{}
	}
	report, err := readRightsizing(t.Context(), q.DB, "pool", 7, pools, now)
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]string{}
	for _, g := range report.Groups {
		cpu, memory := "-", "-"
		if g.CPU != nil {
			cpu = g.CPU.Verdict
		}
		if g.Memory != nil {
			memory = g.Memory.Verdict
		}
		verdicts[g.Key] = fmt.Sprintf("%d/%d/%d %s %s", g.Jobs, g.SampledJobs, g.OOMKills, cpu, memory)
	}
	want := map[string]string{
		"edge": "3/3/0 ok ok", "big": "3/3/0 oversized tight", "few": "1/1/0 too_few_jobs too_few_jobs",
		"free": "1/1/0 unlimited unlimited", "kill": "2/1/2 too_few_jobs tight", "k3s": "1/0/0 - -",
	}
	if fmt.Sprint(verdicts) != fmt.Sprint(want) {
		t.Fatalf("verdicts %v\nwant %v", verdicts, want)
	}
	if report.Groups[0].Key != "big" && report.Groups[0].Key != "edge" {
		t.Fatalf("groups not ordered by jobs: %s", mustJSON(t, report.Groups))
	}

	// Per job, the key is the repository and runner label; old jobs fall out of the window.
	if _, err := q.Exec(`UPDATE jobs SET changed_at = ? WHERE route NOT LIKE '%edge%'`, now.Add(-3*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	report, err = readRightsizing(t.Context(), q.DB, "job", 2, pools, now)
	if err != nil || len(report.Groups) != 1 || report.Groups[0].Key != "owner/repo runquill-1-1-pool__edge__a" ||
		*report.Groups[0].Memory.PeakRatio != 0.8 || report.Groups[0].Memory.Limit != 1000 || report.Groups[0].CPU.PeakMax != 1 {
		t.Fatalf("by job: %s, %v", mustJSON(t, report), err)
	}
	for _, query := range []string{"group=host", "days=0", "days=8", "days=x"} {
		if _, _, ok := parseRightsizingQuery(mustQuery(t, query)); ok {
			t.Errorf("%s accepted", query)
		}
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	query, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return query
}

// GET /api/jobs/{id}, GET /api/usage/rightsizing, the CLI and MCP all show
// what job_resources holds.
func TestJobResourcesThroughPanelCLIAndMCP(t *testing.T) {
	f := newRemoteFixture(t)
	t.Setenv("RUNQUILL_TOKEN", f.token(102, "fleet:read"))
	if _, err := f.q.Exec(`INSERT INTO job_resources (job_id, samples, cpu_limit, memory_limit_mib, cpu_peak, cpu_sum, memory_peak_mib, oom_killed)
        VALUES (8, 4, 2, 1024, 1.5, 4, 1000, 1), (7, 0, NULL, NULL, NULL, 0, NULL, NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.Exec(`UPDATE job_resources SET cpu_limit_max = 2, memory_limit_max_mib = 1280 WHERE job_id = 8`); err != nil {
		t.Fatal(err)
	}
	_, code, stdout, stderr := runRemote(t, "jobs", "show", "8")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "CPU         peak 1.5, average 1 of 2 CPUs (4 samples)") ||
		!strings.Contains(stdout, "MEMORY      peak 1000 MiB of 1024 MiB (raised to 1280 MiB), killed for memory") {
		t.Fatalf("jobs show 8: code %d, stderr %q\n%s", code, stderr, stdout)
	}
	_, code, stdout, _ = runRemote(t, "jobs", "show", "7", "--json")
	var row panelHistoryJob
	if code != 0 || json.Unmarshal([]byte(stdout), &row) != nil || row.Resources == nil || row.Resources.Samples != 0 ||
		row.Resources.CPUPeak != nil || strings.Contains(stdout, "cpu_peak") {
		t.Fatalf("jobs show 7 --json: code %d\n%s", code, stdout)
	}
	if _, _, stdout, _ = runRemote(t, "jobs", "show", "7"); !strings.Contains(stdout, "CPU         -") {
		t.Fatalf("jobs show 7 without samples:\n%s", stdout)
	}

	_, code, stdout, stderr = runRemote(t, "usage", "--rightsizing")
	want := [][]string{
		{"POOL", "JOBS", "SAMPLED", "OOM", "CPU_PEAK", "CPU_LIMIT", "CPU", "MEMORY_PEAK_MIB", "MEMORY_LIMIT_MIB", "MEMORY"},
		{"(default)", "1", "1", "1", "1.5", "2", "too_few_jobs", "(75%)", "1000", "1024", "tight", "(98%)"},
	}
	if code != 0 || stderr != "" || !slices.EqualFunc(rows(stdout), want, slices.Equal) {
		t.Fatalf("usage --rightsizing: code %d, stderr %q\n%s", code, stderr, stdout)
	}
	_, code, stdout, _ = runRemote(t, "usage", "--rightsizing", "--group", "job", "--days", "1", "--json")
	var report rightsizingResponse
	if code != 0 || json.Unmarshal([]byte(stdout), &report) != nil || report.Group != "job" || report.Days != 1 ||
		len(report.Groups) != 1 || report.Groups[0].Key != "owner/lib runquill-8-1-build" {
		t.Fatalf("usage --rightsizing --json: code %d\n%s", code, stdout)
	}
	for _, test := range []struct {
		args    []string
		message string
	}{
		{[]string{"usage", "--rightsizing", "--group", "host"}, "--group pool or job"},
		{[]string{"usage", "--rightsizing", "--days", "8"}, "--days from 1 to 7"},
		{[]string{"usage", "--rightsizing", "--repo", "owner/lib"}, "not --from, --to or --repo"},
	} {
		if _, code, stdout, stderr := runRemote(t, test.args...); code != 2 || stdout != "" || !strings.Contains(stderr, test.message) {
			t.Errorf("%v: code %d, stdout %q, stderr %q", test.args, code, stdout, stderr)
		}
	}

	m := newMCPFixture(t)
	recordTestJob(t, m.q, 5, "runquill-5-1-ci", time.Now())
	if _, err := m.q.Exec(`UPDATE jobs SET phase = ?, changed_at = ? WHERE job_id = 5`, completed, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	recordJobOOM(t.Context(), m.q.DB, 5, true)
	var job mcpJob
	m.toolOK("get_job", map[string]any{"id": 5}, &job)
	if job.Resources == nil || job.Resources.OOMKilled == nil || !*job.Resources.OOMKilled {
		t.Fatalf("get_job resources: %+v", job.Resources)
	}
	var sized rightsizingResponse
	m.toolOK("job_rightsizing", map[string]any{"group": "job"}, &sized)
	if sized.Days != 7 || len(sized.Groups) != 1 || sized.Groups[0].OOMKills != 1 || sized.Groups[0].Memory.Verdict != "tight" {
		t.Fatalf("job_rightsizing: %s", mustJSON(t, sized))
	}
	m.toolFails("job_rightsizing", map[string]any{"group": "host"}, "group must be pool or job")
	m.toolFails("job_rightsizing", map[string]any{"days": 8}, "days must be")
}
