package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetHostCordonWithManagerRunning(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	const host = "ssh://builder@worker.example.com"
	t.Setenv("RUNQUILL_DB", path)
	t.Setenv("RUNQUILL_DOCKER_HOSTS", "local,"+host)
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	if _, err := q.ExecContext(ctx, `INSERT INTO docker_hosts (host, untracked_slots) VALUES (?, 2)`, host); err != nil {
		t.Fatal(err)
	}
	for _, cordoned := range []bool{true, true, false} {
		var out bytes.Buffer
		if err := setHostCordon(ctx, &out, host, cordoned); err != nil {
			t.Fatal(err)
		}
		states, err := readHostStates(ctx, q.DB)
		state := states[host]
		if err != nil || state.cordoned != cordoned || !state.untrackedSlots.Valid || state.untrackedSlots.Int64 != 2 {
			t.Fatalf("host state after %q: %+v, err=%v", out.String(), state, err)
		}
	}
}

func TestSetHostCordonRejectsUnknownHostAndMissingQueue(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "missing.sqlite3")
	t.Setenv("RUNQUILL_DB", path)
	t.Setenv("RUNQUILL_DOCKER_HOSTS", "local")
	if err := setHostCordon(ctx, io.Discard, "ssh://worker", true); err == nil || !strings.Contains(err.Error(), "unknown Docker host") {
		t.Fatalf("expected unknown host error, got %v", err)
	}
	if err := setHostCordon(ctx, io.Discard, "local", true); err == nil || !strings.Contains(err.Error(), "start the manager to initialize the queue") {
		t.Fatalf("expected missing queue error, got %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no database creation, stat returned %v", err)
	}
}
