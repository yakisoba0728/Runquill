package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yakisoba0728/Runquill/internal/route"
)

const workerLabel = "io.runquill.worker"

type workerClient struct {
	endpoint string
	token    string
	engine   string
	session  string
	target   docker
	client   *http.Client
	last     *workerResult
}

func workerCommand(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "enroll" {
		return workerEnrollCommand(ctx, args[1:])
	}
	flags := flag.NewFlagSet("runquill worker", flag.ContinueOnError)
	name := flags.String("name", os.Getenv("RUNQUILL_WORKER_NAME"), "configured agent host name")
	manager := flags.String("manager", os.Getenv("RUNQUILL_WORKER_URL"), "manager HTTPS origin")
	tokenFile := flags.String("token-file", os.Getenv("RUNQUILL_WORKER_TOKEN_FILE"), "private file containing the worker token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("runquill worker accepts flags only")
	}
	endpoint, err := workerEndpoint(*manager, *name)
	if err != nil {
		return err
	}
	token, release, err := lockWorkerToken(*tokenFile)
	if err != nil {
		return err
	}
	defer release()
	image := env("RUNQUILL_RUNNER_IMAGE", "runquill-runner:local")
	if !validWorkerImage(image) {
		return errors.New("set RUNQUILL_RUNNER_IMAGE to the preloaded worker runner image")
	}
	mirror, err := registryMirror()
	if err != nil {
		return err
	}
	target := docker{image: image, worker: *name, mirror: mirror}
	var failures []error
	checkDocker(ctx, target, nil, func(_ string, err error) {
		if err != nil {
			failures = append(failures, err)
		}
	})
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	engine, err := target.engineID(ctx)
	if err != nil {
		return err
	}
	client := &workerClient{endpoint: endpoint, token: token, engine: engine, session: rand.Text(), target: target,
		client: workerHTTPClient(),
	}
	slog.Info("worker started", "host", workerHostPrefix+*name)
	return client.run(ctx)
}

func workerHTTPClient() *http.Client {
	return &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func workerEndpoint(raw, name string) (string, error) {
	if !route.ValidPoolName(name) {
		return "", errors.New("set the worker name to 1–32 lowercase letters, digits or hyphens")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(raw, "?#") {
		return "", errors.New("worker manager must be an HTTPS origin without credentials, path or query")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return "", errors.New("worker manager requires HTTPS except for literal loopback HTTP")
		}
	}
	u.Path = "/worker/v1/" + name + "/poll"
	return u.String(), nil
}

func lockWorkerToken(filename string) (string, func(), error) {
	if filename == "" {
		return "", nil, errors.New("set --token-file or RUNQUILL_WORKER_TOKEN_FILE")
	}
	real, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return "", nil, fmt.Errorf("resolve worker token file: %w", err)
	}
	file, err := os.Open(real)
	if err != nil {
		return "", nil, fmt.Errorf("open worker token file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 128 {
		return "", nil, errors.New("worker token must be a private regular file (0600 or 0400)")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return "", nil, fmt.Errorf("read worker token file: %w", err)
	}
	token := strings.TrimSpace(string(contents))
	if !validHex32(token) {
		return "", nil, errors.New("worker token file must contain a 32-byte lowercase hex token")
	}
	lock, err := os.OpenFile(real+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("open worker lock beside token file: %w", err)
	}
	if err := lockExclusive(lock); err != nil {
		lock.Close()
		return "", nil, fmt.Errorf("lock worker process: %w", err)
	}
	return token, func() { lock.Close() }, nil
}

func (c *workerClient) run(ctx context.Context) error {
	var lastError string
	for ctx.Err() == nil {
		if err := c.exchange(ctx); err != nil {
			if ctx.Err() != nil {
				break
			}
			if err.Error() != lastError {
				slog.Warn("worker exchange failed", "error", err)
				lastError = err.Error()
			}
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
		} else if lastError != "" {
			slog.Info("worker connection restored")
			lastError = ""
		}
	}
	return nil
}

func (c *workerClient) exchange(ctx context.Context) error {
	probe, cancel := context.WithTimeout(ctx, dockerHostProbeTimeout)
	defer cancel()
	engine, err := c.target.engineID(probe)
	if err == nil && engine != c.engine {
		return errors.New("local Docker engine changed. Verify the worker registration and restart it")
	}
	report := workerReport{Engine: c.engine, Session: c.session, Result: c.last,
		Observation: workerObservation{Image: c.target.image},
	}
	if err == nil {
		report.Observation.Containers, err = c.target.containers(probe)
		if err == nil {
			report.Observation.ImageReady = c.target.checkRunnerImage(probe) == nil
		}
	}
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("build worker poll request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(workerFeatureHeader, workerFeatureTTL)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("poll manager: %w", err)
	}
	received := time.Now()
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("manager returned HTTP %d", resp.StatusCode)
	}
	var task workerTask
	if err := decodeWorkerJSON(io.LimitReader(resp.Body, workerJITLimit+(64<<10)), &task); err != nil || !validWorkerTask(task) {
		return errors.New("manager returned an invalid worker task")
	}
	if c.last != nil && c.last.ID == task.ID {
		return nil
	}
	err = c.apply(ctx, task, received)
	c.last = &workerResult{ID: task.ID, OK: err == nil}
	if err != nil {
		slog.Warn("worker runner operation failed", "operation", task.Operation, "runner", task.Name, "error", err)
	}
	return nil
}

// workerTaskDeadline measures a ttl_ms task from its receipt on the worker's clock. Older managers
// send only the absolute expiry, which needs the clocks to agree within a few seconds.
func workerTaskDeadline(task workerTask, received, now time.Time) (time.Time, error) {
	limit := workerTaskTTL + 5*time.Second
	if task.TTL != nil {
		if *task.TTL <= 0 {
			return time.Time{}, errors.New("worker task has an invalid time to live")
		}
		return received.Add(time.Duration(min(*task.TTL, limit.Milliseconds())) * time.Millisecond), nil
	}
	if !task.Expires.After(now) || task.Expires.After(now.Add(limit)) {
		return time.Time{}, errors.New("worker task expired or has an invalid deadline. Check that the worker and manager clocks are synchronized")
	}
	return task.Expires, nil
}

func (c *workerClient) apply(ctx context.Context, task workerTask, received time.Time) error {
	deadline, err := workerTaskDeadline(task, received, time.Now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	engine, err := c.target.engineID(ctx)
	if err != nil {
		return err
	}
	if engine != c.engine {
		return errors.New("refuse task on a different Docker engine")
	}
	if task.Operation == "remove" {
		return c.target.removeWorkerRunner(ctx, task.Name)
	}
	if task.Image != c.target.image {
		return errors.New("worker runner image differs from its locally configured image")
	}
	containers, err := c.target.containers(ctx)
	if err != nil {
		return err
	}
	if state := containers[task.Name]; state != "" {
		if state == "running" {
			return nil
		}
		return errors.New("runner already exists in an uncertain state")
	}
	target := c.target
	target.cpus, target.memoryMiB = task.CPUs, task.MemoryMiB
	target.unprivileged, target.cache = task.Unprivileged, task.CacheRepository > 0
	return target.start(withRunnerRepository(ctx, task.CacheRepository), task.Name, task.JIT)
}

func (d docker) removeWorkerRunner(ctx context.Context, name string) error {
	if _, valid := runnerJobID(name); !valid || !route.ValidPoolName(d.worker) {
		return errors.New("invalid worker runner ownership")
	}
	raw, err := d.command(ctx, nil, "ps", "-a", "--no-trunc", "--filter", "label="+runnerLabel,
		"--filter", "label="+workerLabel+"="+d.worker, "--format", `{{.Names}}|{{.ID}}|{{.Label "io.runquill.runner"}}|{{.Label "io.runquill.worker"}}`)
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		fields := strings.SplitN(line, "|", 4)
		if fields[0] != name {
			continue
		}
		if len(fields) != 4 || !validHex32(fields[1]) || fields[2] != "1" || fields[3] != d.worker {
			return errors.New("cannot verify worker runner ownership")
		}
		// A replacement container reusing this name must not be removed.
		return d.remove(ctx, fields[1])
	}
	return nil
}
