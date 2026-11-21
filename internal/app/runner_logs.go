package app

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	runnerLogDefaultTail  = 200
	runnerLogMaxTail      = 2000
	runnerLogTailTimeout  = 20 * time.Second
	runnerLogFollowLimit  = 15 * time.Minute
	runnerLogMaxBytes     = 8 << 20
	runnerLogMaxStreams   = 8
	runnerLogWriteWindow  = 30 * time.Second
	runnerLogChunkBytes   = 64 << 10
	runnerLogLimitMessage = "\n[Runquill: log output limit reached]\n"
)

var (
	runnerLogName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	// The entrypoint never prints the JIT config, and docker.go copies it in a
	// tar stream rather than argv or env. As a second line of defence, long
	// base64 runs (the JIT config is a multi-kilobyte base64 blob) are masked.
	runnerLogSecret = regexp.MustCompile(`[A-Za-z0-9+/]{200,}={0,2}`)
)

type runnerLogs struct {
	db *sql.DB
	// command runs the container engine CLI named by binary.
	command func(ctx context.Context, binary string, args ...string) *exec.Cmd
	streams chan struct{}
}

func newRunnerLogs(db *sql.DB) *runnerLogs {
	return &runnerLogs{db: db, streams: make(chan struct{}, runnerLogMaxStreams), command: exec.CommandContext}
}

// registerRunnerLogRoutes serves GET /api/runners/{name}/logs?tail=N&follow=1.
func registerRunnerLogRoutes(mux *http.ServeMux, a *panelAccess) {
	newRunnerLogs(a.db).register(mux, a)
}

func (l *runnerLogs) register(mux *http.ServeMux, a *panelAccess) {
	// "browser" admits only signed-in sessions; no token scope reaches it.
	mux.HandleFunc("GET /api/runners/{name}/logs", a.require("browser", func(w http.ResponseWriter, r *http.Request, user panelIdentity) {
		l.serve(w, r, user, a)
	}))
}

// runnerLogPermission mirrors jobs:write: an operator or admin with a session.
func runnerLogPermission(user panelIdentity) bool {
	return user.Scope == "" && (user.Role == "operator" || user.Role == "admin")
}

func runnerLogUnavailable(w http.ResponseWriter, message string) {
	panelJSON(w, http.StatusNotImplemented, map[string]string{"error": message})
}

func (l *runnerLogs) serve(w http.ResponseWriter, r *http.Request, user panelIdentity, a *panelAccess) {
	if !runnerLogPermission(user) {
		http.Error(w, "You do not have access to this operation.", http.StatusForbidden)
		return
	}
	query := r.URL.Query()
	tail := runnerLogDefaultTail
	if raw := query.Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			http.Error(w, "Use a tail between 0 and 2000 lines.", http.StatusBadRequest)
			return
		}
		tail = min(n, runnerLogMaxTail)
	}
	var follow bool
	switch query.Get("follow") {
	case "", "0", "false":
	case "1", "true":
		follow = true
	default:
		http.Error(w, "Use follow=1 or follow=0.", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	if !runnerLogName.MatchString(name) {
		http.Error(w, "This runner is not active in Runquill.", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	var host string
	err := l.db.QueryRowContext(ctx, `SELECT coalesce(runner_host, '') FROM jobs WHERE runner_name = ?`, name).Scan(&host)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && host == "") {
		http.Error(w, "This runner is not active in Runquill.", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Could not read the runner.", http.StatusServiceUnavailable)
		return
	}
	var target docker
	switch {
	case host == kubeHost:
		runnerLogUnavailable(w, "Runner logs are not available for Kubernetes runners.")
		return
	case strings.HasPrefix(host, workerHostPrefix):
		runnerLogUnavailable(w, "Runner logs are not available for agent:// worker runners.")
		return
	case host == "local":
	default:
		// Re-validate the stored host with the fleet's own parser before it reaches the engine.
		fleet, err := parseDockerFleet("", host)
		if err != nil || len(fleet.hosts) != 1 || !strings.HasPrefix(host, "ssh://") {
			runnerLogUnavailable(w, "Runner logs are not available for this runner host.")
			return
		}
		target = fleet.hosts[0]
	}
	engine, err := target.containerEngine()
	if err != nil {
		runnerLogUnavailable(w, "Runner logs are not available for this runner host.")
		return
	}
	var args []string
	if target.host != "" {
		args = engine.remoteArgs(target.host)
	}
	select {
	case l.streams <- struct{}{}:
		defer func() { <-l.streams }()
	default:
		http.Error(w, "Too many runner log streams are open. Try again shortly.", http.StatusTooManyRequests)
		return
	}
	if err := a.change(ctx, user, "runner.logs", name, func(*sql.Tx) error { return nil }); err != nil {
		http.Error(w, "Could not authorize the runner logs.", http.StatusForbidden)
		return
	}
	args = append(args, "logs", "--timestamps", "--tail", strconv.Itoa(tail))
	limit := runnerLogTailTimeout
	if follow {
		args, limit = append(args, "--follow"), runnerLogFollowLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if closing := panelServerClosing(r); closing != nil {
		go func() {
			select {
			case <-closing:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	if err := l.stream(ctx, w, engine.name, append(args, name), follow); err != nil {
		slog.Warn("runner logs", "runner", name, "runner_host", host, "error", err)
	}
}

type runnerLogOutput struct {
	w        http.ResponseWriter
	control  *http.ResponseController
	follow   bool
	started  bool
	written  int
	buffered []byte
}

// write streams a follow response immediately; a tail is buffered so a failed
// engine command can still return an error status.
func (o *runnerLogOutput) write(chunk []byte) error {
	if !o.follow {
		o.buffered = append(o.buffered, chunk...)
		return nil
	}
	if !o.started {
		o.header()
	}
	if err := o.extendDeadline(); err != nil {
		return err
	}
	if _, err := o.w.Write(chunk); err != nil {
		return err
	}
	return o.control.Flush()
}

// extendDeadline outlasts the panel server's WriteTimeout, which a log command can exceed.
func (o *runnerLogOutput) extendDeadline() error {
	err := o.control.SetWriteDeadline(time.Now().Add(runnerLogWriteWindow))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (o *runnerLogOutput) header() {
	o.started = true
	o.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	o.w.Header().Set("X-Accel-Buffering", "no")
	o.w.WriteHeader(http.StatusOK)
}

func (l *runnerLogs) stream(ctx context.Context, w http.ResponseWriter, binary string, args []string, follow bool) error {
	reader, writer, err := os.Pipe()
	if err != nil {
		http.Error(w, "Could not read the runner logs.", http.StatusBadGateway)
		return err
	}
	defer reader.Close()
	cmd := l.command(ctx, binary, args...)
	// docker writes container stderr to its own stderr; keep both, in order.
	cmd.Stdout, cmd.Stderr = writer, writer
	// Kill the whole group so an ssh helper does not outlive the docker CLI.
	killProcessGroupOnCancel(cmd)
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		writer.Close()
		http.Error(w, "Could not read the runner logs.", http.StatusBadGateway)
		return err
	}
	writer.Close()
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			// Unblocks the read even if a grandchild still holds the pipe.
			reader.Close()
		case <-stopped:
		}
	}()
	output := &runnerLogOutput{w: w, control: http.NewResponseController(w), follow: follow}
	copyErr := copyRunnerLog(reader, output)
	if copyErr != nil {
		// A limit or a gone client ends the stream; stop docker either way.
		if cmd.Process != nil {
			cmd.Cancel()
		}
	}
	waitErr := cmd.Wait()
	if follow {
		if !output.started {
			if waitErr != nil && ctx.Err() == nil {
				http.Error(w, "Could not read the runner logs.", http.StatusBadGateway)
				return errors.Join(waitErr, ignoreRunnerLogLimit(copyErr))
			}
			output.header()
		}
		// Disconnects and the duration cap are normal ends of a follow.
		return ignoreRunnerLogLimit(copyErr)
	}
	// A tail can outlast the server's WriteTimeout before its first write.
	if err := output.extendDeadline(); err != nil {
		return errors.Join(err, waitErr, ignoreRunnerLogLimit(copyErr))
	}
	if waitErr != nil && !errors.Is(copyErr, errRunnerLogLimit) {
		http.Error(w, "Could not read the runner logs.", http.StatusBadGateway)
		return errors.Join(waitErr, ignoreRunnerLogLimit(copyErr))
	}
	output.header()
	w.Write(output.buffered)
	return nil
}

var errRunnerLogLimit = errors.New("runner log output limit reached")

func ignoreRunnerLogLimit(err error) error {
	if errors.Is(err, errRunnerLogLimit) || errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func copyRunnerLog(source io.Reader, output *runnerLogOutput) error {
	lines := bufio.NewReaderSize(source, runnerLogChunkBytes)
	for {
		chunk, err := lines.ReadSlice('\n')
		if len(chunk) > 0 {
			chunk = runnerLogSecret.ReplaceAll(chunk, []byte("[redacted]"))
			if output.written+len(chunk) > runnerLogMaxBytes {
				if werr := output.write([]byte(runnerLogLimitMessage)); werr != nil {
					return werr
				}
				return errRunnerLogLimit
			}
			output.written += len(chunk)
			if werr := output.write(chunk); werr != nil {
				return werr
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
