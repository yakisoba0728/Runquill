package app

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func savedSettingsFixture(t *testing.T, hosts, limits, pools, saved string) (*queue, config, runnerConfiguration) {
	t.Helper()
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	cfg := config{limit: 2, pollInterval: 30 * time.Second, runnerImage: "runquill-runner:local"}
	fleet, err := parseDockerFleet(cfg.runnerImage, hosts)
	if err != nil {
		t.Fatal(err)
	}
	hostLimits, err := fleet.limits(limits)
	if err != nil {
		t.Fatal(err)
	}
	workerPools, err := fleet.configurePools(pools)
	if err != nil {
		t.Fatal(err)
	}
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.close() })
	if _, err := q.Exec("INSERT INTO operation_settings VALUES (1, 7, ?)", saved); err != nil {
		t.Fatal(err)
	}
	return q, cfg, runnerConfiguration{runtime: fleet, kind: "docker", hostCount: len(fleet.hosts), hostLimits: hostLimits, pools: workerPools}
}

func loadedHosts(state *operationState) []string {
	var hosts []string
	for _, host := range state.runners.runtime.(*dockerFleet).hosts {
		hosts = append(hosts, host.label())
	}
	return hosts
}

func TestSavedSettingsFollowDeployedAgentHosts(t *testing.T) {
	const saved = `{"docker_hosts":["local","ssh://deploy@build-1"],"host_limits":{"ssh://deploy@build-1":2},
		"worker_pools":{"build":{"hosts":["ssh://deploy@build-1"]}}}`
	q, cfg, base := savedSettingsFixture(t, "local,agent://worker-a", "agent://worker-a=3",
		`{"secure":{"hosts":["agent://worker-a"],"repositories":["owner/app"]}}`, saved)
	ops, err := loadOperations(t.Context(), q.DB, cfg, base)
	if err != nil {
		t.Fatalf("saved settings stopped the manager after an agent host was deployed: %v", err)
	}
	state := ops.current.Load()
	if got := loadedHosts(state); !slices.Equal(got, []string{"local", "ssh://deploy@build-1", "agent://worker-a"}) {
		t.Fatalf("hosts %v", got)
	}
	if state.policy.Revision != 7 || state.runners.hostLimits["ssh://deploy@build-1"] != 2 || state.runners.hostLimits["agent://worker-a"] != 3 {
		t.Fatalf("saved or deployed limits were lost: %+v", state.policy)
	}
	secure := state.runners.pools["secure"]
	if !slices.Equal(secure.Hosts, []string{"agent://worker-a"}) || !slices.Equal(secure.Repositories, []string{"owner/app"}) ||
		!slices.Equal(state.runners.pools["build"].Hosts, []string{"ssh://deploy@build-1"}) {
		t.Fatalf("the agent host left its deployed pool: %+v", state.runners.pools)
	}
	if len(state.adjusted) == 0 {
		t.Fatal("the adjustment was not reported")
	}
	var stored string
	if err := q.QueryRow("SELECT policy FROM operation_settings").Scan(&stored); err != nil || stored != saved {
		t.Fatalf("loading rewrote the saved settings: %q, %v", stored, err)
	}
	// The panel still cannot enroll or remove agent hosts.
	for _, hosts := range [][]string{{"local", "ssh://deploy@build-1"}, {"local", "agent://worker-a", "agent://worker-b"}} {
		policy := state.policy
		policy.Hosts, policy.HostLimits, policy.Pools = hosts, nil, nil
		if _, err := ops.resolve(policy); err == nil {
			t.Fatalf("panel settings changed agent hosts to %v", hosts)
		}
	}
}

func TestSavedSettingsDropRemovedHosts(t *testing.T) {
	const saved = `{"host_limits":{"local":1,"agent://worker-b":1},
		"worker_pools":{"only-b":{"hosts":["agent://worker-b"]},"mixed":{"hosts":["local","agent://worker-b"],"cpus":2}}}`
	q, cfg, base := savedSettingsFixture(t, "local,agent://worker-a", "", "", saved)
	ops, err := loadOperations(t.Context(), q.DB, cfg, base)
	if err != nil {
		t.Fatalf("saved settings stopped the manager after a host was removed: %v", err)
	}
	state := ops.current.Load()
	if got := loadedHosts(state); !slices.Equal(got, []string{"local", "agent://worker-a"}) {
		t.Fatalf("hosts %v", got)
	}
	if _, ok := state.runners.hostLimits["agent://worker-b"]; ok || state.runners.hostLimits["local"] != 1 {
		t.Fatalf("limits %v", state.runners.hostLimits)
	}
	if _, ok := state.runners.pools["only-b"]; ok || !slices.Equal(state.runners.pools["mixed"].Hosts, []string{"local"}) ||
		state.runners.pools["mixed"].CPUs != 2 {
		t.Fatalf("pools %+v", state.runners.pools)
	}
}

func TestUnusableSavedSettingsStillStopTheManager(t *testing.T) {
	// Starting with the deployment settings instead would drop saved pool restrictions.
	q, cfg, base := savedSettingsFixture(t, "local", "", "", `{"slot_limit":0,"docker_hosts":["ftp://nowhere"]}`)
	if _, err := loadOperations(t.Context(), q.DB, cfg, base); err == nil || !strings.Contains(err.Error(), "load saved operation settings") {
		t.Fatalf("unusable saved settings were accepted: %v", err)
	}
}
