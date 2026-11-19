package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fleetTestWorker = "ssh://builder@worker.example.com"

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
  info) if [ "$state" = duplicate-engine ]; then echo local; else printf '%s\n' "$host"; fi ;;
  ps)
    if [ "$host" = local ] || [ "$state" = duplicate-runner ]; then
      printf 'existing\trunning\n'
    else
      printf 'worker-existing\tpaused\n'
    fi
    ;;
  image) if [ "$state" = missing-image ]; then exit 1; fi ;;
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
