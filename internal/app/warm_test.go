package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const warmTestRemote = "ssh://builder@warm.example.com"

// warmTestFleet installs a fake docker CLI that lists the containers in $WARM_PS (or $WARM_PS.remote for
// SSH hosts) and logs each call; the returned function reads that log.
func warmTestFleet(t *testing.T, local, remote string) (*dockerFleet, func() []string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	log := filepath.Join(dir, "docker.log")
	ps := filepath.Join(dir, "ps")
	t.Setenv("WARM_LOG", log)
	t.Setenv("WARM_PS", ps)
	t.Setenv("WARM_RENAME_FAIL", "")
	t.Setenv("WARM_RENAME_APPLIED_FAIL", "")
	t.Setenv("WARM_CREATE_FAIL", "")
	t.Setenv("WARM_EXEC_FAIL", "")
	t.Setenv("WARM_FAIL_COMMAND", "")
	t.Setenv("WARM_REMOVE_FAIL", "")
	for path, content := range map[string]string{ps: local, ps + ".remote": remote} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(`#!/bin/sh
set -eu
list=$WARM_PS
if [ "$1" = --host ]; then list=$WARM_PS.remote; printf '%s ' "$2" >> "$WARM_LOG"; shift 2; fi
printf '%s\n' "$*" >> "$WARM_LOG"
if [ "$1" = "$WARM_FAIL_COMMAND" ] || { [ "$1" = rm ] && [ -n "$WARM_REMOVE_FAIL" ]; }; then
  echo "failed $1" >&2
  exit 1
fi
case "$1" in
  ps)
    case " $* " in
      *' --no-trunc '*) awk -F '\t' '{ printf "%s\t%056d%08x\t%s\n", $1, 0, NR, $2 }' "$list" ;;
      *' --filter id='*) if [ -f "$WARM_LOG.renamed" ]; then cat "$WARM_LOG.renamed"; fi ;;
      *) cat "$list" ;;
    esac ;;
  exec) if [ -n "$WARM_EXEC_FAIL" ]; then exit 1; fi; cat > "$WARM_LOG.jit" ;;
  cp) cat > /dev/null ;;
  rename)
    if [ -n "$WARM_RENAME_APPLIED_FAIL" ]; then
      printf '%s\n' "$3" > "$WARM_LOG.renamed"
      echo 'lost rename response' >&2
      exit 1
    fi
    if [ "$2" = "$WARM_RENAME_FAIL" ]; then exit 1; fi ;;
  create) if [ -n "$WARM_CREATE_FAIL" ]; then exit 1; fi ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	host := docker{image: "runner:local", pool: defaultWorkerPool, engine: containerEngines["docker"], security: &engineSecurity{},
		unprivileged: true, warm: 2, warmMaxIdle: 30 * time.Minute}
	remoteHost := host
	remoteHost.host = warmTestRemote
	fleet := &dockerFleet{hosts: []docker{host, remoteHost}, warm: &warmBackoff{hosts: make(map[string]warmRetry)}}
	return fleet, func() []string {
		data, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

func warmKey(t *testing.T, d docker) string {
	t.Helper()
	_, key, err := d.warmPlan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func warmName(key string, created time.Time, nonce int) string {
	return fmt.Sprintf("%s%s-%d-%08x", warmPrefix, key, created.Unix(), nonce)
}

func calls(log []string, prefix string) []string {
	var matched []string
	for _, line := range log {
		if strings.HasPrefix(line, prefix) {
			matched = append(matched, line)
		}
	}
	return matched
}

func TestWarmPoolConfiguration(t *testing.T) {
	for _, test := range []struct {
		hosts, pools, err string
		warm              int
		idle              time.Duration
	}{
		{hosts: "local", pools: `{"default":{"warm":2}}`, warm: 2, idle: defaultWarmMaxIdle},
		{hosts: "local", pools: `{"default":{"warm":1,"warm_max_idle_minutes":10}}`, warm: 1, idle: 10 * time.Minute},
		{hosts: "local,agent://w1", pools: `{"build":{"hosts":["local"],"warm":1}}`, warm: 1, idle: defaultWarmMaxIdle},
		{hosts: "local", pools: `{"default":{"warm":101}}`, err: "warm must be between 0 and 100"},
		{hosts: "local", pools: `{"default":{"warm":-1}}`, err: "warm must be between"},
		{hosts: "local", pools: `{"default":{"warm":1,"warm_max_idle_minutes":1441}}`, err: "warm_max_idle_minutes must be between"},
		{hosts: "local", pools: `{"default":{"warm":1,"cache":true}}`, err: "warm cannot be combined with cache"},
		{hosts: "local,agent://w1", pools: `{"default":{"warm":1}}`, err: "not supported on outbound worker agent://w1"},
		{hosts: "local,agent://w1", pools: `{"build":{"hosts":["agent://w1"],"warm":1}}`, err: "not supported on outbound worker"},
	} {
		fleet, err := parseDockerFleet("runner:local", test.hosts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fleet.configurePools(test.pools)
		if test.err != "" {
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Errorf("%s: error %v, want %q", test.pools, err, test.err)
			}
			continue
		}
		if err != nil || fleet.hosts[0].warm != test.warm || fleet.hosts[0].warmMaxIdle != test.idle {
			t.Errorf("%s: host %+v, error %v", test.pools, fleet.hosts[0], err)
		}
	}
}

func TestWarmPlanKeyFollowsSettings(t *testing.T) {
	base := docker{image: "runner:local", pool: "build", engine: containerEngines["docker"], unprivileged: true, warmMaxIdle: 30 * time.Minute}
	plan, key, err := base.warmPlan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(plan.create, "RUNQUILL_JIT_WAIT=2100"); i < 1 || plan.create[i-1] != "--env" || plan.create[len(plan.create)-1] != "runner:local" {
		t.Fatalf("warm create args: %q", plan.create)
	}
	cold := base.createPlan(containerEngines["docker"], "", "", "", true)
	if !slices.Equal(slices.Delete(slices.Clone(plan.create), len(plan.create)-3, len(plan.create)-1), cold.create) {
		t.Fatalf("warm args differ from a cold runner's beyond the wait:\n%q\n%q", plan.create, cold.create)
	}
	for _, change := range []func(*docker){
		func(d *docker) { d.image = "runner:other" },
		func(d *docker) { d.pool = "other" },
		func(d *docker) { d.cpus = 2 },
		func(d *docker) { d.memoryMiB = 1024 },
		func(d *docker) { d.unprivileged = false; d.security = &engineSecurity{info: &engineInfo{}} },
		func(d *docker) { d.warmMaxIdle = time.Hour },
	} {
		changed := base
		change(&changed)
		if _, other, err := changed.warmPlan(t.Context()); err != nil || other == key {
			t.Errorf("key %s unchanged for %+v, error %v", key, changed, err)
		}
	}
}

func TestWarmRunnersLeaveSlotsFree(t *testing.T) {
	now := time.Now()
	fleet, _ := warmTestFleet(t, "runquill-1-aaaaaaaaaaaa\trunning\n"+warmName("aaaaaaaaaaaa", now, 1)+"\trunning\n", "")
	fleet.hosts = fleet.hosts[:1]
	hosts, err := fleet.snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts[0].Containers) != 1 || hosts[0].Containers["runquill-1-aaaaaaaaaaaa"] != "running" || hosts[0].Warm != 1 {
		t.Fatalf("snapshot: %+v", hosts[0])
	}
	capacity, _ := fleetCapacity(hosts, nil, nil, nil, 4)
	if capacity[0].Occupied != 1 {
		t.Fatalf("warm runner occupies a job slot: %+v", capacity[0])
	}
}

func TestWarmHandoff(t *testing.T) {
	now := time.Now()
	local := docker{image: "runner:local", pool: defaultWorkerPool, engine: containerEngines["docker"], unprivileged: true, warmMaxIdle: 30 * time.Minute}
	key := warmKey(t, local)
	stale := warmName("0123456789ab", now.Add(-20*time.Minute), 1)
	expired := warmName(key, now.Add(-40*time.Minute), 2)
	exited := warmName(key, now.Add(-15*time.Minute), 3)
	oldest := warmName(key, now.Add(-10*time.Minute), 4)
	newest := warmName(key, now.Add(-time.Minute), 5)
	list := fmt.Sprintf("%s\trunning\n%s\trunning\n%s\texited\n%s\trunning\n%s\trunning\nrunquill-9-aaaaaaaaaaaa\trunning\n", stale, expired, exited, newest, oldest)
	const runner, jit = "runquill-7-abcdefabcdef", "secret-jit-config"
	oldestID := fmt.Sprintf("%064x", 5)

	fleet, read := warmTestFleet(t, list, "")
	if err := fleet.startOn(t.Context(), "local", runner, jit); err != nil {
		t.Fatal(err)
	}
	log := read()
	if renames := calls(log, "rename"); !slices.Equal(renames, []string{"rename " + oldest + " " + runner}) {
		t.Fatalf("claimed %q, want the oldest usable warm runner", renames)
	}
	if execs := calls(log, "exec"); len(execs) != 1 || !strings.HasPrefix(execs[0], "exec -i "+oldestID+" sh -c ") || !strings.HasSuffix(execs[0], " sh /run/runquill/jit") {
		t.Fatalf("JIT delivery: %q", execs)
	}
	if data, err := os.ReadFile(os.Getenv("WARM_LOG") + ".jit"); err != nil || string(data) != jit {
		t.Fatalf("delivered %q, error %v", data, err)
	}
	if strings.Contains(strings.Join(log, "\n"), jit) || len(calls(log, "create")) != 0 {
		t.Fatalf("JIT config in argv or a cold start: %q", log)
	}

	// A claimed runner that cannot receive its config is removed, never left waiting or started cold.
	fleet, read = warmTestFleet(t, list, "")
	t.Setenv("WARM_EXEC_FAIL", "1")
	if err := fleet.startOn(t.Context(), "local", runner, jit); err == nil {
		t.Fatal("failed JIT delivery reported success")
	}
	t.Setenv("WARM_EXEC_FAIL", "")
	if log = read(); !slices.Contains(log, "rm -f -v "+oldestID) || len(calls(log, "create")) != 0 || len(calls(log, "rename")) != 1 {
		t.Fatalf("failed delivery: %q", log)
	}

	// A claim lost to another start moves on to the next warm runner.
	fleet, read = warmTestFleet(t, list, "")
	t.Setenv("WARM_RENAME_FAIL", oldest)
	if err := fleet.startOn(t.Context(), "local", runner, jit); err != nil {
		t.Fatal(err)
	}
	if renames := calls(read(), "rename"); len(renames) != 2 || renames[1] != "rename "+newest+" "+runner {
		t.Fatalf("claims after a lost rename: %q", renames)
	}

	// The engine renamed the container before losing the reply. It must be removed by ID.
	fleet, read = warmTestFleet(t, list, "")
	t.Setenv("WARM_RENAME_APPLIED_FAIL", "1")
	if err := fleet.startOn(t.Context(), "local", runner, jit); err == nil || !strings.Contains(err.Error(), "lost rename response") {
		t.Fatalf("rename error: %v", err)
	}
	if log = read(); !slices.Contains(log, "rm -f -v "+oldestID) || len(calls(log, "create")) != 0 || len(calls(log, "exec")) != 0 {
		t.Fatalf("lost rename response: %q", log)
	}

	// A job that selects another image starts cold and retires a warm runner for its slot.
	fleet, read = warmTestFleet(t, list, "")
	if err := fleet.startOn(withRunnerImage(t.Context(), "runner:debian12"), "local", runner, jit); err != nil {
		t.Fatal(err)
	}
	log = read()
	if len(calls(log, "rename")) != 0 || !slices.Contains(log, "rm -f -v "+oldest) || len(calls(log, "create --name "+runner)) != 1 {
		t.Fatalf("other image: %q", log)
	}

	fleet, read = warmTestFleet(t, list, "")
	t.Setenv("WARM_REMOVE_FAIL", "1")
	if err := fleet.startOn(withRunnerImage(t.Context(), "runner:debian12"), "local", runner, jit); err == nil || len(calls(read(), "create")) != 0 {
		t.Fatalf("failed warm retirement: %v, calls %q", err, read())
	}

	// Warm runners on one host never take a job placed on another.
	fleet, read = warmTestFleet(t, list, "")
	if err := fleet.startOn(t.Context(), warmTestRemote, runner, jit); err != nil {
		t.Fatal(err)
	}
	log = read()
	if len(calls(log, "rename")) != 0 || len(calls(log, warmTestRemote+" create --name "+runner)) != 1 {
		t.Fatalf("remote start: %q", log)
	}

	// Without a usable warm runner the job starts cold.
	fleet, read = warmTestFleet(t, fmt.Sprintf("%s\trunning\n%s\trunning\n", stale, expired), "")
	if err := fleet.startOn(t.Context(), "local", runner, jit); err != nil {
		t.Fatal(err)
	}
	if log = read(); len(calls(log, "rename")) != 0 || len(calls(log, "create --name "+runner)) != 1 {
		t.Fatalf("no usable warm runner: %q", log)
	}
}

func TestDockerFailedStartRemovesCreatedRunner(t *testing.T) {
	for _, step := range []string{"create", "cp", "start"} {
		t.Run(step, func(t *testing.T) {
			fleet, read := warmTestFleet(t, "", "")
			t.Setenv("WARM_FAIL_COMMAND", step)
			t.Setenv("WARM_REMOVE_FAIL", "1")
			err := fleet.startOn(t.Context(), "local", "runner", "test-jit")
			if err == nil || !strings.Contains(err.Error(), "failed "+step) {
				t.Fatalf("start error: %v", err)
			}
			if got := slices.Contains(read(), "rm -f -v runner"); got != (step != "create") || got != strings.Contains(err.Error(), "failed rm") {
				t.Fatalf("cleanup after %s: error %v, calls %q", step, err, read())
			}
		})
	}
}

func TestWarmMaintenance(t *testing.T) {
	now := time.Now()
	local := docker{image: "runner:local", pool: defaultWorkerPool, engine: containerEngines["docker"], unprivileged: true, warmMaxIdle: 30 * time.Minute}
	key := warmKey(t, local)
	good := warmName(key, now.Add(-10*time.Minute), 1)
	newer := warmName(key, now.Add(-time.Minute), 2)
	expired := warmName(key, now.Add(-31*time.Minute), 3)
	stale := warmName("0123456789ab", now.Add(-time.Minute), 4)
	crashed := warmName(key, now.Add(-2*time.Minute), 5)
	capacity := func(occupied int, cordoned bool) []hostCapacity {
		return []hostCapacity{{dockerHostSnapshot: dockerHostSnapshot{Name: "local", Containers: map[string]string{}, Warm: 1},
			Limit: 4, Occupied: occupied, Cordoned: cordoned}}
	}
	removed := func(log []string) []string {
		var names []string
		for _, line := range calls(log, "rm -f -v ") {
			names = append(names, strings.TrimPrefix(line, "rm -f -v "))
		}
		slices.Sort(names)
		return names
	}
	sorted := func(names ...string) []string { return slices.Sorted(slices.Values(names)) }

	// Expired, stale and crashed runners go; a crash backs off before the next start.
	fleet, read := warmTestFleet(t, fmt.Sprintf("%s\trunning\n%s\trunning\n%s\trunning\n%s\texited\n", good, expired, stale, crashed), "")
	fleet.maintainWarm(t.Context(), capacity(0, false), 10, now)
	log := read()
	if got := removed(log); !slices.Equal(got, sorted(expired, stale, crashed)) || len(calls(log, "create")) != 0 {
		t.Fatalf("cleanup: removed %q, log %q", got, log)
	}

	// A restarted manager adopts matching runners and tops the host up by one per pass.
	fleet, read = warmTestFleet(t, good+"\trunning\n", "")
	fleet.maintainWarm(t.Context(), capacity(1, false), 10, now)
	log = read()
	creates := calls(log, "create --name "+warmPrefix+key+"-")
	if len(removed(log)) != 0 || len(creates) != 1 || !strings.Contains(creates[0], "--env RUNQUILL_JIT_WAIT=2100 runner:local") ||
		len(calls(log, "cp - "+warmPrefix)) != 1 || len(calls(log, "start "+warmPrefix)) != 1 {
		t.Fatalf("top up: %q", log)
	}
	name := strings.Fields(creates[0])[2]
	if w, ok := parseWarmName(name); !ok || w.key != key || w.created.Unix() != now.Unix() {
		t.Fatalf("warm runner name %q", name)
	}

	// Warm runners only fill slots that jobs and the global limit leave free.
	for _, test := range []struct {
		occupied, free int
		cordoned       bool
		removed        []string
	}{
		{occupied: 3, free: 10, removed: []string{good}},
		{occupied: 0, free: 1, removed: []string{good}},
		{occupied: 0, free: 0, removed: sorted(good, newer)},
		{occupied: 0, free: 10, cordoned: true, removed: sorted(good, newer)},
	} {
		fleet, read = warmTestFleet(t, fmt.Sprintf("%s\trunning\n%s\trunning\n", good, newer), "")
		fleet.maintainWarm(t.Context(), capacity(test.occupied, test.cordoned), test.free, now)
		log = read()
		if got := removed(log); !slices.Equal(got, test.removed) || len(calls(log, "create")) != 0 {
			t.Errorf("%+v: removed %q, log %q", test, got, log)
		}
	}

	// A failed start backs off, doubling from the launch retry delay.
	fleet, read = warmTestFleet(t, "", "")
	t.Setenv("WARM_CREATE_FAIL", "1")
	fleet.maintainWarm(t.Context(), capacity(0, false), 10, now)
	fleet.maintainWarm(t.Context(), capacity(0, false), 10, now.Add(retryDelay-time.Second))
	if creates := calls(read(), "create"); len(creates) != 1 {
		t.Fatalf("retried inside the backoff: %q", creates)
	}
	fleet.maintainWarm(t.Context(), capacity(0, false), 10, now.Add(retryDelay))
	if creates := calls(read(), "create"); len(creates) != 2 {
		t.Fatalf("no retry after the backoff: %q", creates)
	}

	// Hosts with warm runners off and none left are not touched.
	fleet, read = warmTestFleet(t, "", "")
	fleet.hosts[0].warm = 0
	idle := capacity(0, false)
	idle[0].Warm = 0
	fleet.maintainWarm(t.Context(), idle, 10, now)
	if log := read(); len(log) != 1 || log[0] != "" {
		t.Fatalf("warm off ran %q", log)
	}
}

func TestOrphanRemovalKeepsWarmRunners(t *testing.T) {
	now := time.Now()
	c := runnerContainer{name: warmName("aaaaaaaaaaaa", now.Add(-time.Hour), 1), owned: true, created: now.Add(-time.Hour), state: "running"}
	reason, err := removeOrphan(t.Context(), nil, nil, c, false, "", nil, time.Minute, now)
	if err != nil || !strings.Contains(reason, "warm runner") {
		t.Fatalf("reason %q, error %v", reason, err)
	}
}

type warmSchedulerRuntime struct {
	*testRuntime
	free  []int
	hosts int
}

func (r *warmSchedulerRuntime) maintainWarm(_ context.Context, hosts []hostCapacity, free int, _ time.Time) {
	r.free = append(r.free, free)
	r.hosts = len(hosts)
}

func TestSchedulerMaintainsWarmRunnersEveryTick(t *testing.T) {
	q := testQueue(t)
	runtime := &warmSchedulerRuntime{testRuntime: &testRuntime{hosts: []dockerHostSnapshot{
		{Name: "local", Containers: map[string]string{"untracked": "running"}},
	}}}
	// Concurrent schedulers wrap the runtime; maintenance must still reach it.
	s := &scheduler{queue: q, runtime: runtime, limit: 4, workers: 4}
	if err := s.tick(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	s.limit = 1 // a full fleet returns early but still trims warm runners
	if err := s.tick(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(runtime.free, []int{3, 0}) || runtime.hosts != 1 {
		t.Fatalf("maintenance free slots %v over %d hosts", runtime.free, runtime.hosts)
	}
}

func TestEntrypointWaitsForWarmHandoff(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	script, err := filepath.Abs("../../runner/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, wait string, before func(dir string)) (string, string, error) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\nprintf '%s ' \"$@\" > args\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if before != nil {
			before(dir)
		}
		cmd := exec.CommandContext(t.Context(), bash, script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "RUNQUILL_RUNNER_MODE=plain", "RUNQUILL_JIT_FILE="+filepath.Join(dir, "jit"), "RUNQUILL_JIT_WAIT="+wait)
		output, err := cmd.CombinedOutput()
		args, _ := os.ReadFile(filepath.Join(dir, "args"))
		if _, statErr := os.Stat(filepath.Join(dir, "jit")); !os.IsNotExist(statErr) {
			t.Errorf("JIT file left behind: %v", statErr)
		}
		return string(output), string(args), err
	}
	writeJIT := func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "jit"), []byte("cold-config"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if _, args, err := run(t, "", writeJIT); err != nil || args != "--jitconfig cold-config " {
		t.Fatalf("cold start: args %q, error %v", args, err)
	}
	start := time.Now()
	output, args, err := run(t, "1", nil)
	if err != nil || args != "" || !strings.Contains(output, "No job was handed") || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: output %q, args %q, error %v", output, args, err)
	}
	if _, args, err := run(t, "30", func(dir string) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			tmp := filepath.Join(dir, "jit.tmp")
			if os.WriteFile(tmp, []byte("warm-config"), 0600) == nil {
				os.Rename(tmp, filepath.Join(dir, "jit"))
			}
		}()
	}); err != nil || args != "--jitconfig warm-config " {
		t.Fatalf("handoff: args %q, error %v", args, err)
	}
}

// TestDockerWarmRunner starts warm runners in both runner modes and hands each a job's JIT config.
func TestDockerWarmRunner(t *testing.T) {
	image := os.Getenv("RUNQUILL_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set RUNQUILL_TEST_DOCKER_IMAGE to a locally built runner image")
	}
	entrypoint, err := os.ReadFile("../../runner/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tag := fmt.Sprintf("runquill-warm-smoke:%d", time.Now().UnixNano())
	for file, content := range map[string]string{
		"Dockerfile":    "FROM " + image + "\nCOPY --chmod=755 entrypoint.sh /usr/local/bin/runquill-runner\nCOPY --chmod=755 run.sh /home/runner/run.sh\n",
		"entrypoint.sh": string(entrypoint),
		"run.sh": `#!/bin/bash
set -euo pipefail
test "$(id -un)" = runner
test "$1" = --jitconfig
test "$2" = warm-test-config
test ! -e "${RUNQUILL_JIT_FILE:-/run/runquill-jit}"
if [ "${RUNQUILL_RUNNER_MODE:-}" != plain ]; then docker info >/dev/null; fi
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "build", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("build smoke image: %v\n%s", err, output)
	}
	t.Cleanup(func() { dockerCommand(context.Background(), nil, "image", "rm", tag) })
	for _, unprivileged := range []bool{false, true} {
		t.Run(fmt.Sprintf("unprivileged=%t", unprivileged), func(t *testing.T) {
			testDockerWarmHandoff(t, ctx, docker{image: tag, pool: defaultWorkerPool, security: &engineSecurity{}, warm: 1,
				warmMaxIdle: 30 * time.Minute, unprivileged: unprivileged})
		})
	}
}

func testDockerWarmHandoff(t *testing.T, ctx context.Context, d docker) {
	fleet := &dockerFleet{hosts: []docker{d}, warm: &warmBackoff{hosts: make(map[string]warmRetry)}}
	runner := fmt.Sprintf("runquill-%d-%012x", time.Now().Unix(), time.Now().UnixNano()&0xffffffffffff)
	t.Cleanup(func() {
		if runners, err := d.warmRunners(context.Background()); err == nil {
			for _, w := range runners {
				d.remove(context.Background(), w.name)
			}
		}
		d.remove(context.Background(), runner)
	})
	fleet.maintainWarm(ctx, []hostCapacity{{dockerHostSnapshot: dockerHostSnapshot{Name: "local", Containers: map[string]string{}}, Limit: 4}}, 4, time.Now())
	runners, err := d.warmRunners(ctx)
	if err != nil || len(runners) != 1 || runners[0].state != "running" {
		t.Fatalf("warm runners %+v, error %v", runners, err)
	}
	// Hand over once the nested daemon is up, as a warm runner would be by the time a job arrives.
	for i := 0; !d.unprivileged; i++ {
		if _, err := dockerCommand(ctx, nil, "exec", runners[0].name, "docker", "info"); err == nil {
			break
		} else if i == 60 {
			t.Fatalf("nested Docker never started: %v", err)
		}
		time.Sleep(time.Second)
	}
	started := time.Now()
	if err := fleet.startOn(ctx, "local", runner, "warm-test-config"); err != nil {
		t.Fatal(err)
	}
	t.Logf("handoff took %s", time.Since(started))
	exit, err := exec.CommandContext(ctx, "docker", "wait", runner).CombinedOutput()
	if err != nil || strings.TrimSpace(string(exit)) != "0" {
		logs, _ := exec.Command("docker", "logs", "--tail", "100", runner).CombinedOutput()
		t.Fatalf("runner exit=%s, error=%v\n%s", exit, err, logs)
	}
	if runners, err := d.warmRunners(ctx); err != nil || len(runners) != 0 {
		t.Fatalf("warm runners left after the handoff: %+v, error %v", runners, err)
	}
}
