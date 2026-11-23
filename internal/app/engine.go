package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// containerEngine holds every way a container CLI differs from Docker's, so call sites stay engine-neutral.
type containerEngine struct {
	name string
	// remoteFlag precedes the subcommand to target an ssh:// engine.
	remoteFlag string
	logArgs    []string
	idFormat   string
	// mountLabelsVolume: Docker labels a new named volume from --mount; Podman needs "volume create" first.
	mountLabelsVolume bool
	// sshSocket is appended to ssh:// hosts without a path; Podman needs the remote socket path.
	sshSocket string
}

var containerEngines = map[string]*containerEngine{
	"docker": {name: "docker", remoteFlag: "--host", mountLabelsVolume: true, idFormat: "{{.ID}}",
		logArgs: []string{"--log-driver", "json-file", "--log-opt", "max-size=10m", "--log-opt", "max-file=2"}},
	// Podman's json-file is an alias of k8s-file, which has no max-file rotation option.
	"podman": {name: "podman", remoteFlag: "--url", sshSocket: "/run/podman/podman.sock",
		idFormat: "{{.Host.Hostname}}:{{.Store.GraphRoot}}",
		logArgs:  []string{"--log-driver", "k8s-file", "--log-opt", "max-size=10m"}},
}

var podmanSocketPath = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)

func selectContainerEngine(name string) (*containerEngine, error) {
	if engine := containerEngines[name]; engine != nil {
		return engine, nil
	}
	return nil, errors.New("RUNQUILL_CONTAINER_ENGINE must be docker or podman")
}

func configuredContainerEngine() (*containerEngine, error) {
	return selectContainerEngine(env("RUNQUILL_CONTAINER_ENGINE", "docker"))
}

// remoteArgs selects an ssh:// engine; parseDockerFleet has already validated the URL.
func (e *containerEngine) remoteArgs(host string) []string {
	if e.sshSocket != "" {
		if endpoint, err := url.Parse(host); err == nil && endpoint.Path == "" {
			host += e.sshSocket
		}
	}
	return []string{e.remoteFlag, host}
}

// validSSHPath allows a socket path only where the engine's remote URL carries one.
func (e *containerEngine) validSSHPath(path string) bool {
	return path == "" || e.sshSocket != "" && podmanSocketPath.MatchString(path) && !strings.Contains(path, "/..")
}

type engineInfo struct {
	Engine, Version, OS, Arch, Cgroup string
	Rootless                          bool
	// CPUs and MemoryBytes are the engine host's totals; 0 when the engine did not report them.
	CPUs        int
	MemoryBytes int64
}

// parseEngineInfo reads "info --format {{json .}}" output, whose shape differs per engine.
func parseEngineInfo(engine, raw string) (engineInfo, error) {
	info := engineInfo{Engine: engine}
	switch engine {
	case "docker":
		var host struct {
			ServerVersion, OSType, Architecture, CgroupVersion string
			SecurityOptions                                    []string
			NCPU                                               int
			MemTotal                                           int64
		}
		if err := json.Unmarshal([]byte(raw), &host); err != nil {
			return info, fmt.Errorf("read docker info: %w", err)
		}
		info.Version, info.OS, info.Arch, info.Cgroup = host.ServerVersion, host.OSType, host.Architecture, host.CgroupVersion
		info.CPUs, info.MemoryBytes = host.NCPU, host.MemTotal
		info.Rootless = slices.ContainsFunc(host.SecurityOptions, func(option string) bool {
			name, _, _ := strings.Cut(option, ",")
			return name == "name=rootless"
		})
	case "podman":
		var host struct {
			Host struct {
				Arch, OS      string
				CgroupVersion string `json:"cgroupVersion"`
				Security      struct{ Rootless bool }
				CPUs          int   `json:"cpus"`
				MemTotal      int64 `json:"memTotal"`
			}
			Version struct{ Version string }
		}
		if err := json.Unmarshal([]byte(raw), &host); err != nil {
			return info, fmt.Errorf("read podman info: %w", err)
		}
		info.Version, info.OS, info.Arch, info.Cgroup = host.Version.Version, host.Host.OS, host.Host.Arch, host.Host.CgroupVersion
		info.Rootless = host.Host.Security.Rootless
		info.CPUs, info.MemoryBytes = host.Host.CPUs, host.Host.MemTotal
	default:
		return info, fmt.Errorf("unknown container engine %q", engine)
	}
	if info.Version == "" {
		return info, fmt.Errorf("%s info did not report a server version", engine)
	}
	switch info.Arch {
	case "x86_64":
		info.Arch = "amd64"
	case "aarch64":
		info.Arch = "arm64"
	}
	info.Cgroup = strings.TrimPrefix(info.Cgroup, "v")
	return info, nil
}

// engineSecurity caches a successful probe; it is shared by copies of one configured host.
type engineSecurity struct {
	mu   sync.Mutex
	info *engineInfo
}

func (d docker) containerEngine() (*containerEngine, error) {
	if d.engine != nil {
		return d.engine, nil
	}
	return configuredContainerEngine()
}

func (d docker) probe(ctx context.Context) (engineInfo, error) {
	if d.security != nil {
		// The lock is not held across the engine call, so a slow host cannot stall snapshot timeouts.
		d.security.mu.Lock()
		cached := d.security.info
		d.security.mu.Unlock()
		if cached != nil {
			return *cached, nil
		}
	}
	engine, err := d.containerEngine()
	if err != nil {
		return engineInfo{}, err
	}
	raw, err := d.command(ctx, nil, "info", "--format", "{{json .}}")
	if err != nil {
		return engineInfo{}, err
	}
	info, err := parseEngineInfo(engine.name, raw)
	if err != nil {
		return engineInfo{}, fmt.Errorf("container engine on %q: %w", d.label(), err)
	}
	if d.security != nil {
		d.security.mu.Lock()
		d.security.info = &info
		d.security.mu.Unlock()
	}
	return info, nil
}

func (d docker) rootlessPrivilegedError(engine string) error {
	return fmt.Errorf(`worker pool %q sets "privileged": true, but %s host %q is rootless, where Docker-in-Docker is unreliable; remove the setting or set "privileged": false`,
		d.pool, engine, d.label())
}

// privilegeConflict reports an explicit privileged pool on a rootless engine; nil when unknown or compatible.
func (d docker) privilegeConflict(ctx context.Context) error {
	if !d.requirePrivileged || d.unprivileged {
		return nil
	}
	info, err := d.probe(ctx)
	if err != nil || !info.Rootless {
		return nil
	}
	return d.rootlessPrivilegedError(info.Engine)
}

// runnerMode reports whether a runner starts in plain mode: configured so, or defaulted on a rootless engine.
func (d docker) runnerMode(ctx context.Context) (bool, error) {
	if d.unprivileged {
		return true, nil
	}
	info, err := d.probe(ctx)
	if err != nil {
		// Keep the configured mode; create will report an engine that is actually unreachable.
		slog.Warn("could not detect a rootless container engine", "host", d.label(), "error", err)
		return false, nil
	}
	if info.Rootless && d.requirePrivileged {
		return false, d.rootlessPrivilegedError(info.Engine)
	}
	return info.Rootless, nil
}

// recordPrivilegeRequests marks hosts whose pool explicitly asks for privileged runners.
// configurePools keeps nil and true alike, but only an explicit request must fail on a rootless engine.
func (f *dockerFleet) recordPrivilegeRequests(pools workerPools) {
	for i := range f.hosts {
		privileged := pools[f.hosts[i].pool].Privileged
		f.hosts[i].requirePrivileged = privileged != nil && *privileged
	}
}
