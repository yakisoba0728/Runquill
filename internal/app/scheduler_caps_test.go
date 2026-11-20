package app

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var capTestRepositories = map[string]int64{"owner/a": 2, "owner/b": 3}

// capTestGitHub accepts launches for owner/a and owner/b.
func capTestGitHub(t *testing.T) *github {
	t.Helper()
	var runners atomic.Int64
	return testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if r.Method == http.MethodPost && path == "/app/installations/1/access_tokens" {
			fmt.Fprint(w, `{"token":"stub"}`)
			return
		}
		for name, id := range capTestRepositories {
			switch rest, ok := strings.CutPrefix(path, "/repos/"+name); {
			case !ok:
			case rest == "":
				fmt.Fprintf(w, `{"id":%d,"full_name":%q,"private":true,"owner":{"login":"owner"}}`, id, name)
				return
			case strings.HasPrefix(rest, "/actions/jobs/"):
				fmt.Fprint(w, `{"status":"queued"}`)
				return
			case rest == "/actions/runners/generate-jitconfig":
				fmt.Fprintf(w, `{"runner":{"id":%d},"encoded_jit_config":"test-config"}`, runners.Add(1))
				return
			}
		}
		t.Errorf("unexpected API request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
}

func recordRepositoryJob(t *testing.T, q *queue, id int64, repository, route string, now time.Time) {
	t.Helper()
	j := job{id: id, repository: repository, repositoryID: capTestRepositories[repository], installationID: 1, route: route}
	if _, err := q.record(t.Context(), j, now); err != nil {
		t.Fatal(err)
	}
}

func leasedJobs(t *testing.T, q *queue) []int64 {
	t.Helper()
	jobs, err := q.jobs(t.Context(), "WHERE runner_name IS NOT NULL ORDER BY job_id")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, j := range jobs {
		ids = append(ids, j.id)
	}
	return ids
}

func TestSchedulerRepositoryCapLetsOtherRepositoriesRun(t *testing.T) {
	q := testQueue(t)
	now := time.Now()
	for i, repository := range []string{"owner/a", "owner/a", "owner/a", "owner/b"} {
		recordRepositoryJob(t, q, int64(i+1), repository, "runquill-1-1-ci", now)
	}
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}
	s := &scheduler{queue: q, github: capTestGitHub(t), runtime: runtime, limit: 3, maxPerRepository: 1}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if got := leasedJobs(t, q); !slices.Equal(got, []int64{1, 4}) {
		t.Fatalf("leased %v, want the first job of each repository", got)
	}
	for _, id := range []int64{2, 3} {
		if j := readJob(t, q, id); j.phase != queued || j.retryAt != 0 || j.retryReason != "" || j.failure != "" {
			t.Errorf("capped job %d must wait untouched: %+v", id, j)
		}
	}
	// A pool's own cap takes precedence over the global default.
	s.pools = workerPools{defaultWorkerPool: {MaxPerRepository: 2}}
	runtime.hosts[0].Pool = defaultWorkerPool
	if err := s.tick(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := leasedJobs(t, q); !slices.Equal(got, []int64{1, 2, 4}) {
		t.Fatalf("leased %v, want the pool cap of 2 applied", got)
	}
}

func TestSchedulerSharesContendedSlotsAcrossRepositories(t *testing.T) {
	q := testQueue(t)
	now := time.Now()
	recordRepositoryJob(t, q, 1, "owner/a", "runquill-1-1-ci", now)
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}
	s := &scheduler{queue: q, github: capTestGitHub(t), runtime: runtime, limit: 1}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	for i, repository := range []string{"owner/a", "owner/a", "owner/b"} {
		recordRepositoryJob(t, q, int64(i+2), repository, "runquill-1-1-ci", now)
	}
	// owner/a already holds a runner, so owner/b goes first, then FIFO resumes on the tie.
	s.limit = 3
	if err := s.tick(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := leasedJobs(t, q); !slices.Equal(got, []int64{1, 2, 4}) {
		t.Fatalf("leased %v, want owner/b ahead of owner/a's second job", got)
	}
	if got := runtime.started; len(got) != 3 || !strings.HasPrefix(got[1], "runquill-4-") || !strings.HasPrefix(got[2], "runquill-2-") {
		t.Fatalf("start order %v", got)
	}
}

func TestFairOrderKeepsFIFOForOneRepository(t *testing.T) {
	waiting := []job{{id: 1, repository: "a"}, {id: 2, repository: "a"}, {id: 3, repository: "a"}}
	o := newFairOrder(waiting)
	o.running["a"] = 5
	var got []int64
	for j, more := o.next(); more; j, more = o.next() {
		got = append(got, j.id)
		o.running[j.repository]++
	}
	if !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("order %v", got)
	}
}

func TestWorkerPoolsRejectInvalidRepositoryCap(t *testing.T) {
	fleet, _ := testDockerFleet(t)
	for _, raw := range []string{`{"default":{"max_per_repository":-1}}`, `{"default":{"max_per_repository":10001}}`} {
		if _, err := fleet.configurePools(raw); err == nil || !strings.Contains(err.Error(), "max_per_repository") {
			t.Errorf("%s: %v", raw, err)
		}
	}
	pools, err := fleet.configurePools(`{"default":{"max_per_repository":3}}`)
	if err != nil || pools[defaultWorkerPool].MaxPerRepository != 3 {
		t.Fatalf("valid cap rejected: %+v, %v", pools, err)
	}
}

func TestStalledAgeIgnoresJobsHeldByRepositoryCap(t *testing.T) {
	q := testQueue(t)
	now := time.Now()
	recordTestJob(t, q, 1, "runquill-1-1-running", now.Add(-time.Hour))
	recordTestJob(t, q, 2, "runquill-1-1-held", now.Add(-time.Hour))
	if _, err := q.ExecContext(t.Context(), "UPDATE jobs SET runner_name = 'runquill-1-busy' WHERE job_id = 1"); err != nil {
		t.Fatal(err)
	}
	if age, err := q.oldestQueuedAge(t.Context(), now, true); err != nil || age != 0 {
		t.Fatalf("job held by its repository cap counted as stalled: %v, %v", age, err)
	}
	if age, err := q.oldestQueuedAge(t.Context(), now, false); err != nil || age < 59*time.Minute {
		t.Fatalf("uncapped queue age: %v, %v", age, err)
	}
}
