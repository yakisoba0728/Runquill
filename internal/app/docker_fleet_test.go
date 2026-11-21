package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fleetTestWorker       = "ssh://builder@worker.example.com"
	cacheFixtureImageID   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cacheAlternateImageID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func testDockerFleet(t *testing.T) (*dockerFleet, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	logPath := filepath.Join(dir, "docker.log")
	t.Setenv("FLEET_TEST_LOG", logPath)
	t.Setenv("FLEET_TEST_WORKER_STATE", "")
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(`#!/bin/sh
set -eu
host=local
if [ "$1" = --host ]; then host=$2; shift 2; fi
printf '%s %s\n' "$host" "$*" >> "$FLEET_TEST_LOG"
state=
if [ "$host" != local ]; then state=$FLEET_TEST_WORKER_STATE; fi
if [ "$state" = offline ]; then exit 1; fi
case "$1" in
  info)
    if [ "$3" = '{{json .}}' ]; then
      security='"name=seccomp,profile=builtin"'
      if [ "${FLEET_TEST_ROOTLESS:-}" = "$host" ]; then security="$security,\"name=rootless\""; fi
      printf '{"ServerVersion":"29.0.0","OSType":"linux","Architecture":"x86_64","CgroupVersion":"2","SecurityOptions":[%s]}\n' "$security"
    elif [ "$state" = duplicate-engine ]; then echo local; else printf '%s\n' "$host"; fi ;;
  ps)
    if [ "$host" = local ] || [ "$state" = duplicate-runner ]; then
      printf 'existing\trunning\n'
    else
      printf 'worker-existing\tpaused\n'
    fi
    ;;
  image)
    if [ "$state" = missing-image ]; then exit 1; fi
    if [ "${5:-}" = '`+runnerCacheImageFormat+`' ]; then
      id=${FLEET_TEST_CACHE_ID:-`+cacheFixtureImageID+`}
      if [ "$3" = runner:debian12 ]; then id=`+cacheAlternateImageID+`; fi
      printf '%s|linux|%s\n' "$id" "${FLEET_TEST_CACHE_ARCH:-amd64}"
    fi
    ;;
  cp) cat >"$FLEET_TEST_LOG.tar" ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	fleet, err := newDockerFleet("runner:local", "local,"+fleetTestWorker)
	if err != nil {
		t.Fatal(err)
	}
	return fleet, logPath
}

func TestDockerFleetPlacesAndRemovesRunnersOnTheirHost(t *testing.T) {
	fleet, logPath := testDockerFleet(t)
	if err := fleet.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	hosts, err := fleet.snapshot(t.Context())
	if err != nil || len(hosts) != 2 || hosts[0].Containers["existing"] != "running" ||
		hosts[1].Containers["worker-existing"] != "paused" || hosts[0].Err != nil || hosts[1].Err != nil {
		t.Fatalf("unexpected Docker host snapshot: %+v, %v", hosts, err)
	}
	if err := fleet.startOn(t.Context(), fleetTestWorker, "new-runner", "test-jit"); err != nil {
		t.Fatal(err)
	}
	if err := fleet.removeOn(t.Context(), "local", "existing"); err != nil {
		t.Fatal(err)
	}
	if err := fleet.startOn(t.Context(), "local", "next-runner", "test-jit"); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ssh://builder@worker.example.com create --name new-runner",
		"ssh://builder@worker.example.com cp - new-runner:/\n",
		"ssh://builder@worker.example.com start new-runner",
		"local rm -f -v existing",
		"local create --name next-runner",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("missing %q in Docker calls:\n%s", want, log)
		}
	}
}

func TestDockerFleetScopesToolCacheToEffectiveImage(t *testing.T) {
	fleet, logPath := testDockerFleet(t)
	if _, err := fleet.configurePools(`{"default":{"cache":true}}`); err != nil {
		t.Fatal(err)
	}
	start := func(name, image, id, arch string, repo int64) string {
		t.Helper()
		t.Setenv("FLEET_TEST_CACHE_ARCH", arch)
		t.Setenv("FLEET_TEST_CACHE_ID", id)
		ctx := withRunnerRepository(t.Context(), repo)
		if image != "" {
			ctx = withRunnerImage(ctx, image)
		}
		if err := fleet.startOn(ctx, fleetTestWorker, name, "test-jit"); err != nil {
			t.Fatal(err)
		}
		log, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(string(log), "\n") {
			if !strings.HasPrefix(line, fleetTestWorker+" create --name "+name+" ") {
				continue
			}
			found, _, _ := strings.Cut(strings.TrimSpace(line), " --env RUNNER_TOOL_CACHE=")
			if !strings.HasSuffix(line, " "+id) || found == line {
				t.Fatalf("runner was not pinned to %q with a tool cache: %s", id, line)
			}
			for mount := range strings.SplitSeq(found, " --mount type=volume,src=") {
				volume, rest, _ := strings.Cut(mount, ",")
				if strings.HasPrefix(rest, "dst="+runnerToolCache+",") && cacheVolumeName.MatchString(volume) {
					return volume
				}
			}
			t.Fatalf("could not find tool cache mount in %s", line)
		}
		t.Fatalf("could not find runner %s in recorded Docker calls", name)
		return ""
	}
	base := start("default", "", cacheFixtureImageID, "amd64", 41)
	if sameImage := start("alias", "runner:alias", cacheFixtureImageID, "amd64", 41); sameImage != base {
		t.Fatalf("aliases of the same image received different tool caches: %s and %s", base, sameImage)
	}
	selected := start("selected", "runner:debian12", cacheAlternateImageID, "amd64", 41)
	if selected == base || start("retagged", "", cacheAlternateImageID, "amd64", 41) != selected {
		t.Fatal("tool cache was split by tag or shared between different image IDs")
	}
	if start("arm", "", cacheFixtureImageID, "arm64", 41) == base || start("other-repo", "", cacheFixtureImageID, "amd64", 42) == base {
		t.Fatal("tool cache crossed its platform or repository boundary")
	}
}

func TestDockerFleetPreservesAvailableHosts(t *testing.T) {
	fleet, _ := testDockerFleet(t)
	for _, state := range []string{"offline", "missing-image"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("FLEET_TEST_WORKER_STATE", state)
			hosts, err := fleet.snapshot(t.Context())
			if err != nil || len(hosts) != 2 || hosts[0].Err != nil || hosts[0].Containers["existing"] != "running" ||
				hosts[1].Name != fleetTestWorker || hosts[1].Err == nil {
				t.Fatalf("unexpected Docker host snapshot: %+v, %v", hosts, err)
			}
			if state == "missing-image" && hosts[1].Containers["worker-existing"] != "paused" {
				t.Fatalf("missing runner image hid the host's containers: %+v", hosts[1])
			}
		})
	}
}

func TestDockerFleetRejectsDuplicatesAfterReconnect(t *testing.T) {
	fleet, _ := testDockerFleet(t)
	t.Setenv("FLEET_TEST_WORKER_STATE", "offline")
	if _, err := fleet.snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ state, message string }{
		{"duplicate-engine", "identify the same engine"},
		{"duplicate-runner", "exists on multiple Docker hosts"},
	} {
		t.Setenv("FLEET_TEST_WORKER_STATE", check.state)
		if _, err := fleet.snapshot(t.Context()); err == nil || !strings.Contains(err.Error(), check.message) {
			t.Errorf("expected %q after reconnection, got %v", check.message, err)
		}
	}
}

func TestDockerFleetHostLimits(t *testing.T) {
	fleet, err := newDockerFleet("", "local,"+fleetTestWorker)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := fleet.limits("local = 2, " + fleetTestWorker + "=4")
	if err != nil || len(limits) != 2 || limits["local"] != 2 || limits[fleetTestWorker] != 4 {
		t.Fatalf("unexpected host limits: %v, %v", limits, err)
	}
	for _, raw := range []string{"local=0", "local", "local=1,local=2", "unknown=1", "local=1,"} {
		if _, err := fleet.limits(raw); err == nil {
			t.Errorf("accepted invalid Docker host limits %q", raw)
		}
	}
}

func TestDockerFleetRejectsInvalidHosts(t *testing.T) {
	for _, hosts := range []string{"local,local", "local,", "tcp://worker:2375", "ssh://user:password@worker"} {
		if _, err := newDockerFleet("", hosts); err == nil {
			t.Errorf("accepted invalid Docker targets %q", hosts)
		}
	}
}

// An engine error ends with the engine's stderr, or with the error alone when
// the engine wrote nothing there.
func TestEngineCommandErrorDetail(t *testing.T) {
	dir := t.TempDir()
	for name, script := range map[string]string{"quiet": "exit 1\n", "loud": "echo '  no such container  ' >&2\nexit 1\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ binary, want string }{
		{filepath.Join(dir, "quiet"), filepath.Join(dir, "quiet") + " ps: exit status 1"},
		{filepath.Join(dir, "loud"), filepath.Join(dir, "loud") + " ps: exit status 1: no such container"},
	} {
		if _, err := engineCommand(t.Context(), test.binary, nil, "ps"); err == nil || err.Error() != test.want {
			t.Errorf("got %v, want %q", err, test.want)
		}
	}
	_, err := engineCommand(t.Context(), filepath.Join(dir, "missing"), nil, "ps")
	if err == nil || strings.HasSuffix(err.Error(), ": ") || !strings.Contains(err.Error(), "missing ps: ") {
		t.Errorf("missing engine: %v", err)
	}
}
