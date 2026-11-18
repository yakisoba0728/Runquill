package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	workerHostPrefix = "agent://"
	workerRoute      = "/worker/v1/{name}/poll"
	workerPollWait   = 4 * time.Second
	workerPresence   = 15 * time.Second
	workerSessionTTL = 45 * time.Second
	workerTaskTTL    = 25 * time.Second
	workerBodyLimit  = 256 << 10
	workerJITLimit   = 2 << 20
)

const workerObservationSchema = `
CREATE TABLE IF NOT EXISTS worker_observations (
    host TEXT PRIMARY KEY,
    engine TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    observation TEXT NOT NULL
)`

type workerAuthority struct {
	Engine string `json:"engine_id"`
	Digest string `json:"token_sha256"`
}

type workerObservation struct {
	Image      string            `json:"image"`
	ImageReady bool              `json:"image_ready"`
	Containers map[string]string `json:"containers"`
}

type workerReport struct {
	Engine      string            `json:"engine_id"`
	Session     string            `json:"session"`
	Observation workerObservation `json:"observation"`
	Result      *workerResult     `json:"result,omitempty"`
}

type workerResult struct {
	ID string `json:"id"`
	OK bool   `json:"ok"`
}

type workerTask struct {
	ID        string    `json:"id"`
	Operation string    `json:"operation"`
	Name      string    `json:"name"`
	Image     string    `json:"image,omitempty"`
	JIT       string    `json:"jit,omitempty"`
	CPUs      float64   `json:"cpus,omitempty"`
	MemoryMiB int64     `json:"memory_mib,omitempty"`
	Expires   time.Time `json:"expires_at"`
}

type workerClaim struct {
	task      workerTask
	delivered bool
	done      chan error
}

type remoteWorker struct {
	authority workerAuthority
	mu        sync.Mutex
	wake      chan struct{}
	session   string
	polling   bool
	seen      time.Time
	snapshot  workerObservation
	pending   *workerClaim
}

type workerFleet struct {
	hosts   map[string]*remoteWorker
	db      *sql.DB
	proxyIP netip.Addr
}

func loadWorkerFleet(hosts []docker, authFile string) (*workerFleet, error) {
	want := make(map[string]bool)
	for _, h := range hosts {
		if name := h.agentName(); name != "" {
			want[name] = true
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	if authFile == "" {
		return nil, errors.New("set RUNQUILL_WORKER_AUTH_FILE for agent:// Docker hosts")
	}
	file, err := os.Open(authFile)
	if err != nil {
		return nil, fmt.Errorf("open worker auth file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 64<<10 {
		return nil, errors.New("worker auth file must be a regular file under 64 KiB without group or world write access")
	}
	authorities := make(map[string]workerAuthority)
	if err := decodeWorkerJSON(io.LimitReader(file, (64<<10)+1), &authorities); err != nil {
		return nil, errors.New("worker auth file must be a JSON object mapping names to engine_id and token_sha256")
	}
	if len(authorities) != len(want) {
		return nil, errors.New("worker auth file must cover exactly the configured agent:// hosts")
	}
	fleet := &workerFleet{hosts: make(map[string]*remoteWorker, len(authorities))}
	if raw := os.Getenv("RUNQUILL_WORKER_PROXY_IP"); raw != "" {
		ip, err := netip.ParseAddr(raw)
		if err != nil || ip.Zone() != "" || ip.IsUnspecified() {
			return nil, errors.New("RUNQUILL_WORKER_PROXY_IP must be the IP address of a trusted TLS proxy")
		}
		fleet.proxyIP = ip.Unmap()
	}
	engines, digests := make(map[string]bool), make(map[string]bool)
	for name, a := range authorities {
		if !want[name] {
			return nil, errors.New("worker auth file names must match the configured agent:// hosts")
		}
		if !validEngineID(a.Engine) || !validHex32(a.Digest) {
			return nil, fmt.Errorf("invalid engine_id or token_sha256 for worker %s", name)
		}
		if engines[a.Engine] || digests[a.Digest] {
			return nil, errors.New("worker hosts must use distinct Docker engines and credentials")
		}
		engines[a.Engine], digests[a.Digest] = true, true
		fleet.hosts[name] = &remoteWorker{authority: a, wake: make(chan struct{}, 1)}
	}
	return fleet, nil
}

func (f *workerFleet) attach(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, workerObservationSchema); err != nil {
		return fmt.Errorf("initialize worker observations: %w", err)
	}
	// A previous manager could have been stopped while a worker was starting a runner.
	if _, err := db.ExecContext(ctx, "DELETE FROM worker_observations"); err != nil {
		return fmt.Errorf("reset worker observations: %w", err)
	}
	f.db = db
	return nil
}

func validEngineID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == ':' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func validHex32(token string) bool {
	if len(token) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil && token == strings.ToLower(token)
}

func validWorkerImage(image string) bool {
	return len(image) > 0 && len(image) <= 256 && !strings.HasPrefix(image, "-") && !strings.ContainsAny(image, " \t\r\n\x00")
}

func decodeWorkerJSON(r io.Reader, dst any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func (f *workerFleet) route(fallback http.Handler) http.Handler {
	if f == nil {
		return fallback
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+workerRoute, f.poll)
	mux.Handle("/", fallback)
	return mux
}

func (f *workerFleet) poll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !f.secureRequest(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	host := f.hosts[name]
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if host == nil || !ok || !validHex32(token) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	digest := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(digest[:])), []byte(host.authority.Digest)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var report workerReport
	if err := decodeWorkerJSON(http.MaxBytesReader(w, r.Body, workerBodyLimit), &report); err != nil || !validWorkerReport(report) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if report.Engine != host.authority.Engine {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	now := time.Now()
	host.mu.Lock()
	if host.polling || (host.session != "" && report.Session != host.session && now.Sub(host.seen) < workerSessionTTL) {
		host.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	host.polling = true
	host.session, host.seen, host.snapshot = report.Session, now, report.Observation
	host.finishClaim(report.Result, now)
	if host.pending == nil {
		if err := f.storeObservation(r.Context(), name, host); err != nil {
			host.polling = false
			host.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	host.mu.Unlock()

	defer func() {
		host.mu.Lock()
		host.polling = false
		host.mu.Unlock()
	}()
	timeout := time.NewTimer(workerPollWait)
	defer timeout.Stop()
	for {
		host.mu.Lock()
		claim := host.pending
		if claim != nil && time.Now().Before(claim.task.Expires) {
			claim.delivered = true
			task := claim.task
			host.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)
			return
		}
		host.mu.Unlock()
		select {
		case <-host.wake:
		case <-r.Context().Done():
			return
		case <-timeout.C:
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
}

func (f *workerFleet) secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	host := r.Host
	if bare, _, err := net.SplitHostPort(host); err == nil {
		host = bare
	}
	if hostIP, err := netip.ParseAddr(host); err == nil && hostIP.IsLoopback() && ip.IsLoopback() {
		return true
	}
	trustedProxy := ip.IsLoopback() || ip == f.proxyIP
	return trustedProxy && r.Header.Get("X-Forwarded-Proto") == "https"
}

func validWorkerIdentifier(id string) bool {
	return len(id) >= 26 && len(id) <= 64 && validEngineID(id)
}

func validWorkerReport(report workerReport) bool {
	if !validWorkerIdentifier(report.Session) || !validWorkerImage(report.Observation.Image) {
		return false
	}
	if report.Result != nil && !validWorkerIdentifier(report.Result.ID) {
		return false
	}
	for name, state := range report.Observation.Containers {
		if _, valid := runnerJobID(name); !valid {
			return false
		}
		switch state {
		case "created", "running", "paused", "restarting", "removing", "exited", "dead":
		default:
			return false
		}
	}
	return true
}

func (h *remoteWorker) finishClaim(result *workerResult, now time.Time) {
	claim := h.pending
	if claim == nil {
		return
	}
	var err error
	switch {
	case claim.delivered && result != nil && result.ID == claim.task.ID:
		if !result.OK {
			err = fmt.Errorf("worker failed to %s the runner. Check its local logs", claim.task.Operation)
		}
	case now.After(claim.task.Expires.Add(2 * time.Second)):
		err = errors.New("worker claim expired")
	default:
		return
	}
	h.pending = nil
	claim.done <- err
}

func (f *workerFleet) storeObservation(ctx context.Context, name string, h *remoteWorker) error {
	if f.db == nil {
		return nil
	}
	data, err := json.Marshal(h.snapshot)
	if err != nil {
		return err
	}
	_, err = f.db.ExecContext(ctx, `INSERT INTO worker_observations (host, engine, observed_at, observation)
      VALUES (?, ?, ?, ?) ON CONFLICT(host) DO UPDATE SET
      engine = excluded.engine, observed_at = excluded.observed_at, observation = excluded.observation`,
		name, h.authority.Engine, h.seen.Unix(), string(data))
	return err
}

func (f *workerFleet) execute(ctx context.Context, host docker, task workerTask) error {
	name := host.agentName()
	h := f.hosts[name]
	now := time.Now()
	task.ID, task.Expires = rand.Text(), now.Add(workerTaskTTL)
	if !validWorkerTask(task) {
		return errors.New("invalid worker task")
	}
	ctx, cancel := context.WithDeadline(ctx, task.Expires)
	defer cancel()
	h.mu.Lock()
	if h.pending != nil || h.snapshot.Containers == nil || now.Sub(h.seen) >= workerPresence {
		h.mu.Unlock()
		return errors.New("worker is offline or already processing a claim")
	}
	if task.Operation == "start" && (!h.snapshot.ImageReady || h.snapshot.Image != task.Image) {
		h.mu.Unlock()
		return errors.New("the worker has no matching, ready runner image")
	}
	if f.db != nil {
		if _, err := f.db.ExecContext(ctx, "DELETE FROM worker_observations WHERE host = ?", name); err != nil {
			h.mu.Unlock()
			return fmt.Errorf("invalidate worker observation: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		h.mu.Unlock()
		return err
	}
	claim := &workerClaim{task: task, done: make(chan error, 1)}
	h.pending = claim
	select {
	case h.wake <- struct{}{}:
	default:
	}
	h.mu.Unlock()
	select {
	case err := <-claim.done:
		return err
	case <-ctx.Done():
		h.mu.Lock()
		if h.pending == claim && !claim.delivered {
			h.pending = nil
		}
		h.mu.Unlock()
		return ctx.Err()
	}
}

func validWorkerTask(task workerTask) bool {
	_, nameOK := runnerJobID(task.Name)
	if !nameOK || !validWorkerIdentifier(task.ID) || task.Expires.IsZero() {
		return false
	}
	switch task.Operation {
	case "start":
		return validWorkerImage(task.Image) && len(task.JIT) > 0 && len(task.JIT) <= workerJITLimit &&
			task.CPUs >= 0 && task.CPUs <= 1_000_000 && (task.CPUs == 0 || task.CPUs >= 0.01) &&
			task.MemoryMiB >= 0 && task.MemoryMiB <= (1<<63-1)>>20 && (task.MemoryMiB == 0 || task.MemoryMiB >= 6)
	case "remove":
		return task.Image == "" && task.JIT == "" && task.CPUs == 0 && task.MemoryMiB == 0
	default:
		return false
	}
}

func (f *workerFleet) snapshot(ctx context.Context, target docker) dockerHostSnapshot {
	name := target.agentName()
	h := f.hosts[name]
	result := dockerHostSnapshot{Name: target.label(), Pool: target.pool}
	var seen time.Time
	var observation workerObservation
	if f.db != nil {
		h.mu.Lock()
		if h.pending == nil {
			seen, observation = h.seen, h.snapshot
			observation.Containers = maps.Clone(observation.Containers)
		}
		h.mu.Unlock()
	} else {
		var recorded int64
		var raw string
		db, err := openQueueDB(env("RUNQUILL_DB", "runquill.sqlite3"), "ro")
		if err == nil {
			defer db.Close()
			err = db.QueryRowContext(ctx, `SELECT observed_at, observation FROM worker_observations
                WHERE host = ? AND engine = ?`, name, h.authority.Engine).Scan(&recorded, &raw)
		}
		if err == nil && json.Unmarshal([]byte(raw), &observation) == nil {
			seen = time.Unix(recorded, 0)
		}
	}
	if seen.IsZero() || time.Since(seen) >= workerPresence || observation.Containers == nil {
		result.Err = errors.New("worker inventory unavailable. Check that the agent is connected and its Docker engine is reachable")
		return result
	}
	result.Containers = observation.Containers
	if target.image != "" && (!observation.ImageReady || observation.Image != target.image) {
		result.Err = fmt.Errorf("worker runner image %s is not ready or does not match RUNQUILL_RUNNER_IMAGE on the worker", target.image)
	}
	return result
}
