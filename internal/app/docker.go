package app

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	runnerLabel     = "io.runquill.runner=1"
	runnerCachePath = "/home/runner/.cache"
	runnerToolCache = runnerCachePath + "/tools"
	runnerUID       = 1001
)

// packageCacheEnv moves download caches that do not default to ~/.cache into the repository volume.
// Each variable moves only a cache, never a tool home that holds credentials, config or binaries.
var packageCacheEnv = []string{
	"npm_config_cache=" + runnerCachePath + "/npm",
	"GOMODCACHE=" + runnerCachePath + "/go-mod",
	"NUGET_PACKAGES=" + runnerCachePath + "/nuget-packages",
	"BUN_INSTALL_CACHE_DIR=" + runnerCachePath + "/bun",
}

type runnerRepositoryKey struct{}

// withRunnerRepository scopes a runner start to a repository so cache-enabled pools can mount its volume.
func withRunnerRepository(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, runnerRepositoryKey{}, id)
}

func runnerCacheRepository(ctx context.Context) (int64, error) {
	id, _ := ctx.Value(runnerRepositoryKey{}).(int64)
	if id < 1 {
		return 0, errors.New("runner cache requires a repository ID")
	}
	return id, nil
}

type scratchCacheKey struct{}

// withScratchCache gives a runner of a cache-enabled pool an empty cache that is removed with it,
// instead of the repository's shared volumes, because its run may not write them (cache_writes).
func withScratchCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, scratchCacheKey{}, true)
}

func scratchCache(ctx context.Context) bool {
	scratch, _ := ctx.Value(scratchCacheKey{}).(bool)
	return scratch
}

// runnerCacheVolume is the repository's shared package and build-tool cache.
func runnerCacheVolume(ctx context.Context) (string, error) {
	id, err := runnerCacheRepository(ctx)
	return "runquill-cache-" + strconv.FormatInt(id, 10), err
}

const runnerCacheImageFormat = "{{.Id}}|{{.Os}}|{{.Architecture}}"

func (d docker) toolCache(ctx context.Context, repositoryVolume string) (image, volume string, err error) {
	output, err := d.command(ctx, nil, "image", "inspect", d.image, "--format", runnerCacheImageFormat)
	if err != nil {
		return "", "", fmt.Errorf("inspect runner image for tool cache: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(output), "|")
	if len(fields) != 3 || !validHex32(strings.TrimPrefix(fields[0], "sha256:")) || fields[1] == "" || fields[2] == "" {
		return "", "", errors.New("runner tool cache requires an image ID and platform")
	}
	key := sha256.Sum256([]byte(strings.TrimPrefix(fields[0], "sha256:") + "\x00" + fields[1] + "\x00" + fields[2]))
	// Start by immutable ID so a tag change cannot pick an image that uses another cache.
	return fields[0], fmt.Sprintf("%s-tools-%x", repositoryVolume, key), nil
}

func runnerOccupiesSlot(state string) bool {
	switch state {
	case "created", "pending", "running", "paused", "restarting":
		return true
	default:
		return false
	}
}

type docker struct {
	image     string
	host      string
	pool      string
	worker    string
	cpus      float64
	memoryMiB int64
	// unprivileged runs plain runners as UID 1001 without Docker-in-Docker; the zero value keeps --privileged.
	unprivileged bool
	cache        bool
	// requirePrivileged records an explicit "privileged": true, which a rootless engine rejects.
	requirePrivileged bool
	engine            *containerEngine // nil uses RUNQUILL_CONTAINER_ENGINE
	security          *engineSecurity
	// mirror is the validated RUNQUILL_REGISTRY_MIRROR of whoever runs "create": the manager or the outbound worker.
	mirror string
	// warm is how many idle started runners to keep; see warm.go.
	warm        int
	warmMaxIdle time.Duration
	// scratchCache replaces the shared cache volumes with one anonymous volume that "rm -v" removes.
	scratchCache bool
}

func (d docker) command(ctx context.Context, input []byte, args ...string) (string, error) {
	engine, err := d.containerEngine()
	if err != nil {
		return "", err
	}
	if d.host == "" {
		return engineCommand(ctx, engine.name, input, args...)
	}
	output, err := engineCommand(ctx, engine.name, input, append(engine.remoteArgs(d.host), args...)...)
	if err != nil {
		return "", fmt.Errorf("%s on %s: %w", args[0], d.host, err)
	}
	return output, nil
}

func dockerCommand(ctx context.Context, input []byte, args ...string) (string, error) {
	return engineCommand(ctx, "docker", input, args...)
}

func engineCommand(ctx context.Context, binary string, input []byte, args ...string) (string, error) {
	return engineCommandTimeout(ctx, 30*time.Second, binary, input, args...)
}

func engineCommandTimeout(ctx context.Context, timeout time.Duration, binary string, input []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = time.Second
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	result, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("%s %s: %w: %s", binary, args[0], err, detail)
		}
		return "", fmt.Errorf("%s %s: %w", binary, args[0], err)
	}
	return string(result), nil
}

func (d docker) listContainers(ctx context.Context, options ...string) (string, error) {
	args := []string{"ps", "-a", "--filter", "label=" + runnerLabel}
	if d.worker != "" {
		args = append(args, "--filter", "label="+workerLabel+"="+d.worker)
	}
	return d.command(ctx, nil, append(args, options...)...)
}

func (d docker) containers(ctx context.Context) (map[string]string, error) {
	output, err := d.listContainers(ctx, "--format", "{{.Names}}\t{{.State}}")
	if err != nil {
		return nil, err
	}
	states := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		name, state, ok := strings.Cut(line, "\t")
		if !ok || name == "" || state == "" {
			return nil, errors.New("invalid Docker container list")
		}
		states[name] = state
	}
	return states, nil
}

// runnerCreate is the engine argv and credential layout for one runner, built without side effects.
type runnerCreate struct {
	volumes  [][]string // run first when the engine cannot label a volume from --mount
	create   []string
	jitPath  string
	jitOwner int
	dirs     []string
}

func (d docker) createPlan(engine *containerEngine, name, cacheVolume, toolVolume string, unprivileged bool) runnerCreate {
	plan := runnerCreate{jitPath: "run/runquill-jit"}
	args := []string{"create", "--name", name, "--label", runnerLabel, "--pull", "never", "--init", "--cgroupns", "private"}
	args = append(args, engine.logArgs...)
	if d.worker != "" {
		args = append(args, "--label", workerLabel+"="+d.worker)
	}
	if unprivileged {
		plan.jitPath, plan.jitOwner = "run/runquill/jit", runnerUID
		args = append(args, "--user", "1001:1001", "--security-opt", "no-new-privileges", "--cap-drop", "ALL",
			"--env", "RUNQUILL_RUNNER_MODE=plain", "--env", "RUNQUILL_JIT_FILE=/"+plan.jitPath)
		plan.dirs = append(plan.dirs, "run/runquill/")
	} else {
		args = append(args, "--privileged")
		if d.mirror != "" {
			// Only the DinD entrypoint starts a dockerd that reads it; older images ignore it.
			args = append(args, "--env", "RUNQUILL_REGISTRY_MIRROR="+d.mirror)
		}
	}
	if d.cpus != 0 {
		args = append(args, "--cpus", strconv.FormatFloat(d.cpus, 'f', -1, 64))
	}
	if d.memoryMiB != 0 {
		memory := strconv.FormatInt(d.memoryMiB, 10) + "m"
		args = append(args, "--memory", memory, "--memory-swap", memory)
	}
	if cacheVolume != "" || d.scratchCache {
		for _, env := range packageCacheEnv {
			args = append(args, "--env", env)
		}
	}
	if d.scratchCache {
		// Not read-only shared volumes: tools fail on them, and a privileged runner can remount them writable.
		args = append(args, "--mount", "type=volume,dst="+runnerCachePath, "--env", "RUNNER_TOOL_CACHE="+runnerToolCache)
		plan.dirs = append(plan.dirs, runnerCachePath[1:]+"/", runnerToolCache[1:]+"/")
	} else if cacheVolume != "" {
		// Named volumes survive "rm -v", so the cache outlives each ephemeral runner.
		for _, cache := range []struct {
			name, path string
			noCopy     bool
		}{{cacheVolume, runnerCachePath, false}, {toolVolume, runnerToolCache, true}} {
			mount := "type=volume,src=" + cache.name + ",dst=" + cache.path
			if engine.mountLabelsVolume {
				mount += ",volume-label=" + cacheVolumeLabel
				if cache.noCopy {
					mount += ",volume-nocopy"
				}
			} else {
				create := []string{"volume", "create", "--ignore", "--label", cacheVolumeLabel}
				if cache.noCopy {
					create = append(create, "--opt", "nocopy")
				}
				plan.volumes = append(plan.volumes, append(create, cache.name))
			}
			args = append(args, "--mount", mount)
		}
		args = append(args, "--env", "RUNNER_TOOL_CACHE="+runnerToolCache)
		// A new volume's root is owned by root; both directories are created for UID 1001.
		plan.dirs = append(plan.dirs, runnerCachePath[1:]+"/", runnerToolCache[1:]+"/")
	}
	plan.create = append(args, d.image)
	return plan
}

func (d docker) start(ctx context.Context, name, jit string) (err error) {
	engine, err := d.containerEngine()
	if err != nil {
		return err
	}
	unprivileged, err := d.runnerMode(ctx)
	if err != nil {
		return err
	}
	imageRef := d.image
	var volume, toolVolume string
	d.scratchCache = d.cache && scratchCache(ctx)
	if d.cache && !d.scratchCache {
		if volume, err = runnerCacheVolume(ctx); err != nil {
			return err
		}
		if d.image, toolVolume, err = d.toolCache(ctx, volume); err != nil {
			return err
		}
	}
	plan := d.createPlan(engine, name, volume, toolVolume, unprivileged)
	for _, create := range plan.volumes {
		if _, err := d.command(ctx, nil, create...); err != nil {
			return err
		}
	}
	noteRunnerAssets(d.label(), imageRef, volume, toolVolume)
	if _, err := d.command(ctx, nil, plan.create...); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = d.removeFailedRunner(ctx, name, err)
		}
	}()
	// Copy the JIT credential without exposing it in the engine's command or environment metadata.
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, dir := range plan.dirs {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0700, Uid: runnerUID, Gid: runnerUID}); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: plan.jitPath, Mode: 0600, Size: int64(len(jit)), Uid: plan.jitOwner, Gid: plan.jitOwner}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(jit)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if _, err := d.command(ctx, archive.Bytes(), "cp", "-", name+":/"); err != nil {
		return err
	}
	_, err = d.command(ctx, nil, "start", name)
	return err
}

func (d docker) remove(ctx context.Context, name string) error {
	_, err := d.command(ctx, nil, "rm", "-f", "-v", name)
	return err
}

func (d docker) removeFailedRunner(ctx context.Context, name string, cause error) error {
	if err := d.remove(context.WithoutCancel(ctx), name); err != nil {
		return errors.Join(cause, fmt.Errorf("remove failed runner %s: %w", name, err))
	}
	return cause
}
