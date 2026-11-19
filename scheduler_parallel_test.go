package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// overlapRuntime records whether runtime calls on one host ever overlap.
type overlapRuntime struct {
	*testRuntime
	busy, overlaps atomic.Int32
}

func (r *overlapRuntime) enter() func() {
	if r.busy.Add(1) > 1 {
		r.overlaps.Add(1)
	}
	time.Sleep(5 * time.Millisecond)
	return func() { r.busy.Add(-1) }
}

func (r *overlapRuntime) startOn(ctx context.Context, host, name, jit string) error {
	defer r.enter()()
	return r.testRuntime.startOn(ctx, host, name, jit)
}

func (r *overlapRuntime) removeOn(ctx context.Context, host, name string) error {
	defer r.enter()()
	return r.testRuntime.removeOn(ctx, host, name)
}

// concurrencyGate holds GitHub job lookups until n are in flight at once, so a
// serial scheduler fails instead of passing by accident.
type concurrencyGate struct {
	n, inFlight, peak atomic.Int32
	mu                sync.Mutex
	release           chan struct{}
}

func newConcurrencyGate(n int32) *concurrencyGate {
	g := &concurrencyGate{release: make(chan struct{})}
	g.n.Store(n)
	return g
}

func (g *concurrencyGate) wait(t *testing.T) {
	current := g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	g.mu.Lock()
	if current > g.peak.Load() {
		g.peak.Store(current)
	}
	if current == g.n.Load() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	}
	g.mu.Unlock()
	select {
	case <-g.release:
	case <-time.After(5 * time.Second):
		t.Error("scheduler work did not run concurrently")
	}
}

func TestParallelSchedulerStopsStartingWorkAfterRateLimit(t *testing.T) {
	reset := time.Now().Add(10 * time.Minute).Unix()
	// Both in-flight launches reach GitHub before either sees the pause.
	gate := newConcurrencyGate(2)
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprintf(w, `{"token":"stub","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "GET /repos/owner/repo":
			gate.wait(t)
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
		}
	})
	q := testQueue(t)
	now := time.Now()
	for id := range int64(5) {
		recordTestJob(t, q, id+1, "runquill-1-1-ci", now)
	}
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}
	s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 5, workers: 2}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 5; id++ {
		j := readJob(t, q, id)
		if limited := id <= 2; limited != (j.retryAt == reset+1) || !limited && j.retryReason != "" {
			t.Errorf("job %d: %+v; only the two launches in flight when GitHub paused may be deferred", id, j)
		}
	}
}

func TestSchedulerRunsBoundedConcurrentWork(t *testing.T) {
	var completedJobs atomic.Bool
	launchGate, reconcileGate := newConcurrencyGate(3), newConcurrencyGate(3)
	var runners, deletes atomic.Int64
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch path := r.Method + " " + r.URL.Path; {
		case path == "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case path == "GET /repos/owner/a":
			fmt.Fprint(w, `{"id":2,"full_name":"owner/a","private":true,"owner":{"login":"owner"}}`)
		case strings.HasPrefix(path, "GET /repos/owner/a/actions/jobs/"):
			if id, _ := strings.CutPrefix(path, "GET /repos/owner/a/actions/jobs/"); completedJobs.Load() && id <= "4" {
				reconcileGate.wait(t)
				fmt.Fprint(w, `{"status":"completed"}`)
				return
			}
			launchGate.wait(t)
			fmt.Fprint(w, `{"status":"queued"}`)
		case path == "POST /repos/owner/a/actions/runners/generate-jitconfig":
			fmt.Fprintf(w, `{"runner":{"id":%d},"encoded_jit_config":"test-config"}`, runners.Add(1))
		case strings.HasPrefix(path, "DELETE /repos/owner/a/actions/runners/"):
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	q := testQueue(t)
	now := time.Now()
	for id := range int64(6) {
		recordRepositoryJob(t, q, id+1, "owner/a", "runquill-1-1-ci", now)
	}
	runtime := &overlapRuntime{testRuntime: &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}}
	s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 4, workers: 3}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if leased := leasedJobs(t, q); len(leased) != 4 || len(runtime.started) != 4 {
		t.Fatalf("leased %v, started %v: want exactly the 4 free slots used", leased, runtime.started)
	}
	if peak := launchGate.peak.Load(); peak != 3 {
		t.Fatalf("peak concurrent launches %d, want 3", peak)
	}

	// Every runner exits; the next tick reconciles them concurrently and frees their slots.
	for name := range runtime.hosts[0].Containers {
		runtime.hosts[0].Containers[name] = "exited"
	}
	completedJobs.Store(true)
	if err := s.tick(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if leased := leasedJobs(t, q); len(leased) != 2 || len(runtime.removed) != 4 || deletes.Load() != 4 {
		t.Fatalf("leased %v, removed %v, deletes %d: want 4 runners released and the 2 waiting jobs started",
			leased, runtime.removed, deletes.Load())
	}
	if peak := reconcileGate.peak.Load(); peak < 3 {
		t.Fatalf("peak concurrent reconciles %d, want at least 3", peak)
	}
	if overlaps := runtime.overlaps.Load(); overlaps != 0 {
		t.Fatalf("runtime calls overlapped on one host %d times", overlaps)
	}
}
