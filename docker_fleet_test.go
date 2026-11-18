package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerFleetPlacesAndRemovesRunnersOnTheirHost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FLEET_TEST_LOG", filepath.Join(dir, "docker.log"))
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(`#!/bin/sh
set -eu
host=local
if [ "$1" = --host ]; then host=$2; shift 2; fi
printf '%s %s\n' "$host" "$*" >> "$FLEET_TEST_LOG"
case "$1" in
  info) printf '%s\n' "$host" ;;
  ps) if [ "$host" = local ]; then printf 'existing\trunning\n'; fi ;;
  cp) cat >/dev/null ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	fleet, err := newDockerFleet("runner:local", "local,ssh://builder@worker.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.containers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := fleet.start(t.Context(), "new-runner", "test-jit"); err != nil {
		t.Fatal(err)
	}
	if err := fleet.remove(t.Context(), "existing"); err != nil {
		t.Fatal(err)
	}
	if err := fleet.start(t.Context(), "next-runner", "test-jit"); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(os.Getenv("FLEET_TEST_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ssh://builder@worker.example.com create --name new-runner",
		"ssh://builder@worker.example.com cp - new-runner:/run",
		"ssh://builder@worker.example.com start new-runner",
		"local rm -f -v existing",
		"local create --name next-runner",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("missing %q in Docker calls:\n%s", want, log)
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
