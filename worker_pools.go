package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

const defaultWorkerPool = "default"

var workerPoolName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type workerPool struct {
	Hosts        []string `json:"hosts"`
	Repositories []string `json:"repositories"`
	CPUs         float64  `json:"cpus"`
	MemoryMiB    int64    `json:"memory_mib"`
}

type workerPools map[string]workerPool

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
		if !workerPoolName.MatchString(name) || name != defaultWorkerPool && len(pool.Hosts) == 0 {
			return nil, fmt.Errorf("invalid worker pool %q: named pools require at least one Docker host", name)
		}
		if pool.CPUs < 0 || pool.CPUs > 1_000_000 || pool.CPUs > 0 && pool.CPUs < 0.01 {
			return nil, fmt.Errorf("worker pool %q: cpus must be zero or between 0.01 and 1000000", name)
		}
		if pool.MemoryMiB < 0 || pool.MemoryMiB > (1<<63-1)>>20 || pool.MemoryMiB > 0 && pool.MemoryMiB < 6 {
			return nil, fmt.Errorf("worker pool %q: memory_mib must be zero or at least 6 MiB, within a 64-bit byte count", name)
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
	for i := range f.hosts {
		host := &f.hosts[i]
		host.pool = poolByHost[host.label()]
		if host.pool == "" {
			host.pool = defaultWorkerPool
		}
		pool := pools[host.pool]
		host.cpus, host.memoryMiB = pool.CPUs, pool.MemoryMiB
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
			if !found || suffix == "" || !workerPoolName.MatchString(selected) {
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

func (p workerPool) allows(repo string) bool {
	return p.Repositories == nil || slices.Contains(p.Repositories, repo)
}
