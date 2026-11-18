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
	legacy, err := openQueueDB(path, false)
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

	reader, err := openQueueDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	jobs, err := readStatusJobs(ctx, reader)
	if err != nil || len(jobs) != 1 || jobs[0].id != pending.id || jobs[0].retryReason != "" {
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
	jobs, err = readStatusJobs(ctx, reader)
	if err != nil || len(jobs) != 1 || jobs[0].id != pending.id ||
		jobs[0].retryAt != now.Add(retryDelay).Unix() || jobs[0].retryReason != "provision runner: Docker image missing" {
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
	var out bytes.Buffer
	if err := writeStatus(&out, jobs, containers, 3, now); err != nil {
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
