package app

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// exitingRuntime starts runners that exit at once, like a broken runner image or entrypoint.
type exitingRuntime struct {
	*testRuntime
}

func (r *exitingRuntime) startOn(ctx context.Context, host, name, jit string) error {
	if err := r.testRuntime.startOn(ctx, host, name, jit); err != nil {
		return err
	}
	for _, target := range r.hosts {
		if target.Name == host {
			target.Containers[name] = "exited"
		}
	}
	return nil
}

func TestLaunchDelayBacksOff(t *testing.T) {
	for failures, want := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
		4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		if got := launchDelay(failures); got != want {
			t.Errorf("launchDelay(%d) = %v, want %v", failures, got, want)
		}
	}
	if got := launchDelay(1 << 20); got != maxLaunchDelay {
		t.Errorf("launchDelay after many failures = %v, want %v", got, maxLaunchDelay)
	}
}

func TestSchedulerBacksOffRunnersThatExitImmediately(t *testing.T) {
	var provisioned, deleted atomic.Int32
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo":
			fmt.Fprint(w, `{"id":2,"private":true,"owner":{"login":"owner"}}`)
		case "GET /repos/owner/repo/actions/jobs/9":
			fmt.Fprint(w, `{"status":"queued"}`)
		case "POST /repos/owner/repo/actions/runners/generate-jitconfig":
			provisioned.Add(1)
			fmt.Fprint(w, `{"runner":{"id":55},"encoded_jit_config":"test-config"}`)
		case "DELETE /repos/owner/repo/actions/runners/55":
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	q := testQueue(t)
	start := time.Unix(1_800_000_000, 0)
	recordTestJob(t, q, 9, "runquill-1-1-ci", start)
	runtime := &exitingRuntime{&testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}}
	s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 1}
	const step = 5 * time.Second
	var released []time.Time
	for now := start; now.Before(start.Add(2 * time.Hour)); now = now.Add(step) {
		before := readJob(t, q, 9)
		if err := s.tick(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		after := readJob(t, q, 9)
		if before.runnerName != "" && after.runnerName == "" {
			released = append(released, now)
			if want := now.Add(launchDelay(before.launchFailures)).Unix(); after.retryAt != want || after.launchFailures != before.launchFailures+1 {
				t.Fatalf("release %d: retry at %d after %d failures, want %d after %d", len(released), after.retryAt,
					after.launchFailures, want, before.launchFailures+1)
			}
		}
		if len(released) == 1 && provisioned.Load() == 1 && now.Sub(released[0]) > retryDelay {
			t.Fatalf("a single failed launch must retry after %v as before, still waiting at %v", retryDelay, now.Sub(released[0]))
		}
	}
	// Fixed 15-second retries would register about 360 runners in two hours.
	if n := provisioned.Load(); n < 8 || n > 20 {
		t.Fatalf("registered %d runners in two hours, want a bounded number with backoff", n)
	}
	if int(deleted.Load()) != len(released) {
		t.Fatalf("deleted %d GitHub registrations for %d releases", deleted.Load(), len(released))
	}
	j := readJob(t, q, 9)
	if j.phase != queued || j.failure != "" || j.retryAt-released[len(released)-1].Unix() != int64(maxLaunchDelay.Seconds()) {
		t.Fatalf("job after repeated exits: %+v, last release %v", j, released[len(released)-1])
	}
}

func TestLaunchFailureCountResets(t *testing.T) {
	q := testQueue(t)
	now := time.Unix(1_800_000_000, 0)
	recordTestJob(t, q, 9, "runquill-1-1-ci", now)
	if _, err := q.ExecContext(t.Context(), "UPDATE jobs SET launch_failures = 6, runner_name = 'r', phase = ?", inProgress); err != nil {
		t.Fatal(err)
	}
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo/actions/jobs/9":
			fmt.Fprint(w, `{"status":"completed"}`)
		case "GET /repos/owner/repo/actions/runners":
			fmt.Fprint(w, `{"runners":[]}`)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	s := &scheduler{queue: q, github: gh}
	// A runner that picked the job up resets the count when it is released.
	if _, err := s.reconcile(t.Context(), readJob(t, q, 9), "", jobDiagnostic{}, now); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 9); j.runnerName != "" || j.launchFailures != 0 || j.phase != completed {
		t.Fatalf("finished job kept its launch failures: %+v", j)
	}
}

func TestSchedulerProvisionFailuresBackOff(t *testing.T) {
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo":
			fmt.Fprint(w, `{"id":2,"private":true,"owner":{"login":"owner"}}`)
		case "GET /repos/owner/repo/actions/jobs/9":
			w.WriteHeader(http.StatusBadGateway)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	q := testQueue(t)
	now := time.Unix(1_800_000_000, 0)
	recordTestJob(t, q, 9, "runquill-1-1-ci", now)
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}
	s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 1}
	for failures := range 8 {
		if before := readJob(t, q, 9); before.retryAt > 0 {
			now = time.Unix(before.retryAt, 0)
		}
		if err := s.tick(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		after := readJob(t, q, 9)
		if after.launchFailures != failures+1 || after.retryAt != now.Add(launchDelay(failures)).Unix() || after.runnerName != "" {
			t.Fatalf("failure %d: %+v", failures+1, after)
		}
	}
}

func TestQueueUpgradeAddsLaunchFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	old, err := openQueueDB(path, "rwc")
	if err != nil {
		t.Fatal(err)
	}
	// The first released schema, before any later column was added.
	if _, err := old.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at, retry_at)
        VALUES (9, 'owner/repo', 2, 1, 'runquill-1-1-ci', 0, 1800000000, 1800000015)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		q, err := openQueue(path)
		if err != nil {
			t.Fatal(err)
		}
		j := readJob(t, q, 9)
		if j.launchFailures != 0 || j.retryAt != 1800000015 || j.phase != queued {
			t.Fatalf("upgraded job: %+v", j)
		}
		q.close()
	}
	// An earlier release writes jobs without naming the new column.
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	if _, err := q.Exec(`INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at)
        VALUES (10, 'owner/repo', 2, 1, 'runquill-1-1-ci', 0, 1800000000)`); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 10); j.launchFailures != 0 {
		t.Fatalf("job written without the column: %+v", j)
	}
}
