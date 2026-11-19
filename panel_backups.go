package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// backupStatusReport is GET /api/backups/status: how scheduled backups are
// configured and how the latest run went. It carries booleans and non-secret
// values only; passphrases, access keys and endpoint credentials never leave
// the server.
type backupStatusReport struct {
	Scheduled       bool                `json:"scheduled"`
	IntervalSeconds float64             `json:"interval_seconds,omitempty"`
	Directory       string              `json:"directory,omitempty"`
	Keep            int                 `json:"keep,omitempty"`
	Sealing         bool                `json:"sealing"`
	Remote          backupRemoteSummary `json:"remote"`
	Last            backupSnapshot      `json:"last"`
	Verification    *backupVerification `json:"verification,omitempty"`
}

type backupVerification struct {
	Name      string    `json:"name"`
	CheckedAt time.Time `json:"checked_at"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
}

type backupRemoteSummary struct {
	Configured   bool   `json:"configured"`
	Bucket       string `json:"bucket,omitempty"`
	EndpointHost string `json:"endpoint_host,omitempty"`
	Region       string `json:"region,omitempty"`
	Prefix       string `json:"prefix,omitempty"`
	Keep         int    `json:"keep"` // 0 never prunes
}

func registerBackupRoutes(mux *http.ServeMux, a *panelAccess) {
	var verification atomic.Pointer[backupVerification]
	mux.HandleFunc("GET /api/backups/status", a.require("admin", func(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
		w.Header().Set("Cache-Control", "no-store")
		report := backupStatusSummary(os.Getenv, scheduledBackups.Snapshot())
		report.Verification = verification.Load()
		panelJSON(w, http.StatusOK, report)
	}))
	mux.HandleFunc("POST /api/backups/verify", a.require("admin", func(w http.ResponseWriter, r *http.Request, user panelIdentity) {
		a.verifyLatestBackup(w, r, user, &verification)
	}))
}

func (a *panelAccess) verifyLatestBackup(w http.ResponseWriter, r *http.Request, user panelIdentity, latest *atomic.Pointer[backupVerification]) {
	settings := backupStatusSummary(os.Getenv, backupSnapshot{})
	if !settings.Scheduled || settings.Directory == "" {
		http.Error(w, "Enable scheduled backups before verifying a backup.", http.StatusConflict)
		return
	}
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(panelBackupTimeout + 5*time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		http.Error(w, "Could not prepare backup verification.", http.StatusServiceUnavailable)
		return
	}
	ctx, done, ok := panelOperationContext(w, r, panelBackupTimeout)
	if !ok {
		return
	}
	defer done()
	names, err := listLocalBackups(settings.Directory)
	if err != nil {
		http.Error(w, "Could not list the local backups.", http.StatusServiceUnavailable)
		return
	}
	if len(names) == 0 {
		http.Error(w, "No local scheduled backup is available to verify.", http.StatusConflict)
		return
	}
	name := names[len(names)-1]
	if err := a.change(ctx, user, "queue.backup.verify", name, func(*sql.Tx) error { return nil }); err != nil {
		if errors.Is(err, errPanelUnauthorized) {
			http.Error(w, "You do not have access to this operation.", http.StatusForbidden)
		} else {
			http.Error(w, "Could not record the backup verification.", http.StatusServiceUnavailable)
		}
		return
	}
	err = verifyLocalBackup(ctx, filepath.Join(settings.Directory, name))
	if r.Context().Err() != nil {
		return
	}
	result := &backupVerification{Name: name, CheckedAt: time.Now().UTC(), OK: err == nil}
	if err != nil {
		result.Error = backupVerificationError(err)
	}
	latest.Store(result)
	w.Header().Set("Cache-Control", "no-store")
	panelJSON(w, http.StatusOK, result)
}

func backupVerificationError(err error) string {
	switch {
	case errors.Is(err, errBackupVerificationPassphrase):
		return "Could not load the backup passphrase for verification."
	case errors.Is(err, errSealedAuth) || errors.Is(err, errSealedHeader):
		return "The sealed backup is damaged or the passphrase is wrong."
	case errors.Is(err, errQueueBackupIdentity):
		return "The queue backup belongs to another GitHub owner or app."
	case errors.Is(err, context.DeadlineExceeded):
		return "Backup verification timed out."
	default:
		return "Could not verify the queue backup. It may be incomplete or unreadable."
	}
}

// backupStatusSummary reads the same variables as loadBackupScheduleConfig
// without its side effects (no directory creation, no file reads). The
// manager validated them at startup, so values here are already known good.
func backupStatusSummary(getenv func(string) string, last backupSnapshot) backupStatusReport {
	report := backupStatusReport{Last: last}
	report.Sealing = getenv("RUNQUILL_BACKUP_PASSPHRASE") != "" || getenv("RUNQUILL_BACKUP_PASSPHRASE_FILE") != ""
	interval, err := time.ParseDuration(strings.TrimSpace(getenv("RUNQUILL_BACKUP_INTERVAL")))
	if err != nil || interval < backupMinInterval {
		return report
	}
	report.Scheduled = true
	report.IntervalSeconds = interval.Seconds()
	if dir := strings.TrimSpace(getenv("RUNQUILL_BACKUP_DIR")); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		report.Directory = dir
	}
	report.Keep = keepSetting(getenv("RUNQUILL_BACKUP_KEEP"), defaultBackupKeep)
	endpoint, bucket := strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_ENDPOINT")), strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_BUCKET"))
	if endpoint != "" && bucket != "" {
		remote := backupRemoteSummary{Configured: true, Bucket: bucket, Region: strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_REGION")),
			Prefix: strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_PREFIX")), Keep: keepSetting(getenv("RUNQUILL_BACKUP_S3_KEEP"), defaultBackupKeep)}
		// Only the host: a URL with userinfo or a path must not leak through.
		if u, err := url.Parse(endpoint); err == nil {
			remote.EndpointHost = u.Host
		}
		if remote.Region == "" {
			remote.Region = "us-east-1"
		}
		report.Remote = remote
	}
	return report
}

func keepSetting(raw string, fallback int) int {
	if value, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && value >= 0 {
		return value
	}
	return fallback
}
