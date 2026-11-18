package main

import (
	"bytes"
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
	_, err = (&queue{DB: legacy}).record(ctx, pending, now)
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
	if err := writeStatus(&out, jobs, hosts, 0, 3, now); err != nil {
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
	if err := writeStatus(&out, jobs, hosts, 1, 5, time.Now()); err != nil {
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
	if err := status(ctx, &out); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(got, "local UP CORDONED 1/1") || !strings.Contains(got, fleetTestWorker+" OFFLINE ACTIVE") {
		t.Fatalf("unexpected status output:\n%s", out.String())
	}
}
