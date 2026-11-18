package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWorkerPoolsValidatePolicyAndSetDockerResources(t *testing.T) {
	fleet, logPath := testDockerFleet(t)
	raw := fmt.Sprintf(`{"build":{"hosts":[%q],"repositories":["Owner/project"],"cpus":2.5,"memory_mib":4096}}`, fleetTestWorker)
	pools, err := fleet.configurePools(raw)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := fleet.snapshot(t.Context())
	if err != nil || snapshots[0].Pool != "default" || snapshots[1].Pool != "build" {
		t.Fatalf("incorrect pool assignment: %+v, %v", snapshots, err)
	}
	selected := job{repository: "owner/project", route: "runquill-98-2-pool__build__test-0"}
	if pool, err := pools.forJob(selected); pool != "build" || err != nil {
		t.Fatalf("allowed repository was rejected: %q, %v", pool, err)
	}
	selected.repository = "owner/other"
	if pool, err := pools.forJob(selected); pool != "build" || err == nil {
		t.Fatalf("repository restriction was bypassed: %q, %v", pool, err)
	}
	if err := fleet.startOn(t.Context(), fleetTestWorker, "build-runner", "jit-config"); err != nil {
		t.Fatal(err)
	}
	if err := fleet.startOn(t.Context(), "local", "default-runner", "jit-config"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.Contains(line, "create --name build-runner") &&
			!strings.Contains(line, "--cpus 2.5 --memory 4096m --memory-swap 4096m") {
			t.Errorf("missing Docker resource flags: %s", line)
		}
		if strings.Contains(line, "create --name default-runner") && strings.Contains(line, "--cpus") {
			t.Errorf("unexpected resource limit in default pool: %s", line)
		}
	}
	for _, input := range []string{
		`{"build": {"hosts":["unknown"]}}`,
		`{"build": {"hosts":["local"],"memory_mib":2}}`,
		`{"build": {"hosts":["local"],"cpus":-1}}`,
		`{"build": {"hosts":["local"],"repositories":["../outside"]}}`,
		`{"build": {"hosts":["local"]},"other":{"hosts":["local"]}}`,
		raw + raw,
	} {
		if _, err := fleet.configurePools(input); err == nil {
			t.Errorf("accepted invalid worker pools: %s", input)
		}
	}
}

func TestPoolSchedulerDoesNotSpillOrBlockOtherPools(t *testing.T) {
	const worker = "ssh://builder@worker"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"fixture"}`)
		case r.URL.Path == "/repos/owner/repo":
			fmt.Fprint(w, `{"id":2,"private":true,"owner":{"login":"owner"}}`)
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/actions/jobs/"):
			fmt.Fprint(w, `{"status":"queued"}`)
		case r.URL.Path == "/repos/owner/repo/actions/runners/generate-jitconfig":
			var request struct{ Labels []string }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if len(request.Labels) != 1 {
				t.Errorf("expected a single route label, got %+v", request.Labels)
			}
			labels = append(labels, request.Labels...)
			fmt.Fprint(w, `{"runner":{"id":52},"encoded_jit_config":"test-config"}`)
		default:
			t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	ctx, now := t.Context(), time.Now()
	runtime := &testRuntime{hosts: []dockerHostSnapshot{
		{Name: "local", Pool: "default", Containers: make(map[string]string)},
		{Name: worker, Pool: "build", Containers: map[string]string{"occupying-build-worker": "running"}},
	}}
	s := &scheduler{queue: q, github: &github{client: server.Client(), base: server.URL, key: key, appID: "1", owner: "owner"},
		runtime: runtime, limit: 2, hostLimits: map[string]int{worker: 1},
		pools: workerPools{defaultWorkerPool: {}, "build": {Repositories: []string{"owner/repo"}}},
	}
	for _, j := range []job{
		{id: 1, installationID: 1, repositoryID: 2, repository: "owner/repo", route: "runquill-17-1-pool__build__first"},
		{id: 2, installationID: 1, repositoryID: 9, repository: "owner/other", route: "runquill-18-1-pool__build__denied"},
		{id: 3, installationID: 1, repositoryID: 2, repository: "owner/repo", route: "runquill-17-1-local"},
	} {
		if _, err := q.record(ctx, j, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	leases, err := q.jobs(ctx, "WHERE runner_name IS NOT NULL ORDER BY sequence")
	if err != nil || len(leases) != 1 || leases[0].id != 3 || leases[0].runnerHost != "local" {
		t.Fatalf("busy pool blocked or spilled into another pool: %+v, %v", leases, err)
	}
	var reason string
	if err := q.QueryRowContext(ctx, "SELECT retry_reason FROM jobs WHERE job_id = 2").Scan(&reason); err != nil ||
		!strings.Contains(reason, "is not allowed to use worker pool") {
		t.Fatalf("missing repository pool denial: %q, %v", reason, err)
	}
	clear(runtime.hosts[1].Containers)
	if err := s.tick(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	leases, err = q.jobs(ctx, "WHERE runner_name IS NOT NULL ORDER BY sequence")
	if err != nil || len(leases) != 2 || leases[0].id != 1 || leases[0].runnerHost != worker ||
		!slices.Equal(labels, []string{"runquill-17-1-local", "runquill-17-1-pool__build__first"}) {
		t.Fatalf("wrong worker pool or modified unique labels: %+v %v %v", leases, labels, err)
	}
}
