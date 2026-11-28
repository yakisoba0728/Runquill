package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/yakisoba0728/Runquill/internal/route"
)

func testReleaseImages(t *testing.T, raw string) *runnerImages {
	t.Helper()
	images, err := buildRunnerImages(raw, "ghcr.io/o/runquill", "v1.2.3", "runner:default")
	if err != nil {
		t.Fatal(err)
	}
	return images
}

func TestRouteSuffixGrammar(t *testing.T) {
	for _, test := range []struct{ suffix, pool, os, rest, err string }{
		{"build", "", "", "build", ""},
		{"pool__big__build", "big", "", "build", ""},
		{"os__ubuntu2204__build", "", "ubuntu2204", "build", ""},
		{"os__ubuntu-22-04__build-3", "", "ubuntu-22-04", "build-3", ""},
		{"os__ubuntu-latest__build", "", "ubuntu-latest", "build", ""},
		{"pool__big__os__debian12__build", "big", "debian12", "build", ""},
		// The pool comes first; a job ID that only looks like a selector after the OS is part of the job.
		{"os__debian12__pool__big__x", "", "debian12", "pool__big__x", ""},
		{"os__debian12__", "", "", "", "invalid runner OS"},
		{"os__debian12", "", "", "", "invalid runner OS"},
		{"os____build", "", "", "", "invalid runner OS"},
		{"os__-x__build", "", "", "", "invalid runner OS"},
		{"os__ubuntu_22__build", "", "", "", "invalid runner OS"},
		{"pool__big__os__x", "", "", "", "invalid runner OS"},
		{"pool__Big__build", "", "", "", "invalid worker pool"},
		// Labels without a selector prefix keep their meaning.
		{"osx__build", "", "", "osx__build", ""},
		{"my-os__build", "", "", "my-os__build", ""},
	} {
		pool, osName, rest, err := route.SplitSuffix(test.suffix)
		if test.err != "" {
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Errorf("%s: error %v, want %q", test.suffix, err, test.err)
			}
			continue
		}
		if err != nil || pool != test.pool || osName != test.os || rest != test.rest {
			t.Errorf("%s: got %q %q %q %v", test.suffix, pool, osName, rest, err)
		}
	}
	for route, want := range map[string]string{
		"runquill-1-1-build":                        "",
		"runquill-1-1-pool__big__build":             "",
		"runquill-1-1-os__ubuntu2204__build":        "ubuntu2204",
		"runquill-1-1-pool__big__os__debian12__x-2": "debian12",
		"runquill-1-1":                              "",
	} {
		if got, err := routeOS(route); err != nil || got != want {
			t.Errorf("routeOS(%s) = %q, %v", route, got, err)
		}
	}
	if _, err := routeOS("runquill-1-1-os__bad_name__x"); !errors.Is(err, route.ErrInvalidOS) {
		t.Errorf("invalid OS route: %v", err)
	}
	// The pool selector still works with an OS selector after it, and existing labels are unchanged.
	pools := workerPools{defaultWorkerPool: {}, "big": {}}
	for route, want := range map[string]string{"runquill-1-1-build": "default", "runquill-1-1-pool__big__build": "big",
		"runquill-1-1-pool__big__os__debian12__build": "big", "runquill-1-1-os__debian12__build": "default"} {
		if got, err := pools.forJob(job{route: route}); err != nil || got != want {
			t.Errorf("forJob(%s) = %q, %v", route, got, err)
		}
	}
}

func TestWebhookAcceptsOSSelectorsWithinTheLabelLimit(t *testing.T) {
	repo := repository{ID: 1, FullName: "owner/repo", Private: true}
	repo.Owner.Login = "owner"
	accept := func(label string) bool {
		_, ok, err := workflowJob{ID: 5, RunID: 7, Labels: []string{label}}.queueJob("owner", false, repo, 1, "queued")
		return ok && err == nil
	}
	for _, label := range []string{"runquill-7-1-os__ubuntu2204__build", "runquill-7-1-pool__big__os__ubuntu-latest__build-2", "runquill-7-1-build"} {
		if !accept(label) {
			t.Errorf("%s was not accepted", label)
		}
	}
	long := "runquill-7-1-os__ubuntu2204__" + strings.Repeat("a", 100-len("runquill-7-1-os__ubuntu2204__"))
	if !accept(long) || accept(long+"a") {
		t.Error("the 100 character label limit must apply to OS selectors too")
	}
	// Dots are not part of the label alphabet; the dot-free spelling of a catalogue name is used instead.
	if accept("runquill-7-1-os__ubuntu22.04__build") {
		t.Error("a dotted label was accepted")
	}
}

func TestRunnerImageCatalogue(t *testing.T) {
	release := testReleaseImages(t, "")
	want := map[string]string{
		"debian12":      "ghcr.io/o/runquill-runner:v1.2.3-debian12",
		"debian13":      "ghcr.io/o/runquill-runner:v1.2.3-debian13",
		"ubuntu-latest": "runner:default",
		"ubuntu22.04":   "ghcr.io/o/runquill-runner:v1.2.3-ubuntu22.04",
		"ubuntu24.04":   "ghcr.io/o/runquill-runner:v1.2.3",
		"ubuntu26.04":   "ghcr.io/o/runquill-runner:v1.2.3-ubuntu26.04",
	}
	got := make(map[string]string)
	for _, entry := range release.list() {
		got[entry.Name] = entry.Image
		if _, _, _, err := route.SplitSuffix("os__" + entry.Label + "__test"); err != nil {
			t.Errorf("label %q does not fit a route", entry.Label)
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("release catalogue %v", got)
	}
	for _, spelling := range []string{"ubuntu22.04", "ubuntu-22.04", "ubuntu2204", "ubuntu-2204", "Ubuntu-22-04"} {
		if entry, ok := release.lookup(spelling); !ok || entry.Name != "ubuntu22.04" || entry.Label != "ubuntu2204" {
			t.Errorf("alias %s: %+v %t", spelling, entry, ok)
		}
	}
	// A development build knows only the default image; it never invents release tags.
	for _, ver := range []string{"dev", ""} {
		dev, err := buildRunnerImages("", "", ver, "runner:local")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range dev.list() {
			names = append(names, entry.Name+"="+entry.Image)
		}
		if !slices.Equal(names, []string{"ubuntu-latest=runner:local", "ubuntu24.04=runner:local"}) {
			t.Fatalf("dev catalogue %v", names)
		}
	}
	if dev, _ := buildRunnerImages("", "ghcr.io/o/runquill", "dev", "x:1"); len(dev.list()) != 2 {
		t.Fatal("a release image without a version must not add variants")
	}
	// The default follows live settings.
	if entry, _ := release.withDefault("runner:edited").lookup("ubuntu-latest"); release.withDefault("runner:edited").resolved(entry).Image != "runner:edited" {
		t.Fatal("default entries must follow the edited default image")
	}
	// Configured entries add to and override the built-ins, in any registry.
	configured := testReleaseImages(t, `{"ubuntu-22.04":"docker.io/library/ubuntu-runner:22.04","alpine3":"registry.example:5000/team/runner@sha256:abc","ubuntu-latest":"r/latest:1"}`)
	if entry, _ := configured.lookup("ubuntu2204"); entry.Image != "docker.io/library/ubuntu-runner:22.04" || entry.Source != runnerImageSourceConfigured || entry.Name != "ubuntu-22.04" {
		t.Fatalf("override %+v", entry)
	}
	if entry, _ := configured.lookup("alpine3"); entry.Image != "registry.example:5000/team/runner@sha256:abc" {
		t.Fatalf("added %+v", entry)
	}
	if len(configured.list()) != 7 {
		t.Fatalf("configured catalogue %+v", configured.list())
	}
	for _, raw := range []string{
		`[]`, `"x"`, `null`, `{"a":"b:1"}{}`, `{"a":1}`,
		`{"Ubuntu":"x:1"}`, `{"a_b":"x:1"}`, `{"":"x:1"}`, `{"-a":"x:1"}`, `{"` + strings.Repeat("a", 41) + `":"x:1"}`,
		`{"ubuntu22.04":"x:1","ubuntu2204":"y:1"}`,
		`{"a":"user:secret@registry.example/x:1"}`, `{"a":"https://registry.example/x:1"}`, `{"a":"-x"}`, `{"a":"x y"}`, `{"a":""}`,
	} {
		if _, err := buildRunnerImages(raw, "", "dev", "x:1"); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	t.Setenv(runnerImagesEnv, `{"debian12":"x:1"}`)
	t.Setenv("RUNQUILL_BACKEND", "kubernetes")
	if _, err := configureRunnerRuntime("x:1"); err != nil && strings.Contains(err.Error(), runnerImagesEnv) {
		t.Fatal(err)
	}
	t.Setenv(runnerImagesEnv, `{"bad name":"x:1"}`)
	if _, err := configureRunnerRuntime("x:1"); err == nil || !strings.Contains(err.Error(), runnerImagesEnv) {
		t.Fatalf("an invalid catalogue must stop startup: %v", err)
	}
}

func TestPoolImageRestrictionAndEffectiveImage(t *testing.T) {
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	fleet, err := parseDockerFleet("runner:default", "local,ssh://ci@build")
	if err != nil {
		t.Fatal(err)
	}
	pools, err := fleet.configurePools(`{"build":{"hosts":["ssh://ci@build"],"image":"runner:build","images":["ubuntu22.04","debian-12"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"build":{"hosts":["ssh://ci@build"],"images":["bad name"]}}`, `{"build":{"hosts":["ssh://ci@build"],"images":"x"}}`} {
		if _, err := fleet.configurePools(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	images := testReleaseImages(t, "")
	build, open := pools["build"], pools[defaultWorkerPool]
	for _, test := range []struct {
		route, effective, err string
		pool                  workerPool
	}{
		{"runquill-1-1-pool__build__x", "runner:build", "", build},
		{"runquill-1-1-pool__build__os__ubuntu2204__x", "ghcr.io/o/runquill-runner:v1.2.3-ubuntu22.04", "", build},
		{"runquill-1-1-pool__build__os__debian12__x", "ghcr.io/o/runquill-runner:v1.2.3-debian12", "", build},
		{"runquill-1-1-pool__build__os__debian13__x", "", "does not allow runner image", build},
		// ubuntu-latest is the default image, even in a pool with its own image, and obeys the pool's list.
		{"runquill-1-1-pool__build__os__ubuntu-latest__x", "", "does not allow runner image", build},
		{"runquill-1-1-os__ubuntu-latest__x", "runner:default", "", open},
		{"runquill-1-1-pool__big__os__ubuntu-latest__x", "runner:default", "", workerPool{Image: "runner:big"}},
		{"runquill-1-1-x", "runner:default", "", open},
		{"runquill-1-1-os__debian13__x", "ghcr.io/o/runquill-runner:v1.2.3-debian13", "", open},
		{"runquill-1-1-os__nope__x", "", "not in the runner image catalogue", open},
	} {
		_, err := images.forJob(test.route, test.pool)
		if test.err != "" && (err == nil || !strings.Contains(err.Error(), test.err)) || test.err == "" && err != nil {
			t.Errorf("%s: %v", test.route, err)
		}
		if got := images.effective(test.route, test.pool); got != test.effective {
			t.Errorf("%s: effective %q, want %q", test.route, got, test.effective)
		}
	}
	var none *runnerImages
	if _, err := none.forJob("runquill-1-1-os__debian12__x", open); err == nil {
		t.Error("no catalogue must not resolve a selector")
	}
	if selected, err := none.forJob("runquill-1-1-x", open); err != nil || selected.Image != "" {
		t.Error("no catalogue must keep jobs without a selector working")
	}
}

// imageTestRuntime is a testRuntime whose hosts behave like a dockerFleet for image placement,
// recording the image each runner was started from.
type imageTestRuntime struct {
	*testRuntime
	fleet  *dockerFleet
	images map[string]string
}

func (r *imageTestRuntime) startOn(ctx context.Context, host, name, jit string) error {
	if err := r.testRuntime.startOn(ctx, host, name, jit); err != nil {
		return err
	}
	r.images[name] = runnerImageFrom(ctx)
	return nil
}

func (r *imageTestRuntime) canRun(host, image string) (bool, bool) {
	return r.fleet.canRun(host, image)
}
func (r *imageTestRuntime) imageReady(ctx context.Context, host, image string, now time.Time) imageState {
	return r.fleet.imageReady(ctx, host, image, now)
}
func (r *imageTestRuntime) requestImage(host, image string, now time.Time) {
	r.fleet.requestImage(host, image, now)
}

func newImageTestRuntime(t *testing.T, hosts string, snapshots ...dockerHostSnapshot) *imageTestRuntime {
	t.Helper()
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	fleet, err := parseDockerFleet("runner:1", hosts)
	if err != nil {
		t.Fatal(err)
	}
	fleet.images = testImagePull(t, "missing")
	return &imageTestRuntime{testRuntime: &testRuntime{hosts: snapshots}, fleet: fleet, images: make(map[string]string)}
}

func TestSchedulerPullsCatalogueImagesOnDemand(t *testing.T) {
	q := testQueue(t)
	now := time.Now()
	recordRepositoryJob(t, q, 1, "owner/a", "runquill-1-1-os__debian12__ci", now)
	var requests atomic.Int64
	runtime := newImageTestRuntime(t, "local,ssh://ci@build",
		dockerHostSnapshot{Name: "local", Containers: map[string]string{}, Image: "runner:1", ImageStatus: imagePullStatusReady},
		dockerHostSnapshot{Name: "ssh://ci@build", Containers: map[string]string{}, Image: "runner:1", ImageStatus: imagePullStatusReady})
	images, err := buildRunnerImages(`{"debian12":"runner:debian12"}`, "", "dev", "runner:1")
	if err != nil {
		t.Fatal(err)
	}
	// Two workers wrap the runtime per host; placement must still see the fleet.
	s := &scheduler{queue: q, github: imageGateGitHub(t, &requests), runtime: runtime, limit: 8, images: images, workers: 2}
	for i := range 2 {
		if err := s.tick(t.Context(), now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	j := readJob(t, q, 1)
	if requests.Load() != 0 || len(runtime.started) != 0 || j.retryReason != "waiting for runner image runner:debian12 on local" || j.retryAt != 0 {
		t.Fatalf("provisioned before the image was ready: %d requests, %+v", requests.Load(), j)
	}
	// Only the chosen host pulls the catalogue image; the default images stay as they were.
	var pairs []string
	for _, pair := range runtime.fleet.imagePairs() {
		pairs = append(pairs, pair.target.label()+"="+pair.image)
	}
	if !slices.Equal(pairs, []string{"local=runner:1", "ssh://ci@build=runner:1", "local=runner:debian12"}) {
		t.Fatalf("pairs %v", pairs)
	}
	select {
	case <-runtime.fleet.images.wake:
	default:
		t.Fatal("a new demand must wake the puller")
	}
	engine := newFakeImageEngine()
	engine.present[imageKey{"local", "runner:1"}] = true
	engine.present[imageKey{"ssh://ci@build", "runner:1"}] = true
	deps := imagePullerDeps{Images: runtime.fleet.images, Run: engine.run, Now: time.Now, Pairs: runtime.fleet.imagePairs}
	deps.cycle(t.Context())
	runtime.fleet.images.pulls.Wait()
	if got := engine.pulled(); !slices.Equal(got, []imageKey{{"local", "runner:debian12"}}) {
		t.Fatalf("pulled %v", got)
	}
	if err := s.tick(t.Context(), now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	j = readJob(t, q, 1)
	if j.runnerHost != "local" || j.retryReason != "" || len(runtime.started) != 1 || runtime.images[j.runnerName] != "runner:debian12" {
		t.Fatalf("job not started from its image: %+v %v", j, runtime.images)
	}
	// A host that already holds the image is preferred over a less busy one that does not.
	recordRepositoryJob(t, q, 2, "owner/a", "runquill-1-1-os__debian12__ci", now)
	if err := s.tick(t.Context(), now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 2); j.runnerHost != "local" || runtime.images[j.runnerName] != "runner:debian12" ||
		len(runtime.fleet.images.demanded(now)) != 1 {
		t.Fatalf("second job %+v", j)
	}
	// Jobs without a selector keep the host image and no override; ubuntu-latest is the host's own default image.
	recordRepositoryJob(t, q, 3, "owner/a", "runquill-1-1-ci", now)
	recordRepositoryJob(t, q, 5, "owner/a", "runquill-1-1-os__ubuntu-latest__ci", now)
	if err := s.tick(t.Context(), now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 3); j.runnerName == "" || runtime.images[j.runnerName] != "" {
		t.Fatalf("plain job %+v %v", j, runtime.images)
	}
	if j := readJob(t, q, 5); j.runnerName == "" || runtime.images[j.runnerName] != "runner:1" {
		t.Fatalf("ubuntu-latest job %+v %v", j, runtime.images)
	}
	// An unknown name keeps the job waiting with a reason an administrator can act on.
	recordRepositoryJob(t, q, 4, "owner/a", "runquill-1-1-os__plan9__ci", now)
	if err := s.tick(t.Context(), now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 4); j.phase != queued || j.runnerName != "" || j.retryAt == 0 ||
		j.retryReason != `select runner image: runner image "plan9" is not in the runner image catalogue; add it to RUNQUILL_RUNNER_IMAGES` {
		t.Fatalf("unknown image job %+v", j)
	}
	// Once no job asked for an hour the puller forgets the image, so a removed image is inspected and pulled
	// again instead of spending a registration on a runner that cannot start.
	if got := runtime.fleet.images.demanded(now.Add(3 * imageDemandTTL)); len(got) != 0 {
		t.Fatalf("demand did not expire: %v", got)
	}
	engine.mu.Lock()
	delete(engine.present, imageKey{"local", "runner:debian12"})
	engine.mu.Unlock()
	before := requests.Load()
	recordRepositoryJob(t, q, 6, "owner/a", "runquill-1-1-os__debian12__ci", now)
	if err := s.tick(t.Context(), now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 6); j.runnerName != "" || requests.Load() != before || !strings.HasPrefix(j.retryReason, imageWaitPrefix+"runner:debian12 on ") {
		t.Fatalf("stale image readiness: %+v, %d requests", j, requests.Load()-before)
	}
	deps.cycle(t.Context())
	runtime.fleet.images.pulls.Wait()
	if got := engine.pulled(); len(got) != 2 {
		t.Fatalf("the image was not pulled again: %v", got)
	}
}

func TestSchedulerUsesTheLiveCatalogue(t *testing.T) {
	q := testQueue(t)
	now := time.Now()
	recordRepositoryJob(t, q, 1, "owner/a", "runquill-1-1-os__ubuntu2404__ci", now)
	var requests atomic.Int64
	runtime := newImageTestRuntime(t, "local", dockerHostSnapshot{Name: "local", Containers: map[string]string{}})
	images, err := buildRunnerImages("", "", "dev", "runner:1")
	if err != nil {
		t.Fatal(err)
	}
	// A panel edit changed the default image to runner:2; ubuntu24.04 follows it in a development build.
	ops := &operations{}
	ops.current.Store(&operationState{policy: operationPolicy{Limit: 2}, runners: runnerConfiguration{runtime: runtime, images: images.withDefault("runner:2")}})
	s := &scheduler{queue: q, github: imageGateGitHub(t, &requests), operations: ops, limit: 2}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 1); j.runnerName != "" || j.retryReason != "waiting for runner image runner:2 on local" || requests.Load() != 0 {
		t.Fatalf("job %+v", j)
	}
	// A failed pull is shown with the waiting reason.
	runtime.fleet.images.claim("local", "runner:2", false, now)
	runtime.fleet.images.finish("local", "runner:2", false, errors.New("manifest unknown"), now)
	runtime.fleet.images.pulls.Done()
	if err := s.tick(t.Context(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 1); j.retryReason != "waiting for runner image runner:2 on local; pull failed: manifest unknown" {
		t.Fatalf("job %+v", j)
	}
}

func TestSchedulerKeepsCatalogueImagesOffMismatchedAgents(t *testing.T) {
	q := testQueue(t)
	now := time.Now()
	recordRepositoryJob(t, q, 1, "owner/a", "runquill-1-1-pool__edge__os__debian12__ci", now)
	recordRepositoryJob(t, q, 2, "owner/a", "runquill-1-1-pool__edge__os__same__ci", now)
	var requests atomic.Int64
	runtime := newImageTestRuntime(t, "local,agent://w1",
		dockerHostSnapshot{Name: "local", Pool: defaultWorkerPool, Containers: map[string]string{}},
		dockerHostSnapshot{Name: "agent://w1", Pool: "edge", Containers: map[string]string{}})
	pools, err := runtime.fleet.configurePools(`{"edge":{"hosts":["agent://w1"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	images, err := buildRunnerImages(`{"debian12":"runner:debian12","same":"runner:1"}`, "", "dev", "runner:1")
	if err != nil {
		t.Fatal(err)
	}
	s := &scheduler{queue: q, github: imageGateGitHub(t, &requests), runtime: runtime, limit: 4, images: images, pools: pools}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if j := readJob(t, q, 1); j.runnerName != "" || j.retryAt == 0 || !strings.Contains(j.retryReason, `no host in worker pool "edge" can start runner image runner:debian12`) {
		t.Fatalf("mismatched agent job %+v", j)
	}
	// The agent's own image is eligible, and the agent receives it as its task image.
	if j := readJob(t, q, 2); j.runnerHost != "agent://w1" || runtime.images[j.runnerName] != "runner:1" {
		t.Fatalf("matching agent job %+v", j)
	}
	if len(runtime.fleet.images.demanded(now)) != 0 {
		t.Fatal("agents must never get on-demand pulls")
	}
	if err := runtime.fleet.startOn(withRunnerImage(t.Context(), "runner:debian12"), "agent://w1", "runquill-9-x", "jit"); err == nil ||
		!strings.Contains(err.Error(), "starts only its own runner image") {
		t.Fatalf("an agent was sent a foreign image: %v", err)
	}
}

func TestNeverPolicyInspectsCatalogueImagesBeforeProvisioning(t *testing.T) {
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	fleet, err := parseDockerFleet("runner:1", "local")
	if err != nil {
		t.Fatal(err)
	}
	fleet.images = testImagePull(t, "never")
	engine := newFakeImageEngine()
	fleet.inspect = func(ctx context.Context, target docker, _ time.Duration, args ...string) (string, error) {
		return engine.run(ctx, target, imageInspectTimeout, args...)
	}
	now := time.Now()
	if fleet.imageReady(t.Context(), "local", "runner:debian12", now).ready {
		t.Fatal("a missing image was ready")
	}
	engine.present[imageKey{"local", "runner:debian12"}] = true
	if fleet.imageReady(t.Context(), "local", "runner:debian12", now.Add(time.Second)).ready || engine.inspected.Load() != 1 {
		t.Fatalf("inspected %d times within the cache period", engine.inspected.Load())
	}
	if !fleet.imageReady(t.Context(), "local", "runner:debian12", now.Add(imageCheckTTL)).ready {
		t.Fatal("an image added by the operator was not seen")
	}
	fleet.requestImage("local", "runner:debian12", now)
	if len(fleet.images.demanded(now)) != 0 {
		t.Fatal("never must not request pulls")
	}
	// Without a readiness store the operator owns images entirely.
	fleet.images = nil
	if !fleet.imageReady(t.Context(), "local", "x:1", now).ready {
		t.Fatal("no store must not block launches")
	}
}

func TestImageDemandExpires(t *testing.T) {
	images := testImagePull(t, "missing")
	now := time.Now()
	images.want("local", "a:1", now)
	images.want("local", "a:1", now.Add(time.Minute))
	if got := images.demanded(now.Add(imageDemandTTL)); len(got) != 1 {
		t.Fatalf("demand %v", got)
	}
	if got := images.demanded(now.Add(time.Minute + imageDemandTTL + time.Second)); len(got) != 0 {
		t.Fatalf("expired demand %v", got)
	}
	if state := images.status("local", "a:1"); state.ready || state.busy {
		t.Fatal("expired demand still busy")
	}
}

type recordingPods struct {
	typed.PodInterface
	created []*core.Pod
}

func (p *recordingPods) Create(_ context.Context, pod *core.Pod, _ meta.CreateOptions) (*core.Pod, error) {
	p.created = append(p.created, pod)
	return pod, nil
}

type recordingSecrets struct{ typed.SecretInterface }

func (recordingSecrets) Create(_ context.Context, secret *core.Secret, _ meta.CreateOptions) (*core.Secret, error) {
	return secret, nil
}

func TestKubernetesRunsCatalogueImagesPerJob(t *testing.T) {
	pods := &recordingPods{}
	k := &kubeRunners{pods: pods, secrets: recordingSecrets{}, options: kubeRunnerOptions{image: "runner:1", mode: "plain"}}
	if err := k.startOn(t.Context(), kubeHost, "runquill-1-a", "jit"); err != nil {
		t.Fatal(err)
	}
	if err := k.startOn(withRunnerImage(t.Context(), "runner:debian12"), kubeHost, "runquill-2-b", "jit"); err != nil {
		t.Fatal(err)
	}
	images := func(pod *core.Pod) [2]string {
		return [2]string{pod.Spec.InitContainers[0].Image, pod.Spec.Containers[0].Image}
	}
	if images(pods.created[0]) != [2]string{"runner:1", "runner:1"} || images(pods.created[1]) != [2]string{"runner:debian12", "runner:debian12"} {
		t.Fatalf("pod images %v %v", images(pods.created[0]), images(pods.created[1]))
	}
	if images(k.runnerPod("x")) != [2]string{"runner:1", "runner:1"} {
		t.Fatal("the default pod changed")
	}
}

func TestRunnerImageVisibility(t *testing.T) {
	images := testReleaseImages(t, "")
	pools := workerPools{defaultWorkerPool: {}, "big": {Image: "runner:big"}}
	jobs := []job{{id: 1, repository: "owner/a", route: "runquill-1-1-os__ubuntu2204__a"},
		{id: 2, repository: "owner/a", route: "runquill-1-1-pool__big__b"}}
	report := summarizeStatus(jobs, nil, 0, 2, time.Now())
	report.configure("docker", "", pools, jobs, nil, jobPlacementDiagnostics{images: images})
	if report.Jobs[0].RunnerOS != "ubuntu2204" || report.Jobs[0].RunnerImage != "ghcr.io/o/runquill-runner:v1.2.3-ubuntu22.04" ||
		report.Jobs[1].RunnerOS != "" || report.Jobs[1].RunnerImage != "runner:big" {
		t.Fatalf("jobs %+v", report.Jobs)
	}
	var out strings.Builder
	if err := report.writeText(&out); err != nil || !strings.Contains(out.String(), "os=ubuntu2204 image=ghcr.io/o/runquill-runner:v1.2.3-ubuntu22.04") {
		t.Fatalf("status text:\n%s", out.String())
	}
	encoded, _ := json.Marshal(report.Jobs[1])
	if strings.Contains(string(encoded), "runner_os") {
		t.Fatalf("runner_os must be optional: %s", encoded)
	}
	if row := panelHistoryRow(jobs[0], 0, pools, time.Now()); row.RunnerOS != "ubuntu2204" {
		t.Fatalf("history row %+v", row)
	}
	runners := panelRunnerInventory(nil, []statusJob{{ID: 1, Runner: "r1", RunnerOS: "ubuntu2204", RunnerImage: "x:1"}})
	if len(runners) != 1 || runners[0].RunnerOS != "ubuntu2204" || runners[0].RunnerImage != "x:1" {
		t.Fatalf("runners %+v", runners)
	}
	// The settings response lists the catalogue read-only.
	ops := &operations{base: runnerConfiguration{runtime: &kubeRunners{}, kind: "kubernetes", images: images}, webhook: true}
	ops.current.Store(&operationState{runners: ops.base})
	recorder := httptest.NewRecorder()
	(&panelState{operations: ops}).operationSettings(recorder, httptest.NewRequest(http.MethodGet, "/api/settings", nil), panelIdentity{})
	var settings operationSettingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &settings); err != nil || len(settings.RunnerImages) != 6 || settings.RunnerImages[0].Name != "debian12" {
		t.Fatalf("settings %s", recorder.Body)
	}
	// runquill doctor lists every entry and whether the local engine holds it.
	var lines []string
	reportRunnerImages(t.Context(), images, nil, func(name string, err error) { lines = append(lines, name) })
	if len(lines) != 6 || lines[0] != "runner image os__debian12__ = ghcr.io/o/runquill-runner:v1.2.3-debian12 (release)" {
		t.Fatalf("doctor lines %v", lines)
	}
}

func TestRunnerImageSettingsFollowLiveDefault(t *testing.T) {
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	base, err := parseDockerFleet("runner:1", "local")
	if err != nil {
		t.Fatal(err)
	}
	images, _ := buildRunnerImages("", "", "dev", "runner:1")
	ops := &operations{base: runnerConfiguration{runtime: base, kind: "docker", images: images}, webhook: true}
	state, err := ops.resolve(operationPolicy{Limit: 2, PollInterval: "30s", Image: "runner:2", Hosts: []string{"local"},
		Pools: workerPools{"default": {Images: []string{"debian12"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if entry, _ := state.runners.images.lookup("ubuntu24.04"); state.runners.images.resolved(entry).Image != "runner:2" {
		t.Fatal("the catalogue default did not follow the edited runner image")
	}
	if state.runners.pools["default"].Images[0] != "debian12" {
		t.Fatal("pool images were dropped")
	}
}

func TestRunnerImageMessagesHaveTranslations(t *testing.T) {
	images := testReleaseImages(t, "")
	var messages []string
	_, unknown := images.forJob("runquill-1-1-os__plan9__x", workerPool{})
	_, denied := images.forJob("runquill-1-1-os__debian12__x", workerPool{Images: []string{"ubuntu22.04"}})
	_, invalid := images.forJob("runquill-1-1-os__a_b__x", workerPool{})
	poolErr := checkPoolImageNames([]string{"bad name"})
	tooMany := checkPoolImageNames(make([]string, maxRunnerImages+1))
	for _, err := range []error{unknown, denied, invalid} {
		messages = append(messages, retryMessage("select runner image", err))
	}
	messages = append(messages, fmt.Sprintf("worker pool %q: %v", "build", poolErr), tooMany.Error(),
		"select runner image: no host in worker pool \"edge\" can start runner image x:1; outbound agent:// workers start only their own image",
		"select runner image: no host can start runner image x:1; outbound agent:// workers start only their own image",
		"waiting for runner image x:1 on ssh://ci@build; pull failed: unauthorized")
	placeholder := regexp.MustCompile(`\{[a-z]+\}`)
	for _, catalogue := range panelTranslations(t) {
		keys, patterns := catalogue.keys, catalogue.patterns
		var match func(string) bool
		match = func(message string) bool {
			if keys[message] {
				return true
			}
			for pattern := range patterns {
				parts := placeholder.Split(pattern, -1)
				for i := range parts {
					parts[i] = regexp.QuoteMeta(parts[i])
				}
				m := regexp.MustCompile("^(?s)" + strings.Join(parts, "(.+?)") + "$").FindStringSubmatch(message)
				if m == nil || !keys[pattern] {
					continue
				}
				// A {detail} must itself be translated.
				if strings.HasSuffix(pattern, "{detail}") && !match(m[len(m)-1]) {
					continue
				}
				return true
			}
			return false
		}
		for _, message := range messages {
			if !match(message) {
				t.Errorf("%s: no translated server pattern for %q", catalogue.name, message)
			}
		}
	}
}

// TestDockerRunnerCatalogueImages starts a runner from each catalogue image on the local engine through
// the fleet's per-job image override. Set RUNQUILL_TEST_RUNNER_IMAGES to a catalogue JSON object.
func TestDockerRunnerCatalogueImages(t *testing.T) {
	raw := os.Getenv("RUNQUILL_TEST_RUNNER_IMAGES")
	if raw == "" {
		t.Skip("set RUNQUILL_TEST_RUNNER_IMAGES to a JSON catalogue of locally built runner images")
	}
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	catalogue, err := buildRunnerImages(raw, "", "dev", "runquill-runner:not-used")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	for _, entry := range catalogue.list() {
		if entry.Source != runnerImageSourceConfigured {
			continue
		}
		t.Run(entry.Label, func(t *testing.T) {
			// The runner image's own run.sh is replaced by one that reports the OS it runs on.
			dir := t.TempDir()
			tag := fmt.Sprintf("runquill-os-%s:%d", entry.Label, time.Now().UnixNano())
			files := map[string]string{
				"Dockerfile": "FROM " + entry.Image + "\nCOPY --chmod=755 run.sh /home/runner/run.sh\n",
				"run.sh": "#!/bin/bash\nset -euo pipefail\ntest \"$1\" = --jitconfig\ntest \"$2\" = test-config\n" +
					". /etc/os-release\necho \"os=$ID$VERSION_ID\"\n",
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if output, err := exec.CommandContext(ctx, "docker", "build", "-t", tag, dir).CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, output)
			}
			t.Cleanup(func() { dockerCommand(context.Background(), nil, "image", "rm", tag) })
			fleet, err := parseDockerFleet("runquill-runner:not-used", "local")
			if err != nil {
				t.Fatal(err)
			}
			fleet.hosts[0].unprivileged = true
			name := strings.NewReplacer(":", "-", ".", "-").Replace(tag)
			t.Cleanup(func() { fleet.hosts[0].remove(context.Background(), name) })
			if err := fleet.startOn(withRunnerImage(ctx, tag), "local", name, "test-config"); err != nil {
				t.Fatal(err)
			}
			exit, err := exec.CommandContext(ctx, "docker", "wait", name).CombinedOutput()
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			if err != nil || strings.TrimSpace(string(exit)) != "0" {
				t.Fatalf("exit=%s error=%v\n%s", exit, err, logs)
			}
			want := "os=" + entry.Label
			if got := strings.TrimSpace(string(logs)); runnerImageKey(got) != runnerImageKey(want) {
				t.Fatalf("runner reported %q, want %q", got, want)
			}
		})
	}
}

// TestReleaseRunnerOSMatchesBuilds keeps the published variants in step with the image builds and
// checks that each pinned OS base still names its variant's release, so a base bump cannot swap one.
func TestReleaseRunnerOSMatchesBuilds(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	want := slices.Sorted(slices.Values(releaseRunnerOS))
	check := func(source string, got []string) {
		t.Helper()
		if slices.Sort(got); !slices.Equal(got, want) {
			t.Errorf("%s lists runner OS variants %v, want %v as in releaseRunnerOS", source, got, want)
		}
	}

	matrix := regexp.MustCompile(`(?m)^\s+os: \[([^\]]+)\]`).FindStringSubmatch(read("../../.github/workflows/images.yml"))
	if matrix == nil {
		t.Fatal("images.yml has no variants os matrix")
	}
	check("images.yml", strings.Split(strings.ReplaceAll(matrix[1], " ", ""), ","))

	var tags []string
	for _, m := range regexp.MustCompile(`\$image-runner:\$GITHUB_REF_NAME-([a-z0-9.]+)"`).FindAllStringSubmatch(read("../../.github/workflows/release.yml"), -1) {
		tags = append(tags, m[1])
	}
	check("release.yml", tags)

	var stages []string
	for _, m := range regexp.MustCompile(`(?m)^FROM ([a-z]+):([0-9.]+)@sha256:[0-9a-f]{64} AS pinned-([a-z0-9.]+)$`).FindAllStringSubmatch(read("../../runner/Dockerfile"), -1) {
		if m[1]+m[2] != m[3] {
			t.Errorf("runner/Dockerfile stage pinned-%s uses %s:%s", m[3], m[1], m[2])
		}
		stages = append(stages, m[3])
	}
	check("runner/Dockerfile", stages)
}
