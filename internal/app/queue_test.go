package app

import (
	"testing"
	"time"
)

func TestQueueRecordMovesQueuedJobToNewInstallation(t *testing.T) {
	q := testQueue(t)
	first := time.Unix(1_800_000_000, 0)
	recordTestJob(t, q, 9, "runquill-1-1-ci", first)
	record := func(id, installation int64, p phase, at time.Time) bool {
		t.Helper()
		changed, err := q.record(t.Context(), job{id: id, repository: "owner/repo", repositoryID: 2, installationID: installation,
			route: "runquill-1-1-ci", phase: p, discoverySource: "poll"}, at)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}

	// The App was reinstalled: the same queued job is seen from the new installation.
	if !record(9, 5, queued, first.Add(time.Hour)) {
		t.Fatal("re-observation from a new installation reported no change")
	}
	if j := readJob(t, q, 9); j.installationID != 5 || j.changedAt != first.Unix() || j.phase != queued {
		t.Fatalf("queued job after reinstall: %+v, want installation 5 and its original age", j)
	}
	if record(9, 5, queued, first.Add(2*time.Hour)) {
		t.Fatal("a repeated observation changed the job")
	}

	// A leased job keeps the installation that registered its runner.
	if _, err := q.ExecContext(t.Context(), "UPDATE jobs SET runner_name = 'runquill-9-a' WHERE job_id = 9"); err != nil {
		t.Fatal(err)
	}
	if record(9, 6, queued, first.Add(3*time.Hour)) {
		t.Fatal("a leased job moved to another installation")
	}
	if j := readJob(t, q, 9); j.installationID != 5 {
		t.Fatalf("leased job changed installation: %+v", j)
	}

	// A phase change keeps the installation and still updates the phase.
	if !record(9, 7, inProgress, first.Add(4*time.Hour)) {
		t.Fatal("phase change was not recorded")
	}
	if j := readJob(t, q, 9); j.installationID != 5 || j.phase != inProgress || j.changedAt != first.Add(4*time.Hour).Unix() {
		t.Fatalf("job after phase change: %+v", j)
	}

	// A different repository ID under the same job ID is not the same job.
	recordTestJob(t, q, 10, "runquill-1-1-ci", first)
	if _, err := q.record(t.Context(), job{id: 10, repository: "owner/repo", repositoryID: 3, installationID: 5,
		route: "runquill-1-1-ci", phase: queued}, first); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 10); j.installationID != 1 || j.repositoryID != 2 {
		t.Fatalf("job moved for another repository: %+v", j)
	}
}
