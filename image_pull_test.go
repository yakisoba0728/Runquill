package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testImagePull(t *testing.T, policy string) *imageReadiness {
	t.Helper()
	images, err := loadImagePull(func(name string) string {
		if name == "RUNQUILL_IMAGE_PULL" {
			return policy
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return images
}

func TestLoadImagePull(t *testing.T) {
	for _, test := range []struct {
		policy, interval string
		want             imagePullPolicy
		every            time.Duration
		err              string
	}{
		{"", "", imagePullMissing, 6 * time.Hour, ""},
		{"always", "30m", imagePullAlways, 30 * time.Minute, ""},
		{" never ", "", imagePullNever, 6 * time.Hour, ""},
		{"Always", "", "", 0, "RUNQUILL_IMAGE_PULL must be missing, always or never"},
		{"if-not-present", "", "", 0, "RUNQUILL_IMAGE_PULL must be missing, always or never"},
		{"always", "10s", "", 0, "RUNQUILL_IMAGE_PULL_INTERVAL must be a duration between 1m and 168h"},
		{"always", "soon", "", 0, "RUNQUILL_IMAGE_PULL_INTERVAL must be a duration between 1m and 168h"},
	} {
		images, err := loadImagePull(func(name string) string {
			return map[string]string{"RUNQUILL_IMAGE_PULL": test.policy, "RUNQUILL_IMAGE_PULL_INTERVAL": test.interval}[name]
		})
		if test.err != "" {
			if err == nil || err.Error() != test.err {
				t.Errorf("%q/%q: error %v, want %q", test.policy, test.interval, err, test.err)
			}
			continue
		}
		if err != nil || images.policy != test.want || images.interval != test.every {
			t.Errorf("%q/%q: %+v, %v", test.policy, test.interval, images, err)
		}
	}
	for image, want := range map[string]string{"robot:hunter2@registry.example/team/runner:1": "registry.example/team/runner:1",
		"busybox:1.36@sha256:ab": "busybox:1.36@sha256:ab", "registry:5000/r@sha256:ab": "registry:5000/r@sha256:ab"} {
		if got := logImage(image); got != want {
			t.Errorf("logImage(%q) = %q", image, got)
		}
	}
	if (*imageReadiness)(nil).enabled() || testImagePull(t, "never").enabled() || !testImagePull(t, "").enabled() {
		t.Fatal("only missing and always pull images")
	}
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	t.Setenv("RUNQUILL_IMAGE_PULL", "sometimes")
	for _, backend := range []string{"docker", "kubernetes"} {
		t.Setenv("RUNQUILL_BACKEND", backend)
		if _, err := configureRunnerRuntime("img"); err == nil || !strings.Contains(err.Error(), "RUNQUILL_IMAGE_PULL") {
			t.Fatalf("an invalid policy must stop %s startup: %v", backend, err)
		}
	}
}

func TestImagePairsFollowLiveSettings(t *testing.T) {
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	base, err := parseDockerFleet("runner:1", "local,ssh://ci@build")
	if err != nil {
		t.Fatal(err)
	}
	base.images = testImagePull(t, "missing")
	pools, err := base.configurePools(`{"build":{"hosts":["ssh://ci@build"],"image":"runner:build"}}`)
	if err != nil {
		t.Fatal(err)
	}
	ops := &operations{base: runnerConfiguration{runtime: base, kind: "docker", pools: pools}, webhook: true}
	ops.current.Store(&operationState{runners: ops.base})
	pairs := func() []string {
		var got []string
		for _, pair := range imagePairsFrom(ops)() {
			got = append(got, pair.target.label()+"="+pair.image)
		}
		return got
	}
	if got := pairs(); !slices.Equal(got, []string{"local=runner:1", "ssh://ci@build=runner:build"}) {
		t.Fatalf("pairs %v", got)
	}
	// A panel edit rebuilds the fleet; the next cycle sees the new images on the same readiness store.
	state, err := ops.resolve(operationPolicy{Limit: 2, PollInterval: "30s", Image: "runner:2",
		Hosts: []string{"local", "ssh://ci@build"}, Pools: workerPools{"build": {Hosts: []string{"ssh://ci@build"}, Image: "runner:3"}}})
	if err != nil {
		t.Fatal(err)
	}
	ops.current.Store(state)
	if got := pairs(); !slices.Equal(got, []string{"local=runner:2", "ssh://ci@build=runner:3"}) {
		t.Fatalf("pairs after edit %v", got)
	}
	if state.runners.runtime.(*dockerFleet).images != base.images {
		t.Fatal("settings edits must keep the shared image readiness")
	}
	// Outbound agents keep their preloaded image, and Kubernetes pulls through the kubelet.
	agents := &dockerFleet{hosts: []docker{{image: "runner:1", host: "agent://w1"}, {image: "runner:1"}, {image: "runner:1"}}}
	if got := agents.imagePairs(); len(got) != 1 || got[0].target.label() != "local" {
		t.Fatalf("agent pairs %+v", got)
	}
	ops.current.Store(&operationState{runners: runnerConfiguration{runtime: &kubeRunners{}, kind: "kubernetes"}})
	if got := pairs(); len(got) != 0 {
		t.Fatalf("kubernetes pairs %v", got)
	}
}

// fakeImageEngine answers inspect from present and blocks each pull until gate yields.
type fakeImageEngine struct {
	mu        sync.Mutex
	present   map[imageKey]bool
	failing   map[string]bool
	down      bool
	pulls     []imageKey
	inflight  int
	peak      int
	gate      chan struct{}
	started   chan struct{}
	inspected atomic.Int64
}

func newFakeImageEngine() *fakeImageEngine {
	return &fakeImageEngine{present: make(map[imageKey]bool), failing: make(map[string]bool), started: make(chan struct{}, 16)}
}

func (e *fakeImageEngine) run(ctx context.Context, target docker, timeout time.Duration, args ...string) (string, error) {
	switch args[0] {
	case "image":
		e.inspected.Add(1)
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.down {
			return "", errors.New("docker image: exit status 1: Cannot connect to the Docker daemon")
		}
		if timeout != imageInspectTimeout || !e.present[imageKey{target.label(), args[2]}] {
			return "", fmt.Errorf("docker image: exit status 1: Error response from daemon: No such image: %s", args[2])
		}
		return "sha256:1\n", nil
	case "pull":
		key := imageKey{target.label(), args[1]}
		e.mu.Lock()
		e.pulls = append(e.pulls, key)
		e.inflight++
		e.peak = max(e.peak, e.inflight)
		gate := e.gate
		e.mu.Unlock()
		e.started <- struct{}{}
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.inflight--
		if timeout != imagePullTimeout {
			return "", errors.New("pull without its own long timeout")
		}
		if e.failing[args[1]] {
			return "", errors.New(`docker pull: exit status 1: Error response from daemon: Get "https://robot:hunter2@registry.example/v2/": unauthorized`)
		}
		e.present[key] = true
		return "", nil
	}
	return "", fmt.Errorf("unexpected command %v", args)
}

func (e *fakeImageEngine) pulled() []imageKey {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.pulls)
}

func testPairs(pairs ...string) func() []imagePair {
	return func() []imagePair {
		var out []imagePair
		for _, pair := range pairs {
			host, image, _ := strings.Cut(pair, "=")
			target := docker{image: image}
			if host != "local" {
				target.host = host
			}
			out = append(out, imagePair{target: target, image: image})
		}
		return out
	}
}

func TestImagePullerCapsAndDeduplicatesPulls(t *testing.T) {
	engine := newFakeImageEngine()
	engine.gate = make(chan struct{})
	images := testImagePull(t, "missing")
	deps := imagePullerDeps{Images: images, Run: engine.run, Now: time.Now,
		Pairs: testPairs("local=a:1", "local=b:1", "ssh://one=a:1", "ssh://two=a:1", "local=a:1")}
	deps.cycle(t.Context())
	for range 2 {
		<-engine.started
	}
	// Running cycles again neither repeats a pull in flight nor exceeds the cap or one pull per host.
	deps.cycle(t.Context())
	deps.cycle(t.Context())
	got := engine.pulled()
	if len(got) != 2 || !slices.Contains(got, imageKey{"local", "a:1"}) || !slices.Contains(got, imageKey{"ssh://one", "a:1"}) {
		t.Fatalf("first pulls %v", got)
	}
	if status, wait := images.observe("local", "a:1", false); status != "pulling" || wait != "pulling runner image a:1" {
		t.Fatalf("in-flight status %q %q", status, wait)
	}
	close(engine.gate)
	images.pulls.Wait()
	deps.cycle(t.Context())
	images.pulls.Wait()
	got = engine.pulled()
	if len(got) != 4 || !slices.Contains(got, imageKey{"local", "b:1"}) || !slices.Contains(got, imageKey{"ssh://two", "a:1"}) {
		t.Fatalf("all pulls %v", got)
	}
	if engine.peak > imagePullConcurrency {
		t.Fatalf("%d concurrent pulls", engine.peak)
	}
	deps.cycle(t.Context())
	images.pulls.Wait()
	if len(engine.pulled()) != 4 {
		t.Fatalf("present images pulled again under missing: %v", engine.pulled())
	}
	for _, key := range []imageKey{{"local", "a:1"}, {"local", "b:1"}, {"ssh://one", "a:1"}, {"ssh://two", "a:1"}} {
		if status, _ := images.observe(key.host, key.image, true); status != imagePullStatusReady {
			t.Fatalf("%v status %q", key, status)
		}
	}
}

func TestImagePullerBacksOffAndKeepsPresentImages(t *testing.T) {
	engine := newFakeImageEngine()
	engine.failing["gone:1"] = true
	now := time.Unix(1_800_000_000, 0)
	images := testImagePull(t, "missing")
	deps := imagePullerDeps{Images: images, Run: engine.run, Now: func() time.Time { return now }, Pairs: testPairs("local=gone:1")}
	pullsAfter := func(advance time.Duration) int {
		t.Helper()
		now = now.Add(advance)
		deps.cycle(t.Context())
		images.pulls.Wait()
		return len(engine.pulled())
	}
	if pullsAfter(0) != 1 {
		t.Fatal("a missing image was not pulled")
	}
	status, wait := images.observe("local", "gone:1", false)
	if status != "failed" || !strings.HasPrefix(wait, "runner image gone:1 is missing; pull failed: ") ||
		strings.Contains(wait, "hunter2") || !strings.Contains(wait, "https://registry.example") {
		t.Fatalf("failure reason %q %q", status, wait)
	}
	// 30s, then 60s, doubling to the 10 minute cap; inspects continue during backoff.
	inspected := engine.inspected.Load()
	if pullsAfter(29*time.Second) != 1 || engine.inspected.Load() == inspected {
		t.Fatal("pulled again inside the first backoff, or stopped inspecting")
	}
	if pullsAfter(time.Second) != 2 || pullsAfter(59*time.Second) != 2 || pullsAfter(time.Second) != 3 {
		t.Fatal("backoff is not exponential")
	}
	for range 8 {
		pullsAfter(imagePullMaxBackoff)
	}
	count := len(engine.pulled())
	if pullsAfter(imagePullMaxBackoff-time.Second) != count || pullsAfter(time.Second) != count+1 {
		t.Fatal("backoff is not capped at 10 minutes")
	}
	// An image built by hand during backoff is ready at once and never pulled under missing.
	engine.mu.Lock()
	engine.present[imageKey{"local", "gone:1"}] = true
	engine.mu.Unlock()
	if pullsAfter(time.Second) != count+1 {
		t.Fatal("pulled a present image under missing")
	}
	if status, wait := images.observe("local", "gone:1", true); status != imagePullStatusReady || wait != "" {
		t.Fatalf("hand-built image status %q %q", status, wait)
	}
	// An unreachable engine is not a missing image: no pull, no new failure.
	engine.mu.Lock()
	engine.down = true
	engine.mu.Unlock()
	deps.Pairs = testPairs("local=other:1")
	if pullsAfter(time.Hour) != count+1 {
		t.Fatal("pulled while the engine was unreachable")
	}

	// Under always, a present local-only image is refreshed on the interval; a failed refresh keeps it.
	engine = newFakeImageEngine()
	engine.present[imageKey{"local", "runquill-runner:local"}] = true
	engine.failing["runquill-runner:local"] = true
	images = testImagePull(t, "always")
	deps = imagePullerDeps{Images: images, Run: engine.run, Now: func() time.Time { return now }, Pairs: testPairs("local=runquill-runner:local")}
	if pullsAfter(0) != 1 || pullsAfter(time.Hour) != 1 || pullsAfter(5*time.Hour) != 2 {
		t.Fatalf("always refreshes: %v", engine.pulled())
	}
	if status, wait := images.observe("local", "runquill-runner:local", true); status != imagePullStatusReady || wait != "" {
		t.Fatalf("a failed refresh must keep the present image: %q %q", status, wait)
	}
}

func TestRunImagePullerStopsAndHonoursNever(t *testing.T) {
	engine := newFakeImageEngine()
	runImagePuller(t.Context(), imagePullerDeps{Images: testImagePull(t, "never"), Run: engine.run, Pairs: testPairs("local=a:1")})
	runImagePuller(t.Context(), imagePullerDeps{Run: engine.run, Pairs: testPairs("local=a:1")})
	if engine.inspected.Load() != 0 {
		t.Fatal("never must not touch images")
	}
	images := testImagePull(t, "missing")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runImagePuller(ctx, imagePullerDeps{Images: images, Run: engine.run, Cycle: time.Hour, Pairs: testPairs("local=a:1")})
		close(done)
	}()
	<-engine.started
	cancel()
	<-done
	images.pulls.Wait()
}

func TestFleetImageReadinessReplacesMissingImageError(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	t.Setenv("FAKE_INFO", dockerRootfulInfo)
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(`#!/bin/sh
case "$1" in
  ps) exit 0 ;;
  info) printf '%s\n' "$FAKE_INFO" ;;
  image) echo "Error response from daemon: No such image: $3" >&2; exit 1 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	fleet, err := parseDockerFleet("runner:1", "local")
	if err != nil {
		t.Fatal(err)
	}
	// never: today's startup failure and unavailable host.
	if err := fleet.prepare(t.Context()); err == nil || !strings.Contains(err.Error(), "No such image") {
		t.Fatalf("prepare without pulls: %v", err)
	}
	hosts, err := fleet.snapshot(t.Context())
	if err != nil || hosts[0].Err == nil || hosts[0].ImageWait != "" || hosts[0].ImageStatus != "" {
		t.Fatalf("snapshot without pulls: %+v, %v", hosts, err)
	}
	fleet.images = testImagePull(t, "never")
	if err := fleet.prepare(t.Context()); err == nil {
		t.Fatal("never must keep the startup failure")
	}
	fleet.images = testImagePull(t, "missing")
	if err := fleet.prepare(t.Context()); err != nil {
		t.Fatalf("a missing image must not stop startup: %v", err)
	}
	hosts, err = fleet.snapshot(t.Context())
	want := "runner image runner:1 is missing; waiting to pull it"
	if err != nil || hosts[0].Err != nil || hosts[0].ImageWait != want || hosts[0].ImageStatus != "missing" || hosts[0].Image != "runner:1" {
		t.Fatalf("snapshot with pulls: %+v, %v", hosts, err)
	}
	select {
	case <-fleet.images.wake:
	default:
		t.Fatal("a missing image must wake the puller")
	}
	if again, err := fleet.snapshot(t.Context()); err != nil || again[0].ImageWait != want || len(fleet.images.wake) != 0 {
		t.Fatalf("every tick woke the puller: %+v, %v", again, err)
	}
	var out strings.Builder
	doctor := func(name string, err error) { fmt.Fprintf(&out, "%s %v\n", name, err) }
	checkDocker(t.Context(), fleet.hosts[0], fleet.images, doctor)
	checkDocker(t.Context(), fleet.hosts[0], testImagePull(t, "never"), doctor)
	if text := out.String(); !strings.Contains(text, "runner image runner:1 is not on this host yet; the manager pulls it (RUNQUILL_IMAGE_PULL=missing) <nil>\n") ||
		!strings.Contains(text, "runner image runner:1 docker image: exit status 1: Error response from daemon: No such image: runner:1\n") {
		t.Fatalf("doctor output:\n%s", text)
	}
	capacity, _ := fleetCapacity(hosts, nil, nil, nil, 2)
	report := summarizeStatus(nil, capacity, 0, 2, time.Now())
	if row := report.Hosts[0]; row.Status != "UNAVAILABLE" || row.Error != want || row.ImageStatus != "missing" {
		t.Fatalf("status row %+v", row)
	}
	if d := report.explainWaiting("", capacity); d.Code != "workers_unavailable" {
		t.Fatalf("diagnostic %+v", d)
	}
}

// imageGateGitHub accepts launches for owner/a and counts every request.
func imageGateGitHub(t *testing.T, requests *atomic.Int64) *github {
	t.Helper()
	return testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch path := r.URL.Path; {
		case path == "/app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case path == "/repos/owner/a":
			fmt.Fprint(w, `{"id":2,"full_name":"owner/a","private":true,"owner":{"login":"owner"}}`)
		case strings.HasPrefix(path, "/repos/owner/a/actions/jobs/"):
			fmt.Fprint(w, `{"status":"queued"}`)
		case path == "/repos/owner/a/actions/runners/generate-jitconfig":
			fmt.Fprint(w, `{"runner":{"id":9},"encoded_jit_config":"test-config"}`)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestSchedulerWaitsForRunnerImageBeforeProvisioning(t *testing.T) {
	q := testQueue(t)
	now := time.Now()
	recordRepositoryJob(t, q, 1, "owner/a", "runquill-1-1-ci", now)
	var requests atomic.Int64
	waiting := dockerHostSnapshot{Name: "local", Containers: map[string]string{}, Image: "runner:1",
		ImageStatus: "pulling", ImageWait: "pulling runner image runner:1"}
	runtime := &testRuntime{hosts: []dockerHostSnapshot{waiting}}
	s := &scheduler{queue: q, github: imageGateGitHub(t, &requests), runtime: runtime, limit: 2}
	for range 2 {
		if err := s.tick(t.Context(), now); err != nil {
			t.Fatal(err)
		}
	}
	j := readJob(t, q, 1)
	if requests.Load() != 0 || len(runtime.started) != 0 || j.runnerName != "" {
		t.Fatalf("provisioned without a ready image: %d requests, started %v", requests.Load(), runtime.started)
	}
	if j.retryReason != "waiting for runner image runner:1 on local" || j.retryAt != 0 {
		t.Fatalf("waiting job %+v", j)
	}
	// Another eligible host with the image takes the job.
	runtime.hosts = append(runtime.hosts, dockerHostSnapshot{Name: "ssh://ci@build", Containers: map[string]string{}, Image: "runner:1", ImageStatus: imagePullStatusReady})
	if err := s.tick(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 1); j.runnerHost != "ssh://ci@build" || j.retryReason != "" || len(runtime.started) != 1 {
		t.Fatalf("job not started on the ready host: %+v", j)
	}
	// A reason clears once no host waits for an image, even before a launch.
	recordRepositoryJob(t, q, 2, "owner/a", "runquill-1-1-ci", now)
	runtime.hosts = []dockerHostSnapshot{waiting}
	if err := s.tick(t.Context(), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 2); j.retryReason != "waiting for runner image runner:1 on local" {
		t.Fatalf("second job %+v", j)
	}
	s.pools = workerPools{defaultWorkerPool: {}, "other": {Hosts: []string{"ssh://other"}}}
	runtime.hosts = []dockerHostSnapshot{{Name: "local", Pool: defaultWorkerPool, Containers: map[string]string{}, Err: errors.New("down")},
		{Name: "ssh://other", Pool: "other", Containers: map[string]string{}}}
	if err := s.tick(t.Context(), now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 2); j.retryReason != "" {
		t.Fatalf("stale image reason %+v", j)
	}
	// Under never a missing image is a host error as before: the job waits untouched.
	before := requests.Load()
	s.pools = nil
	runtime.hosts = []dockerHostSnapshot{{Name: "local", Containers: map[string]string{},
		Err: errors.New("runner image runner:1 on Docker host \"local\": No such image")}}
	if err := s.tick(t.Context(), now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 2); j.retryReason != "" || j.runnerName != "" || requests.Load() != before {
		t.Fatalf("never changed behaviour: %+v", j)
	}
}

func TestImageMessagesHaveTranslations(t *testing.T) {
	keys, patterns := panelTranslations(t)
	images := testImagePull(t, "missing")
	var messages []string
	_, wait := images.observe("ssh://ci@build:22", "ghcr.io/o/r@sha256:ab", false)
	messages = append(messages, wait, imageWaitPrefix+"ghcr.io/o/r:1 on ssh://ci@build")
	images.claim("local", "x:1", false, time.Now())
	_, wait = images.observe("local", "x:1", false)
	messages = append(messages, wait)
	images.finish("local", "x:1", false, errors.New("denied"), time.Now())
	_, wait = images.observe("local", "x:1", false)
	messages = append(messages, wait)
	images.pulls.Done()
	placeholder := regexp.MustCompile(`\{[a-z]+\}`)
	for _, message := range messages {
		matched := false
		for pattern := range patterns {
			parts := placeholder.Split(pattern, -1)
			for i := range parts {
				parts[i] = regexp.QuoteMeta(parts[i])
			}
			if regexp.MustCompile("^(?s)"+strings.Join(parts, "(.+?)")+"$").MatchString(message) && keys[pattern] {
				matched = true
			}
		}
		if !matched {
			t.Errorf("panelweb/i18n.js has no translated serverPatterns template for %q", message)
		}
	}
}

// TestImagePullOnRealDocker removes busybox:1.36, lets the puller fetch it, and records a failure for a tag
// that does not exist, while a present local-only tag keeps working. It runs only with RUNQUILL_TEST_DOCKER_PULL=1.
func TestImagePullOnRealDocker(t *testing.T) {
	if os.Getenv("RUNQUILL_TEST_DOCKER_PULL") != "1" {
		t.Skip("set RUNQUILL_TEST_DOCKER_PULL=1 to pull from Docker Hub")
	}
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	const image, absent, local = "busybox:1.36", "busybox:runquill-no-such-tag", "runquill-pull-test:local"
	exec.Command("docker", "image", "rm", image).Run()
	exec.Command("docker", "image", "rm", local).Run()
	t.Cleanup(func() { exec.Command("docker", "image", "rm", local).Run() })
	target := docker{image: image}
	if _, err := target.command(t.Context(), nil, "image", "inspect", image); err == nil || !imageMissing(err) {
		t.Fatalf("%s is still present: %v", image, err)
	}
	images := testImagePull(t, "missing")
	deps := imagePullerDeps{Images: images, Run: runImageCommand, Now: time.Now, Pairs: testPairs("local=" + image)}
	deps.cycle(t.Context())
	images.pulls.Wait()
	if err := target.checkRunnerImage(t.Context()); err != nil {
		t.Fatalf("not pulled: %v", err)
	}
	if status, _ := images.observe("local", image, true); status != imagePullStatusReady {
		t.Fatalf("status %q", status)
	}
	deps.Pairs = testPairs("local=" + absent)
	deps.cycle(t.Context())
	images.pulls.Wait()
	if status, wait := images.observe("local", absent, false); status != "failed" || !strings.Contains(wait, "pull failed") {
		t.Fatalf("absent tag %q %q", status, wait)
	}
	// always: a local-only tag fails to refresh but stays ready.
	if out, err := exec.Command("docker", "tag", image, local).CombinedOutput(); err != nil {
		t.Fatalf("tag: %v %s", err, out)
	}
	images = testImagePull(t, "always")
	deps = imagePullerDeps{Images: images, Run: runImageCommand, Now: time.Now, Pairs: testPairs("local=" + local)}
	deps.cycle(t.Context())
	images.pulls.Wait()
	images.mu.Lock()
	refreshed := !images.entries[imageKey{"local", local}].refreshed.IsZero()
	images.mu.Unlock()
	if status, _ := images.observe("local", local, true); status != imagePullStatusReady || !refreshed {
		t.Fatalf("local-only image after a failed refresh: %q refreshed=%t", status, refreshed)
	}
	if err := (docker{image: local}).checkRunnerImage(t.Context()); err != nil {
		t.Fatalf("local-only image removed: %v", err)
	}
}
