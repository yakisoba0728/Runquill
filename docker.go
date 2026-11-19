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
}

func (d docker) command(ctx context.Context, input []byte, args ...string) (string, error) {
	if d.host == "" {
		return dockerCommand(ctx, input, args...)
	}
	output, err := dockerCommand(ctx, input, append([]string{"--host", d.host}, args...)...)
	if err != nil {
		return "", fmt.Errorf("%s on %s: %w", args[0], d.host, err)
	}
	return output, nil
}

func dockerCommand(ctx context.Context, input []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
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
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
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

func (d docker) start(ctx context.Context, name, jit string) error {
	args := []string{"create", "--name", name, "--label", runnerLabel,
		"--pull", "never", "--init", "--cgroupns", "private", "--log-driver", "json-file",
		"--log-opt", "max-size=10m", "--log-opt", "max-file=2"}
	if d.worker != "" {
		args = append(args, "--label", workerLabel+"="+d.worker)
	}
	jitPath, jitOwner := "run/runquill-jit", 0
	if d.unprivileged {
		jitPath, jitOwner = "run/runquill/jit", runnerUID
		args = append(args, "--user", "1001:1001", "--security-opt", "no-new-privileges", "--cap-drop", "ALL",
			"--env", "RUNQUILL_RUNNER_MODE=plain", "--env", "RUNQUILL_JIT_FILE=/"+jitPath)
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
	var dirs []string
	if d.unprivileged {
		dirs = append(dirs, "run/runquill/")
	}
	if d.cache {
		volume, err := runnerCacheVolume(ctx)
		if err != nil {
			return err
		}
		// Named volumes survive "rm -v", so the cache outlives each ephemeral runner.
		args = append(args, "--mount", "type=volume,src="+volume+",dst="+runnerCachePath+",volume-label=io.runquill.cache=1")
		dirs = append(dirs, runnerCachePath[1:]+"/") // a new volume's root is owned by root
	}
	_, err := d.command(ctx, nil, append(args, d.image)...)
	if err != nil {
		return err
	}
	// Copy the JIT credential without exposing it in Docker's command or environment metadata.
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, dir := range dirs {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0700, Uid: runnerUID, Gid: runnerUID}); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: jitPath, Mode: 0600, Size: int64(len(jit)), Uid: jitOwner, Gid: jitOwner}); err != nil {
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
