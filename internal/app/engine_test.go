package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
)

// "info --format {{json .}}" output trimmed to the fields Runquill reads. The rootful Docker sample is
// captured from Docker 29 (version and architecture edited); the rootless and Podman samples are
// hand-written from the engines' documented info fields and have not been checked against a live engine.
const (
	dockerRootfulInfo  = `{"ID":"57d3b200-8a04-4a38-93a7-7d896362765a","ServerVersion":"29.0.0","OSType":"linux","Architecture":"x86_64","CgroupVersion":"2","SecurityOptions":["name=seccomp,profile=builtin","name=cgroupns"],"NCPU":8,"MemTotal":16663138304}`
	dockerRootlessInfo = `{"ID":"9a1f","ServerVersion":"28.5.1","OSType":"linux","Architecture":"aarch64","CgroupVersion":"2","SecurityOptions":["name=seccomp,profile=builtin","name=rootless","name=cgroupns"]}`
	podmanRootlessInfo = `{"host":{"arch":"amd64","cgroupManager":"systemd","cgroupVersion":"v2","cpus":4,"memTotal":8227803136,"hostname":"build1","os":"linux","security":{"apparmorEnabled":false,"rootless":true,"seccompEnabled":true}},"store":{"graphRoot":"/home/ci/.local/share/containers/storage"},"version":{"APIVersion":"5.4.2","Version":"5.4.2","GoVersion":"go1.23.7"}}`
	podmanRootfulInfo  = `{"host":{"arch":"arm64","cgroupVersion":"v1","hostname":"build2","os":"linux","security":{"rootless":false}},"version":{"Version":"4.9.3"}}`
)

func TestParseEngineInfo(t *testing.T) {
	for _, test := range []struct {
		engine, raw string
		want        engineInfo
	}{
		{"docker", dockerRootfulInfo, engineInfo{"docker", "29.0.0", "linux", "amd64", "2", false, 8, 16663138304}},
		{"docker", dockerRootlessInfo, engineInfo{"docker", "28.5.1", "linux", "arm64", "2", true, 0, 0}},
		{"podman", podmanRootlessInfo, engineInfo{"podman", "5.4.2", "linux", "amd64", "2", true, 4, 8227803136}},
		{"podman", podmanRootfulInfo, engineInfo{"podman", "4.9.3", "linux", "arm64", "1", false, 0, 0}},
	} {
		got, err := parseEngineInfo(test.engine, test.raw)
		if err != nil || got != test.want {
			t.Errorf("%s info: got %+v, %v; want %+v", test.engine, got, err, test.want)
		}
	}
	for _, raw := range []string{"local", `{"OSType":"linux"}`, ""} {
		if _, err := parseEngineInfo("docker", raw); err == nil {
			t.Errorf("accepted invalid docker info %q", raw)
		}
	}
	if _, err := parseEngineInfo("podman", dockerRootfulInfo); err == nil {
		t.Error("accepted docker info as podman info")
	}
}

func TestContainerEngineCreateArgs(t *testing.T) {
	common := "create --name r1 --label io.runquill.runner=1 --pull never --init --cgroupns private "
	plain := "--user 1001:1001 --security-opt no-new-privileges --cap-drop ALL --env RUNQUILL_RUNNER_MODE=plain --env RUNQUILL_JIT_FILE=/run/runquill/jit"
	dockerLogs := "--log-driver json-file --log-opt max-size=10m --log-opt max-file=2 "
	podmanLogs := "--log-driver k8s-file --log-opt max-size=10m "
	dockerMounts := " --mount type=volume,src=runquill-cache-7,dst=/home/runner/.cache,volume-label=io.runquill.cache=1" +
		" --mount type=volume,src=runquill-cache-7-tools-key,dst=/home/runner/.cache/tools,volume-label=io.runquill.cache=1,volume-nocopy"
	podmanMounts := " --mount type=volume,src=runquill-cache-7,dst=/home/runner/.cache" +
		" --mount type=volume,src=runquill-cache-7-tools-key,dst=/home/runner/.cache/tools"
	packageEnv := " --env npm_config_cache=/home/runner/.cache/npm --env GOMODCACHE=/home/runner/.cache/go-mod" +
		" --env NUGET_PACKAGES=/home/runner/.cache/nuget-packages --env BUN_INSTALL_CACHE_DIR=/home/runner/.cache/bun"
	toolEnv := " --env RUNNER_TOOL_CACHE=/home/runner/.cache/tools img"
	for _, test := range []struct {
		name, engine, volume, tools string
		unprivileged                bool
		target                      docker
		create, jit                 string
		volumeArgs, dirs            []string
	}{
		{"docker dind", "docker", "", "", false, docker{image: "img"},
			common + dockerLogs + "--privileged img", "run/runquill-jit", nil, nil},
		{"docker plain cached", "docker", "runquill-cache-7", "runquill-cache-7-tools-key", true, docker{image: "img", cpus: 1.5, memoryMiB: 512, worker: "w1"},
			common + dockerLogs + "--label io.runquill.worker=w1 " + plain +
				" --cpus 1.5 --memory 512m --memory-swap 512m" + packageEnv + dockerMounts + toolEnv,
			"run/runquill/jit", nil, []string{"run/runquill/", "home/runner/.cache/", "home/runner/.cache/tools/"}},
		{"docker dind mirror", "docker", "", "", false, docker{image: "img", mirror: "http://10.0.0.5:5000"},
			common + dockerLogs + "--privileged --env RUNQUILL_REGISTRY_MIRROR=http://10.0.0.5:5000 img", "run/runquill-jit", nil, nil},
		{"docker dind cached mirror", "docker", "runquill-cache-7", "runquill-cache-7-tools-key", false, docker{image: "img", mirror: "https://mirror.internal"},
			common + dockerLogs + "--privileged --env RUNQUILL_REGISTRY_MIRROR=https://mirror.internal" + packageEnv + dockerMounts + toolEnv,
			"run/runquill-jit", nil, []string{"home/runner/.cache/", "home/runner/.cache/tools/"}},
		{"docker plain ignores mirror", "docker", "", "", true, docker{image: "img", mirror: "http://10.0.0.5:5000"},
			common + dockerLogs + plain + " img", "run/runquill/jit", nil, []string{"run/runquill/"}},
		{"podman dind", "podman", "", "", false, docker{image: "img"},
			common + podmanLogs + "--privileged img", "run/runquill-jit", nil, nil},
		{"podman plain cached", "podman", "runquill-cache-7", "runquill-cache-7-tools-key", true, docker{image: "img"},
			common + podmanLogs + plain + packageEnv + podmanMounts + toolEnv, "run/runquill/jit",
			[]string{"volume create --ignore --label io.runquill.cache=1 runquill-cache-7", "volume create --ignore --label io.runquill.cache=1 --opt nocopy runquill-cache-7-tools-key"},
			[]string{"run/runquill/", "home/runner/.cache/", "home/runner/.cache/tools/"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := test.target.createPlan(containerEngines[test.engine], "r1", test.volume, test.tools, test.unprivileged)
			if got := strings.Join(plan.create, " "); got != test.create {
				t.Errorf("create argv\n got %s\nwant %s", got, test.create)
			}
			var volumeArgs []string
			for _, args := range plan.volumes {
				volumeArgs = append(volumeArgs, strings.Join(args, " "))
			}
			if got := volumeArgs; !slices.Equal(got, test.volumeArgs) {
				t.Errorf("volume argv: got %q, want %q", got, test.volumeArgs)
			}
			wantOwner := 0
			if test.unprivileged {
				wantOwner = runnerUID
			}
			if plan.jitPath != test.jit || plan.jitOwner != wantOwner || !slices.Equal(plan.dirs, test.dirs) {
				t.Errorf("credential layout: %+v", plan)
			}
		})
	}
}

func TestRegistryMirror(t *testing.T) {
	for raw, want := range map[string]string{
		"":                          "",
		"http://10.0.0.5:5000":      "http://10.0.0.5:5000",
		"http://10.0.0.5:5000/":     "http://10.0.0.5:5000",
		"https://mirror.internal":   "https://mirror.internal",
		"http://[fd00::5]:5000":     "http://[fd00::5]:5000",
		"https://Mirror-1.lan:8443": "https://Mirror-1.lan:8443",
	} {
		t.Setenv("RUNQUILL_REGISTRY_MIRROR", raw)
		if got, err := registryMirror(); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"10.0.0.5:5000", "ftp://mirror", "unix:///run/docker.sock", "http://", "http://:5000", "http://mirror:",
		"http://mirror/v2", "http://mirror?x=1", "http://mirror#x", "http://user:pass@mirror", "http://user@mirror",
		"http://localhost:5000", "http://LOCALHOST", "http://127.0.0.1:5000", "http://[::1]:5000", "http://0.0.0.0:5000", "http://127.1:5000", "http://2130706433",
		"http://mirror :5000", "http://mirror\t", "http://mirror\n--debug", "http://mir%20ror", "http://-mirror", "http:mirror",
		" http://mirror", "http://mirror/ --insecure-registry=x",
	} {
		t.Setenv("RUNQUILL_REGISTRY_MIRROR", raw)
		if got, err := registryMirror(); err == nil {
			t.Errorf("accepted mirror %q as %q", raw, got)
		}
	}

	// The manager refuses to start with an invalid value, and carries a valid one to every host.
	t.Setenv("RUNQUILL_REGISTRY_MIRROR", "http://localhost:5000")
	if _, err := parseDockerFleet("img", "local"); err == nil || !strings.Contains(err.Error(), "RUNQUILL_REGISTRY_MIRROR") {
		t.Errorf("fleet accepted an invalid mirror: %v", err)
	}
	t.Setenv("RUNQUILL_K8S_RUNNER_NAMESPACE", "runquill-runners")
	if _, err := readKubeRunnerOptions("img"); err == nil {
		t.Error("Kubernetes accepted an invalid mirror")
	}
	t.Setenv("RUNQUILL_REGISTRY_MIRROR", "http://10.0.0.5:5000/")
	fleet, err := parseDockerFleet("img", "local,ssh://ci@build1")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range fleet.hosts {
		if host.mirror != "http://10.0.0.5:5000" {
			t.Errorf("%s mirror = %q", host.label(), host.mirror)
		}
	}

	// Only a DinD Pod receives the mirror.
	for mode, want := range map[string]bool{"dind": true, "plain": false} {
		t.Setenv("RUNQUILL_K8S_RUNNER_MODE", mode)
		opts, err := readKubeRunnerOptions("img")
		if err != nil {
			t.Fatal(err)
		}
		var got bool
		for _, env := range (&kubeRunners{options: opts}).runnerPod("r1").Spec.Containers[0].Env {
			got = got || env == core.EnvVar{Name: "RUNQUILL_REGISTRY_MIRROR", Value: "http://10.0.0.5:5000"}
		}
		if got != want {
			t.Errorf("%s Pod mirror env = %v, want %v", mode, got, want)
		}
	}
}

func TestContainerEngineRemoteArgs(t *testing.T) {
	for _, test := range []struct{ engine, host, want string }{
		{"docker", "ssh://ci@build1:2222", "--host ssh://ci@build1:2222"},
		{"podman", "ssh://ci@build1:2222", "--url ssh://ci@build1:2222/run/podman/podman.sock"},
		{"podman", "ssh://ci@build1/run/user/1001/podman/podman.sock", "--url ssh://ci@build1/run/user/1001/podman/podman.sock"},
	} {
		if got := strings.Join(containerEngines[test.engine].remoteArgs(test.host), " "); got != test.want {
			t.Errorf("%s %s: got %q, want %q", test.engine, test.host, got, test.want)
		}
	}
}

func TestContainerEngineSelection(t *testing.T) {
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "containerd")
	if _, err := parseDockerFleet("img", "local"); err == nil || !strings.Contains(err.Error(), "RUNQUILL_CONTAINER_ENGINE must be docker or podman") {
		t.Errorf("accepted an unknown engine: %v", err)
	}
	if _, err := configureRunnerRuntime("img"); err == nil {
		t.Error("runner runtime started with an unknown engine")
	}
	if _, err := (docker{image: "img"}).command(t.Context(), nil, "ps"); err == nil {
		t.Error("ran a command with an unknown engine")
	}

	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	if _, err := parseDockerFleet("img", "ssh://ci@build1/run/docker.sock"); err == nil {
		t.Error("docker accepted an ssh:// socket path")
	}
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "podman")
	fleet, err := parseDockerFleet("img", "local,ssh://ci@build1/run/user/1001/podman/podman.sock,ssh://ci@build2")
	if err != nil || fleet.hosts[1].engine.name != "podman" || fleet.hosts[0].security == nil {
		t.Fatalf("podman fleet: %+v, %v", fleet, err)
	}
	for _, hosts := range []string{"ssh://ci@build1/run/../etc/podman.sock", "ssh://ci@build1/", "ssh://ci@build1/a b"} {
		if _, err := parseDockerFleet("img", hosts); err == nil {
			t.Errorf("podman accepted %q", hosts)
		}
	}

	t.Setenv("RUNQUILL_BACKEND", "kubernetes")
	if _, err := configureRunnerRuntime("img"); err == nil || !strings.Contains(err.Error(), "unset RUNQUILL_CONTAINER_ENGINE for Kubernetes mode") {
		t.Errorf("kubernetes accepted podman: %v", err)
	}
}

func TestRootlessEngineDefaultsToPlainRunners(t *testing.T) {
	fleet, logPath := testDockerFleet(t)
	t.Setenv("FLEET_TEST_ROOTLESS", "local")
	pools, err := fleet.configurePools(`{"default":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	fleet.recordPrivilegeRequests(pools)
	if err := fleet.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := fleet.startOn(t.Context(), "local", "rootless-runner", "jit"); err != nil {
		t.Fatal(err)
	}
	if err := fleet.startOn(t.Context(), fleetTestWorker, "rootful-runner", "jit"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var rootless, rootful bool
	for line := range strings.SplitSeq(string(data), "\n") {
		rootless = rootless || strings.HasPrefix(line, "local create --name rootless-runner") &&
			strings.Contains(line, "--user 1001:1001") && !strings.Contains(line, "--privileged")
		rootful = rootful || strings.HasPrefix(line, fleetTestWorker+" create --name rootful-runner") && strings.Contains(line, "--privileged")
	}
	if !rootless || !rootful {
		t.Fatalf("runner modes on rootless and rootful hosts:\n%s", data)
	}
}

func TestRootlessEngineRejectsExplicitPrivilege(t *testing.T) {
	fleet, _ := testDockerFleet(t)
	t.Setenv("FLEET_TEST_ROOTLESS", fleetTestWorker)
	pools, err := fleet.configurePools(`{"build":{"hosts":["` + fleetTestWorker + `"],"privileged":true}}`)
	if err != nil {
		t.Fatal(err)
	}
	fleet.recordPrivilegeRequests(pools)
	const message = `worker pool "build" sets "privileged": true, but docker host "ssh://builder@worker.example.com" is rootless`
	if err := fleet.prepare(t.Context()); err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("prepare: %v", err)
	}
	hosts, err := fleet.snapshot(t.Context())
	if err != nil || hosts[0].Err != nil || hosts[1].Err == nil || !strings.Contains(hosts[1].Err.Error(), message) {
		t.Fatalf("snapshot: %+v, %v", hosts, err)
	}
	if err := fleet.startOn(t.Context(), fleetTestWorker, "r1", "jit"); err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("start: %v", err)
	}
	// An explicit "privileged": false is honoured without probing rootless hosts.
	pools, err = fleet.configurePools(`{"build":{"hosts":["` + fleetTestWorker + `"],"privileged":false}}`)
	if err != nil {
		t.Fatal(err)
	}
	fleet.recordPrivilegeRequests(pools)
	if err := fleet.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPodmanEngineCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "podman")
	logPath := filepath.Join(dir, "podman.log")
	t.Setenv("PODMAN_TEST_LOG", logPath)
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(`#!/bin/sh
set -eu
host=local
if [ "$1" = --url ]; then host=$2; shift 2; fi
printf '%s %s\n' "$host" "$*" >> "$PODMAN_TEST_LOG"
case "$1" in
  info)
    if [ "$3" = '{{json .}}' ]; then printf '%s\n' '`+podmanRootlessInfo+`'; else printf '%s:/graph\n' "$host"; fi ;;
  image)
    if [ "${5:-}" = '`+runnerCacheImageFormat+`' ]; then printf '%s\n' '`+strings.TrimPrefix(cacheFixtureImageID, "sha256:")+`|linux|amd64'; fi ;;
  cp) cat >/dev/null ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	fleet, err := parseDockerFleet("img", "local,ssh://ci@build1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.configurePools(`{"default":{"cache":true}}`); err != nil {
		t.Fatal(err)
	}
	if err := fleet.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := fleet.startOn(withRunnerRepository(t.Context(), 3), "ssh://ci@build1", "r1", "jit"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	remote := "ssh://ci@build1/run/podman/podman.sock "
	for _, want := range []string{
		"local info --format {{.Host.Hostname}}:{{.Store.GraphRoot}}",
		remote + "volume create --ignore --label io.runquill.cache=1 runquill-cache-3",
		remote + "volume create --ignore --label io.runquill.cache=1 --opt nocopy runquill-cache-3-tools-",
		remote + "create --name r1 --label io.runquill.runner=1 --pull never --init --cgroupns private --log-driver k8s-file --log-opt max-size=10m --user 1001:1001",
		remote + "cp - r1:/",
		remote + "start r1",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q in podman calls:\n%s", want, data)
		}
	}
}
