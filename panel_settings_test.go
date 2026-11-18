package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPanelSettingsAreAuditedAndAppliedWithoutRestart(t *testing.T) {
	ctx := t.Context()
	t.Setenv("RUNQUILL_BACKEND", "docker")
	t.Setenv("RUNQUILL_DOCKER_HOSTS", "local")
	t.Setenv("RUNQUILL_DOCKER_HOST_LIMITS", "")
	t.Setenv("RUNQUILL_WORKER_POOLS", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	cfg := config{limit: 2, pollInterval: 30 * time.Second, runnerImage: "runquill-runner:local"}
	base, err := configureRunnerRuntime(cfg.runnerImage)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := loadOperations(ctx, q.DB, cfg, base)
	if err != nil {
		t.Fatal(err)
	}
	panel := newPanel(cfg)
	panel.operations, panel.backend = ops, base.kind
	access := panelAccessForTest(t, q)
	if _, err := q.Exec("INSERT INTO panel_users(github_id, role) VALUES (102, 'viewer')"); err != nil {
		t.Fatal(err)
	}
	login := func(id int64) string {
		t.Helper()
		key, err := access.startSession(ctx, panelIdentity{GitHubID: id, Login: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	admin, viewer := login(101), login(102)
	handler := panelHandler(panel, access)
	call := func(method, session, origin string, policy operationPolicy, expected int) {
		t.Helper()
		body, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, access.cfg.origin+"/api/settings", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		if session != "" {
			req.AddCookie(&http.Cookie{Name: access.cookieName("session"), Value: session})
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != expected {
			t.Fatalf("%s settings returned %d, want %d: %s", method, res.Code, expected, res.Body)
		}
	}
	initial := ops.current.Load()
	policy := initial.policy
	policy.Limit, policy.PollInterval = 4, "45s"
	policy.HostLimits = map[string]int{"local": 3}
	policy.Pools = workerPools{"build": {Hosts: []string{"local"}, CPUs: 2}}
	call("GET", "", "", policy, http.StatusUnauthorized)
	call("GET", viewer, "", policy, http.StatusForbidden)
	call("PUT", viewer, access.cfg.origin, policy, http.StatusForbidden)
	call("PUT", admin, "https://other.example", policy, http.StatusForbidden)
	invalid := policy
	invalid.PollInterval = "0"
	call("PUT", admin, access.cfg.origin, invalid, http.StatusBadRequest)
	call("PUT", admin, access.cfg.origin, policy, http.StatusOK)
	call("PUT", admin, access.cfg.origin, policy, http.StatusConflict)
	select {
	case <-initial.pollEdited:
	default:
		t.Fatal("the poller was not notified of the new interval")
	}
	scheduler := &scheduler{queue: q, operations: ops, panel: panel}
	if err := scheduler.tick(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	fleet := panel.snapshot.Fleet
	if fleet == nil || fleet.SlotLimit != 4 || len(fleet.Hosts) != 1 || fleet.Hosts[0].SlotLimit != 3 || fleet.Hosts[0].Pool != "build" {
		t.Fatalf("scheduler did not apply settings: %+v", fleet)
	}
	cli, err := inspectStatus(ctx, q.DB)
	if err != nil || cli.SlotLimit != 4 || len(cli.Hosts) != 1 || cli.Hosts[0].SlotLimit != 3 {
		t.Fatalf("status does not report saved settings: %v", err)
	}
	restarted, err := loadOperations(ctx, q.DB, cfg, base)
	if err != nil {
		t.Fatal(err)
	}
	state := restarted.current.Load()
	if state.policy.Revision != 1 || state.policy.Limit != 4 || state.interval != 45*time.Second {
		t.Fatalf("saved configuration could not be reloaded: %+v", state.policy)
	}
	cfg.runnerImage = "runquill-runner:upgraded"
	updatedBase, err := configureRunnerRuntime(cfg.runnerImage)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := loadOperations(ctx, q.DB, cfg, updatedBase)
	if err != nil {
		t.Fatal(err)
	}
	updated := upgraded.current.Load()
	if updated.policy.Image != cfg.runnerImage || updated.policy.Limit != 4 || updated.runners.hostLimits["local"] != 3 {
		t.Fatalf("untouched image did not follow new default, or edited limits were lost: %+v", updated.policy)
	}
	var changes int
	if err := q.QueryRow("SELECT count(*) FROM panel_audit WHERE action='settings.update' AND actor=101").Scan(&changes); err != nil || changes != 1 {
		t.Fatalf("settings audit count %d: %v", changes, err)
	}
}
