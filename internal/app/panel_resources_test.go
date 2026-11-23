package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func resourcePanel(runtime runnerRuntime, hosts ...statusHost) *panelState {
	panel := newPanel(config{})
	panel.operations = &operations{}
	panel.operations.current.Store(&operationState{runners: runnerConfiguration{runtime: runtime, kind: "docker"}})
	panel.snapshot.Fleet = &statusReport{Hosts: hosts}
	return panel
}

func readResources(t *testing.T, panel *panelState, query string) hostResourcesReport {
	t.Helper()
	res := httptest.NewRecorder()
	panel.hostResources(res, httptest.NewRequest("GET", "/api/hosts/resources"+query, nil), nil)
	var report hostResourcesReport
	if err := json.Unmarshal(res.Body.Bytes(), &report); err != nil {
		t.Fatalf("%d %s: %v", res.Code, res.Body, err)
	}
	return report
}

func TestHostResourcesExplainWhatTheManagerCannotRead(t *testing.T) {
	probed := &engineSecurity{info: &engineInfo{Engine: "docker", Version: "29", CPUs: 8, MemoryBytes: 16 << 30}}
	missing := &containerEngine{name: "runquill-test-missing-engine", remoteFlag: "--host"}
	fleet := &dockerFleet{hosts: []docker{
		{security: probed, cpus: 2, memoryMiB: 4096},
		{host: "ssh://ci@build", engine: missing, security: &engineSecurity{}},
		{host: "agent://gpu", cpus: 4},
	}}
	panel := resourcePanel(fleet, statusHost{Name: "local", OccupiedSlots: 3}, statusHost{Name: "agent://gpu", OccupiedSlots: 1})
	report := readResources(t, panel, "")
	if len(report.Hosts) != 3 || report.DiskMeasuredAt != nil || report.DiskMeasuring {
		t.Fatalf("report %+v", report)
	}
	local, ssh, agent := report.Hosts[0], report.Hosts[1], report.Hosts[2]
	if local.CPUs != 8 || local.MemoryBytes != 16<<30 || local.Error != "" || local.Usage != nil || local.Disk != nil ||
		*local.Reserved != (hostReservation{Runners: 3, RunnerCPUs: 2, RunnerMemoryBytes: 4 << 30, CPUs: 6, MemoryBytes: 12 << 30}) {
		t.Fatalf("local %+v %+v", local, local.Reserved)
	}
	if ssh.Error == "" || ssh.CPUs != 0 || ssh.Unavailable != "" || *ssh.Reserved != (hostReservation{}) {
		t.Fatalf("an unreachable engine must report an error, not zero capacity: %+v", ssh)
	}
	if agent.Unavailable != "agent" || agent.CPUs != 0 || agent.Reserved.Runners != 1 || agent.Reserved.CPUs != 4 {
		t.Fatalf("agent %+v %+v", agent, agent.Reserved)
	}

	// The elastic loop's sample replaces the pool's limits with the runners' current ones and adds usage.
	panel.elastic = newElasticState(elasticConfig{Mode: elasticObserve})
	panel.elastic.observe(elasticHost{Name: "local", Runners: []elasticRunner{
		{ID: "a", Name: "r1", CPULimit: 3, MemoryLimitMiB: 6144, CPUUsage: 1.5, MemoryUsageMiB: 512},
	}}, elasticHostPlan{Host: "local"})
	local = readResources(t, panel, "").Hosts[0]
	if local.Usage == nil || *local.Usage != (hostUsage{CPUs: 1.5, MemoryBytes: 512 << 20}) ||
		local.Reserved.Runners != 1 || local.Reserved.CPUs != 3 || local.Reserved.MemoryBytes != 6<<30 {
		t.Fatalf("elastic %+v %+v", local.Usage, local.Reserved)
	}

	kube := resourcePanel(nil, statusHost{Name: "kubernetes"})
	if got := readResources(t, kube, "").Hosts; len(got) != 1 || got[0] != (hostResources{Host: "kubernetes", Unavailable: "kubernetes"}) {
		t.Fatalf("kubernetes %+v", got)
	}
	if got := readResources(t, newPanel(config{}), ""); got.Hosts == nil || len(got.Hosts) != 0 {
		t.Fatalf("no operations %+v", got)
	}
}

func TestHostResourcesMeasureDiskOnDemand(t *testing.T) {
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	f := newAssetFixture(t)
	probed := &engineSecurity{info: &engineInfo{Engine: "docker", Version: "29", CPUs: 2, MemoryBytes: 1 << 30}}
	panel := resourcePanel(&dockerFleet{hosts: []docker{{security: probed}, {host: "agent://gpu"}}})
	panel.engineRun = f.engine.run
	measure := func(query string) hostResourcesReport {
		res := httptest.NewRecorder()
		panel.hostResources(res, httptest.NewRequest("GET", "/api/hosts/resources"+query, nil), f.db)
		var report hostResourcesReport
		if err := json.Unmarshal(res.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	if report := measure(""); report.Hosts[0].Disk != nil || len(f.engine.calls) != 0 {
		t.Fatalf("a plain read must not touch the engine: %+v %q", report.Hosts[0], f.engine.calls)
	}
	report := measure("?disk=1")
	local := report.Hosts[0]
	if report.DiskMeasuredAt == nil || report.DiskMeasuring || local.Disk == nil || local.DiskError != "" ||
		local.Disk.CacheVolumes != 1 || local.Disk.CacheBytes == nil || *local.Disk.CacheBytes != 1_500_000 ||
		local.Disk.Images != 1 || local.Disk.ImageBytes != 2_000_000 {
		t.Fatalf("disk %+v %+v", report, local.Disk)
	}
	if report.Hosts[1].Disk != nil || report.Hosts[1].Unavailable != "agent" {
		t.Fatalf("agent %+v", report.Hosts[1])
	}
	calls := len(f.engine.calls)
	if again := measure("?disk=1"); again.Hosts[0].Disk == nil || len(f.engine.calls) != calls {
		t.Fatalf("a second request within the TTL measured again: %q", f.engine.calls[calls:])
	}
	if slices.ContainsFunc(f.engine.calls, func(call string) bool { return strings.HasPrefix(call, "agent://") }) {
		t.Fatalf("measured an outbound worker: %q", f.engine.calls)
	}
}

func TestHostDiskCacheRunsOneMeasurement(t *testing.T) {
	var cache hostDiskCache
	var runs atomic.Int32
	release := make(chan struct{})
	measure := func(context.Context) map[string]hostDisk {
		runs.Add(1)
		<-release
		return map[string]hostDisk{"local": {err: "x"}}
	}
	now := time.Now()
	first, second := cache.refresh(now, measure), cache.refresh(now, measure)
	if first != second {
		t.Fatal("a concurrent refresh started a second measurement")
	}
	if _, at, measuring := cache.read(); at != nil || !measuring {
		t.Fatalf("while measuring: %v %t", at, measuring)
	}
	close(release)
	<-first
	<-cache.refresh(now, measure)
	if runs.Load() != 1 {
		t.Fatalf("runs = %d within the TTL", runs.Load())
	}
	<-cache.refresh(now.Add(hostDiskTTL+time.Minute), measure)
	if runs.Load() != 2 {
		t.Fatalf("runs = %d after the TTL", runs.Load())
	}
}
