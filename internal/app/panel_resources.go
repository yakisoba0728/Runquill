package app

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// GET /api/hosts/resources reads no host on its own except through caches: capacity comes from the
// engine probe that runner placement already caches, reservations from pool limits and the snapshot,
// usage from the elastic loop, and disk use only when ?disk=1 asks for it, at most once per hostDiskTTL.

const (
	hostDiskTTL     = 10 * time.Minute
	hostDiskTimeout = 2 * time.Minute
	// hostDiskWait keeps the handler below the panel's 10s WriteTimeout; a longer measurement finishes in the background.
	hostDiskWait = 7 * time.Second
)

type hostResourcesReport struct {
	Hosts []hostResources `json:"hosts"`
	// DiskMeasuredAt is when the cached disk measurement finished; absent before the first one.
	DiskMeasuredAt *time.Time `json:"disk_measured_at,omitempty"`
	DiskMeasuring  bool       `json:"disk_measuring"`
}

type hostResources struct {
	Host string `json:"host"`
	// Unavailable is agent for outbound workers and kubernetes for K3s; the manager reads nothing from such hosts.
	Unavailable string `json:"unavailable,omitempty"`
	// Error is set when the engine probe failed; capacity is then absent, not zero.
	Error       string           `json:"error,omitempty"`
	CPUs        int              `json:"cpus,omitempty"`
	MemoryBytes int64            `json:"memory_bytes,omitempty"`
	Reserved    *hostReservation `json:"reserved,omitempty"`
	Usage       *hostUsage       `json:"usage,omitempty"`
	Disk        *runnerDiskUse   `json:"disk,omitempty"`
	DiskError   string           `json:"disk_error,omitempty"`
}

// hostReservation is what runners holding a slot were promised; a zero per-runner limit means unlimited.
type hostReservation struct {
	Runners           int     `json:"runners"`
	RunnerCPUs        float64 `json:"runner_cpus"`
	RunnerMemoryBytes int64   `json:"runner_memory_bytes"`
	CPUs              float64 `json:"cpus"`
	MemoryBytes       int64   `json:"memory_bytes"`
}

// hostUsage is the runners' use in the elastic loop's last sample of the host.
type hostUsage struct {
	CPUs        float64 `json:"cpus"`
	MemoryBytes int64   `json:"memory_bytes"`
}

type hostDisk struct {
	use *runnerDiskUse
	err string
}

// hostDiskCache holds the last disk measurement; one measurement runs at a time for all panel users.
type hostDiskCache struct {
	mu      sync.Mutex
	at      time.Time
	hosts   map[string]hostDisk
	running chan struct{}
}

// refresh starts a measurement unless one is running or the last is younger than hostDiskTTL; the channel closes when it ends.
func (c *hostDiskCache) refresh(now time.Time, measure func(context.Context) map[string]hostDisk) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running != nil {
		return c.running
	}
	if c.hosts != nil && now.Sub(c.at) < hostDiskTTL {
		done := make(chan struct{})
		close(done)
		return done
	}
	done := make(chan struct{})
	c.running = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), hostDiskTimeout)
		hosts := measure(ctx)
		cancel()
		c.mu.Lock()
		c.hosts, c.at, c.running = hosts, time.Now(), nil
		c.mu.Unlock()
		close(done)
	}()
	return done
}

func (c *hostDiskCache) read() (map[string]hostDisk, *time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hosts == nil {
		return nil, nil, c.running != nil
	}
	at := c.at
	return c.hosts, &at, c.running != nil
}

// resourceHosts copies the configured Docker hosts, one per label; ok is false for a non-Docker backend.
func (p *panelState) resourceHosts() (hosts []docker, ok bool) {
	if p.operations == nil {
		return nil, false
	}
	p.operations.topology.RLock()
	defer p.operations.topology.RUnlock()
	state := p.operations.current.Load()
	if state == nil {
		return nil, false
	}
	fleet, ok := state.runners.runtime.(*dockerFleet)
	if !ok {
		return nil, false
	}
	seen := make(map[string]bool)
	for _, host := range fleet.hosts {
		if !seen[host.label()] {
			seen[host.label()] = true
			hosts = append(hosts, host)
		}
	}
	return hosts, true
}

func (p *panelState) hostResources(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	if r.URL.Query().Get("disk") == "1" {
		done := p.disk.refresh(time.Now(), func(ctx context.Context) map[string]hostDisk {
			return p.measureDisk(ctx, db)
		})
		select {
		case <-done:
		case <-time.After(hostDiskWait):
		case <-r.Context().Done():
			return
		}
	}
	panelJSON(w, http.StatusOK, p.resources(r.Context()))
}

func (p *panelState) resources(ctx context.Context) hostResourcesReport {
	p.mu.RLock()
	fleet := p.snapshot.Fleet
	p.mu.RUnlock()
	occupied := make(map[string]int)
	var snapshotHosts []string
	if fleet != nil {
		for _, host := range fleet.Hosts {
			occupied[host.Name] = host.OccupiedSlots
			snapshotHosts = append(snapshotHosts, host.Name)
		}
	}
	disks, measuredAt, measuring := p.disk.read()
	report := hostResourcesReport{Hosts: []hostResources{}, DiskMeasuredAt: measuredAt, DiskMeasuring: measuring}
	hosts, ok := p.resourceHosts()
	if !ok {
		// Kubernetes: the manager sees Pods, not the nodes they run on.
		for _, name := range snapshotHosts {
			report.Hosts = append(report.Hosts, hostResources{Host: name, Unavailable: "kubernetes"})
		}
		return report
	}
	elastic := p.elastic.report()
	usage := make(map[string]*hostUsage)
	limits := make(map[string]*hostReservation)
	for _, plan := range elastic.Hosts {
		usage[plan.Host] = &hostUsage{}
	}
	for _, runner := range elastic.Runners {
		if u := usage[runner.Host]; u != nil {
			u.CPUs += runner.CPUUsage
			u.MemoryBytes += int64(runner.MemoryUsageMiB * (1 << 20))
		}
		if limits[runner.Host] == nil {
			limits[runner.Host] = &hostReservation{}
		}
		limits[runner.Host].Runners++
		limits[runner.Host].CPUs += runner.CPUs
		limits[runner.Host].MemoryBytes += runner.MemoryMiB << 20
	}
	report.Hosts = make([]hostResources, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		name := host.label()
		reserved := &hostReservation{Runners: occupied[name], RunnerCPUs: host.cpus, RunnerMemoryBytes: host.memoryMiB << 20}
		reserved.CPUs, reserved.MemoryBytes = host.cpus*float64(reserved.Runners), reserved.RunnerMemoryBytes*int64(reserved.Runners)
		if sampled := limits[name]; sampled != nil {
			// The elastic loop saw the runners' current limits, which may differ from the pool's after a resize.
			reserved.Runners, reserved.CPUs, reserved.MemoryBytes = sampled.Runners, sampled.CPUs, sampled.MemoryBytes
		}
		entry := hostResources{Host: name, Reserved: reserved}
		if host.agentName() != "" {
			entry.Unavailable = "agent"
			report.Hosts[i] = entry
			continue
		}
		entry.Usage = usage[name]
		if disk, ok := disks[name]; ok {
			entry.Disk, entry.DiskError = disk.use, disk.err
		}
		report.Hosts[i] = entry
		wg.Go(func() {
			probeCtx, cancel := context.WithTimeout(ctx, dockerHostProbeTimeout)
			defer cancel()
			info, err := host.probe(probeCtx)
			if err != nil {
				report.Hosts[i].Error = "The engine on this host did not answer. Check the manager logs."
				return
			}
			report.Hosts[i].CPUs, report.Hosts[i].MemoryBytes = info.CPUs, info.MemoryBytes
		})
	}
	wg.Wait()
	return report
}

// measureDisk measures every local and ssh:// host in parallel; outbound workers are left out.
func (p *panelState) measureDisk(ctx context.Context, db *sql.DB) map[string]hostDisk {
	hosts, _ := p.resourceHosts()
	run := p.engineRun
	if run == nil {
		run = runImageCommand
	}
	cleaner := assetCleaner{run: run}
	var mu sync.Mutex
	var wg sync.WaitGroup
	result := make(map[string]hostDisk)
	for _, host := range hosts {
		if host.agentName() != "" {
			continue
		}
		wg.Go(func() {
			use, err := measureRunnerDisk(ctx, cleaner, db, host)
			disk := hostDisk{use: &use}
			if err != nil {
				slog.Warn("measure runner disk use", "host", host.label(), "error", oneLine(err))
				disk = hostDisk{err: "Disk use could not be measured. Check the manager logs."}
			}
			mu.Lock()
			result[host.label()] = disk
			mu.Unlock()
		})
	}
	wg.Wait()
	return result
}
