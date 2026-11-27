package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/yakisoba0728/Runquill/internal/route"
)

const (
	defaultWorkerPool     = route.DefaultPool
	maxPerRepositoryLimit = 10_000
)

type workerPool struct {
	Hosts        []string `json:"hosts"`
	Repositories []string `json:"repositories"`
	CPUs         float64  `json:"cpus"`
	MemoryMiB    int64    `json:"memory_mib"`
	Image        string   `json:"image"`
	Privileged   *bool    `json:"privileged,omitempty"` // nil keeps the Docker-in-Docker default
	Cache        bool     `json:"cache,omitempty"`
	// CacheWrites is "all" or "default_branch"; empty means "all", or "default_branch" for public repositories.
	CacheWrites string `json:"cache_writes,omitempty"`
	// Runtime is the OCI runtime name passed to --runtime; empty keeps the engine's default.
	Runtime string `json:"runtime,omitempty"`
	// MaxPerRepository caps one repository's runners in this pool; 0 uses RUNQUILL_MAX_PER_REPOSITORY.
	MaxPerRepository int `json:"max_per_repository,omitempty"`
	// Images limits the runner image catalogue names jobs in this pool may select; nil allows all.
	Images []string `json:"images,omitempty"`
	// Warm keeps this many started runners per local or SSH host waiting for a job's JIT config.
	Warm int `json:"warm,omitempty"`
	// WarmMaxIdleMinutes replaces a warm runner no job took; 0 uses defaultWarmMaxIdle.
	WarmMaxIdleMinutes int `json:"warm_max_idle_minutes,omitempty"`
}

type workerPools map[string]workerPool

type invalidWarmPoolError struct{ error }

func (f *dockerFleet) configurePools(raw string) (workerPools, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var pools workerPools
	if err := decoder.Decode(&pools); err != nil {
		return nil, fmt.Errorf("RUNQUILL_WORKER_POOLS: %w", err)
	}
	if len(pools) == 0 {
		return nil, errors.New("RUNQUILL_WORKER_POOLS must define at least one pool")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("RUNQUILL_WORKER_POOLS must contain exactly one JSON object")
	}

	poolByHost := make(map[string]string)
	for name, pool := range pools {
		if !route.ValidPoolName(name) || name != defaultWorkerPool && len(pool.Hosts) == 0 {
			return nil, fmt.Errorf("invalid worker pool %q: named pools require at least one Docker host", name)
		}
		if pool.CPUs < 0 || pool.CPUs > 1_000_000 || pool.CPUs > 0 && pool.CPUs < 0.01 {
			return nil, fmt.Errorf("worker pool %q: cpus must be zero or between 0.01 and 1000000", name)
		}
		if pool.MemoryMiB < 0 || pool.MemoryMiB > (1<<63-1)>>20 || pool.MemoryMiB > 0 && pool.MemoryMiB < 6 {
			return nil, fmt.Errorf("worker pool %q: memory_mib must be zero or at least 6 MiB, within a 64-bit byte count", name)
		}
		if pool.MaxPerRepository < 0 || pool.MaxPerRepository > maxPerRepositoryLimit {
			return nil, fmt.Errorf("worker pool %q: max_per_repository must be between 0 and %d", name, maxPerRepositoryLimit)
		}
		if strings.HasPrefix(pool.Image, "-") || strings.ContainsAny(pool.Image, " \t\r\n") {
			return nil, fmt.Errorf("worker pool %q: image must be a Docker image reference", name)
		}
		if pool.CacheWrites != "" && pool.CacheWrites != cacheWritesAll && pool.CacheWrites != cacheWritesDefaultBranch {
			return nil, fmt.Errorf("worker pool %q: cache_writes must be %q or %q", name, cacheWritesAll, cacheWritesDefaultBranch)
		}
		if pool.Runtime != "" && !validRuntimeName(pool.Runtime) {
			return nil, fmt.Errorf("worker pool %q: runtime must be an OCI runtime name matching %s", name, runtimeNamePattern)
		}
		if err := checkPoolImageNames(pool.Images); err != nil {
			return nil, fmt.Errorf("worker pool %q: %w", name, err)
		}
		for i, nameInConfig := range pool.Repositories {
			owner, _, _ := strings.Cut(nameInConfig, "/")
			repo, valid := ownedRepositoryPath(nameInConfig, owner)
			if !valid {
				return nil, fmt.Errorf("worker pool %q: invalid repository %q", name, nameInConfig)
			}
			pool.Repositories[i] = repo
		}
		for _, host := range pool.Hosts {
			if _, err := f.host(host); err != nil {
				return nil, fmt.Errorf("worker pool %q: %w", name, err)
			}
			if existing, present := poolByHost[host]; present {
				return nil, fmt.Errorf("Docker host %q appears more than once in worker pools (%s, %s)", host, existing, name)
			}
			poolByHost[host] = name
		}
	}
	if _, ok := pools[defaultWorkerPool]; !ok {
		pools[defaultWorkerPool] = workerPool{}
	}
	for name, pool := range pools {
		if err := checkWarmPool(pool); err != nil {
			return nil, invalidWarmPoolError{fmt.Errorf("worker pool %q: %w", name, err)}
		}
	}
	for i := range f.hosts {
		host := &f.hosts[i]
		host.pool = poolByHost[host.label()]
		if host.pool == "" {
			host.pool = defaultWorkerPool
		}
		pool := pools[host.pool]
		host.cpus, host.memoryMiB = pool.CPUs, pool.MemoryMiB
		host.unprivileged, host.cache, host.runtime = pool.Privileged != nil && !*pool.Privileged, pool.Cache, pool.Runtime
		if pool.Image != "" {
			host.image = pool.Image
		}
		if pool.Warm > 0 && host.agentName() != "" {
			return nil, invalidWarmPoolError{fmt.Errorf("worker pool %q: warm runners are not supported on outbound worker %s", host.pool, host.label())}
		}
		host.warm, host.warmMaxIdle = pool.Warm, warmMaxIdle(pool)
	}
	return pools, nil
}

func (p workerPools) forJob(j job) (string, error) {
	if p == nil {
		return "", nil
	}
	name := defaultWorkerPool
	parts := strings.SplitN(j.route, "-", 4)
	if len(parts) == 4 {
		if requested, ok := strings.CutPrefix(parts[3], "pool__"); ok {
			selected, suffix, found := strings.Cut(requested, "__")
			if !found || suffix == "" || !route.ValidPoolName(selected) {
				return "", errors.New("invalid worker pool label suffix (use pool__POOL__JOB)")
			}
			name = selected
		}
	}
	pool, found := p[name]
	if !found {
		return name, fmt.Errorf("worker pool %q is not configured", name)
	}
	if !pool.allows(j.repository) {
		return name, fmt.Errorf("repository %s is not allowed to use worker pool %q", j.repository, name)
	}
	return name, nil
}

const (
	cacheWritesAll           = "all"
	cacheWritesDefaultBranch = "default_branch"
)

// cacheWritesRestricted reports whether only default-branch runs of repo may write the pool's cache.
// Public repositories default to it because anyone can trigger some of their workflows.
func (p workerPool) cacheWritesRestricted(repo repository) bool {
	return p.Cache && (p.CacheWrites == cacheWritesDefaultBranch || p.CacheWrites == "" && !repo.Private)
}

func (p workerPool) allows(repo string) bool {
	return p.Repositories == nil || slices.Contains(p.Repositories, repo)
}
