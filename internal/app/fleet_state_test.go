package app

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFleetCapacityRestoresOfflineOrphanSlotsAndLegacyLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	ctx, now := t.Context(), time.Now()
	const worker = "ssh://runner@worker"
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if _, err := q.record(ctx, job{id: id, repository: "owner/repo"}, now); err != nil {
			q.close()
			t.Fatal(err)
		}
	}
	if _, err := q.ExecContext(ctx, "UPDATE jobs SET runner_name = 'old-runner' WHERE job_id = 1"); err != nil {
		q.close()
		t.Fatal(err)
	}
	if _, err := q.ExecContext(ctx, "UPDATE jobs SET runner_name = 'owned-runner', runner_host = ? WHERE job_id = 2", worker); err != nil {
		q.close()
		t.Fatal(err)
	}
	runtime := &testRuntime{hosts: []dockerHostSnapshot{
		{Name: "local", Containers: map[string]string{"old-runner": "running"}},
		{Name: worker, Containers: map[string]string{"owned-runner": "running", "orphan-runner": "paused"}},
	}}
	s := &scheduler{queue: q, runtime: runtime, limit: 4}
	err = s.tick(ctx, now)
	q.close()
	if err != nil {
		t.Fatal(err)
	}

	q, err = openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	active, err := q.jobs(ctx, "WHERE runner_name IS NOT NULL ORDER BY sequence")
	if err != nil || len(active) != 2 || active[0].runnerHost != "local" {
		t.Fatalf("legacy lease adoption: %+v, error %v", active, err)
	}
	states, err := readHostStates(ctx, q.DB)
	if err != nil {
		t.Fatal(err)
	}
	runtime.hosts[1].Containers = nil
	hosts, unknown := fleetCapacity(runtime.hosts, states, active, nil, s.limit)
	if unknown != 0 || hosts[0].Occupied != 1 || hosts[1].Occupied != 2 {
		t.Fatalf("capacity after worker failure: %+v, unknown=%d", hosts, unknown)
	}
}
