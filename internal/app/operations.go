package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
		state, err = ops.resolve(policy)
		if err != nil {
			return nil, fmt.Errorf("load saved operation settings: %w", err)
		}
	}
	state.pollEdited = make(chan struct{})
	ops.current.Store(state)
	return ops, nil
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
