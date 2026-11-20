package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStatusReadsLegacyQueueAndPersistedRetries(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1_800_000_000, 0)
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	legacy, err := openQueueDB(path, "rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, schema); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	pending := job{id: 40, repository: "owner/repo", repositoryID: 2, installationID: 1,
		route: "runquill-17-1-ci"}
	_, err = legacy.ExecContext(ctx, `INSERT INTO jobs
        (job_id, repository, repository_id, installation_id, route, phase, changed_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)`, pending.id, pending.repository, pending.repositoryID,
		pending.installationID, pending.route, pending.phase, now.Unix())
	legacy.Close()
	if err != nil {
		t.Fatal(err)
	}

	reader, err := openQueueDB(path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	jobs, err := readStatusJobs(ctx, reader)
	if err != nil || len(jobs) != 1 || jobs[0].id != pending.id || jobs[0].retryReason != "" || jobs[0].runnerHost != "" {
		t.Fatalf("read legacy queue: jobs=%+v, err=%v", jobs, err)
	}
	if _, err := reader.ExecContext(ctx, "UPDATE jobs SET phase = 2"); err == nil {
		t.Fatal("status connection allowed a write")
	}

	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	s := &scheduler{queue: q}
	if err := s.retry(ctx, pending, now, "provision runner", errors.New("Docker image\nmissing")); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ExecContext(ctx, "UPDATE jobs SET runner_name = ?, runner_host = ? WHERE job_id = ?", "runner-a", "local", pending.id); err != nil {
		t.Fatal(err)
	}
	jobs, err = readStatusJobs(ctx, reader)
	if err != nil || len(jobs) != 1 || jobs[0].id != pending.id ||
		jobs[0].retryAt != now.Add(retryDelay).Unix() || jobs[0].retryReason != "provision runner: Docker image missing" || jobs[0].runnerHost != "local" {
		t.Fatalf("read live upgraded queue: jobs=%+v, err=%v", jobs, err)
	}
}

func TestStatusShowsCapacityQueueOrderAndLinks(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	jobs := []job{
		{id: 90, runnerName: "runner-a", phase: inProgress},
		{id: 80, retryAt: now.Add(5 * time.Second).Unix(), retryReason: "Docker image missing"},
		{id: 60, repository: "owner/repo", route: "runquill-17-1-test-0"},
		{id: 20, repository: "owner/other", route: "runquill-19-2-ci", retryReason: "GitHub unavailable"},
	}
	containers := map[string]string{"runner-a": "running", "runner-b": "created", "runner-c": "exited"}
	hosts := []hostCapacity{{dockerHostSnapshot: dockerHostSnapshot{Name: "local", Containers: containers}, Limit: 3, Occupied: 2}}
	var out bytes.Buffer
	if err := summarizeStatus(jobs, hosts, 0, 3, now).writeText(&out); err != nil {
		t.Fatal(err)
	}
	var got []string
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			got = append(got, strings.Join(fields, " "))
		}
	}
	want := []string{
		"Runners: 1 running, 2/3 slots occupied",
		"HOST STATUS MODE SLOTS DETAIL",
		"local UP ACTIVE 2/3",
		"STATE POSITION JOB DETAIL",
		"RUNNING - 90 runner-a",
		"RETRY - 80 in 5s — Docker image missing",
		"WAITING 1 60 owner/repo",
		"https://github.com/owner/repo/actions/runs/17/job/60",
		"WAITING 2 20 last retry — GitHub unavailable",
		"https://github.com/owner/other/actions/runs/19/job/20",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected status output:\n%s", out.String())
	}
}

func TestStatusShowsOfflineAndCordonedHosts(t *testing.T) {
	hosts := []hostCapacity{
		{dockerHostSnapshot: dockerHostSnapshot{Name: "local", Containers: map[string]string{"runner-a": "running"}}, Limit: 2, Occupied: 1, Cordoned: true},
		{dockerHostSnapshot: dockerHostSnapshot{Name: "ssh://worker", Err: errors.New("connection refused")}, Limit: 3, Occupied: 1},
		{dockerHostSnapshot: dockerHostSnapshot{Name: "ssh://builder", Containers: map[string]string{"runner-d": "running"}, Err: errors.New("runner image missing")}, Limit: 2, Occupied: 1},
	}
	jobs := []job{
		{id: 90, runnerName: "runner-a", runnerHost: "local", phase: inProgress},
		{id: 80, runnerName: "runner-b", runnerHost: "ssh://worker", phase: inProgress},
		{id: 70, runnerName: "runner-c", phase: inProgress},
		{id: 60, runnerName: "runner-d", runnerHost: "ssh://builder", phase: inProgress},
	}
	var out bytes.Buffer
	if err := summarizeStatus(jobs, hosts, 1, 5, time.Now()).writeText(&out); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"Runners: 2 known running, 4/5 slots occupied or reserved",
		"Slots reserved without a known host: 1",
		"local UP CORDONED 1/2",
		"ssh://worker OFFLINE ACTIVE 1/3 reserved connection refused",
		"ssh://builder UNAVAILABLE ACTIVE 1/2 runner image missing",
		"RUNNING - 90 runner-a on local",
		"OFFLINE - 80 runner-b on ssh://worker",
		"UNKNOWN - 70 runner-c",
		"RUNNING - 60 runner-d on ssh://builder",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in status output:\n%s", want, out.String())
		}
	}
}

func TestStatusReadsConfiguredLimitsAndCordon(t *testing.T) {
	testDockerFleet(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	t.Setenv("RUNQUILL_DB", path)
	t.Setenv("RUNQUILL_MAX_RUNNERS", "3")
	t.Setenv("RUNQUILL_DOCKER_HOSTS", "local,"+fleetTestWorker)
	t.Setenv("RUNQUILL_DOCKER_HOST_LIMITS", "local=1,"+fleetTestWorker+"=2")
	t.Setenv("FLEET_TEST_WORKER_STATE", "offline")
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	if err := setHostCordon(ctx, &bytes.Buffer{}, "local", true); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := status(ctx, &out, false); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(got, "local UP CORDONED 1/1") || !strings.Contains(got, fleetTestWorker+" OFFLINE ACTIVE") {
		t.Fatalf("unexpected status output:\n%s", out.String())
	}
	now := time.Now()
	for _, id := range []int64{42, 51} {
		j := job{id: id, repository: "owner/repo", route: "runquill-172-1-ci"}
		if _, err := q.record(ctx, j, now); err != nil {
			t.Fatal(err)
		}
	}
	retryAt := now.Add(time.Minute).Unix()
	if _, err := q.ExecContext(ctx, "UPDATE jobs SET retry_at = ?, retry_reason = 'GitHub unavailable' WHERE job_id = 42", retryAt); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := status(ctx, &out, true); err != nil {
		t.Fatal(err)
	}
	var decoded statusReport
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 1 || decoded.SlotLimit != 3 || decoded.Runners == nil ||
		decoded.Runners.KnownRunning != 1 || decoded.Runners.OccupiedSlots != 1 ||
		decoded.Runners.ReservedSlots != 2 || decoded.Runners.OfflineHosts != 1 ||
		len(decoded.Hosts) != 2 || !decoded.Hosts[0].Cordoned || decoded.Hosts[1].Status != "OFFLINE" ||
		decoded.Hosts[1].ReservedSlots != 2 || decoded.DockerError != "" {
		t.Fatalf("unexpected JSON host snapshot: %s", out.String())
	}
	if len(decoded.Jobs) != 2 || decoded.Jobs[0].Status != "RETRY" || decoded.Jobs[0].RetryAt == nil ||
		decoded.Jobs[0].RetryAt.Unix() != retryAt || decoded.Jobs[0].RetryReason != "GitHub unavailable" ||
		decoded.Jobs[1].QueuePosition != 1 || decoded.Jobs[1].URL != "https://github.com/owner/repo/actions/runs/172/job/51" {
		t.Fatalf("unexpected JSON jobs: %s", out.String())
	}
	t.Setenv("FLEET_TEST_WORKER_STATE", "duplicate-engine")
	out.Reset()
	if err := status(ctx, &out, true); err == nil {
		t.Fatal("expected conflicting Docker topology to fail")
	}
	decoded = statusReport{}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Runners != nil || decoded.Hosts == nil || len(decoded.Hosts) != 0 || len(decoded.Jobs) != 2 ||
		!strings.Contains(decoded.DockerError, "identify the same engine") {
		t.Fatalf("unexpected JSON on Docker failure: %s", out.String())
	}
}

func TestStatusReportsObservedJobStagesWithoutOverwritingDiscovery(t *testing.T) {
	ctx := t.Context()
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	now := time.Now().UTC().Truncate(time.Second)
	j := job{id: 7, phase: queued, githubCreatedAt: now.Add(-4 * time.Second).Unix(), discoverySource: "poll"}
	if changed, err := q.record(ctx, j, now); !changed || err != nil {
		t.Fatalf("record poll discovery: %v, %v", changed, err)
	}
	j.discoverySource = "webhook"
	if changed, err := q.record(ctx, j, now.Add(time.Second)); changed || err != nil {
		t.Fatalf("record duplicate webhook: %v, %v", changed, err)
	}
	observed := workflowJob{Status: "in_progress", StartedAt: now.Add(5 * time.Second).Format(time.RFC3339)}
	if err := q.advanceGitHubJob(ctx, j.id, inProgress, now.Add(6*time.Second), observed); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ExecContext(ctx, `UPDATE jobs SET runner_name = 'runner', provisioned_at = ? WHERE job_id = ?`, now.Add(2*time.Second).Unix(), j.id); err != nil {
		t.Fatal(err)
	}
	rows, err := readStatusJobs(ctx, q.DB)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read job stages: %+v, %v", rows, err)
	}
	timing := jobTiming(rows[0], now.Add(time.Minute))
	if timing == nil || timing.DiscoverySource != "poll" || !timing.DiscoveredAt.Equal(now) ||
		timing.DiscoverySeconds == nil || *timing.DiscoverySeconds != 4 ||
		timing.QueueSeconds == nil || *timing.QueueSeconds != 2 ||
		timing.AssignmentSeconds == nil || *timing.AssignmentSeconds != 3 {
		t.Fatalf("unexpected timing: %+v", timing)
	}
}

func TestStageTimingsDistinguishRegistrationRuntimeAndAssignment(t *testing.T) {
	j := job{
		githubCreatedAt: 10, discoveredAt: 12, provisionedAt: 14, githubStartedAt: 19,
		jitRequestedMS: 13600, jitRegisteredMS: 14100, runnerLaunchMS: 15250,
	}
	timing := jobTiming(j, time.Unix(20, 0))
	for _, check := range []struct {
		value *int64
		want  int64
	}{
		{timing.DiscoverySeconds, 2}, {timing.SchedulerMillis, 1600}, {timing.JITMillis, 500},
		{timing.RuntimeMillis, 1150}, {timing.GitHubAssignSeconds, 4}, {timing.AssignmentSeconds, 5},
	} {
		if check.value == nil || *check.value != check.want {
			t.Fatalf("timing %+v does not preserve %d for stage %v", timing, check.want, check.value)
		}
	}
	j.jitRequestedMS, j.jitRegisteredMS, j.runnerLaunchMS = 0, 0, 0
	legacy := jobTiming(j, time.Unix(20, 0))
	if legacy.JITMillis != nil || legacy.RuntimeMillis != nil || legacy.GitHubAssignSeconds != nil || legacy.AssignmentSeconds == nil {
		t.Fatalf("legacy timing fabricated unavailable stages: %+v", legacy)
	}
	claimed := jobTiming(job{jitRequestedMS: 13600}, time.Unix(20, 0))
	if claimed == nil || claimed.JITRequestedAt == nil || claimed.JITRequestedAt.UnixMilli() != 13600 || claimed.JITMillis != nil {
		t.Fatalf("timing did not retain a JIT attempt on an older job: %+v", claimed)
	}
}
