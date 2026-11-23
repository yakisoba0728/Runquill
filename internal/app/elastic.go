package app

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type elasticMode string

const (
	elasticOff     elasticMode = "off"
	elasticObserve elasticMode = "observe"
	elasticApply   elasticMode = "apply"

	elasticDecisionHistory = 200
)

// elasticPolicy holds the knobs that decide when a runner may borrow host capacity.
type elasticPolicy struct {
	ReserveCPUs      float64       `json:"host_reserve_cpus"`
	ReserveMemoryMiB int64         `json:"host_reserve_memory_mib"`
	MemoryTrigger    float64       `json:"memory_trigger"` // raise memory once usage passes this share of the limit
	MaxMultiple      float64       `json:"max_multiple"`   // no limit grows past this multiple of the guarantee
	CPUBusy          float64       `json:"cpu_busy"`       // share of the current CPU limit that counts as saturated
	CPUIdle          float64       `json:"cpu_idle"`       // share of the guarantee below which borrowed CPU goes back
	MinCPUStep       float64       `json:"min_cpu_step"`
	MinMemoryStepMiB int64         `json:"min_memory_step_mib"`
	Cooldown         time.Duration `json:"-"`
}

type elasticConfig struct {
	Mode     elasticMode
	Interval time.Duration
	Policy   elasticPolicy
	// JobResources records each job's sampled usage, even with Mode off.
	JobResources bool
}

func loadElasticConfig(getenv func(string) string) (elasticConfig, error) {
	cfg := elasticConfig{Mode: elasticOff, Interval: 10 * time.Second, JobResources: true, Policy: elasticPolicy{
		ReserveCPUs: 1, ReserveMemoryMiB: 512, MemoryTrigger: 0.9, MaxMultiple: 2,
		CPUBusy: 0.9, CPUIdle: 0.5, MinCPUStep: 0.1, MinMemoryStepMiB: 64,
	}}
	switch mode := elasticMode(strings.TrimSpace(getenv("RUNQUILL_ELASTIC"))); mode {
	case "":
	case elasticOff, elasticObserve, elasticApply:
		cfg.Mode = mode
	default:
		return cfg, errors.New("RUNQUILL_ELASTIC must be off, observe or apply")
	}
	switch strings.TrimSpace(getenv("RUNQUILL_JOB_RESOURCES")) {
	case "", "on":
	case "off":
		cfg.JobResources = false
	default:
		return cfg, errors.New("RUNQUILL_JOB_RESOURCES must be on or off")
	}
	duration := func(name string, target *time.Duration) error {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return nil
		}
		value, err := time.ParseDuration(raw)
		if err != nil || value < time.Second || value > time.Hour {
			return fmt.Errorf("%s must be a duration between 1s and 1h", name)
		}
		*target = value
		return nil
	}
	number := func(name string, target *float64, low, high float64) error {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return nil
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || value < low || value > high {
			return fmt.Errorf("%s must be a number between %g and %g", name, low, high)
		}
		*target = value
		return nil
	}
	if err := duration("RUNQUILL_ELASTIC_INTERVAL", &cfg.Interval); err != nil {
		return cfg, err
	}
	cfg.Policy.Cooldown = 3 * cfg.Interval
	reserveMemory := float64(cfg.Policy.ReserveMemoryMiB)
	for _, err := range []error{
		duration("RUNQUILL_ELASTIC_COOLDOWN", &cfg.Policy.Cooldown),
		number("RUNQUILL_ELASTIC_HOST_RESERVE_CPUS", &cfg.Policy.ReserveCPUs, 0, 1_000_000),
		number("RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB", &reserveMemory, 0, 1<<40),
		number("RUNQUILL_ELASTIC_MEMORY_TRIGGER", &cfg.Policy.MemoryTrigger, 0.5, 0.99),
		number("RUNQUILL_ELASTIC_MAX_MULTIPLE", &cfg.Policy.MaxMultiple, 1, 16),
	} {
		if err != nil {
			return cfg, err
		}
	}
	if reserveMemory != math.Trunc(reserveMemory) {
		return cfg, errors.New("RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB must be a whole number of MiB")
	}
	cfg.Policy.ReserveMemoryMiB = int64(reserveMemory)
	if cfg.Policy.Cooldown < cfg.Interval {
		return cfg, errors.New("RUNQUILL_ELASTIC_COOLDOWN must not be shorter than RUNQUILL_ELASTIC_INTERVAL")
	}
	return cfg, nil
}

// elasticRunner is one runner container as sampled; zero guarantees mean the pool leaves it unlimited.
type elasticRunner struct {
	ID, Name           string
	GuaranteeCPUs      float64
	GuaranteeMemoryMiB int64
	CPULimit           float64 // 0 is unlimited
	MemoryLimitMiB     int64   // 0 is unlimited
	Sampled            bool    // usage below is from this window; unsampled runners are only counted
	CPUUsage           float64 // cores used over the sample window
	MemoryUsageMiB     float64
	LastCPUChange      time.Time
	LastMemoryChange   time.Time
}

// elasticHost is one engine's capacity, the runners on it, and the slots still held for queued jobs.
type elasticHost struct {
	Name          string
	CPUs          float64
	MemoryMiB     int64
	HeldSlots     int
	SlotCPUs      float64
	SlotMemoryMiB int64
	Runners       []elasticRunner
}

type elasticChange struct {
	Host     string  `json:"host"`
	ID       string  `json:"-"`
	Runner   string  `json:"runner"`
	Resource string  `json:"resource"` // "cpu" in cores or "memory" in MiB
	From     float64 `json:"from"`
	To       float64 `json:"to"`
	Reason   string  `json:"reason"`
	// Forced reclaims restore the host invariant and skip damping.
	Forced bool `json:"forced,omitempty"`
}

type elasticHostPlan struct {
	Host               string          `json:"host"`
	CPUs               float64         `json:"cpus"`
	MemoryMiB          int64           `json:"memory_mib"`
	HeldSlots          int             `json:"held_slots"`
	SurplusCPUs        float64         `json:"surplus_cpus"`
	LentCPUs           float64         `json:"lent_cpus"`
	SurplusMemoryMiB   int64           `json:"surplus_memory_mib"`
	ExtraMemoryMiB     int64           `json:"extra_memory_mib"`
	MemoryOvercommited bool            `json:"memory_overcommitted,omitempty"`
	Changes            []elasticChange `json:"-"`
}

// cpuBase is what a runner is promised; a runner started before its pool grew keeps the limit it was given.
func cpuBase(r elasticRunner) float64 {
	if r.GuaranteeCPUs == 0 {
		return r.CPULimit
	}
	return r.GuaranteeCPUs
}

func memoryBase(r elasticRunner) int64 {
	if r.GuaranteeMemoryMiB == 0 {
		return r.MemoryLimitMiB
	}
	return r.GuaranteeMemoryMiB
}

func cpuManaged(r elasticRunner) bool {
	return r.GuaranteeCPUs > 0 && r.CPULimit >= r.GuaranteeCPUs
}

func memoryManaged(r elasticRunner) bool {
	return r.GuaranteeMemoryMiB > 0 && r.MemoryLimitMiB >= r.GuaranteeMemoryMiB
}

// elasticCPUBudget returns the surplus beyond every guarantee, held slot and the host reserve, and how much of it is lent.
func elasticCPUBudget(h elasticHost, p elasticPolicy) (surplus, lent float64) {
	surplus = h.CPUs - p.ReserveCPUs - float64(h.HeldSlots)*h.SlotCPUs
	for _, r := range h.Runners {
		surplus -= cpuBase(r)
		lent += max(0, r.CPULimit-cpuBase(r))
	}
	return surplus, lent
}

func elasticMemoryBudget(h elasticHost, p elasticPolicy) (surplus, extra int64) {
	surplus = h.MemoryMiB - p.ReserveMemoryMiB - int64(h.HeldSlots)*h.SlotMemoryMiB
	for _, r := range h.Runners {
		surplus -= memoryBase(r)
		extra += max(0, r.MemoryLimitMiB-memoryBase(r))
	}
	return surplus, extra
}

func floorCPU(v float64) float64 { return math.Floor(v*100+1e-9) / 100 }

// planElastic decides new limits for one host. It never lowers a limit below its guarantee,
// never lowers memory, only lends the surplus beyond guarantees, held slots and the reserve,
// and changes each runner's resource at most once per cooldown unless reclaiming lent CPU.
func planElastic(h elasticHost, p elasticPolicy, now time.Time) elasticHostPlan {
	plan := elasticHostPlan{Host: h.Name, CPUs: h.CPUs, MemoryMiB: h.MemoryMiB, HeldSlots: h.HeldSlots}
	runners := slices.Clone(h.Runners)
	slices.SortFunc(runners, func(a, b elasticRunner) int { return cmp.Compare(a.Name+"\x00"+a.ID, b.Name+"\x00"+b.ID) })
	cooled := func(last time.Time) bool { return last.IsZero() || now.Sub(last) >= p.Cooldown }
	change := func(r elasticRunner, resource string, from, to float64, forced bool, reason string, args ...any) {
		plan.Changes = append(plan.Changes, elasticChange{Host: h.Name, ID: r.ID, Runner: r.Name, Resource: resource,
			From: from, To: to, Forced: forced, Reason: fmt.Sprintf(reason, args...)})
	}

	surplus, lent := elasticCPUBudget(h, p)
	plan.SurplusCPUs, plan.LentCPUs = surplus, lent
	free := max(0, surplus) - lent
	limits := make([]float64, len(runners))
	for i, r := range runners {
		limits[i] = r.CPULimit
	}
	if free < -1e-9 {
		// The surplus shrank under existing loans: take CPU back from the biggest borrowers first, at once.
		order := make([]int, 0, len(runners))
		for i, r := range runners {
			if cpuManaged(r) && r.CPULimit > r.GuaranteeCPUs {
				order = append(order, i)
			}
		}
		slices.SortStableFunc(order, func(a, b int) int {
			return cmp.Compare(runners[b].CPULimit-runners[b].GuaranteeCPUs, runners[a].CPULimit-runners[a].GuaranteeCPUs)
		})
		for _, i := range order {
			if free >= -1e-9 {
				break
			}
			r := runners[i]
			target := max(r.GuaranteeCPUs, floorCPU(r.CPULimit+free))
			free += r.CPULimit - target
			limits[i] = target
			change(r, "cpu", r.CPULimit, target, true, "host surplus is %.2f CPUs but %.2f are lent; returning %.2f borrowed CPUs",
				max(0, surplus), lent, r.CPULimit-target)
		}
	}
	for i, r := range runners {
		if !cpuManaged(r) || !r.Sampled || limits[i] != r.CPULimit || r.CPULimit <= r.GuaranteeCPUs || !cooled(r.LastCPUChange) {
			continue
		}
		if r.CPUUsage < p.CPUIdle*r.GuaranteeCPUs {
			free += r.CPULimit - r.GuaranteeCPUs
			limits[i] = r.GuaranteeCPUs
			change(r, "cpu", r.CPULimit, r.GuaranteeCPUs, false, "idle at %.2f of %.2f guaranteed CPUs; returning %.2f borrowed CPUs",
				r.CPUUsage, r.GuaranteeCPUs, r.CPULimit-r.GuaranteeCPUs)
		}
	}
	for i, r := range runners {
		if !cpuManaged(r) || !r.Sampled || limits[i] != r.CPULimit || !cooled(r.LastCPUChange) || r.CPUUsage < p.CPUBusy*r.CPULimit {
			continue
		}
		ceiling := floorCPU(r.GuaranteeCPUs * p.MaxMultiple)
		target := floorCPU(min(r.CPULimit+max(p.MinCPUStep, r.GuaranteeCPUs/2), ceiling, r.CPULimit+free))
		if target <= r.CPULimit || target-r.CPULimit < p.MinCPUStep-1e-9 && target < ceiling {
			continue
		}
		free -= target - r.CPULimit
		limits[i] = target
		change(r, "cpu", r.CPULimit, target, false, "busy at %.2f of %.2f CPUs; borrowing %.2f unused host CPUs",
			r.CPUUsage, r.CPULimit, target-r.CPULimit)
	}

	memSurplus, extra := elasticMemoryBudget(h, p)
	plan.SurplusMemoryMiB, plan.ExtraMemoryMiB = memSurplus, extra
	memFree := max(0, memSurplus) - extra
	plan.MemoryOvercommited = memFree < 0
	pressure := func(r elasticRunner) float64 { return r.MemoryUsageMiB / float64(r.MemoryLimitMiB) }
	order := make([]elasticRunner, 0, len(runners))
	for _, r := range runners {
		if memoryManaged(r) && r.Sampled && cooled(r.LastMemoryChange) && pressure(r) >= p.MemoryTrigger {
			order = append(order, r)
		}
	}
	// The runner closest to its limit gets memory first.
	slices.SortStableFunc(order, func(a, b elasticRunner) int { return cmp.Compare(pressure(b), pressure(a)) })
	for _, r := range order {
		ceiling := int64(float64(r.GuaranteeMemoryMiB) * p.MaxMultiple)
		target := min(r.MemoryLimitMiB+max(p.MinMemoryStepMiB, r.GuaranteeMemoryMiB/4), ceiling, r.MemoryLimitMiB+max(0, memFree))
		if target <= r.MemoryLimitMiB || target-r.MemoryLimitMiB < p.MinMemoryStepMiB && target < ceiling {
			continue
		}
		memFree -= target - r.MemoryLimitMiB
		change(r, "memory", float64(r.MemoryLimitMiB), float64(target), false,
			"memory at %.0f%% of %d MiB; adding %d MiB no other runner was promised",
			100*pressure(r), r.MemoryLimitMiB, target-r.MemoryLimitMiB)
	}
	return plan
}

type elasticDecision struct {
	At time.Time `json:"at"`
	elasticChange
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
}

type elasticRunnerView struct {
	Host           string    `json:"host"`
	Runner         string    `json:"runner"`
	CPUs           float64   `json:"cpus"`
	GuaranteeCPUs  float64   `json:"guarantee_cpus"`
	MemoryMiB      int64     `json:"memory_mib"`
	GuaranteeMiB   int64     `json:"guarantee_memory_mib"`
	CPUUsage       float64   `json:"cpu_usage"`
	MemoryUsageMiB float64   `json:"memory_usage_mib"`
	lastCPU        time.Time // last CPU change, observed or applied
	lastMemory     time.Time
}

// elasticState is what the loop has seen and decided, shared with the panel and metrics.
type elasticState struct {
	mu        sync.Mutex
	mode      elasticMode
	interval  time.Duration
	hosts     map[string]elasticHostPlan
	runners   map[string]elasticRunnerView // by container ID
	decisions []elasticDecision
	next      int
	changes   map[bool]uint64 // by applied
	failures  uint64
}

func newElasticState(cfg elasticConfig) *elasticState {
	return &elasticState{mode: cfg.Mode, interval: cfg.Interval, hosts: make(map[string]elasticHostPlan),
		runners: make(map[string]elasticRunnerView), changes: make(map[bool]uint64)}
}

func (s *elasticState) record(d elasticDecision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.decisions) < elasticDecisionHistory {
		s.decisions = append(s.decisions, d)
	} else {
		s.decisions[s.next] = d
	}
	s.next = (s.next + 1) % elasticDecisionHistory
	if d.Error != "" {
		s.failures++
	} else {
		s.changes[d.Applied]++
	}
	// Failed changes also start the cooldown so a rejected update is not retried every tick.
	if view, ok := s.runners[d.ID]; ok {
		if d.Resource == "cpu" {
			view.lastCPU = d.At
			if d.Applied {
				view.CPUs = d.To
			}
		} else {
			view.lastMemory = d.At
			if d.Applied {
				view.MemoryMiB = int64(d.To)
			}
		}
		s.runners[d.ID] = view
	}
}

// recent returns decisions newest first.
func (s *elasticState) recent() []elasticDecision {
	n := len(s.decisions)
	out := make([]elasticDecision, 0, n)
	for i := range n {
		out = append(out, s.decisions[(s.next-1-i+2*n)%n])
	}
	return out
}

// observe replaces one host's view; runners that left the host are forgotten.
func (s *elasticState) observe(host elasticHost, plan elasticHostPlan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[host.Name] = plan
	seen := make(map[string]bool, len(host.Runners))
	for _, r := range host.Runners {
		seen[r.ID] = true
		previous := s.runners[r.ID]
		s.runners[r.ID] = elasticRunnerView{Host: host.Name, Runner: r.Name, CPUs: r.CPULimit, GuaranteeCPUs: r.GuaranteeCPUs,
			MemoryMiB: r.MemoryLimitMiB, GuaranteeMiB: r.GuaranteeMemoryMiB, CPUUsage: r.CPUUsage, MemoryUsageMiB: r.MemoryUsageMiB,
			lastCPU: previous.lastCPU, lastMemory: previous.lastMemory}
	}
	for id, view := range s.runners {
		if view.Host == host.Name && !seen[id] {
			delete(s.runners, id)
		}
	}
}

func (s *elasticState) forgetHost(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hosts, name)
	for id, view := range s.runners {
		if view.Host == name {
			delete(s.runners, id)
		}
	}
}

func (s *elasticState) lastChanges(id string) (cpu, memory time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := s.runners[id]
	return view.lastCPU, view.lastMemory
}

type elasticReport struct {
	Mode            elasticMode         `json:"mode"`
	IntervalSeconds float64             `json:"interval_seconds"`
	Hosts           []elasticHostPlan   `json:"hosts"`
	Runners         []elasticRunnerView `json:"runners"`
	Decisions       []elasticDecision   `json:"decisions"`
}

func (s *elasticState) report() elasticReport {
	if s == nil {
		return elasticReport{Mode: elasticOff, Hosts: []elasticHostPlan{}, Runners: []elasticRunnerView{}, Decisions: []elasticDecision{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	report := elasticReport{Mode: s.mode, IntervalSeconds: s.interval.Seconds(), Hosts: make([]elasticHostPlan, 0, len(s.hosts)),
		Runners: make([]elasticRunnerView, 0, len(s.runners)), Decisions: s.recent()}
	for _, host := range s.hosts {
		report.Hosts = append(report.Hosts, host)
	}
	for _, runner := range s.runners {
		report.Runners = append(report.Runners, runner)
	}
	slices.SortFunc(report.Hosts, func(a, b elasticHostPlan) int { return cmp.Compare(a.Host, b.Host) })
	slices.SortFunc(report.Runners, func(a, b elasticRunnerView) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Runner, b.Runner))
	})
	return report
}

// registerElasticRoutes exposes the elastic loop's state read-only; a nil state reports mode off.
func registerElasticRoutes(mux *http.ServeMux, a *panelAccess, state *elasticState) {
	mux.HandleFunc("GET /api/elastic", a.require("fleet:read", func(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
		w.Header().Set("Cache-Control", "no-store")
		panelJSON(w, http.StatusOK, state.report())
	}))
}

// writePrometheus appends per-host elastic gauges; runner names stay out of labels to bound cardinality.
func (s *elasticState) writePrometheus(w io.Writer) {
	if s == nil {
		return
	}
	report := s.report()
	s.mu.Lock()
	applied, observed, failures := s.changes[true], s.changes[false], s.failures
	s.mu.Unlock()
	fmt.Fprintf(w, "# HELP runquill_elastic_changes_total Elastic limit decisions by whether they were applied.\n# TYPE runquill_elastic_changes_total counter\n")
	fmt.Fprintf(w, "runquill_elastic_changes_total{applied=\"true\"} %d\nrunquill_elastic_changes_total{applied=\"false\"} %d\n", applied, observed)
	fmt.Fprintf(w, "# HELP runquill_elastic_update_errors_total Elastic limit changes Docker rejected.\n# TYPE runquill_elastic_update_errors_total counter\nrunquill_elastic_update_errors_total %d\n", failures)
	gauges := []struct {
		name, help string
		value      func(elasticHostPlan) float64
	}{
		{"elastic_surplus_cpus", "CPUs beyond guarantees, held slots and the host reserve.", func(h elasticHostPlan) float64 { return h.SurplusCPUs }},
		{"elastic_lent_cpus", "CPUs lent to runners above their guarantee.", func(h elasticHostPlan) float64 { return h.LentCPUs }},
		{"elastic_surplus_memory_bytes", "Memory beyond guarantees, held slots and the host reserve.", func(h elasticHostPlan) float64 { return float64(h.SurplusMemoryMiB << 20) }},
		{"elastic_extra_memory_bytes", "Memory granted to runners above their guarantee.", func(h elasticHostPlan) float64 { return float64(h.ExtraMemoryMiB << 20) }},
		{"elastic_held_slots", "Runner slots whose guarantees are held for queued jobs.", func(h elasticHostPlan) float64 { return float64(h.HeldSlots) }},
	}
	for _, g := range gauges {
		fmt.Fprintf(w, "# HELP runquill_%s %s\n# TYPE runquill_%s gauge\n", g.name, g.help, g.name)
		for _, host := range report.Hosts {
			fmt.Fprintf(w, "runquill_%s{host=\"%s\"} %s\n", g.name, prometheusLabel(host.Host), strconv.FormatFloat(g.value(host), 'f', -1, 64))
		}
	}
}

func prometheusLabel(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value)
}
