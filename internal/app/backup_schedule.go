package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yakisoba0728/Runquill/internal/backupseal"
)

const (
	backupFirstDelay  = time.Minute
	backupMinInterval = time.Minute
	backupStagePrefix = ".runquill-backup-"
)

type backupScheduleConfig struct {
	interval   time.Duration
	dir        string
	keep       int
	passphrase []byte // nil writes unsealed backups
	s3         *s3Config
}

func (c *backupScheduleConfig) secrets() []string {
	secrets := []string{string(c.passphrase)}
	if c.s3 != nil {
		secrets = append(secrets, c.s3.secrets()...)
	}
	return secrets
}

// loadBackupScheduleConfig returns nil when RUNQUILL_BACKUP_INTERVAL is unset.
// Call it at startup so invalid settings stop the manager before it serves.
func loadBackupScheduleConfig(getenv func(string) string) (*backupScheduleConfig, error) {
	raw := strings.TrimSpace(getenv("RUNQUILL_BACKUP_INTERVAL"))
	if raw == "" {
		return nil, nil
	}
	cfg := &backupScheduleConfig{keep: defaultBackupKeep}
	var err error
	cfg.interval, err = time.ParseDuration(raw)
	if err != nil || cfg.interval < backupMinInterval {
		return nil, errors.New("RUNQUILL_BACKUP_INTERVAL must be a duration of at least 1m, such as 24h")
	}
	dir := strings.TrimSpace(getenv("RUNQUILL_BACKUP_DIR"))
	if dir == "" {
		return nil, errors.New("set RUNQUILL_BACKUP_DIR to schedule backups")
	}
	if cfg.dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.dir, 0700); err != nil {
		return nil, fmt.Errorf("create RUNQUILL_BACKUP_DIR: %w", err)
	}
	if info, err := os.Stat(cfg.dir); err != nil || !info.IsDir() {
		return nil, errors.New("RUNQUILL_BACKUP_DIR must be a directory")
	}
	if raw := strings.TrimSpace(getenv("RUNQUILL_BACKUP_KEEP")); raw != "" {
		cfg.keep, err = strconv.Atoi(raw)
		if err != nil || cfg.keep < 1 || cfg.keep > 1000 {
			return nil, errors.New("RUNQUILL_BACKUP_KEEP must be an integer from 1 to 1000")
		}
	}
	if cfg.passphrase, err = backupPassphrase(getenv, ""); err != nil {
		return nil, err
	}
	// A configured but empty passphrase must stop startup, not fall back to plaintext.
	if getenv("RUNQUILL_BACKUP_PASSPHRASE") != "" || getenv("RUNQUILL_BACKUP_PASSPHRASE_FILE") != "" {
		if err := backupseal.CheckPassphrase(cfg.passphrase); err != nil {
			return nil, err
		}
	} else {
		cfg.passphrase = nil
	}
	if cfg.s3, err = loadS3Config(getenv); err != nil {
		return nil, err
	}
	if cfg.s3 != nil && cfg.passphrase == nil && !cfg.s3.allowPlaintext {
		return nil, errors.New("set RUNQUILL_BACKUP_PASSPHRASE to seal uploaded backups, or RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true to accept the risk")
	}
	if cfg.s3 != nil && cfg.passphrase == nil {
		slog.Warn("Scheduled backups will be UPLOADED UNENCRYPTED because RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true")
	}
	return cfg, nil
}

// backupSnapshot is the latest scheduled backup outcome for the panel and metrics.
type backupSnapshot struct {
	LastAttempt   time.Time `json:"last_attempt,omitzero"`
	LastSuccess   time.Time `json:"last_success,omitzero"`
	Name          string    `json:"name,omitempty"`
	Size          int64     `json:"size,omitempty"`
	Sealed        bool      `json:"sealed"`
	Error         string    `json:"error,omitempty"`
	Remote        string    `json:"remote,omitempty"` // "", "uploaded" or "failed"
	RemoteAt      time.Time `json:"remote_at,omitzero"`
	RemoteError   string    `json:"remote_error,omitempty"`
	RemotePending int       `json:"remote_pending,omitempty"`
}

type backupStatus struct {
	mu   sync.Mutex
	last backupSnapshot
}

// scheduledBackups is the status the running manager updates.
var scheduledBackups backupStatus

func (s *backupStatus) Snapshot() backupSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *backupStatus) update(change func(*backupSnapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.last)
}

type backupScheduleDeps struct {
	config     *backupScheduleConfig
	database   string
	status     *backupStatus
	s3         *s3Client
	now        func() time.Time
	after      func(time.Duration) <-chan time.Time
	firstDelay time.Duration
}

func (d *backupScheduleDeps) defaults() {
	if d.status == nil {
		d.status = &scheduledBackups
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.after == nil {
		d.after = time.After
	}
	if d.firstDelay == 0 {
		d.firstDelay = backupFirstDelay
	}
	if d.s3 == nil && d.config.s3 != nil {
		d.s3 = newS3Client(d.config.s3)
	}
}

// runBackupSchedule takes the first backup once an interval has passed since
// the newest local backup, or shortly after start, and then one per interval
// until ctx ends. Failures are logged and kept in the status.
func runBackupSchedule(ctx context.Context, deps backupScheduleDeps) {
	if deps.config == nil {
		return
	}
	deps.defaults()
	wait := firstBackupWait(deps)
	for {
		select {
		case <-ctx.Done():
			return
		case <-deps.after(wait):
		}
		runScheduledBackup(ctx, deps)
		wait = deps.config.interval
	}
}

// firstBackupWait keeps frequent restarts from replacing every good backup:
// the count-based pruning would otherwise drop the oldest copies on each start.
func firstBackupWait(deps backupScheduleDeps) time.Duration {
	latest, ok := latestLocalBackupTime(deps.config.dir)
	now := deps.now()
	if !ok || latest.After(now) { // none yet, or the clock moved back
		return deps.firstDelay
	}
	return max(latest.Add(deps.config.interval).Sub(now), deps.firstDelay)
}

// latestLocalBackupTime reads the time from the newest backup's name, which
// copying or touching the file does not change.
func latestLocalBackupTime(dir string) (time.Time, bool) {
	names, err := listLocalBackups(dir)
	if err != nil || len(names) == 0 {
		return time.Time{}, false
	}
	at, err := time.Parse("20060102T150405Z", backupFileName.FindStringSubmatch(names[len(names)-1])[1])
	return at, err == nil
}

func runScheduledBackup(ctx context.Context, deps backupScheduleDeps) {
	deps.defaults()
	defer func() {
		if recovered := recover(); recovered != nil {
			message := redactSecrets(fmt.Sprint(recovered), deps.config.secrets()...)
			slog.Warn("scheduled backup panicked", "error", message)
			deps.status.update(func(s *backupSnapshot) { s.Error = "scheduled backup panicked: " + message })
		}
	}()
	started := deps.now()
	name, size, err := takeScheduledBackup(ctx, deps, started)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		message := redactSecrets(err.Error(), deps.config.secrets()...)
		slog.Warn("scheduled backup failed", "error", message)
		deps.status.update(func(s *backupSnapshot) { s.LastAttempt, s.Error = started, message })
	} else {
		slog.Info("scheduled backup created", "name", name, "bytes", size)
		deps.status.update(func(s *backupSnapshot) {
			*s = backupSnapshot{LastAttempt: started, LastSuccess: started, Name: name, Size: size, Sealed: deps.config.passphrase != nil,
				Remote: s.Remote, RemoteAt: s.RemoteAt, RemoteError: s.RemoteError, RemotePending: s.RemotePending}
		})
	}
	if deps.s3 == nil {
		return
	}
	// Catch-up runs even when this tick's backup failed.
	local, listErr := listLocalBackups(deps.config.dir)
	result, err := deps.s3.syncRemote(ctx, deps.config.dir, local)
	err = errors.Join(listErr, err)
	if ctx.Err() != nil {
		return
	}
	deps.status.update(func(s *backupSnapshot) {
		s.RemotePending = result.pending
		if err != nil {
			s.Remote, s.RemoteError = "failed", redactSecrets(err.Error(), deps.config.secrets()...)
			return
		}
		s.Remote, s.RemoteAt, s.RemoteError = "uploaded", deps.now(), ""
	})
	if err != nil {
		slog.Warn("upload scheduled backups", "error", redactSecrets(err.Error(), deps.config.secrets()...), "pending", result.pending)
	} else if len(result.uploaded) > 0 {
		slog.Info("uploaded scheduled backups", "names", result.uploaded)
	}
}

// takeScheduledBackup writes a verified backup under a temporary name,
// publishes it under its final name and then prunes the oldest copies.
func takeScheduledBackup(ctx context.Context, deps backupScheduleDeps, at time.Time) (string, int64, error) {
	cfg := deps.config
	identity, err := currentQueueBackupIdentity()
	if err != nil {
		return "", 0, err
	}
	removeStaleBackupStages(cfg.dir)
	removeStaleVerifyDirs(cfg.dir)
	stage, err := os.MkdirTemp(cfg.dir, backupStagePrefix+"*")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(stage)
	name := backupObjectName(at, cfg.passphrase != nil)
	plain := filepath.Join(stage, "queue.sqlite3")
	if err := backupQueue(ctx, io.Discard, deps.database, plain); err != nil {
		return "", 0, err
	}
	if err := verifyQueueBackup(ctx, plain, identity); err != nil {
		return "", 0, err
	}
	artifact := plain
	if cfg.passphrase != nil {
		artifact = filepath.Join(stage, name)
		if err := sealFileTo(ctx, plain, artifact, cfg.passphrase); err != nil {
			return "", 0, err
		}
	}
	info, err := os.Stat(artifact)
	if err != nil {
		return "", 0, err
	}
	if err := publishFile(artifact, filepath.Join(cfg.dir, name)); err != nil {
		return "", 0, err
	}
	if err := pruneLocalBackups(cfg.dir, cfg.keep); err != nil {
		return name, info.Size(), fmt.Errorf("prune local backups: %w", err)
	}
	return name, info.Size(), nil
}

// removeStaleBackupStages clears staging directories an interrupted run left.
func removeStaleBackupStages(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), backupStagePrefix) {
			os.RemoveAll(filepath.Join(dir, entry.Name()))
		}
	}
}

// listLocalBackups returns this feature's regular files, oldest first.
func listLocalBackups(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && backupFileName.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sortBackupNames(names)
	return names, nil
}

func pruneLocalBackups(dir string, keep int) error {
	names, err := listLocalBackups(dir)
	if err != nil {
		return err
	}
	var failures []error
	for len(names) > keep {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
		names = names[1:]
	}
	return errors.Join(failures...)
}
