package main

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerPoolsValidatePolicyAndSetDockerResources(t *testing.T) {
	fleet, logPath := testDockerFleet(t)
	raw := fmt.Sprintf(`{"build":{"hosts":[%q],"repositories":["Owner/project"],"cpus":2.5,"memory_mib":4096,"image":"registry.example/runquill:build"}}`, fleetTestWorker)
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
	var buildFlags, defaultFlags bool
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.Contains(line, "create --name build-runner") {
			buildFlags = strings.Contains(line, "--cpus 2.5 --memory 4096m --memory-swap 4096m") && strings.HasSuffix(line, " registry.example/runquill:build")
		}
		if strings.Contains(line, "create --name default-runner") {
			defaultFlags = !strings.Contains(line, "--memory") && !strings.Contains(line, "--cpus") && !strings.Contains(line, "registry.example/runquill:build")
		}
	}
	if !buildFlags || !defaultFlags {
		t.Fatalf("incorrect Docker flags in %s", data)
	}
	for _, input := range []string{
		`{"build": {"hosts":["unknown"]}}`,
		`{"build": {"hosts":["local"],"memory_mib":2}}`,
		`{"build": {"hosts":["local"],"cpus":-1}}`,
		`{"build": {"hosts":["local"],"repositories":["../outside"]}}`,
		`{"build": {"hosts":["local"]},"other":{"hosts":["local"]}}`,
		`{"build": {"hosts":["local"],"privileged":"no"}}`,
		`{"build": {"hosts":["local"],"cache":"/cache"}}`,
		raw + raw,
	} {
		if _, err := fleet.configurePools(input); err == nil {
			t.Errorf("accepted invalid worker pools: %s", input)
		}
	}
}

func TestWorkerPoolsSetPrivilegeAndRepositoryCache(t *testing.T) {
	fleet, logPath := testDockerFleet(t)
	raw := fmt.Sprintf(`{"build":{"hosts":[%q],"privileged":false,"cache":true},"default":{"privileged":true}}`, fleetTestWorker)
	if _, err := fleet.configurePools(raw); err != nil {
		t.Fatal(err)
	}
	if err := fleet.startOn(t.Context(), fleetTestWorker, "uncached", "jit-config"); err == nil {
		t.Fatal("cache-enabled pool started a runner without a repository")
	}
	archive := func() map[string]*tar.Header {
		t.Helper()
		file, err := os.Open(logPath + ".tar")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		entries := make(map[string]*tar.Header)
		for reader := tar.NewReader(file); ; {
			header, err := reader.Next()
			if err == io.EOF {
				return entries
			}
			if err != nil {
				t.Fatal(err)
			}
			entries[header.Name] = header
		}
	}
	for _, repo := range []int64{7, 8} {
		if err := fleet.startOn(withRunnerRepository(t.Context(), repo), fleetTestWorker, fmt.Sprint("cached-", repo), "jit-config"); err != nil {
			t.Fatal(err)
		}
	}
	entries := archive()
	if jit, dir, cache, tools := entries["run/runquill/jit"], entries["run/runquill/"], entries["home/runner/.cache/"], entries["home/runner/.cache/tools/"]; jit == nil || jit.Uid != 1001 ||
		dir == nil || dir.Uid != 1001 || cache == nil || cache.Uid != 1001 || tools == nil || tools.Uid != 1001 || len(entries) != 4 {
		t.Fatalf("unprivileged runner archive: %+v", entries)
	}
	if err := fleet.startOn(withRunnerRepository(t.Context(), 7), "local", "default-runner", "jit-config"); err != nil {
		t.Fatal(err)
	}
	if entries := archive(); len(entries) != 1 || entries["run/runquill-jit"] == nil || entries["run/runquill-jit"].Uid != 0 {
		t.Fatalf("privileged runner archive: %+v", entries)
	}
	if _, err := fleet.configurePools(`{"default":{"cache":true}}`); err != nil {
		t.Fatal(err)
	}
	if err := fleet.startOn(withRunnerRepository(t.Context(), 7), "local", "privileged-cached", "jit-config"); err != nil {
		t.Fatal(err)
	}
	if entries := archive(); len(entries) != 3 || entries["run/runquill-jit"] == nil || entries["run/runquill-jit"].Uid != 0 ||
		entries["home/runner/.cache/"] == nil || entries["home/runner/.cache/"].Uid != 1001 ||
		entries["home/runner/.cache/tools/"] == nil || entries["home/runner/.cache/tools/"].Uid != 1001 {
		t.Fatalf("privileged cached runner archive: %+v", entries)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var cached7, cached8, privileged, privilegedCached bool
	for line := range strings.SplitSeq(string(data), "\n") {
		unprivileged := !strings.Contains(line, "--privileged") &&
			strings.Contains(line, "--user 1001:1001 --security-opt no-new-privileges --cap-drop ALL --env RUNQUILL_RUNNER_MODE=plain --env RUNQUILL_JIT_FILE=/run/runquill/jit")
		mount := "--mount type=volume,src=runquill-cache-%d,dst=/home/runner/.cache,volume-label=io.runquill.cache=1 --env RUNNER_TOOL_CACHE=/home/runner/.cache/tools"
		switch {
		case strings.Contains(line, "create --name cached-7"):
			cached7 = unprivileged && strings.Contains(line, fmt.Sprintf(mount, 7))
		case strings.Contains(line, "create --name cached-8"):
			cached8 = unprivileged && strings.Contains(line, fmt.Sprintf(mount, 8)) && !strings.Contains(line, "runquill-cache-7")
		case strings.Contains(line, "create --name default-runner"):
			privileged = strings.Contains(line, "--privileged") && !strings.Contains(line, "--mount") && !strings.Contains(line, "--user") &&
				!strings.Contains(line, "RUNNER_TOOL_CACHE")
		case strings.Contains(line, "create --name privileged-cached"):
			privilegedCached = strings.Contains(line, "--privileged") && strings.Contains(line, fmt.Sprintf(mount, 7)) && !strings.Contains(line, "--user")
		case strings.Contains(line, "create --name uncached"):
			t.Errorf("created a runner without its cache scope: %s", line)
		}
	}
	if !cached7 || !cached8 || !privileged || !privilegedCached {
		t.Fatalf("incorrect Docker runtime options in %s", data)
	}
}

func TestPoolSchedulerDoesNotSpillOrBlockOtherPools(t *testing.T) {
	const worker = "ssh://builder@worker"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	var labelsMu sync.Mutex
	var renamed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"fixture"}`)
		case r.URL.Path == "/repos/owner/repo":
			name := "owner/repo"
			if renamed.Load() {
				name = "owner/renamed"
			}
			fmt.Fprintf(w, `{"id":2,"full_name":%q,"private":true,"owner":{"login":"owner"}}`, name)
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
			labelsMu.Lock()
			labels = append(labels, request.Labels...)
			labelsMu.Unlock()
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
	scoped := &cacheScopeRuntime{testRuntime: runtime}
	s := &scheduler{queue: q, github: &github{client: server.Client(), base: server.URL, key: key, appID: "1", owner: "owner"},
		runtime: scoped, limit: 2, hostLimits: map[string]int{worker: 1},
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
	renamed.Store(true)
	if err := s.tick(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRowContext(ctx, "SELECT retry_reason FROM jobs WHERE job_id = 1").Scan(&reason); err != nil ||
		!strings.Contains(reason, "owner/renamed") || !strings.Contains(reason, "is not allowed") {
		t.Fatalf("renamed repository escaped pool restrictions: %q, %v", reason, err)
	}
	renamed.Store(false)
	if err := s.tick(ctx, now.Add(retryDelay+time.Second)); err != nil {
		t.Fatal(err)
	}
	labelsMu.Lock()
	defer labelsMu.Unlock()
	leases, err = q.jobs(ctx, "WHERE runner_name IS NOT NULL ORDER BY sequence")
	if err != nil || len(leases) != 2 || leases[0].id != 1 || leases[0].runnerHost != worker ||
		!slices.Equal(labels, []string{"runquill-17-1-local", "runquill-17-1-pool__build__first"}) {
		t.Fatalf("wrong worker pool or modified unique labels: %+v %v %v", leases, labels, err)
	}
	if !slices.Equal(scoped.volumes, []string{"runquill-cache-2", "runquill-cache-2"}) {
		t.Fatalf("runner starts were not scoped to their repository: %v", scoped.volumes)
	}
}

type cacheScopeRuntime struct {
	*testRuntime
	volumes []string
}

func (r *cacheScopeRuntime) startOn(ctx context.Context, host, name, jit string) error {
	volume, err := runnerCacheVolume(ctx)
	if err != nil {
		return err
	}
	r.volumes = append(r.volumes, volume)
	return r.testRuntime.startOn(ctx, host, name, jit)
}
