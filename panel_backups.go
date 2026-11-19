package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
}

type backupRemoteSummary struct {
	Configured   bool   `json:"configured"`
	Bucket       string `json:"bucket,omitempty"`
	EndpointHost string `json:"endpoint_host,omitempty"`
	Region       string `json:"region,omitempty"`
	Prefix       string `json:"prefix,omitempty"`
	Keep         int    `json:"keep"` // 0 never prunes
}

// registerBackupRoutes serves the scheduled-backup status to admins. The
// on-demand download stays at POST /api/backup.
func registerBackupRoutes(mux *http.ServeMux, a *panelAccess) {
	mux.HandleFunc("GET /api/backups/status", a.require("admin", func(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
		w.Header().Set("Cache-Control", "no-store")
		panelJSON(w, http.StatusOK, backupStatusSummary(os.Getenv, scheduledBackups.Snapshot()))
	}))
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
