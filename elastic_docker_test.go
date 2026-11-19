package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const probeID = "0e5a9c6bbe9e19e6cf4bca0378aceeae2f2e3a4a1228734cb5b6337072226401"

func elasticSample(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/elastic/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestElasticParsesRealDockerOutput(t *testing.T) {
	// Captured from a throwaway "alpine sleep" container started with --cpus 0.5 --memory 64m running a busy loop.
	containers, err := parseDockerInspect(elasticSample(t, "inspect.json"))
	if err != nil || len(containers) != 1 {
		t.Fatalf("inspect: %v %+v", err, containers)
	}
	c := containers[0]
	if c.ID != probeID || c.Name != "/rq-elastic-probe" || c.State.Status != "running" || c.HostConfig.NanoCPUs != 5e8 ||
		c.HostConfig.Memory != 64<<20 || c.HostConfig.MemorySwap != 64<<20 || c.Config.Labels["io.runquill.elastic-probe"] != "1" {
		t.Fatalf("inspect fields: %+v", c)
	}
	usage := make(map[string][2]float64)
	if err := parseDockerStats(elasticSample(t, "stats.json"), usage); err != nil {
		t.Fatal(err)
	}
	if got := usage[probeID]; got[0] != 0.4975 || got[1] != 2.266 {
		t.Fatalf("stats usage %v", got)
	}
	cpus, memory, err := parseDockerCapacity("10 2059165696\n")
	if err != nil || cpus != 10 || memory != 1963 {
		t.Fatalf("capacity %v %v %v", cpus, memory, err)
	}
	for value, want := range map[string]float64{"512B": 512, "1.5KiB": 1536, "2GiB": 2 << 30, "0B": 0} {
		if got, err := parseDockerSize(value); err != nil || got != want {
			t.Errorf("size %q = %v, %v", value, got, err)
		}
	}
	for _, bad := range []string{"", "12", "1.5MB", "-1MiB", "xMiB"} {
		if _, err := parseDockerSize(bad); err == nil {
			t.Errorf("size %q accepted", bad)
		}
	}
	for _, bad := range []string{"", "10", "0 100", "x 100", "4 -1"} {
		if _, _, err := parseDockerCapacity(bad); err == nil {
			t.Errorf("capacity %q accepted", bad)
		}
	}
	if err := parseDockerStats(`{"ID":"x","CPUPerc":"--","MemUsage":"1MiB / 2MiB"}`, usage); err == nil {
		t.Error("invalid CPU percentage accepted")
	}
	if _, err := parseDockerInspect(`{"Id":"x"}`); err == nil {
		t.Error("non-array inspect accepted")
	}
}

type fakeDocker struct {
	mu    sync.Mutex
	calls [][]string
	reply func(args []string) (string, error)
}

func (f *fakeDocker) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	f.mu.Unlock()
	return f.reply(args)
}

func (f *fakeDocker) updates() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var updates [][]string
	for _, call := range f.calls {
		if call[0] == "update" {
			updates = append(updates, call)
		}
	}
	return updates
}

func inspectJSON(id, name, status string, cpus float64, memoryMiB int64, labels string) string {
	return fmt.Sprintf(`{"Id":%q,"Name":"/%s","State":{"Status":%q},"HostConfig":{"NanoCpus":%d,"Memory":%d,"MemorySwap":%d},"Config":{"Labels":%s}}`,
		id, name, status, int64(cpus*1e9), memoryMiB<<20, memoryMiB<<20, labels)
}

// fleetDocker serves a local engine with 8 CPUs and 8 GiB: a busy runner, an idle borrower, a memory-starved
// runner, an exited runner, an agent-owned container and a container that only matched the label filter by name.
func fleetDocker(failUpdate func([]string) bool) *fakeDocker {
	runner := `{"io.runquill.runner":"1"}`
	containers := map[string]string{
		"busy":    inspectJSON("busy", "runquill-busy", "running", 1, 1024, runner),
		"idle":    inspectJSON("idle", "runquill-idle", "running", 2, 1024, runner),
		"mem":     inspectJSON("mem", "runquill-mem", "running", 1, 1024, runner),
		"done":    inspectJSON("done", "runquill-done", "exited", 1, 1024, runner),
		"agent":   inspectJSON("agent", "runquill-agent", "running", 1, 1024, `{"io.runquill.runner":"1","io.runquill.worker":"w1"}`),
		"foreign": inspectJSON("foreign", "db", "running", 1, 1024, `{"app":"db"}`),
	}
	stats := map[string]string{
		"busy":  `{"ID":"busy","CPUPerc":"99.80%","MemUsage":"100MiB / 1GiB"}`,
		"idle":  `{"ID":"idle","CPUPerc":"5.00%","MemUsage":"100MiB / 1GiB"}`,
		"mem":   `{"ID":"mem","CPUPerc":"50.00%","MemUsage":"1000MiB / 1GiB"}`,
		"agent": `{"ID":"agent","CPUPerc":"100.00%","MemUsage":"1020MiB / 1GiB"}`,
	}
	f := &fakeDocker{}
	f.reply = func(args []string) (string, error) {
		switch args[0] {
		case "info":
			return "8 8589934592\n", nil
		case "ps":
			return "busy\nidle\nmem\ndone\nagent\nforeign\n", nil
		case "inspect":
			var parts []string
			for _, id := range args[3:] {
				parts = append(parts, containers[id])
			}
			return "[" + strings.Join(parts, ",") + "]", nil
		case "stats":
			var lines []string
			for _, id := range args[5:] {
				if id == "idle" && len(args) > 6 {
					return "", errors.New("docker stats: No such container: idle")
				}
				lines = append(lines, stats[id])
			}
			return strings.Join(lines, "\n"), nil
		case "update":
			if failUpdate != nil && failUpdate(args) {
				return "", errors.New("docker update: denied")
			}
			return args[len(args)-1] + "\n", nil
		}
		return "", fmt.Errorf("unexpected docker %v", args)
	}
	return f
}

func elasticTestDeps(mode elasticMode, f *fakeDocker) elasticDeps {
	p := testElasticPolicy()
	p.ReserveCPUs, p.ReserveMemoryMiB = 1, 1024
	cfg := elasticConfig{Mode: mode, Interval: 10 * time.Second, Policy: p}
	return elasticDeps{Config: cfg, State: newElasticState(cfg), Docker: f.run,
		Now: func() time.Time { return time.Unix(1_800_000_000, 0) },
		Hosts: func() []elasticHostConfig {
			return []elasticHostConfig{{Name: "local", Local: true, CPUs: 1, MemoryMiB: 1024, SlotLimit: 5}}
		}}
}

func TestElasticApplyUpdatesOnlyTrackedRunners(t *testing.T) {
	f := fleetDocker(nil)
	deps := elasticTestDeps(elasticApply, f)
	deps.tick(t.Context())
	// 8 CPUs - 1 reserve - 3 runner guarantees (the exited runner holds no slot) - 2 held slots = 2 spare, 1 lent to idle.
	want := [][]string{
		{"update", "--cpus", "1.00", "idle"},
		{"update", "--cpus", "1.50", "busy"},
		{"update", "--memory", "1280m", "--memory-swap", "1280m", "mem"},
	}
	if got := f.updates(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("updates:\n got %q\nwant %q", got, want)
	}
	report := deps.State.report()
	if len(report.Decisions) != 3 || !report.Decisions[0].Applied || len(report.Hosts) != 1 || report.Hosts[0].HeldSlots != 2 {
		t.Fatalf("report: %+v", report)
	}
	for _, r := range report.Runners {
		if r.Runner == "runquill-agent" || r.Runner == "db" {
			t.Fatalf("untracked container reported: %+v", r)
		}
	}
	// The next tick is inside the cooldown, so nothing changes again.
	deps.tick(t.Context())
	if got := len(f.updates()); got != 3 {
		t.Fatalf("cooldown ignored: %d updates", got)
	}
}

func TestElasticObserveChangesNothing(t *testing.T) {
	f := fleetDocker(nil)
	deps := elasticTestDeps(elasticObserve, f)
	deps.tick(t.Context())
	if got := f.updates(); len(got) != 0 {
		t.Fatalf("observe mode updated %q", got)
	}
	report := deps.State.report()
	if len(report.Decisions) != 3 || report.Decisions[0].Applied {
		t.Fatalf("observe decisions: %+v", report.Decisions)
	}
}

func TestElasticFailedReductionHoldsBackRaises(t *testing.T) {
	f := fleetDocker(func(args []string) bool { return args[len(args)-1] == "idle" })
	deps := elasticTestDeps(elasticApply, f)
	deps.tick(t.Context())
	if got := f.updates(); len(got) != 1 {
		t.Fatalf("raises were not held back: %q", got)
	}
	for _, d := range deps.State.report().Decisions {
		if d.Applied || d.Error == "" {
			t.Fatalf("decision should be recorded as not applied: %+v", d)
		}
	}
}

func TestElasticSkipsRemoteHostsAndOffMode(t *testing.T) {
	f := &fakeDocker{reply: func(args []string) (string, error) { return "", fmt.Errorf("unexpected docker %v", args) }}
	deps := elasticTestDeps(elasticApply, f)
	deps.Hosts = func() []elasticHostConfig {
		return []elasticHostConfig{{Name: "ssh://ci@build", CPUs: 1}, {Name: "agent://w1", CPUs: 1}, {Name: "kubernetes"}, {Name: "local", Local: true}}
	}
	deps.tick(t.Context())
	if len(f.calls) != 0 {
		t.Fatalf("remote or unlimited hosts sampled: %q", f.calls)
	}
	deps.Config.Mode = elasticOff
	deps.Hosts = func() []elasticHostConfig { t.Fatal("off mode read the topology"); return nil }
	runElastic(t.Context(), deps)

	// Docker errors and panics are logged, never fatal.
	deps = elasticTestDeps(elasticApply, &fakeDocker{reply: func([]string) (string, error) { panic("boom") }})
	deps.tick(t.Context())
	deps = elasticTestDeps(elasticApply, &fakeDocker{reply: func([]string) (string, error) { return "", errors.New("down") }})
	deps.tick(t.Context())
	if len(deps.State.report().Hosts) != 0 {
		t.Fatal("failed sample recorded a host")
	}
}

func TestElasticLoopStopsWithContext(t *testing.T) {
	f := fleetDocker(nil)
	deps := elasticTestDeps(elasticObserve, f)
	deps.Config.Interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { runElastic(ctx, deps); close(done) }()
	deadline := time.After(5 * time.Second)
	for len(deps.State.report().Decisions) == 0 {
		select {
		case <-deadline:
			t.Fatal("loop never ticked")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestElasticHostsFromOperations(t *testing.T) {
	fleet := &dockerFleet{hosts: []docker{{cpus: 2, memoryMiB: 2048}, {host: "ssh://ci@build", cpus: 1}}}
	ops := &operations{}
	ops.current.Store(&operationState{policy: operationPolicy{Limit: 4},
		runners: runnerConfiguration{runtime: fleet, kind: "docker", hostLimits: map[string]int{"local": 2}}})
	got := elasticHostsFrom(ops)()
	want := []elasticHostConfig{{Name: "local", Local: true, CPUs: 2, MemoryMiB: 2048, SlotLimit: 2}, {Name: "ssh://ci@build", CPUs: 1, SlotLimit: 4}}
	if !slices.Equal(got, want) {
		t.Fatalf("hosts %+v", got)
	}
	ops.current.Store(&operationState{runners: runnerConfiguration{kind: "kubernetes"}})
	if got := elasticHostsFrom(ops)(); len(got) != 1 || got[0].Local {
		t.Fatalf("kubernetes hosts %+v", got)
	}
}

// TestElasticApplyOnRealDocker raises CPU and memory on a throwaway alpine container and reads them back.
// It runs only with RUNQUILL_ELASTIC_DOCKER_TEST=1 and never touches containers without its own label.
func TestElasticApplyOnRealDocker(t *testing.T) {
	if os.Getenv("RUNQUILL_ELASTIC_DOCKER_TEST") != "1" {
		t.Skip("set RUNQUILL_ELASTIC_DOCKER_TEST=1 to exercise docker update")
	}
	name := fmt.Sprintf("rq-elastic-test-%d", time.Now().UnixNano())
	label := "io.runquill.elastic-test=" + name
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("run", "-d", "--name", name, "--label", label, "--cpus", "0.5", "--memory", "64m", "--memory-swap", "64m", "alpine", "sleep", "300")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	run("exec", "-d", name, "sh", "-c", "while :; do :; done")
	run("exec", name, "dd", "if=/dev/zero", "of=/dev/shm/fill", "bs=1M", "count=58")
	time.Sleep(2 * time.Second)

	p := testElasticPolicy()
	cfg := elasticConfig{Mode: elasticApply, Interval: time.Second, Policy: p}
	deps := elasticDeps{Config: cfg, State: newElasticState(cfg), Label: label, Now: time.Now,
		Docker: func(ctx context.Context, args ...string) (string, error) { return dockerCommand(ctx, nil, args...) },
		Hosts: func() []elasticHostConfig {
			return []elasticHostConfig{{Name: "local", Local: true, CPUs: 0.5, MemoryMiB: 64, SlotLimit: 1}}
		}}
	deps.tick(t.Context())
	for _, d := range deps.State.report().Decisions {
		t.Logf("%+v", d)
	}
	got := run("inspect", name, "--format", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}")
	if got != fmt.Sprint(int64(0.75e9), " ", 128<<20, " ", 128<<20) {
		t.Fatalf("limits after apply: %s", got)
	}
}
