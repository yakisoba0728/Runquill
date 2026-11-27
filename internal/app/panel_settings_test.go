package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPanelSettingsAreAuditedAndAppliedWithoutRestart(t *testing.T) {
	ctx := t.Context()
	t.Setenv("RUNQUILL_BACKEND", "docker")
	t.Setenv("RUNQUILL_DOCKER_HOSTS", "local")
	t.Setenv("RUNQUILL_DOCKER_HOST_LIMITS", "")
	t.Setenv("RUNQUILL_WORKER_POOLS", `{"build":{"hosts":["local"],"cpus":2,"runtime":"sysbox-runc"}}`)
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
	call := func(method, session, origin string, policy operationPolicy, expected int) operationPolicy {
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
		var response operationSettingsResponse
		if expected == http.StatusOK {
			if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
		}
		return response.Policy
	}
	initial := ops.current.Load()
	policy := call("GET", admin, "", operationPolicy{}, http.StatusOK)
	build := policy.Pools["build"]
	if build.Runtime != sysboxRuntime {
		t.Fatalf("settings did not expose the configured runtime: %+v", build)
	}
	build.CPUs = 3
	policy.Pools["build"] = build
	policy.Limit, policy.PollInterval = 4, "45s"
	policy.HostLimits = map[string]int{"local": 3}
	call("GET", "", "", policy, http.StatusUnauthorized)
	call("GET", viewer, "", policy, http.StatusForbidden)
	call("PUT", viewer, access.cfg.origin, policy, http.StatusForbidden)
	call("PUT", admin, "https://other.example", policy, http.StatusForbidden)
	invalid := policy
	invalid.PollInterval = "0"
	call("PUT", admin, access.cfg.origin, invalid, http.StatusBadRequest)
	saved := call("PUT", admin, access.cfg.origin, policy, http.StatusOK)
	if saved.Pools["build"].Runtime != sysboxRuntime {
		t.Fatalf("saving settings lost the runtime: %+v", saved.Pools)
	}
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
	if state.policy.Revision != 1 || state.policy.Limit != 4 || state.interval != 45*time.Second ||
		state.policy.Pools["build"].Runtime != sysboxRuntime || state.runners.runtime.(*dockerFleet).hosts[0].runtime != sysboxRuntime {
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
	var target string
	if err := q.QueryRow("SELECT target FROM panel_audit WHERE action='settings.update'").Scan(&target); err != nil ||
		target != "docker: slot_limit 2 -> 4; poll_interval 30s -> 45s; host_limits[local] unset -> 3; worker_pools[build].cpus 2 -> 3" {
		t.Fatalf("settings audit target %q: %v", target, err)
	}
}

func TestSettingsChangeSummaryNamesFieldsWithoutSecrets(t *testing.T) {
	off := false
	old := operationPolicy{Limit: 2, PollInterval: "30s", Image: "runner:1", Hosts: []string{"local", "ssh://a@b"},
		HostLimits: map[string]int{"local": 2},
		Pools:      workerPools{"build": {Hosts: []string{"local"}, CPUs: 2}, "old": {Hosts: []string{"ssh://a@b"}}}}
	next := operationPolicy{Limit: 2, PollInterval: "30s", Image: "runner:2", Hosts: []string{"local", "agent://c"},
		Pools: workerPools{"build": {Hosts: []string{"local"}, CPUs: 4, Privileged: &off, Cache: true}}}
	got := settingsChangeSummary("docker", old, next)
	want := "docker: runner_image runner:1 -> runner:2; docker_hosts +agent://c; docker_hosts -ssh://a@b; " +
		"host_limits[local] 2 -> unset; worker_pools[build].cache default -> true; worker_pools[build].cpus 2 -> 4; " +
		"worker_pools[build].privileged default -> false; worker_pools -old"
	if got != want {
		t.Fatalf("summary\n got %s\nwant %s", got, want)
	}
	if got := settingsChangeSummary("kubernetes", old, old); got != "kubernetes: no changes" {
		t.Fatalf("unchanged summary: %s", got)
	}
	long := next
	long.Image = strings.Repeat("x", 2000)
	if got := []rune(settingsChangeSummary("docker", old, long)); len(got) != settingsSummaryLimit {
		t.Fatalf("summary was not capped: %d", len(got))
	}
	if !secretSettingName.MatchString("api_token") || secretSettingName.MatchString("max_per_repository") {
		t.Fatal("secret field names are not recognised")
	}
}
