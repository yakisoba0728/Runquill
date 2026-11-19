package main

import (
	"slices"
	"testing"
	"time"
)

func TestSchedulerSendsFleetNotifications(t *testing.T) {
	srv, sink := notifyTestServer(t, nil)
	n := notifyTestNew(t, notifyConfig{URL: srv.URL, StallAfter: time.Minute})
	q := testQueue(t)
	now := time.Now()
	recordTestJob(t, q, 1, "runquill-1-1-expired", now.Add(-queueExpiry-time.Minute))
	recordTestJob(t, q, 2, "runquill-1-1-stalled", now.Add(-5*time.Minute))
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local"}}}
	// Ticks run on a copy of the scheduler, so state must survive through operations.
	ops := &operations{}
	ops.current.Store(&operationState{policy: operationPolicy{Limit: 1}, runners: runnerConfiguration{runtime: runtime}})
	s := &scheduler{queue: q, operations: ops, alerts: &fleetAlerts{notifier: n}}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ExecContext(t.Context(), "DELETE FROM jobs WHERE job_id = 2"); err != nil {
		t.Fatal(err)
	}
	runtime.hosts[0].Containers = map[string]string{}
	if err := s.tick(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	notifyTestClose(t, n)
	want := []string{"job_failed", "queue_stalled", "host_offline", "queue_recovered", "host_recovered"}
	if got := sink.kinds(); !slices.Equal(got, want) {
		t.Fatalf("notifications %v, want %v", got, want)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if failed := sink.bodies[0]; failed["repository"] != "owner/repo" || failed["job_id"] != float64(1) ||
		failed["reason"] != "not started within 24 hours" {
		t.Fatalf("job failure payload %v", failed)
	}
}

func TestSchedulerWithoutNotifierIgnoresAlerts(t *testing.T) {
	q := testQueue(t)
	now := time.Now()
	recordTestJob(t, q, 1, "runquill-1-1-expired", now.Add(-queueExpiry-time.Minute))
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local"}}}
	for _, alerts := range []*fleetAlerts{nil, {}} {
		s := &scheduler{queue: q, runtime: runtime, limit: 1, alerts: alerts}
		if err := s.tick(t.Context(), now); err != nil {
			t.Fatal(err)
		}
	}
	if j := readJob(t, q, 1); j.phase != completed {
		t.Fatalf("expired job not failed: %+v", j)
	}
}
