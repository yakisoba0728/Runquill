package app

// Warm runners are started runner containers that wait for a job's JIT config. A job's runner
// name and label are only known once the job is assigned, so the container is renamed to the
// job's runner name when it is claimed and the JIT config is written into it. A container still
// named runquill-warm-… has never held a credential, so removing it can never cancel a job.

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	warmPrefix         = "runquill-warm-"
	maxWarmPerHost     = 100
	defaultWarmMaxIdle = 30 * time.Minute
	maxWarmIdleMinutes = 24 * 60
	// warmExitGrace lets the entrypoint outlive the manager's own replacement, so it exits only when the manager is gone.
	warmExitGrace = 5 * time.Minute
)

func checkWarmPool(pool workerPool) error {
	switch {
	case pool.Warm < 0 || pool.Warm > maxWarmPerHost:
		return fmt.Errorf("warm must be between 0 and %d", maxWarmPerHost)
	case pool.WarmMaxIdleMinutes < 0 || pool.WarmMaxIdleMinutes > maxWarmIdleMinutes:
		return fmt.Errorf("warm_max_idle_minutes must be between 0 and %d", maxWarmIdleMinutes)
	case pool.Warm > 0 && pool.Cache:
		// The cache volume belongs to the job's repository, which a warm runner cannot know when it starts.
		return errors.New("warm cannot be combined with cache")
	}
	return nil
}

func warmMaxIdle(pool workerPool) time.Duration {
	return cmp.Or(time.Duration(pool.WarmMaxIdleMinutes)*time.Minute, defaultWarmMaxIdle)
}

func isWarmRunner(name string) bool {
	return strings.HasPrefix(name, warmPrefix)
}

type warmRunner struct {
	name, id, state, key string
	created              time.Time
}

// parseWarmName reads runquill-warm-<key>-<unix seconds>-<nonce>; the manager's clock set the time.
func parseWarmName(name string) (warmRunner, bool) {
	rest, _ := strings.CutPrefix(name, warmPrefix)
	parts := strings.Split(rest, "-")
	if !isWarmRunner(name) || len(parts) != 3 || len(parts[0]) != 12 || len(parts[2]) != 8 {
		return warmRunner{}, false
	}
	seconds, err := strconv.ParseInt(parts[1], 10, 64)
	if _, hexErr := hex.DecodeString(parts[0] + parts[2]); err != nil || hexErr != nil || seconds <= 0 {
		return warmRunner{}, false
	}
	return warmRunner{name: name, key: parts[0], created: time.Unix(seconds, 0)}, true
}

// warmPlan is the create argv of a warm runner on d and the key that names it. The key hashes the
// pool and the argv without the name, so a runner is only used while it matches the current settings.
func (d docker) warmPlan(ctx context.Context) (runnerCreate, string, error) {
	engine, err := d.containerEngine()
	if err != nil {
		return runnerCreate{}, "", err
	}
	unprivileged, err := d.runnerMode(ctx)
	if err != nil {
		return runnerCreate{}, "", err
	}
	plan := d.createPlan(engine, "", "", "", unprivileged)
	wait := "RUNQUILL_JIT_WAIT=" + strconv.Itoa(int((d.warmMaxIdle + warmExitGrace).Seconds()))
	last := len(plan.create) - 1
	plan.create = append(plan.create[:last:last], "--env", wait, plan.create[last])
	hash := sha256.Sum256([]byte(d.pool + "\x00" + strings.Join(plan.create, "\x00")))
	return plan, hex.EncodeToString(hash[:6]), nil
}

func (d docker) usableWarm(w warmRunner, key string, now time.Time) bool {
	return w.state == "running" && w.key == key && now.Sub(w.created) < d.warmMaxIdle
}

// warmRunners lists d's warm runners, oldest first.
func (d docker) warmRunners(ctx context.Context) ([]warmRunner, error) {
	output, err := d.listContainers(ctx, "--no-trunc", "--format", "{{.Names}}\t{{.ID}}\t{{.State}}")
	if err != nil {
		return nil, err
	}
	var runners []warmRunner
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		name, rest, _ := strings.Cut(line, "\t")
		if !isWarmRunner(name) {
			continue
		}
		id, state, ok := strings.Cut(rest, "\t")
		if !ok || !validHex32(id) || state == "" {
			return nil, errors.New("invalid Docker warm container list")
		}
		// An unrecognized name gets no key or age and is removed like an expired warm runner.
		w, _ := parseWarmName(name)
		w.name, w.id, w.state = name, id, state
		runners = append(runners, w)
	}
	slices.SortFunc(runners, func(a, b warmRunner) int {
		return cmp.Or(a.created.Compare(b.created), strings.Compare(a.name, b.name))
	})
	return runners, nil
}

// handoff gives a job's JIT config to an idle warm runner on d, renamed to the job's runner name. It
// reports false when none is ready. A job that needs another image retires one warm runner instead,
// so the cold runner takes its slot.
func (f *dockerFleet) handoff(ctx context.Context, d docker, name, jit string, otherImage bool) (bool, error) {
	plan, key, err := d.warmPlan(ctx)
	if err != nil {
		return false, err
	}
	runners, err := d.warmRunners(ctx)
	if err != nil {
		return false, err
	}
	now := time.Now()
	for _, w := range runners {
		if !d.usableWarm(w, key, now) {
			continue
		}
		if otherImage {
			if err := d.remove(ctx, w.name); err != nil {
				return false, fmt.Errorf("retire warm runner %s: %w", w.name, err)
			}
			return false, nil
		}
		// The rename is the claim: it fails if the runner was claimed or removed meanwhile.
		if _, err := d.command(ctx, nil, "rename", w.name, name); err != nil {
			// A disconnect can lose the reply after the engine has renamed this container.
			cause := fmt.Errorf("claim warm runner %s: %w", w.name, err)
			current, checkErr := d.listContainers(context.WithoutCancel(ctx), "--filter", "id="+w.id, "--format", "{{.Names}}")
			if checkErr != nil {
				return true, errors.Join(cause, fmt.Errorf("check warm runner claim: %w", checkErr))
			}
			if strings.TrimSpace(current) == name {
				return true, d.removeFailedRunner(ctx, w.id, cause)
			}
			if ctx.Err() != nil {
				return false, cause
			}
			slog.Warn("could not claim warm runner", "runner_host", d.label(), "error", cause)
			continue
		}
		// mv makes the file appear whole; the JIT config travels on stdin, never in argv or the environment.
		_, err := d.command(ctx, []byte(jit), "exec", "-i", w.id, "sh", "-c", `umask 077 && cat >"$1.tmp" && mv "$1.tmp" "$1"`, "sh", "/"+plan.jitPath)
		if err != nil {
			// Never leave a claimed runner without its config; reconcile releases the lease.
			return true, d.removeFailedRunner(ctx, w.id, fmt.Errorf("hand job to warm runner %s: %w", w.name, err))
		}
		f.warm.reset(d.label())
		slog.Info("warm runner took a job", "runner", name, "warm_runner", w.name, "runner_host", d.label())
		return true, nil
	}
	return false, nil
}

// warmRuntime is implemented by runtimes that keep warm runners between scheduler passes.
type warmRuntime interface {
	maintainWarm(ctx context.Context, hosts []hostCapacity, free int, now time.Time)
}

// maintainWarm removes warm runners that are stale, exited, beyond the free slots or on cordoned
// hosts, then starts at most one per host. free is how many runners the global limit still allows.
func (f *dockerFleet) maintainWarm(ctx context.Context, hosts []hostCapacity, free int, now time.Time) {
	for _, capacity := range hosts {
		host, err := f.host(capacity.Name)
		if err != nil || capacity.Containers == nil || capacity.Err != nil || host.warm == 0 && capacity.Warm == 0 {
			continue
		}
		kept, err := f.maintainHost(ctx, host, capacity, free, now)
		free -= kept
		if err != nil && ctx.Err() == nil {
			slog.Warn("maintain warm runners", "runner_host", host.label(), "error", err)
		}
	}
}

func (f *dockerFleet) maintainHost(ctx context.Context, d docker, capacity hostCapacity, free int, now time.Time) (int, error) {
	desired := min(d.warm, capacity.Limit-capacity.Occupied, free)
	if capacity.Cordoned || capacity.ImageWait != "" {
		desired = 0
	}
	var plan runnerCreate
	var key string
	var failures []error
	if desired > 0 {
		var err error
		if plan, key, err = d.warmPlan(ctx); err != nil {
			desired, failures = 0, append(failures, err)
		}
	}
	runners, err := d.warmRunners(ctx)
	if err != nil {
		return 0, errors.Join(append(failures, err)...)
	}
	var keep []warmRunner
	failed := false
	for _, w := range runners {
		switch {
		case d.usableWarm(w, key, now):
			keep = append(keep, w)
			continue
		case w.state == "running" && w.key == key:
			f.warm.reset(d.label()) // reached its idle age healthy
		case w.state != "running" && !w.created.IsZero() && now.Sub(w.created) < d.warmMaxIdle+warmExitGrace:
			failed = true // exited before the entrypoint's own timeout: the image or engine is broken
		}
		if err := d.remove(ctx, w.name); err != nil {
			failures = append(failures, err)
		}
	}
	for len(keep) > max(desired, 0) {
		if err := d.remove(ctx, keep[0].name); err != nil {
			failures = append(failures, err)
		}
		keep = keep[1:]
	}
	if failed {
		f.warm.fail(d.label(), now)
	}
	kept := len(keep)
	if kept < desired && f.warm.ready(d.label(), now) {
		if err := d.createWarm(ctx, plan, key, now); err != nil {
			f.warm.fail(d.label(), now)
			failures = append(failures, fmt.Errorf("start warm runner: %w", err))
		} else {
			kept++
		}
	}
	return kept, errors.Join(failures...)
}

func (d docker) createWarm(ctx context.Context, plan runnerCreate, key string, now time.Time) error {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := fmt.Sprintf("%s%s-%d-%s", warmPrefix, key, now.Unix(), hex.EncodeToString(nonce[:]))
	create := slices.Clone(plan.create)
	create[slices.Index(create, "--name")+1] = name
	noteRunnerAssets(d.label(), d.image)
	if _, err := d.command(ctx, nil, create...); err != nil {
		return err
	}
	err := func() error {
		if len(plan.dirs) > 0 {
			// Plain runners wait in a directory that UID 1001 owns, as docker.start creates it.
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			for _, dir := range plan.dirs {
				if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0700, Uid: runnerUID, Gid: runnerUID}); err != nil {
					return err
				}
			}
			if err := tw.Close(); err != nil {
				return err
			}
			if _, err := d.command(ctx, archive.Bytes(), "cp", "-", name+":/"); err != nil {
				return err
			}
		}
		_, err := d.command(ctx, nil, "start", name)
		return err
	}()
	if err != nil {
		return d.removeFailedRunner(ctx, name, err)
	}
	return nil
}

// warmBackoff spaces out warm runner starts on a host whose runners keep failing.
type warmBackoff struct {
	mu    sync.Mutex
	hosts map[string]warmRetry
}

type warmRetry struct {
	failures int
	at       time.Time
}

func (b *warmBackoff) ready(host string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !now.Before(b.hosts[host].at)
}

func (b *warmBackoff) fail(host string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.hosts[host]
	r.failures++
	r.at = now.Add(launchDelay(r.failures - 1))
	b.hosts[host] = r
}

func (b *warmBackoff) reset(host string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.hosts, host)
}
