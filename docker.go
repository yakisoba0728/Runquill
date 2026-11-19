package main

import (
	"archive/tar"
	"bytes"
	"context"
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

// runnerCacheVolume names the per-repository cache volume from the numeric repository ID only.
func runnerCacheVolume(ctx context.Context) (string, error) {
	id, err := runnerCacheRepository(ctx)
	return "runquill-cache-" + strconv.FormatInt(id, 10), err
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
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", binary, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(result), nil
}

func (d docker) containers(ctx context.Context) (map[string]string, error) {
	args := []string{"ps", "-a", "--filter", "label=" + runnerLabel}
	if d.worker != "" {
		args = append(args, "--filter", "label="+workerLabel+"="+d.worker)
	}
	output, err := d.command(ctx, nil, append(args, "--format", "{{.Names}}\t{{.State}}")...)
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
	volume   []string // run first when the engine cannot label a volume from --mount
	create   []string
	jitPath  string
	jitOwner int
	dirs     []string
}

func (d docker) createPlan(engine *containerEngine, name, cacheVolume string, unprivileged bool) runnerCreate {
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
	}
	if d.cpus != 0 {
		args = append(args, "--cpus", strconv.FormatFloat(d.cpus, 'f', -1, 64))
	}
	if d.memoryMiB != 0 {
		memory := strconv.FormatInt(d.memoryMiB, 10) + "m"
		args = append(args, "--memory", memory, "--memory-swap", memory)
	}
	if cacheVolume != "" {
		// Named volumes survive "rm -v", so the cache outlives each ephemeral runner.
		mount := "type=volume,src=" + cacheVolume + ",dst=" + runnerCachePath
		if engine.mountLabelsVolume {
			mount += ",volume-label=io.runquill.cache=1"
		} else {
			plan.volume = []string{"volume", "create", "--ignore", "--label", "io.runquill.cache=1", cacheVolume}
		}
		// The runner reads RUNNER_TOOL_CACHE for its tool directory, so setup-* toolchains stay in this repository's volume.
		args = append(args, "--mount", mount, "--env", "RUNNER_TOOL_CACHE="+runnerToolCache)
		// A new volume's root is owned by root; both directories are created for UID 1001.
		plan.dirs = append(plan.dirs, runnerCachePath[1:]+"/", runnerToolCache[1:]+"/")
	}
	plan.create = append(args, d.image)
	return plan
}

func (d docker) start(ctx context.Context, name, jit string) error {
	engine, err := d.containerEngine()
	if err != nil {
		return err
	}
	unprivileged, err := d.runnerMode(ctx)
	if err != nil {
		return err
	}
	var volume string
	if d.cache {
		if volume, err = runnerCacheVolume(ctx); err != nil {
			return err
		}
	}
	plan := d.createPlan(engine, name, volume, unprivileged)
	if plan.volume != nil {
		if _, err := d.command(ctx, nil, plan.volume...); err != nil {
			return err
		}
	}
	noteRunnerAssets(d.label(), plan.create, volume)
	if _, err := d.command(ctx, nil, plan.create...); err != nil {
		return err
	}
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
