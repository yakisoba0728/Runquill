package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBackupStatusRoute(t *testing.T) {
	secrets := map[string]string{
		"RUNQUILL_BACKUP_PASSPHRASE":           "passphrase-value-7f3a",
		"RUNQUILL_BACKUP_S3_ACCESS_KEY_ID":     "access-key-value-91c2",
		"RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY": "secret-key-value-4be8",
	}
	for name, value := range secrets {
		t.Setenv(name, value)
	}
	t.Setenv("RUNQUILL_BACKUP_INTERVAL", "6h")
	t.Setenv("RUNQUILL_BACKUP_DIR", "/var/lib/runquill/backups")
	t.Setenv("RUNQUILL_BACKUP_KEEP", "3")
	// A pasted URL with credentials must only ever show its host.
	t.Setenv("RUNQUILL_BACKUP_S3_ENDPOINT", "https://userinfo-name:userinfo-pass-55d1@s3.example.com:9000")
	t.Setenv("RUNQUILL_BACKUP_S3_BUCKET", "fleet-backups")
	t.Setenv("RUNQUILL_BACKUP_S3_KEEP", "0")

	previous := scheduledBackups.Snapshot()
	t.Cleanup(func() { scheduledBackups.update(func(s *backupSnapshot) { *s = previous }) })
	at := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	scheduledBackups.update(func(s *backupSnapshot) {
		*s = backupSnapshot{LastAttempt: at, LastSuccess: at, Name: "runquill-queue-20261119T030000Z.sqlite3.sealed", Size: 4096, Sealed: true,
			Remote: "failed", RemoteError: "upload refused", RemotePending: 2}
	})

	f := newPanelFixture(t)
	mux := http.NewServeMux()
	registerBackupRoutes(mux, f.access)
	f.handler = mux

	f.expect(f.call("GET", "/api/backups/status", "", "", false), http.StatusUnauthorized)
	f.expect(f.call("GET", "/api/backups/status", "", "viewer", false), http.StatusForbidden)
	f.expect(f.call("GET", "/api/backups/status", "", "operator", false), http.StatusForbidden)
	f.expect(f.call("POST", "/api/backups/status", "", "admin", true), http.StatusMethodNotAllowed)
	rec := f.call("GET", "/api/backups/status", "", "admin", false)
	f.expect(rec, http.StatusOK)

	body := rec.Body.String()
	for _, value := range append([]string{"userinfo-name", "userinfo-pass-55d1"}, secrets["RUNQUILL_BACKUP_PASSPHRASE"],
		secrets["RUNQUILL_BACKUP_S3_ACCESS_KEY_ID"], secrets["RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY"]) {
		if strings.Contains(body, value) {
			t.Fatalf("status leaks %q: %s", value, body)
		}
	}
	var report backupStatusReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Scheduled || report.IntervalSeconds != 6*3600 || report.Directory != "/var/lib/runquill/backups" || report.Keep != 3 || !report.Sealing {
		t.Fatalf("schedule: %+v", report)
	}
	if r := report.Remote; !r.Configured || r.Bucket != "fleet-backups" || r.EndpointHost != "s3.example.com:9000" || r.Region != "us-east-1" || r.Keep != 0 {
		t.Fatalf("remote: %+v", r)
	}
	if !report.Last.LastSuccess.Equal(at) || report.Last.Size != 4096 || report.Last.Remote != "failed" || report.Last.RemotePending != 2 {
		t.Fatalf("last: %+v", report.Last)
	}
	if !strings.Contains(body, `"keep":0`) {
		t.Fatalf("remote keep 0 must be explicit: %s", body)
	}
}

func TestBackupStatusSummaryUnscheduled(t *testing.T) {
	env := map[string]string{"RUNQUILL_BACKUP_PASSPHRASE_FILE": "/run/secrets/passphrase", "RUNQUILL_BACKUP_S3_BUCKET": "b"}
	report := backupStatusSummary(func(name string) string { return env[name] }, backupSnapshot{})
	if report.Scheduled || report.Directory != "" || report.Keep != 0 || report.Remote.Configured || !report.Sealing {
		t.Fatalf("unscheduled: %+v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "/run/secrets") {
		t.Fatalf("passphrase file path leaked: %s", encoded)
	}
}
