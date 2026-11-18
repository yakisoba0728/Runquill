package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var sshName = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)

type dockerFleet struct {
	hosts    []docker
	runners  map[string]int
	states   map[string]string
	occupied []int
}

func newDockerFleet(image, hosts string) (*dockerFleet, error) {
	fleet := &dockerFleet{}
	seen := make(map[string]bool)
	for _, host := range strings.Split(hosts, ",") {
		host = strings.TrimSpace(host)
		if seen[host] {
			return nil, fmt.Errorf("duplicate Docker host %q", host)
		}
		seen[host] = true
		if host == "local" {
			fleet.hosts = append(fleet.hosts, docker{image: image})
			continue
		}
		endpoint, err := url.Parse(host)
		if err != nil || endpoint.Scheme != "ssh" ||
			(!sshName.MatchString(endpoint.Hostname()) && net.ParseIP(endpoint.Hostname()) == nil) ||
			endpoint.Path != "" || strings.ContainsAny(host, "?#") {
			return nil, errors.New("RUNQUILL_DOCKER_HOSTS must be a comma-separated list of local or ssh://[user@]host[:port]")
		}
		if endpoint.User != nil {
			if _, hasPassword := endpoint.User.Password(); hasPassword || !sshName.MatchString(endpoint.User.Username()) {
				return nil, errors.New("Docker SSH hosts require an SSH user and key authentication")
			}
		}
		fleet.hosts = append(fleet.hosts, docker{image: image, host: host})
	}
	return fleet, nil
}

func (f *dockerFleet) prepare(ctx context.Context) error {
	engines := make(map[string]bool, len(f.hosts))
	for _, host := range f.hosts {
		if len(f.hosts) > 1 {
			id, err := host.command(ctx, nil, "info", "--format", "{{.ID}}")
			if err != nil {
				return err
			}
			id = strings.TrimSpace(id)
			if id == "" || engines[id] {
				return errors.New("each configured Docker host must identify a distinct engine")
			}
			engines[id] = true
		}
		if _, err := host.command(ctx, nil, "image", "inspect", host.image, "--format", "{{.Id}}"); err != nil {
			return fmt.Errorf("runner image %s is required on each Docker host: %w", host.image, err)
		}
	}
	return nil
}

func (f *dockerFleet) containers(ctx context.Context) (map[string]string, error) {
	runners := make(map[string]int)
	states := make(map[string]string)
	occupied := make([]int, len(f.hosts))
	for index, host := range f.hosts {
		containers, err := host.containers(ctx)
		if err != nil {
			// Keep leases until all engines can be checked for their containers.
			return nil, err
		}
		for name, state := range containers {
			if _, found := runners[name]; found {
				return nil, fmt.Errorf("runner %q exists on multiple Docker hosts", name)
			}
			runners[name], states[name] = index, state
			if runnerOccupiesSlot(state) {
				occupied[index]++
			}
		}
	}
	// The scheduler mutates its copy as it reconciles containers.
	f.runners, f.states, f.occupied = runners, states, occupied
	result := make(map[string]string, len(states))
	for name, state := range states {
		result[name] = state
	}
	return result, nil
}

func (f *dockerFleet) start(ctx context.Context, name, jit string) error {
	index := 0
	for i, count := range f.occupied {
		if count < f.occupied[index] {
			index = i
		}
	}
	host := f.hosts[index]
	// Charge the attempt even if Docker fails after creating a container.
	f.occupied[index]++
	if err := host.start(ctx, name, jit); err != nil {
		return err
	}
	if len(f.hosts) > 1 {
		slog.Info("runner placed", "runner", name, "docker_host", host.label())
	}
	return nil
}

func (f *dockerFleet) remove(ctx context.Context, name string) error {
	index, ok := f.runners[name]
	if !ok {
		return fmt.Errorf("runner %q is not in the Docker snapshot", name)
	}
	if err := f.hosts[index].remove(ctx, name); err != nil {
		return err
	}
	if runnerOccupiesSlot(f.states[name]) {
		f.occupied[index]--
	}
	delete(f.runners, name)
	delete(f.states, name)
	return nil
}

func (d docker) label() string {
	if d.host == "" {
		return "local"
	}
	return d.host
}
