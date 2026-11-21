package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func scheduleEnv(dir string, extra ...string) func(string) string {
	env := map[string]string{"RUNQUILL_BACKUP_INTERVAL": "24h", "RUNQUILL_BACKUP_DIR": dir}
	for i := 0; i < len(extra); i += 2 {
		env[extra[i]] = extra[i+1]
	}
	return func(name string) string { return env[name] }
}

func TestBackupScheduleConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")
	if cfg, err := loadBackupScheduleConfig(func(string) string { return "" }); cfg != nil || err != nil {
		t.Fatalf("unset interval enabled backups: %v", err)
	}
	cfg, err := loadBackupScheduleConfig(scheduleEnv(dir))
	if err != nil || cfg.interval != 24*time.Hour || cfg.keep != 7 || cfg.passphrase != nil || cfg.s3 != nil {
		t.Fatalf("defaults %+v, %v", cfg, err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("backup directory %v, %v", info, err)
	}
	for name, getenv := range map[string]func(string) string{
		"interval":          scheduleEnv(dir, "RUNQUILL_BACKUP_INTERVAL", "daily"),
		"short interval":    scheduleEnv(dir, "RUNQUILL_BACKUP_INTERVAL", "10s"),
		"no dir":            scheduleEnv(""),
		"keep":              scheduleEnv(dir, "RUNQUILL_BACKUP_KEEP", "0"),
		"short passphrase":  scheduleEnv(dir, "RUNQUILL_BACKUP_PASSPHRASE", "short"),
		"plaintext upload":  scheduleEnv(dir, "RUNQUILL_BACKUP_S3_ENDPOINT", "https://s3.example.com", "RUNQUILL_BACKUP_S3_BUCKET", "bucket", "RUNQUILL_BACKUP_S3_ACCESS_KEY_ID", "id", "RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY", "secret"),
		"incomplete bucket": scheduleEnv(dir, "RUNQUILL_BACKUP_S3_BUCKET", "bucket"),
	} {
		if _, err := loadBackupScheduleConfig(getenv); err == nil {
			t.Fatalf("%s: accepted invalid config", name)
		}
	}
	for _, content := range []string{"", "\n"} {
		file := filepath.Join(t.TempDir(), "passphrase")
		os.WriteFile(file, []byte(content), 0600)
		if _, err := loadBackupScheduleConfig(scheduleEnv(dir, "RUNQUILL_BACKUP_PASSPHRASE_FILE", file)); err == nil {
			t.Fatalf("empty passphrase file %q fell back to unsealed backups", content)
		}
	}
	if _, err := loadBackupScheduleConfig(scheduleEnv(dir, "RUNQUILL_BACKUP_PASSPHRASE_FILE", filepath.Join(dir, "missing"))); err == nil {
		t.Fatal("missing passphrase file fell back to unsealed backups")
	}
	if _, err := loadBackupScheduleConfig(scheduleEnv(dir, "RUNQUILL_BACKUP_S3_ENDPOINT", "https://s3.example.com", "RUNQUILL_BACKUP_S3_BUCKET", "bucket",
		"RUNQUILL_BACKUP_S3_ACCESS_KEY_ID", "id", "RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY", "secret", "RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT", "true")); err != nil {
		t.Fatalf("explicit plaintext upload: %v", err)
	}
}

func TestScheduledBackupsVerifyPruneAndRestore(t *testing.T) {
	ctx := t.Context()
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	root := t.TempDir()
	live := filepath.Join(root, "live.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	if _, err := q.Exec("INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at) VALUES (3, 'owner/repo', 8, 9, 'runquill-12-1-scheduled', 0, 123)"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "backups")
	cfg, err := loadBackupScheduleConfig(scheduleEnv(dir, "RUNQUILL_BACKUP_KEEP", "2", "RUNQUILL_BACKUP_PASSPHRASE", testPassphrase))
	if err != nil {
		t.Fatal(err)
	}
	unrelated := []string{"notes.txt", "runquill-queue-manual.sqlite3", "runquill-queue-20200101T000000Z.sqlite3.bak"}
	for _, name := range unrelated {
		os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0600)
	}
	os.Mkdir(filepath.Join(dir, backupStagePrefix+"stale"), 0700)
	clock := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	status := &backupStatus{}
	deps := backupScheduleDeps{config: cfg, database: live, status: status, now: func() time.Time { return clock }}
	for range 3 {
		runScheduledBackup(ctx, deps)
		clock = clock.Add(time.Hour)
	}
	kept, err := listLocalBackups(dir)
	if err != nil || strings.Join(kept, ",") != "runquill-queue-20260601T010000Z.sqlite3.sealed,runquill-queue-20260601T020000Z.sqlite3.sealed" {
		t.Fatalf("kept %v, %v", kept, err)
	}
	for _, name := range unrelated {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("pruned an unrelated file %s", name)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), backupStagePrefix) {
			t.Fatalf("staging directory %s remains", entry.Name())
		}
	}
	snapshot := status.Snapshot()
	info, _ := os.Stat(filepath.Join(dir, kept[1]))
	if snapshot.Error != "" || snapshot.Name != kept[1] || snapshot.Size != info.Size() || !snapshot.Sealed ||
		!snapshot.LastSuccess.Equal(time.Date(2026, 6, 1, 2, 0, 0, 0, time.UTC)) || snapshot.Remote != "" {
		t.Fatalf("status %+v", snapshot)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions %o", info.Mode().Perm())
	}

	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE", testPassphrase)
	target := filepath.Join(root, "recovered.sqlite3")
	if err := restoreQueue(ctx, io.Discard, filepath.Join(dir, kept[1]), target); err != nil {
		t.Fatal(err)
	}
	if err := activateQueue(ctx, io.Discard, target); err != nil {
		t.Fatal(err)
	}

	// A failed run keeps the last good backup and records the error.
	deps.database = filepath.Join(root, "missing.sqlite3")
	logs := captureLogs(t)
	runScheduledBackup(ctx, deps)
	failed := status.Snapshot()
	if failed.Error == "" || failed.Name != kept[1] || !failed.LastAttempt.Equal(clock) || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("failure status %+v, logs %s", failed, logs.String())
	}
	if after, _ := listLocalBackups(dir); len(after) != 2 {
		t.Fatalf("failed run changed kept backups: %v", after)
	}
}

func TestScheduledBackupUnsealedVerifiesCopy(t *testing.T) {
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	root := t.TempDir()
	live := filepath.Join(root, "live.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	cfg, err := loadBackupScheduleConfig(scheduleEnv(filepath.Join(root, "backups")))
	if err != nil {
		t.Fatal(err)
	}
	status := &backupStatus{}
	runScheduledBackup(t.Context(), backupScheduleDeps{config: cfg, database: live, status: status})
	snapshot := status.Snapshot()
	if snapshot.Error != "" || snapshot.Sealed || !strings.HasSuffix(snapshot.Name, ".sqlite3") {
		t.Fatalf("status %+v", snapshot)
	}
	identity, _ := currentQueueBackupIdentity()
	if err := verifyQueueBackup(t.Context(), filepath.Join(cfg.dir, snapshot.Name), identity); err != nil {
		t.Fatal(err)
	}
	if err := verifyQueueBackup(t.Context(), live, identity); err == nil {
		t.Fatal("verified a live queue as a backup")
	}
}

func TestBackupScheduleTiming(t *testing.T) {
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	cfg, err := loadBackupScheduleConfig(scheduleEnv(t.TempDir(), "RUNQUILL_BACKUP_INTERVAL", "6h"))
	if err != nil {
		t.Fatal(err)
	}
	waits := make(chan time.Duration)
	fire := make(chan time.Time)
	status := &backupStatus{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBackupSchedule(ctx, backupScheduleDeps{config: cfg, database: filepath.Join(t.TempDir(), "missing.sqlite3"), status: status,
			firstDelay: 30 * time.Second, after: func(d time.Duration) <-chan time.Time { waits <- d; return fire }})
	}()
	if wait := <-waits; wait != 30*time.Second {
		t.Fatalf("first wait %s", wait)
	}
	fire <- time.Now()
	if wait := <-waits; wait != 6*time.Hour {
		t.Fatalf("interval wait %s", wait)
	}
	if status.Snapshot().Error == "" {
		t.Fatal("failed run was not recorded")
	}
	cancel()
	<-done
	runBackupSchedule(ctx, backupScheduleDeps{}) // disabled config returns at once
}

func TestBackupScheduleWaitsForIntervalAfterRestart(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadBackupScheduleConfig(scheduleEnv(dir, "RUNQUILL_BACKUP_INTERVAL", "6h"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	firstWait := func() time.Duration {
		t.Helper()
		waits := make(chan time.Duration, 1)
		ctx, cancel := context.WithCancel(t.Context())
		runBackupSchedule(ctx, backupScheduleDeps{config: cfg, now: func() time.Time { return clock }, firstDelay: time.Minute,
			after: func(d time.Duration) <-chan time.Time { waits <- d; cancel(); return nil }})
		return <-waits
	}
	if got := firstWait(); got != time.Minute {
		t.Fatalf("no backup yet: first wait %s", got)
	}
	// Only the newest backup name counts.
	os.WriteFile(filepath.Join(dir, "runquill-queue-manual.sqlite3"), []byte("x"), 0600)
	os.WriteFile(filepath.Join(dir, backupObjectName(clock.Add(-30*time.Hour), false)), []byte("x"), 0600)
	os.WriteFile(filepath.Join(dir, backupObjectName(clock.Add(-2*time.Hour), true)), []byte("x"), 0600)
	if got := firstWait(); got != 4*time.Hour {
		t.Fatalf("recent backup: first wait %s, want 4h", got)
	}
	clock = clock.Add(4*time.Hour - 10*time.Second)
	if got := firstWait(); got != time.Minute {
		t.Fatalf("backup almost due: first wait %s", got)
	}
	clock = clock.Add(24 * time.Hour)
	if got := firstWait(); got != time.Minute {
		t.Fatalf("overdue backup: first wait %s", got)
	}
	clock = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) // the clock moved back
	if got := firstWait(); got != time.Minute {
		t.Fatalf("backup from the future: first wait %s", got)
	}
}
