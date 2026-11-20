package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
	"time"
)

const elasticEpsilon = 1e-6

func testElasticPolicy() elasticPolicy {
	return elasticPolicy{ReserveCPUs: 0, ReserveMemoryMiB: 0, MemoryTrigger: 0.9, MaxMultiple: 2,
		CPUBusy: 0.9, CPUIdle: 0.5, MinCPUStep: 0.1, MinMemoryStepMiB: 64, Cooldown: 30 * time.Second}
}

// applyPlan returns the host as it would be after Docker accepted every change.
func applyPlan(h elasticHost, plan elasticHostPlan, now time.Time) elasticHost {
	h.Runners = append([]elasticRunner(nil), h.Runners...)
	for _, c := range plan.Changes {
		for i := range h.Runners {
			if h.Runners[i].ID != c.ID {
				continue
			}
			if c.Resource == "cpu" {
				h.Runners[i].CPULimit, h.Runners[i].LastCPUChange = c.To, now
			} else {
				h.Runners[i].MemoryLimitMiB, h.Runners[i].LastMemoryChange = int64(c.To), now
			}
		}
	}
	return h
}

func randomElasticHost(rng *rand.Rand) (elasticHost, elasticPolicy, time.Time) {
	now := time.Unix(1_800_000_000, 0)
	p := testElasticPolicy()
	p.ReserveCPUs = float64(rng.IntN(3)) / 2
	p.ReserveMemoryMiB = int64(rng.IntN(4)) * 256
	p.MaxMultiple = 1 + float64(rng.IntN(4))/2
	slotCPUs := []float64{0, 0.25, 0.5, 1, 2, 4}[rng.IntN(6)]
	slotMemory := []int64{0, 128, 512, 1024, 4096}[rng.IntN(5)]
	h := elasticHost{Name: "local", CPUs: float64(1 + rng.IntN(32)), MemoryMiB: int64(512 + rng.IntN(64*1024)),
		HeldSlots: rng.IntN(5), SlotCPUs: slotCPUs, SlotMemoryMiB: slotMemory}
	for i := range rng.IntN(7) {
		r := elasticRunner{ID: fmt.Sprintf("id%d", i), Name: fmt.Sprintf("runner-%d", rng.IntN(4)),
			GuaranteeCPUs: slotCPUs, GuaranteeMemoryMiB: slotMemory, Sampled: rng.IntN(5) > 0}
		switch rng.IntN(5) {
		case 0:
			r.CPULimit = 0 // started while the pool was unlimited
		case 1:
			r.CPULimit = slotCPUs / 2 // started before the pool grew
		default:
			r.CPULimit = floorCPU(slotCPUs * (1 + rng.Float64()*(p.MaxMultiple-1)))
		}
		switch rng.IntN(4) {
		case 0:
			r.MemoryLimitMiB = 0
		default:
			r.MemoryLimitMiB = slotMemory + int64(rng.Float64()*float64(slotMemory)*(p.MaxMultiple-1))
		}
		r.CPUUsage = rng.Float64() * max(r.CPULimit, 1)
		r.MemoryUsageMiB = rng.Float64() * float64(max(r.MemoryLimitMiB, 1))
		if rng.IntN(3) == 0 {
			r.CPUUsage = r.CPULimit
			r.MemoryUsageMiB = float64(r.MemoryLimitMiB)
		}
		if rng.IntN(2) == 0 {
			r.LastCPUChange = now.Add(-time.Duration(rng.IntN(60)) * time.Second)
			r.LastMemoryChange = now.Add(-time.Duration(rng.IntN(60)) * time.Second)
		}
		h.Runners = append(h.Runners, r)
	}
	return h, p, now
}

func TestElasticPlanInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	kinds := make(map[string]int)
	for iteration := range 20_000 {
		h, p, now := randomElasticHost(rng)
		plan := planElastic(h, p, now)
		after := applyPlan(h, plan, now)
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("iteration %d: %s\nhost %+v\nplan %+v", iteration, fmt.Sprintf(format, args...), h, plan.Changes)
		}
		byID := make(map[string]elasticRunner)
		for _, r := range h.Runners {
			byID[r.ID] = r
		}
		seen := make(map[string]bool)
		for _, c := range plan.Changes {
			r := byID[c.ID]
			key := c.ID + c.Resource
			if seen[key] {
				fail("two %s changes for %s", c.Resource, c.ID)
			}
			seen[key] = true
			switch {
			case c.Forced:
				kinds["reclaim"]++
			case c.Resource == "memory":
				kinds["memory"]++
			case c.To > c.From:
				kinds["borrow"]++
			default:
				kinds["idle"]++
			}
			if c.Reason == "" {
				fail("change without a reason")
			}
			switch c.Resource {
			case "cpu":
				if !cpuManaged(r) || c.To < r.GuaranteeCPUs-elasticEpsilon || c.To > r.GuaranteeCPUs*p.MaxMultiple+elasticEpsilon {
					fail("cpu %v -> %v outside guarantee %v", c.From, c.To, r.GuaranteeCPUs)
				}
				if !c.Forced && !r.LastCPUChange.IsZero() && now.Sub(r.LastCPUChange) < p.Cooldown {
					fail("cpu change inside the cooldown")
				}
				if !r.Sampled && !c.Forced {
					fail("unsampled runner changed")
				}
			case "memory":
				if !memoryManaged(r) || c.To <= c.From || c.To > float64(r.GuaranteeMemoryMiB)*p.MaxMultiple {
					fail("memory %v -> %v not a bounded raise", c.From, c.To)
				}
				if !r.LastMemoryChange.IsZero() && now.Sub(r.LastMemoryChange) < p.Cooldown {
					fail("memory change inside the cooldown")
				}
			default:
				fail("unknown resource %q", c.Resource)
			}
		}
		// Guarantees, held slots and the reserve stay satisfiable: lending only uses the surplus.
		surplus, lent := elasticCPUBudget(after, p)
		if lent > max(0, surplus)+elasticEpsilon {
			fail("cpu lent %v exceeds surplus %v", lent, surplus)
		}
		memSurplus, extraBefore := elasticMemoryBudget(h, p)
		_, extraAfter := elasticMemoryBudget(after, p)
		if extraAfter > extraBefore && extraAfter > max(0, memSurplus) {
			fail("memory extra %v exceeds surplus %v", extraAfter, memSurplus)
		}
	}
	for _, kind := range []string{"reclaim", "memory", "borrow", "idle"} {
		if kinds[kind] < 100 {
			t.Fatalf("random hosts exercised too few %s changes: %v", kind, kinds)
		}
	}
	t.Log(kinds)
}

func TestElasticLendsAndReturnsCPU(t *testing.T) {
	p := testElasticPolicy()
	now := time.Unix(1_800_000_000, 0)
	h := elasticHost{Name: "local", CPUs: 8, MemoryMiB: 8192, HeldSlots: 1, SlotCPUs: 2, SlotMemoryMiB: 1024, Runners: []elasticRunner{
		{ID: "a", Name: "busy", GuaranteeCPUs: 2, GuaranteeMemoryMiB: 1024, CPULimit: 2, MemoryLimitMiB: 1024, Sampled: true, CPUUsage: 1.99},
	}}
	plan := planElastic(h, p, now)
	if len(plan.Changes) != 1 || plan.Changes[0].To != 3 || !strings.Contains(plan.Changes[0].Reason, "busy at 1.99 of 2.00 CPUs") {
		t.Fatalf("busy runner should borrow one CPU: %+v", plan.Changes)
	}
	h = applyPlan(h, plan, now)
	h.Runners[0].CPUUsage = 0.5
	if plan := planElastic(h, p, now.Add(10*time.Second)); len(plan.Changes) != 0 {
		t.Fatalf("change inside the cooldown: %+v", plan.Changes)
	}
	plan = planElastic(h, p, now.Add(p.Cooldown))
	if len(plan.Changes) != 1 || plan.Changes[0].To != 2 || !strings.Contains(plan.Changes[0].Reason, "idle") {
		t.Fatalf("idle borrower should return its loan: %+v", plan.Changes)
	}
}

// The room held for queued jobs is never lent, so starting them keeps every guarantee, and a
// shrinking surplus takes CPU back at once.
func TestElasticHeldSlotsStayProtected(t *testing.T) {
	p := testElasticPolicy()
	now := time.Unix(1_800_000_000, 0)
	busy := elasticRunner{ID: "a", Name: "a", GuaranteeCPUs: 1, CPULimit: 1, Sampled: true, CPUUsage: 1}
	h := elasticHost{Name: "local", CPUs: 4, HeldSlots: 2, SlotCPUs: 1, Runners: []elasticRunner{busy}}
	for range 3 {
		h = applyPlan(h, planElastic(h, p, now), now)
		h.Runners[0].CPUUsage = h.Runners[0].CPULimit
		now = now.Add(p.Cooldown)
	}
	if got := h.Runners[0].CPULimit; got != 2 {
		t.Fatalf("borrower limit %v, want the 2x ceiling", got)
	}
	// A held slot is used: the new runner's guarantee was never lent.
	h.HeldSlots--
	h.Runners = append(h.Runners, elasticRunner{ID: "b", Name: "b", GuaranteeCPUs: 1, CPULimit: 1, Sampled: true, CPUUsage: 0.6})
	if plan := planElastic(h, p, now); len(plan.Changes) != 0 {
		t.Fatalf("using a held slot needs no change: %+v", plan.Changes)
	}
	if surplus, lent := elasticCPUBudget(h, p); lent > surplus {
		t.Fatalf("lent %v over surplus %v", lent, surplus)
	}
	// The slot limit grows, so one more guarantee is held; the loan comes back even inside the cooldown.
	h.HeldSlots++
	h.Runners[0].LastCPUChange = now
	plan := planElastic(h, p, now)
	if len(plan.Changes) != 1 || !plan.Changes[0].Forced || plan.Changes[0].To != 1 {
		t.Fatalf("shrunk surplus must reclaim the loan: %+v", plan.Changes)
	}
}

func TestElasticDampsChanges(t *testing.T) {
	p := testElasticPolicy()
	now := time.Unix(1_800_000_000, 0)
	// Only 0.05 CPUs are free: below the minimum step, nothing moves.
	h := elasticHost{Name: "local", CPUs: 2.05, SlotCPUs: 1, Runners: []elasticRunner{
		{ID: "a", Name: "a", GuaranteeCPUs: 1, CPULimit: 1, Sampled: true, CPUUsage: 1},
		{ID: "b", Name: "b", GuaranteeCPUs: 1, CPULimit: 1, Sampled: true, CPUUsage: 0.2},
	}}
	if plan := planElastic(h, p, now); len(plan.Changes) != 0 {
		t.Fatalf("sub-step change: %+v", plan.Changes)
	}
	// Between the idle and busy thresholds a borrower keeps its loan.
	h = elasticHost{Name: "local", CPUs: 8, SlotCPUs: 1, Runners: []elasticRunner{
		{ID: "a", Name: "a", GuaranteeCPUs: 1, CPULimit: 1.5, Sampled: true, CPUUsage: 0.8},
	}}
	if plan := planElastic(h, p, now); len(plan.Changes) != 0 {
		t.Fatalf("hysteresis band changed limits: %+v", plan.Changes)
	}
	// A steady demand settles without reversing direction.
	for _, demand := range []float64{0.3, 0.7, 1.2, 1.5, 1.9, 4} {
		h := elasticHost{Name: "local", CPUs: 16, SlotCPUs: 1, Runners: []elasticRunner{{ID: "a", Name: "a", GuaranteeCPUs: 1, CPULimit: 1, Sampled: true}}}
		direction, reversals, changes := 0.0, 0, 0
		at := now
		for range 40 {
			h.Runners[0].CPUUsage = min(demand, h.Runners[0].CPULimit)
			plan := planElastic(h, p, at)
			for _, c := range plan.Changes {
				changes++
				step := math.Copysign(1, c.To-c.From)
				if direction != 0 && step != direction {
					reversals++
				}
				direction = step
			}
			h = applyPlan(h, plan, at)
			at = at.Add(10 * time.Second)
		}
		if reversals != 0 || changes > 3 {
			t.Fatalf("demand %v: %d changes with %d reversals", demand, changes, reversals)
		}
	}
}

func TestElasticMemoryOnlyGrows(t *testing.T) {
	p := testElasticPolicy()
	now := time.Unix(1_800_000_000, 0)
	h := elasticHost{Name: "local", CPUs: 4, MemoryMiB: 4096, HeldSlots: 1, SlotMemoryMiB: 1024, Runners: []elasticRunner{
		{ID: "a", Name: "a", GuaranteeMemoryMiB: 1024, MemoryLimitMiB: 1024, Sampled: true, MemoryUsageMiB: 950},
		{ID: "b", Name: "b", GuaranteeMemoryMiB: 1024, MemoryLimitMiB: 1024, Sampled: true, MemoryUsageMiB: 1000},
	}}
	plan := planElastic(h, p, now)
	// 1 GiB is spare; the runner closest to its limit is served first, both in 256 MiB steps.
	if len(plan.Changes) != 2 || plan.Changes[0].Runner != "b" || plan.Changes[0].To != 1280 || plan.Changes[1].To != 1280 {
		t.Fatalf("memory raises: %+v", plan.Changes)
	}
	h = applyPlan(h, plan, now)
	h.Runners[0].MemoryUsageMiB, h.Runners[1].MemoryUsageMiB = 10, 10
	h.HeldSlots = 2 // the surplus is now gone
	if plan := planElastic(h, p, now.Add(time.Hour)); len(plan.Changes) != 0 || !plan.MemoryOvercommited {
		t.Fatalf("memory must never be lowered: %+v", plan)
	}
	h.Runners[0].MemoryUsageMiB = 1270
	if plan := planElastic(h, p, now.Add(time.Hour)); len(plan.Changes) != 0 {
		t.Fatalf("raise without surplus: %+v", plan.Changes)
	}
}

func TestElasticLeavesUnlimitedRunnersAlone(t *testing.T) {
	p := testElasticPolicy()
	h := elasticHost{Name: "local", CPUs: 64, MemoryMiB: 1 << 16, Runners: []elasticRunner{
		{ID: "a", Name: "a", Sampled: true, CPUUsage: 8, MemoryUsageMiB: 900},
		{ID: "b", Name: "b", GuaranteeCPUs: 2, GuaranteeMemoryMiB: 1024, Sampled: true, CPUUsage: 2, MemoryUsageMiB: 2000},
		{ID: "c", Name: "c", GuaranteeCPUs: 2, CPULimit: 1, Sampled: true, CPUUsage: 1},
	}}
	if plan := planElastic(h, p, time.Now()); len(plan.Changes) != 0 {
		t.Fatalf("unlimited or legacy runners changed: %+v", plan.Changes)
	}
}

func TestElasticConfig(t *testing.T) {
	env := func(values map[string]string) func(string) string { return func(k string) string { return values[k] } }
	cfg, err := loadElasticConfig(env(nil))
	if err != nil || cfg.Mode != elasticOff || cfg.Interval != 10*time.Second || cfg.Policy.Cooldown != 30*time.Second ||
		cfg.Policy.MaxMultiple != 2 || cfg.Policy.MemoryTrigger != 0.9 {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	cfg, err = loadElasticConfig(env(map[string]string{"RUNQUILL_ELASTIC": "apply", "RUNQUILL_ELASTIC_INTERVAL": "5s",
		"RUNQUILL_ELASTIC_HOST_RESERVE_CPUS": "0.5", "RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB": "2048",
		"RUNQUILL_ELASTIC_MEMORY_TRIGGER": "0.8", "RUNQUILL_ELASTIC_MAX_MULTIPLE": "3"}))
	if err != nil || cfg.Mode != elasticApply || cfg.Interval != 5*time.Second || cfg.Policy.Cooldown != 15*time.Second ||
		cfg.Policy.ReserveCPUs != 0.5 || cfg.Policy.ReserveMemoryMiB != 2048 || cfg.Policy.MemoryTrigger != 0.8 || cfg.Policy.MaxMultiple != 3 {
		t.Fatalf("explicit: %+v %v", cfg, err)
	}
	for name, value := range map[string]string{
		"RUNQUILL_ELASTIC": "on", "RUNQUILL_ELASTIC_INTERVAL": "100ms", "RUNQUILL_ELASTIC_COOLDOWN": "2s",
		"RUNQUILL_ELASTIC_HOST_RESERVE_CPUS": "-1", "RUNQUILL_ELASTIC_HOST_RESERVE_MEMORY_MIB": "1.5",
		"RUNQUILL_ELASTIC_MEMORY_TRIGGER": "1", "RUNQUILL_ELASTIC_MAX_MULTIPLE": "NaN",
	} {
		if _, err := loadElasticConfig(env(map[string]string{name: value})); err == nil {
			t.Errorf("%s=%s was accepted", name, value)
		}
	}
}

func TestElasticStateRingAndReport(t *testing.T) {
	state := newElasticState(elasticConfig{Mode: elasticObserve, Interval: 10 * time.Second})
	h := elasticHost{Name: "local", CPUs: 4, Runners: []elasticRunner{{ID: "id", Name: "r", GuaranteeCPUs: 1, CPULimit: 1}}}
	state.observe(h, elasticHostPlan{Host: "local", SurplusCPUs: 3})
	start := time.Unix(1_800_000_000, 0)
	for i := range elasticDecisionHistory + 5 {
		state.record(elasticDecision{At: start.Add(time.Duration(i) * time.Second), elasticChange: elasticChange{ID: "id", Resource: "cpu", To: float64(i)}})
	}
	report := state.report()
	if len(report.Decisions) != elasticDecisionHistory || report.Decisions[0].To != elasticDecisionHistory+4 || report.Decisions[elasticDecisionHistory-1].To != 5 {
		t.Fatalf("ring buffer order: first %v last %v", report.Decisions[0].To, report.Decisions[len(report.Decisions)-1].To)
	}
	if cpu, _ := state.lastChanges("id"); !cpu.Equal(start.Add((elasticDecisionHistory + 4) * time.Second)) {
		t.Fatalf("last cpu change %v", cpu)
	}
	if report.Runners[0].CPUs != 1 {
		t.Fatalf("observed decisions must not change the reported limit: %+v", report.Runners)
	}
	state.observe(elasticHost{Name: "local", CPUs: 4}, elasticHostPlan{Host: "local"})
	if report := state.report(); len(report.Runners) != 0 {
		t.Fatalf("departed runner still reported: %+v", report.Runners)
	}

	var out bytes.Buffer
	state.writePrometheus(&out)
	for _, want := range []string{"runquill_elastic_changes_total{applied=\"false\"} 205", "runquill_elastic_surplus_cpus{host=\"local\"} 0",
		"# TYPE runquill_elastic_lent_cpus gauge"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("metrics missing %q:\n%s", want, out.String())
		}
	}
	(*elasticState)(nil).writePrometheus(&out)
	if got := prometheusLabel("ssh://a\"b\\c\n"); got != `ssh://a\"b\\c\n` {
		t.Fatalf("label escape %q", got)
	}
}

func TestElasticRoute(t *testing.T) {
	f := newPanelFixture(t)
	state := newElasticState(elasticConfig{Mode: elasticApply, Interval: 10 * time.Second})
	state.observe(elasticHost{Name: "local", CPUs: 4}, elasticHostPlan{Host: "local", SurplusCPUs: 2.5})
	state.record(elasticDecision{Applied: true, elasticChange: elasticChange{Host: "local", ID: "secret-id", Runner: "r", Resource: "cpu", From: 1, To: 2, Reason: "busy"}})
	mux := http.NewServeMux()
	registerElasticRoutes(mux, f.access, state)
	f.handler = mux

	f.expect(f.call("GET", "/api/elastic", "", "", false), http.StatusUnauthorized)
	rec := f.call("GET", "/api/elastic", "", "viewer", false)
	f.expect(rec, http.StatusOK)
	var report elasticReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Mode != elasticApply || len(report.Hosts) != 1 || report.Hosts[0].SurplusCPUs != 2.5 ||
		len(report.Decisions) != 1 || !report.Decisions[0].Applied || strings.Contains(rec.Body.String(), "secret-id") {
		t.Fatalf("report: %s", rec.Body)
	}
	f.expect(f.call("POST", "/api/elastic", "", "admin", true), http.StatusMethodNotAllowed)

	mux = http.NewServeMux()
	registerElasticRoutes(mux, f.access, nil)
	f.handler = mux
	rec = f.call("GET", "/api/elastic", "", "viewer", false)
	f.expect(rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"mode":"off"`) || !strings.Contains(rec.Body.String(), `"decisions":[]`) {
		t.Fatalf("nil state: %s", rec.Body)
	}
}
