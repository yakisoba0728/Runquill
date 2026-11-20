package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const workerEnrollmentRoute = "/worker/v1/{name}/enroll"
const workerEnrollmentTTL = 10 * time.Minute

const workerAuthoritySchema = `CREATE TABLE IF NOT EXISTS worker_authorities (
    host TEXT PRIMARY KEY NOT NULL,
    engine TEXT NOT NULL UNIQUE,
    token_digest TEXT NOT NULL UNIQUE
)`

const workerEnrollmentSchema = `CREATE TABLE IF NOT EXISTS worker_enrollment_codes (
    host TEXT PRIMARY KEY NOT NULL,
    code_digest TEXT NOT NULL UNIQUE,
    expires_at INTEGER NOT NULL
)`

type workerEnrollmentCommand struct {
	Command      string    `json:"command"`
	StartCommand string    `json:"start_command"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func workerSecret() string {
	var secret [32]byte
	rand.Read(secret[:])
	return hex.EncodeToString(secret[:])
}

func workerDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (f *workerFleet) loadEnrolledWorkers(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, workerEnrollmentSchema); err != nil {
		return fmt.Errorf("initialize worker enrollment: %w", err)
	}
	rows, err := db.QueryContext(ctx, "SELECT host, engine, token_digest FROM worker_authorities")
	if err != nil {
		return err
	}
	engines, digests := map[string]bool{}, map[string]bool{}
	var manual []string
	for name, worker := range f.hosts {
		if authority := worker.authority.Load(); authority != nil {
			manual = append(manual, name)
			engines[authority.Engine], digests[authority.Digest] = true, true
		}
	}
	for rows.Next() {
		var name string
		var authority workerAuthority
		if err := rows.Scan(&name, &authority.Engine, &authority.Digest); err != nil {
			rows.Close()
			return err
		}
		worker := f.hosts[name]
		if worker == nil {
			continue
		}
		if worker.authority.Load() != nil {
			continue
		}
		if !validEngineID(authority.Engine) || !validHex32(authority.Digest) || engines[authority.Engine] || digests[authority.Digest] {
			rows.Close()
			return fmt.Errorf("invalid or conflicting saved credentials for worker %s", name)
		}
		worker.authority.Store(&authority)
		engines[authority.Engine], digests[authority.Digest] = true, true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range manual {
		if _, err := db.ExecContext(ctx, "DELETE FROM worker_enrollment_codes WHERE host = ?", name); err != nil {
			return err
		}
		// An operator who supplies a replacement file credential revokes the old one.
		if _, err := db.ExecContext(ctx, "DELETE FROM worker_authorities WHERE host = ?", name); err != nil {
			return err
		}
	}
	if f.enrollmentURL == "" {
		for name, worker := range f.hosts {
			if worker.authority.Load() == nil {
				return fmt.Errorf("worker %s needs credentials in RUNQUILL_WORKER_AUTH_FILE or a RUNQUILL_WORKER_ENROLLMENT_URL", name)
			}
		}
	}
	return nil
}

func (f *workerFleet) workerAuthority(ctx context.Context, name string) (*workerAuthority, error) {
	if authority := f.hosts[name].authority.Load(); authority != nil || f.db != nil {
		return authority, nil
	}
	// Read-only CLI processes have no attached fleet. Read the live authority from the queue.
	path := env("RUNQUILL_DB", "runquill.sqlite3")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	db, err := openQueueDB(path, "ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var table bool
	err = db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'worker_authorities')").Scan(&table)
	if err != nil || !table {
		return nil, err
	}
	var authority workerAuthority
	err = db.QueryRowContext(ctx, "SELECT engine, token_digest FROM worker_authorities WHERE host = ?", name).
		Scan(&authority.Engine, &authority.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &authority, nil
}

func (a *panelAccess) createWorkerEnrollment(w http.ResponseWriter, r *http.Request, user panelIdentity, ops *operations) {
	state := ops.current.Load()
	if state == nil {
		http.Error(w, "Worker enrollment is unavailable.", http.StatusConflict)
		return
	}
	fleet, ok := state.runners.runtime.(*dockerFleet)
	if !ok || fleet.workers == nil || fleet.workers.enrollmentURL == "" {
		http.Error(w, "Worker enrollment is unavailable.", http.StatusConflict)
		return
	}
	f := fleet.workers
	name := r.PathValue("name")
	worker := f.hosts[name]
	host, err := fleet.host(workerHostPrefix + name)
	if worker == nil || err != nil {
		http.Error(w, "Choose a configured outbound worker.", http.StatusBadRequest)
		return
	}
	f.enrollmentMu.Lock()
	defer f.enrollmentMu.Unlock()
	if worker.authority.Load() != nil {
		http.Error(w, "This worker already has credentials.", http.StatusConflict)
		return
	}
	code := workerSecret()
	expiresAt := time.Now().UTC().Add(workerEnrollmentTTL).Truncate(time.Second)
	ctx := r.Context()
	err = a.change(ctx, user, "worker.enrollment.create", host.label(), func(tx *sql.Tx) error {
		var active bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE runner_host = ? AND runner_name IS NOT NULL)
            OR EXISTS(SELECT 1 FROM docker_hosts WHERE host = ? AND untracked_slots > 0)`, host.label(), host.label()).Scan(&active)
		if err != nil {
			return err
		}
		if active {
			return errWorkerInUse
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM worker_enrollment_codes WHERE expires_at <= ?", time.Now().Unix()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO worker_enrollment_codes(host, code_digest, expires_at) VALUES (?, ?, ?)
            ON CONFLICT(host) DO UPDATE SET code_digest = excluded.code_digest, expires_at = excluded.expires_at`,
			name, workerDigest(code), expiresAt.Unix())
		return err
	})
	switch {
	case errors.Is(err, errPanelUnauthorized):
		http.Error(w, "You do not have access to this operation.", http.StatusForbidden)
	case errors.Is(err, errWorkerInUse):
		http.Error(w, "Restore the credentials for this worker before resolving its reserved runners.", http.StatusConflict)
	case err != nil:
		http.Error(w, "Could not create a worker enrollment code.", http.StatusServiceUnavailable)
	default:
		args := " --name " + quoteWorkerArg(name) + " --manager " + quoteWorkerArg(f.enrollmentURL) +
			" --token-file " + quoteWorkerArg("runquill-"+name+".token")
		start := "RUNQUILL_RUNNER_IMAGE=" + quoteWorkerArg(host.image) + " runquill worker" + args
		panelJSON(w, http.StatusCreated, workerEnrollmentCommand{
			Command:      "runquill worker enroll" + args + " --code " + quoteWorkerArg(code) + " && " + start,
			StartCommand: start,
			ExpiresAt:    expiresAt,
		})
	}
}

func quoteWorkerArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (f *workerFleet) enroll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !f.secureRequest(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	worker := f.hosts[name]
	code, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if worker == nil || !ok || !validHex32(code) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.db == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	f.enrollmentMu.Lock()
	defer f.enrollmentMu.Unlock()
	ctx := r.Context()
	digest := workerDigest(code)
	var valid bool
	if err := f.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_enrollment_codes
            WHERE host = ? AND code_digest = ? AND expires_at > ?)`, name, digest, time.Now().Unix()).Scan(&valid); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !valid || worker.authority.Load() != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var request workerAuthority
	if err := decodeWorkerJSON(http.MaxBytesReader(w, r.Body, 1024), &request); err != nil ||
		!validEngineID(request.Engine) || !validHex32(request.Digest) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, other := range f.hosts {
		if authority := other.authority.Load(); authority != nil && (authority.Engine == request.Engine || authority.Digest == request.Digest) {
			w.WriteHeader(http.StatusConflict)
			return
		}
	}
	hosts := f.configured
	if ops := f.operations; ops != nil {
		ops.topology.RLock()
		defer ops.topology.RUnlock()
		hosts = ops.current.Load().runners.runtime.(*dockerFleet).hosts
	}
	probe, cancel := context.WithTimeout(ctx, dockerHostProbeTimeout)
	defer cancel()
	for _, host := range hosts {
		if host.agentName() != "" {
			continue
		}
		engine, err := host.engineID(probe)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if engine == request.Engine {
			w.WriteHeader(http.StatusConflict)
			return
		}
	}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer tx.Rollback()
	deleted, err := tx.ExecContext(ctx, "DELETE FROM worker_enrollment_codes WHERE host = ? AND code_digest = ? AND expires_at > ?",
		name, digest, time.Now().Unix())
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if count, _ := deleted.RowsAffected(); count != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	added, err := tx.ExecContext(ctx, "INSERT INTO worker_authorities(host, engine, token_digest) VALUES (?, ?, ?) ON CONFLICT DO NOTHING",
		name, request.Engine, request.Digest)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if count, _ := added.RowsAffected(); count != 1 {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if err := tx.Commit(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	worker.authority.Store(&request)
	w.WriteHeader(http.StatusNoContent)
}
