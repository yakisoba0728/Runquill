package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type operationPolicy struct {
	Revision     int64          `json:"revision"`
	Limit        int            `json:"slot_limit"`
	PollInterval string         `json:"poll_interval"`
	Image        string         `json:"runner_image"`
	Hosts        []string       `json:"docker_hosts"`
	HostLimits   map[string]int `json:"host_limits"`
	Pools        workerPools    `json:"worker_pools"`
}

type operationState struct {
	policy     operationPolicy
	runners    runnerConfiguration
	interval   time.Duration
	pollEdited chan struct{}
	// adjusted lists how loading fitted saved settings to this deployment.
	adjusted []string
}

type operations struct {
	current  atomic.Pointer[operationState]
	updates  sync.Mutex
	topology sync.RWMutex
	base     runnerConfiguration
	defaults operationPolicy
	webhook  bool
}

func loadOperations(ctx context.Context, db *sql.DB, cfg config, base runnerConfiguration) (*operations, error) {
	defaults := operationPolicy{Limit: cfg.limit, PollInterval: cfg.pollInterval.String(), Image: cfg.runnerImage,
		HostLimits: base.hostLimits, Pools: base.pools}
	if fleet, ok := base.runtime.(*dockerFleet); ok {
		for _, host := range fleet.hosts {
			defaults.Hosts = append(defaults.Hosts, host.label())
		}
	}
	ops := &operations{base: base, defaults: defaults, webhook: cfg.secret != ""}
	policy, set, err := readOperationPolicy(ctx, db, defaults)
	if err != nil {
		return nil, err
	}
	state := &operationState{policy: defaults, runners: base, interval: cfg.pollInterval}
	if set {
		// A changed environment or a restored backup must not stop the manager. The saved
		// row stays as it is: read-only commands load it too, and the panel saves a fix.
		policy, adjusted := ops.reconcileSaved(policy)
		if next, err := ops.resolve(policy); err == nil {
			state = next
		} else {
			adjusted = []string{"ignored all saved settings and used the deployment settings: " + err.Error()}
			state.policy.Revision = policy.Revision
		}
		state.adjusted = adjusted
		for _, change := range adjusted {
			slog.Warn("saved operation settings do not match this deployment; save Settings in the panel to update them", "change", change)
		}
	}
	state.pollEdited = make(chan struct{})
	ops.current.Store(state)
	return ops, nil
}

// reconcileSaved fits saved settings to the deployment's Docker hosts. Outbound
// agent hosts always follow RUNQUILL_DOCKER_HOSTS, and limits or pools that name a
// host the fleet no longer has lose that reference. It returns what it changed.
func (ops *operations) reconcileSaved(policy operationPolicy) (operationPolicy, []string) {
	base, ok := ops.base.runtime.(*dockerFleet)
	if !ok {
		return policy, nil
	}
	var changes []string
	agents := make(map[string]string) // agent host -> deployment pool
	for _, host := range base.hosts {
		if host.agentName() != "" {
			agents[host.label()] = host.pool
		}
	}
	hosts := make([]string, 0, len(policy.Hosts)+len(agents))
	for _, host := range policy.Hosts {
		host = strings.TrimSpace(host)
		if _, deployed := agents[host]; strings.HasPrefix(host, workerHostPrefix) && !deployed {
			changes = append(changes, "removed "+host+" from docker_hosts: it is not in RUNQUILL_DOCKER_HOSTS")
			continue
		}
		hosts = append(hosts, host)
	}
	var added []string
	for _, host := range base.hosts {
		if label := host.label(); host.agentName() != "" && !slices.Contains(hosts, label) {
			hosts, added = append(hosts, label), append(added, label)
			changes = append(changes, "added "+label+" to docker_hosts from RUNQUILL_DOCKER_HOSTS")
		}
	}
	policy.Hosts = hosts
	for _, name := range slices.Sorted(maps.Keys(policy.HostLimits)) {
		if !slices.Contains(hosts, name) {
			delete(policy.HostLimits, name)
			changes = append(changes, "removed host_limits["+name+"]: the worker is not configured")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(policy.Pools)) {
		pool := policy.Pools[name]
		pool.Hosts = slices.DeleteFunc(pool.Hosts, func(host string) bool {
			if slices.Contains(hosts, host) {
				return false
			}
			changes = append(changes, "removed "+host+" from worker_pools["+name+"]: the worker is not configured")
			return true
		})
		if len(pool.Hosts) == 0 && name != defaultWorkerPool {
			// Named pools need a worker; their jobs wait until an administrator fixes the pool.
			delete(policy.Pools, name)
			changes = append(changes, "removed worker_pools["+name+"]: none of its workers are configured")
			continue
		}
		policy.Pools[name] = pool
	}
	// An agent host new to the saved settings keeps the limit and pool the deployment gives it,
	// so a saved pool list cannot widen which repositories reach it.
	for _, host := range added {
		if limit, ok := ops.base.hostLimits[host]; ok {
			if _, set := policy.HostLimits[host]; !set {
				if policy.HostLimits == nil {
					policy.HostLimits = make(map[string]int)
				}
				policy.HostLimits[host] = limit
			}
		}
		name := agents[host]
		if name == "" || name == defaultWorkerPool || slices.ContainsFunc(slices.Collect(maps.Values(policy.Pools)),
			func(pool workerPool) bool { return slices.Contains(pool.Hosts, host) }) {
			continue
		}
		pool, ok := policy.Pools[name]
		if !ok {
			pool = ops.base.pools[name]
			pool.Hosts, pool.Repositories, pool.Images = nil, slices.Clone(pool.Repositories), slices.Clone(pool.Images)
		}
		pool.Hosts = append(pool.Hosts, host)
		if policy.Pools == nil {
			policy.Pools = make(workerPools)
		}
		policy.Pools[name] = pool
	}
	return policy, changes
}

func readOperationPolicy(ctx context.Context, db *sql.DB, defaults operationPolicy) (operationPolicy, bool, error) {
	var policy operationPolicy
	var hasTable bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE type='table' AND name='operation_settings')").Scan(&hasTable); err != nil || !hasTable {
		return policy, false, err
	}
	var raw string
	var revision int64
	err := db.QueryRowContext(ctx, "SELECT revision, policy FROM operation_settings WHERE id=1").Scan(&revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, false, nil
	}
	if err != nil {
		return policy, false, err
	}
	fields, err := policyFields(defaults)
	if err != nil {
		return policy, false, err
	}
	// Decode into a separate map so a stored map or null replaces the default.
	var overrides map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil || overrides == nil {
		return policy, false, errors.New("saved operation settings are invalid")
	}
	maps.Copy(fields, overrides)
	effective, err := json.Marshal(fields)
	if err != nil {
		return policy, false, err
	}
	if err := json.Unmarshal(effective, &policy); err != nil {
		return policy, false, errors.New("saved operation settings are invalid")
	}
	policy.Revision = revision
	return policy, true, nil
}

func policyFields(policy operationPolicy) (map[string]json.RawMessage, error) {
	// Empty maps and nil maps have the same meaning at the policy boundary.
	if len(policy.HostLimits) == 0 {
		policy.HostLimits = nil
	}
	if len(policy.Pools) == 0 {
		policy.Pools = nil
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	err = json.Unmarshal(data, &fields)
	delete(fields, "revision")
	return fields, err
}

func (ops *operations) encodeOverrides(policy operationPolicy) ([]byte, error) {
	defaults, err := policyFields(ops.defaults)
	if err != nil {
		return nil, err
	}
	fields, err := policyFields(policy)
	if err != nil {
		return nil, err
	}
	maps.DeleteFunc(fields, func(name string, value json.RawMessage) bool {
		return string(value) == string(defaults[name])
	})
	return json.Marshal(fields)
}

func (ops *operations) resolve(policy operationPolicy) (*operationState, error) {
	if policy.Limit < 1 || !validWorkerImage(policy.Image) {
		return nil, errors.New("Enter a positive slot limit and a valid runner image.")
	}
	interval, err := time.ParseDuration(policy.PollInterval)
	if err != nil || interval < 0 {
		return nil, errors.New("Enter a polling duration such as 30s, or 0 to disable polling.")
	}
	if interval == 0 && !ops.webhook {
		return nil, errors.New("Enable GitHub webhooks before disabling polling.")
	}
	policy.PollInterval = interval.String()
	runners := ops.base
	runners.images = runners.images.withDefault(policy.Image)
	switch base := ops.base.runtime.(type) {
	case *dockerFleet:
		fleet, err := parseDockerFleet(policy.Image, strings.Join(policy.Hosts, ","))
		if err != nil {
			return nil, err
		}
		if !slices.Equal(agentHosts(fleet), agentHosts(base)) {
			return nil, errors.New("Use the deployment credentials to enroll or remove outbound agent hosts.")
		}
		for name, limit := range policy.HostLimits {
			if _, err := fleet.host(name); err != nil || limit < 1 {
				return nil, errors.New("Set positive per-worker limits for configured workers only.")
			}
		}
		for _, pool := range policy.Pools {
			if pool.Image != "" && !validWorkerImage(pool.Image) {
				return nil, errors.New("Enter valid runner images for worker pools.")
			}
		}
		if len(policy.Pools) == 0 {
			policy.Pools = nil
		}
		rawPools := ""
		if policy.Pools != nil {
			raw, err := json.Marshal(policy.Pools)
			if err != nil {
				return nil, err
			}
			rawPools = string(raw)
		}
		pools, err := fleet.configurePools(rawPools)
		if err != nil {
			return nil, err
		}
		fleet.recordPrivilegeRequests(pools)
		policy.Hosts = make([]string, len(fleet.hosts))
		for i, host := range fleet.hosts {
			policy.Hosts[i] = host.label()
		}
		fleet.workers, fleet.images = base.workers, base.images
		runners.runtime, runners.hostCount, runners.hostLimits, runners.pools = fleet, len(fleet.hosts), maps.Clone(policy.HostLimits), pools
	case *kubeRunners:
		if len(policy.Hosts) != 0 || len(policy.HostLimits) != 0 || len(policy.Pools) != 0 {
			return nil, errors.New("Docker workers and pools are unavailable in Kubernetes mode.")
		}
		kube := *base
		kube.options.image = policy.Image
		runners.runtime = &kube
	default:
		return nil, errors.New("Runner backend is unavailable.")
	}
	return &operationState{policy: policy, runners: runners, interval: interval}, nil
}

func agentHosts(fleet *dockerFleet) []string {
	names := make([]string, 0)
	for _, host := range fleet.hosts {
		if name := host.agentName(); name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func (ops *operations) publish(old, next *operationState) {
	if old.interval == next.interval {
		next.pollEdited = old.pollEdited
		ops.current.Store(next)
		return
	}
	next.pollEdited = make(chan struct{})
	ops.current.Store(next)
	close(old.pollEdited)
}

// CLI tools use the same policy as the scheduler, including saved overrides.
func appliedRunnerSettings(ctx context.Context, db *sql.DB, image string, limit int) (*operationState, error) {
	base, err := configureRunnerRuntime(image)
	if err != nil {
		return nil, err
	}
	interval, _ := time.ParseDuration(env("RUNQUILL_POLL_INTERVAL", "30s"))
	cfg := config{limit: limit, pollInterval: interval, runnerImage: image, secret: env("RUNQUILL_WEBHOOK_SECRET", "")}
	ops, err := loadOperations(ctx, db, cfg, base)
	if err != nil {
		return nil, err
	}
	return ops.current.Load(), nil
}
