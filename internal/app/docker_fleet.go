package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yakisoba0728/Runquill/internal/route"
)

var sshName = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)

type dockerFleet struct {
	hosts   []docker
	workers *workerFleet
	// images is shared by every fleet built from saved settings; nil leaves images to the operator.
	images *imageReadiness
	// inspect checks on-demand images when Runquill does not pull them; nil runs the engine.
	inspect imageCommand
}

type dockerHostSnapshot struct {
	Name                string
	Pool                string
	Containers          map[string]string
	RunnerDiagnostics   map[string]jobDiagnostic
	Err                 error
	WorkerEngineID      string
	UnregisteredWorker  bool
	EnrollmentAvailable bool
	// The image fields are set while Runquill pulls images; ImageWait says why the host cannot start runners yet.
	Image, ImageStatus, ImageWait string
}

const dockerHostProbeTimeout = 5 * time.Second

func newDockerFleet(image, hosts string) (*dockerFleet, error) {
	fleet, err := parseDockerFleet(image, hosts)
	if err != nil {
		return nil, err
	}
	fleet.workers, err = loadWorkerFleet(fleet.hosts, os.Getenv("RUNQUILL_WORKER_AUTH_FILE"))
	if err != nil {
		return nil, err
	}
	if fleet.images, err = loadImagePull(os.Getenv); err != nil {
		return nil, err
	}
	return fleet, nil
}

func parseDockerFleet(image, hosts string) (*dockerFleet, error) {
	engine, err := configuredContainerEngine()
	if err != nil {
		return nil, err
	}
	mirror, err := registryMirror()
	if err != nil {
		return nil, err
	}
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
		if name, agent := strings.CutPrefix(host, workerHostPrefix); agent {
			if !route.ValidPoolName(name) {
				return nil, errors.New("Docker worker hosts require agent://NAME with 1–32 lowercase letters, digits or hyphens")
			}
			fleet.hosts = append(fleet.hosts, docker{image: image, host: host})
			continue
		}
		endpoint, err := url.Parse(host)
		if err != nil || endpoint.Scheme != "ssh" ||
			(!sshName.MatchString(endpoint.Hostname()) && net.ParseIP(endpoint.Hostname()) == nil) ||
			!engine.validSSHPath(endpoint.Path) || strings.ContainsAny(host, "?#") {
			if engine.sshSocket != "" {
				return nil, errors.New("RUNQUILL_DOCKER_HOSTS must be a comma-separated list of local, ssh://[user@]host[:port][/socket/path] or agent://name")
			}
			return nil, errors.New("RUNQUILL_DOCKER_HOSTS must be a comma-separated list of local, ssh://[user@]host[:port] or agent://name")
		}
		if endpoint.User != nil {
			if _, hasPassword := endpoint.User.Password(); hasPassword || !sshName.MatchString(endpoint.User.Username()) {
				return nil, errors.New("Docker SSH hosts require an SSH user and key authentication")
			}
		}
		fleet.hosts = append(fleet.hosts, docker{image: image, host: host})
	}
	for i := range fleet.hosts {
		fleet.hosts[i].engine, fleet.hosts[i].security, fleet.hosts[i].mirror = engine, &engineSecurity{}, mirror
	}
	return fleet, nil
}

// registryMirror reads the Docker Hub pull-through mirror that Docker-in-Docker runners pass to their own dockerd.
// A value dockerd rejects would stop every DinD runner, so only a reachable http(s) origin is accepted.
func registryMirror() (string, error) {
	raw := os.Getenv("RUNQUILL_REGISTRY_MIRROR")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(raw, "?# \t") ||
		strings.HasSuffix(u.Host, ":") || !validMirrorHost(u.Hostname()) {
		return "", errors.New("RUNQUILL_REGISTRY_MIRROR must be an http:// or https:// origin that runner containers can reach, such as http://10.0.0.5:5000")
	}
	return strings.TrimSuffix(raw, "/"), nil
}

// validMirrorHost refuses loopback names, which inside a runner container reach the runner itself.
func validMirrorHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback() && !ip.IsUnspecified()
	}
	return sshName.MatchString(host) && !strings.EqualFold(host, "localhost")
}

func (f *dockerFleet) prepare(ctx context.Context) error {
	engines := make(map[string]bool, len(f.hosts))
	for _, host := range f.hosts {
		if len(f.hosts) > 1 {
			var id string
			var err error
			if name := host.agentName(); name != "" {
				var authority *workerAuthority
				authority, err = f.workers.workerAuthority(ctx, name)
				if authority != nil {
					id = authority.Engine
				}
			} else {
				id, err = host.engineID(ctx)
			}
			if err != nil {
				return err
			}
			if id != "" && engines[id] {
				return errors.New("each configured Docker host must identify a distinct engine")
			}
			engines[id] = true
		}
		if host.agentName() != "" {
			continue
		}
		if err := host.checkRunnerImage(ctx); err != nil {
			if !f.images.enabled() || !imageMissing(err) {
				return err
			}
			slog.Info("runner image is missing; pulling it in the background", "host", host.label(), "image", logImage(host.image))
		}
		info, err := host.probe(ctx)
		if err != nil {
			return err
		}
		if info.Rootless && !host.unprivileged {
			if host.requirePrivileged {
				return host.rootlessPrivilegedError(info.Engine)
			}
			slog.Info("rootless container engine: runners start unprivileged without Docker-in-Docker", "host", host.label(), "engine", info.Engine)
		}
	}
	return nil
}

func (f *dockerFleet) limits(raw string) (map[string]int, error) {
	limits := make(map[string]int)
	if strings.TrimSpace(raw) == "" {
		return limits, nil
	}
	for item := range strings.SplitSeq(raw, ",") {
		name, value, ok := strings.Cut(item, "=")
		name = strings.TrimSpace(name)
		count, err := strconv.Atoi(strings.TrimSpace(value))
		if !ok || err != nil || count < 1 {
			return nil, errors.New("RUNQUILL_DOCKER_HOST_LIMITS must list host=positive_count pairs")
		}
		if _, err := f.host(name); err != nil {
			return nil, fmt.Errorf("RUNQUILL_DOCKER_HOST_LIMITS: %w", err)
		}
		if _, exists := limits[name]; exists {
			return nil, fmt.Errorf("duplicate runner limit for Docker host %q", name)
		}
		limits[name] = count
	}
	return limits, nil
}

func (f *dockerFleet) snapshot(ctx context.Context) ([]dockerHostSnapshot, error) {
	hosts := make([]dockerHostSnapshot, len(f.hosts))
	engineIDs := make([]string, len(f.hosts))
	var probes sync.WaitGroup
	for i, host := range f.hosts {
		probes.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, dockerHostProbeTimeout)
			defer cancel()
			result := &hosts[i]
			if host.agentName() != "" {
				*result = f.workers.snapshot(ctx, host)
				engineIDs[i] = result.WorkerEngineID
				return
			}
			result.Name, result.Pool = host.label(), host.pool
			if len(f.hosts) > 1 {
				engineIDs[i], result.Err = host.engineID(ctx)
				if result.Err != nil {
					return
				}
			}
			result.Containers, result.Err = host.containers(ctx)
			if result.Err == nil && host.image != "" {
				result.Err = host.checkRunnerImage(ctx)
				if f.images.enabled() && (result.Err == nil || imageMissing(result.Err)) {
					result.Image = host.image
					result.ImageStatus, result.ImageWait = f.images.observe(result.Name, host.image, result.Err == nil)
					result.Err = nil
				}
			}
			if result.Err == nil {
				result.Err = host.privilegeConflict(ctx)
			}
		})
	}
	probes.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	engines := make(map[string]string)
	runners := make(map[string]bool)
	for index, host := range hosts {
		if id := engineIDs[index]; id != "" {
			if existing, exists := engines[id]; exists {
				return nil, fmt.Errorf("Docker hosts %q and %q identify the same engine", existing, host.Name)
			}
			engines[id] = host.Name
		}
		for name := range host.Containers {
			if runners[name] {
				return nil, fmt.Errorf("runner %q exists on multiple Docker hosts", name)
			}
			runners[name] = true
		}
	}
	return hosts, nil
}

func (f *dockerFleet) host(name string) (docker, error) {
	for _, host := range f.hosts {
		if host.label() == name {
			return host, nil
		}
	}
	return docker{}, fmt.Errorf("unknown Docker host %q", name)
}

func (f *dockerFleet) startOn(ctx context.Context, target, name, jit string) error {
	host, err := f.host(target)
	if err != nil {
		return err
	}
	if image := runnerImageFrom(ctx); image != "" && image != host.image {
		if host.agentName() != "" {
			return fmt.Errorf("outbound worker %s starts only its own runner image %s", host.label(), host.image)
		}
		host.image = image
	}
	if host.agentName() != "" {
		task := workerTask{Operation: "start", Name: name, JIT: jit,
			Image: host.image, CPUs: host.cpus, MemoryMiB: host.memoryMiB, Unprivileged: host.unprivileged}
		if host.cache {
			if task.CacheRepository, err = runnerCacheRepository(ctx); err != nil {
				return err
			}
		}
		return f.workers.execute(ctx, host, task)
	}
	return host.start(ctx, name, jit)
}

func (f *dockerFleet) removeOn(ctx context.Context, target, name string) error {
	host, err := f.host(target)
	if err != nil {
		return err
	}
	if host.agentName() != "" {
		return f.workers.execute(ctx, host, workerTask{Operation: "remove", Name: name})
	}
	return host.remove(ctx, name)
}

func (d docker) engineID(ctx context.Context) (string, error) {
	engine, err := d.containerEngine()
	if err != nil {
		return "", err
	}
	output, err := d.command(ctx, nil, "info", "--format", engine.idFormat)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(output)
	if id == "" {
		return "", fmt.Errorf("Docker host %q did not report an engine ID", d.label())
	}
	return id, nil
}

func (d docker) checkRunnerImage(ctx context.Context) error {
	if _, err := d.command(ctx, nil, "image", "inspect", d.image, "--format", "{{.Id}}"); err != nil {
		return fmt.Errorf("runner image %s on Docker host %q: %w", d.image, d.label(), err)
	}
	return nil
}

func (d docker) label() string {
	if d.host == "" {
		return "local"
	}
	return d.host
}

func (d docker) agentName() string {
	name, ok := strings.CutPrefix(d.host, workerHostPrefix)
	if !ok {
		return ""
	}
	return name
}
