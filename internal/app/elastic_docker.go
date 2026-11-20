package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// elasticCommand runs the local docker CLI with args and returns its standard output.
type elasticCommand func(ctx context.Context, args ...string) (string, error)

// elasticHostConfig is one configured runner host as the elastic loop needs it.
type elasticHostConfig struct {
	Name      string
	Local     bool
	CPUs      float64 // pool guarantee per runner; 0 is unlimited
	MemoryMiB int64
	SlotLimit int
}

type elasticDeps struct {
	Config elasticConfig
	State  *elasticState
	// Hosts returns the current topology; it is read every tick so settings edits apply.
	Hosts  func() []elasticHostConfig
	Docker elasticCommand
	Label  string // runner label filter; defaults to runnerLabel
	Now    func() time.Time
}

// elasticHostsFrom reads the live topology the scheduler uses, without holding its lock across Docker calls.
func elasticHostsFrom(ops *operations) func() []elasticHostConfig {
	return func() []elasticHostConfig {
		ops.topology.RLock()
		defer ops.topology.RUnlock()
		state := ops.current.Load()
		fleet, ok := state.runners.runtime.(*dockerFleet)
		if !ok {
			return []elasticHostConfig{{Name: state.runners.kind}}
		}
		hosts := make([]elasticHostConfig, 0, len(fleet.hosts))
		for _, host := range fleet.hosts {
			limit := state.policy.Limit
			if override, ok := state.runners.hostLimits[host.label()]; ok {
				limit = min(override, limit)
			}
			hosts = append(hosts, elasticHostConfig{Name: host.label(), Local: host.host == "",
				CPUs: host.cpus, MemoryMiB: host.memoryMiB, SlotLimit: limit})
		}
		return hosts
	}
}

// runElastic adjusts runner limits on local Docker hosts on its own ticker until ctx ends.
func runElastic(ctx context.Context, deps elasticDeps) {
	if deps.Config.Mode == elasticOff || deps.Config.Mode == "" {
		return
	}
	if deps.Docker == nil {
		deps.Docker = func(ctx context.Context, args ...string) (string, error) { return dockerCommand(ctx, nil, args...) }
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.State == nil {
		deps.State = newElasticState(deps.Config)
	}
	var skipped []string
	for _, host := range deps.Hosts() {
		if !host.Local {
			skipped = append(skipped, host.Name)
		}
	}
	if len(skipped) > 0 {
		slog.Info("elastic runner limits are applied only on the local Docker host; not applied here", "hosts", skipped)
	}
	slog.Info("elastic runner limits enabled", "mode", deps.Config.Mode, "interval", deps.Config.Interval)
	ticker := time.NewTicker(deps.Config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deps.tick(ctx)
		}
	}
}

func (deps elasticDeps) tick(ctx context.Context) {
	defer func() {
		if err := recover(); err != nil {
			slog.Error("elastic runner limits tick failed", "panic", err)
		}
	}()
	for _, host := range deps.Hosts() {
		if !host.Local {
			continue
		}
		if host.CPUs == 0 && host.MemoryMiB == 0 {
			deps.State.forgetHost(host.Name)
			continue
		}
		sample, err := deps.sample(ctx, host)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("sample runner resources", "host", host.Name, "error", err)
			}
			continue
		}
		plan := planElastic(sample, deps.Config.Policy, deps.Now())
		deps.State.observe(sample, plan)
		deps.actuate(ctx, plan)
	}
}

type dockerInspectRunner struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status string `json:"Status"`
	} `json:"State"`
	HostConfig struct {
		NanoCPUs   int64 `json:"NanoCpus"`
		Memory     int64 `json:"Memory"`
		MemorySwap int64 `json:"MemorySwap"`
	} `json:"HostConfig"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

type dockerStatsLine struct {
	ID       string `json:"ID"`
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
}

func (deps elasticDeps) label() (key, value string) {
	label := deps.Label
	if label == "" {
		label = runnerLabel
	}
	key, value, _ = strings.Cut(label, "=")
	return key, value
}

func (deps elasticDeps) sample(ctx context.Context, cfg elasticHostConfig) (elasticHost, error) {
	host := elasticHost{Name: cfg.Name, SlotCPUs: cfg.CPUs, SlotMemoryMiB: cfg.MemoryMiB}
	info, err := deps.Docker(ctx, "info", "--format", "{{.NCPU}} {{.MemTotal}}")
	if err != nil {
		return host, err
	}
	if host.CPUs, host.MemoryMiB, err = parseDockerCapacity(info); err != nil {
		return host, err
	}
	key, value := deps.label()
	listed, err := deps.Docker(ctx, "ps", "-a", "--no-trunc", "--filter", "label="+key+"="+value, "--format", "{{.ID}}")
	if err != nil {
		return host, err
	}
	ids := strings.Fields(listed)
	inspected := deps.inspect(ctx, ids)
	occupied := 0
	var running []string
	for _, c := range inspected {
		// Re-check ownership: only this manager's runners are counted or touched, never an agent's.
		if c.Config.Labels[key] != value || c.Config.Labels[workerLabel] != "" {
			continue
		}
		if runnerOccupiesSlot(c.State.Status) {
			occupied++
		}
		if c.State.Status == "running" {
			running = append(running, c.ID)
		}
	}
	host.HeldSlots = max(0, cfg.SlotLimit-occupied)
	usage := deps.stats(ctx, running)
	for _, c := range inspected {
		if c.Config.Labels[key] != value || c.Config.Labels[workerLabel] != "" || !runnerOccupiesSlot(c.State.Status) {
			continue
		}
		r := elasticRunner{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), GuaranteeCPUs: cfg.CPUs, GuaranteeMemoryMiB: cfg.MemoryMiB,
			CPULimit: float64(c.HostConfig.NanoCPUs) / 1e9, MemoryLimitMiB: c.HostConfig.Memory >> 20}
		if stat, ok := usage[c.ID]; ok {
			r.Sampled, r.CPUUsage, r.MemoryUsageMiB = true, stat[0], stat[1]
		}
		r.LastCPUChange, r.LastMemoryChange = deps.State.lastChanges(c.ID)
		host.Runners = append(host.Runners, r)
	}
	return host, nil
}

// inspect reads containers in one call, falling back to one call each so a vanished runner only skips itself.
func (deps elasticDeps) inspect(ctx context.Context, ids []string) []dockerInspectRunner {
	if len(ids) == 0 {
		return nil
	}
	output, err := deps.Docker(ctx, append([]string{"inspect", "--type", "container"}, ids...)...)
	if err == nil {
		if containers, err := parseDockerInspect(output); err == nil {
			return containers
		}
	}
	if len(ids) == 1 {
		if ctx.Err() == nil {
			slog.Warn("inspect runner container", "container", ids[0], "error", err)
		}
		return nil
	}
	var containers []dockerInspectRunner
	for _, id := range ids {
		containers = append(containers, deps.inspect(ctx, []string{id})...)
	}
	return containers
}

// stats returns [cores, MiB] per container ID; runners without a sample are counted but not changed.
func (deps elasticDeps) stats(ctx context.Context, ids []string) map[string][2]float64 {
	usage := make(map[string][2]float64)
	if len(ids) == 0 {
		return usage
	}
	output, err := deps.Docker(ctx, append([]string{"stats", "--no-stream", "--no-trunc", "--format", "{{json .}}"}, ids...)...)
	if err == nil {
		err = parseDockerStats(output, usage)
	}
	if err == nil {
		return usage
	}
	if len(ids) == 1 {
		if ctx.Err() == nil {
			slog.Warn("sample runner usage", "container", ids[0], "error", err)
		}
		return usage
	}
	clear(usage)
	for _, id := range ids {
		for k, v := range deps.stats(ctx, []string{id}) {
			usage[k] = v
		}
	}
	return usage
}

func parseDockerCapacity(output string) (float64, int64, error) {
	fields := strings.Fields(output)
	if len(fields) != 2 {
		return 0, 0, errors.New("invalid Docker capacity report")
	}
	cpus, err := strconv.Atoi(fields[0])
	memory, merr := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || merr != nil || cpus < 1 || memory < 1 {
		return 0, 0, errors.New("invalid Docker capacity report")
	}
	return float64(cpus), memory >> 20, nil
}

func parseDockerInspect(output string) ([]dockerInspectRunner, error) {
	var containers []dockerInspectRunner
	if err := json.Unmarshal([]byte(output), &containers); err != nil {
		return nil, fmt.Errorf("invalid Docker inspect output: %w", err)
	}
	for _, c := range containers {
		if c.ID == "" || c.HostConfig.NanoCPUs < 0 || c.HostConfig.Memory < 0 {
			return nil, errors.New("invalid Docker inspect output")
		}
	}
	return containers, nil
}

func parseDockerStats(output string, usage map[string][2]float64) error {
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var stat dockerStatsLine
		if err := json.Unmarshal([]byte(line), &stat); err != nil || stat.ID == "" {
			return errors.New("invalid Docker stats output")
		}
		percent, err := strconv.ParseFloat(strings.TrimSuffix(stat.CPUPerc, "%"), 64)
		if err != nil || percent < 0 {
			return fmt.Errorf("invalid Docker CPU usage %q", stat.CPUPerc)
		}
		used, _, _ := strings.Cut(stat.MemUsage, "/")
		memory, err := parseDockerSize(strings.TrimSpace(used))
		if err != nil {
			return err
		}
		usage[stat.ID] = [2]float64{percent / 100, memory / (1 << 20)}
	}
	return nil
}

// parseDockerSize reads the binary sizes docker stats prints for memory (for example "3.172MiB").
func parseDockerSize(value string) (float64, error) {
	units := []struct {
		suffix string
		scale  float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"PiB", 1 << 50}, {"B", 1}}
	for _, unit := range units {
		if number, ok := strings.CutSuffix(value, unit.suffix); ok {
			n, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
			if err != nil || n < 0 {
				break
			}
			return n * unit.scale, nil
		}
	}
	return 0, fmt.Errorf("invalid Docker memory size %q", value)
}

// elasticUpdateArgs builds one docker update; swap matches memory as at docker create, so no swap is ever added.
func elasticUpdateArgs(id string, cpus float64, memoryMiB int64) []string {
	args := []string{"update"}
	if cpus > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(cpus, 'f', 2, 64))
	}
	if memoryMiB > 0 {
		memory := strconv.FormatInt(memoryMiB, 10) + "m"
		args = append(args, "--memory", memory, "--memory-swap", memory)
	}
	return append(args, id)
}

// actuate records every change and, in apply mode, makes it; reductions go first, and a failed
// reduction holds back this host's raises so lending never outruns the surplus.
func (deps elasticDeps) actuate(ctx context.Context, plan elasticHostPlan) {
	now := deps.Now()
	var lowers, raises []elasticChange
	for _, c := range plan.Changes {
		if c.To < c.From {
			lowers = append(lowers, c)
		} else {
			raises = append(raises, c)
		}
	}
	reductionFailed := false
	for _, group := range [][]elasticChange{lowers, raises} {
		for _, c := range group {
			d := elasticDecision{At: now, elasticChange: c}
			if deps.Config.Mode == elasticApply {
				switch {
				case reductionFailed && c.To > c.From:
					d.Error = "held back because a CPU reduction on this host failed"
				case c.Resource == "memory" && c.To < c.From:
					d.Error = "memory limits are never lowered"
				default:
					var args []string
					if c.Resource == "cpu" {
						args = elasticUpdateArgs(c.ID, c.To, 0)
					} else {
						args = elasticUpdateArgs(c.ID, 0, int64(c.To))
					}
					if _, err := deps.Docker(ctx, args...); err != nil {
						slog.Warn("update runner limits", "host", c.Host, "runner", c.Runner, "resource", c.Resource, "error", err)
						d.Error = "Docker did not accept the new limit"
						reductionFailed = reductionFailed || c.To < c.From
					} else {
						d.Applied = true
						slog.Info("runner limit changed", "host", c.Host, "runner", c.Runner, "resource", c.Resource,
							"from", c.From, "to", c.To, "reason", c.Reason)
					}
				}
			}
			deps.State.record(d)
		}
	}
}
