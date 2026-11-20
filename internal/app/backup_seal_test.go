package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yakisoba0728/Runquill/internal/backupseal"
)

const testPassphrase = "correct horse battery staple"

func TestBackupPassphraseSources(t *testing.T) {
	env := map[string]string{}
	getenv := func(name string) string { return env[name] }
	if pass, err := backupPassphrase(getenv, ""); err != nil || pass != nil {
		t.Fatalf("unset passphrase: %q, %v", pass, err)
	}
	file := filepath.Join(t.TempDir(), "passphrase")
	os.WriteFile(file, []byte(testPassphrase+"\n"), 0600)
	if pass, err := backupPassphrase(getenv, file); err != nil || string(pass) != testPassphrase {
		t.Fatalf("file passphrase: %q, %v", pass, err)
	}
	env["RUNQUILL_BACKUP_PASSPHRASE"] = "from-env-passphrase"
	if pass, err := backupPassphrase(getenv, ""); err != nil || string(pass) != "from-env-passphrase" {
		t.Fatalf("env passphrase: %q, %v", pass, err)
	}
	env["RUNQUILL_BACKUP_PASSPHRASE_FILE"] = file
	if _, err := backupPassphrase(getenv, ""); err == nil {
		t.Fatal("accepted two passphrase sources")
	}
	if err := backupseal.Encrypt(io.Discard, strings.NewReader("x"), []byte("short")); err == nil {
		t.Fatal("sealed with a short passphrase")
	}
}

func TestSealedBackupRestoresEndToEnd(t *testing.T) {
	ctx := t.Context()
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE", "")
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	if _, err := q.Exec("INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at) VALUES (7, 'owner/repo', 8, 9, 'runquill-12-1-sealed', 0, 123)"); err != nil {
		t.Fatal(err)
	}
	passFile := filepath.Join(dir, "passphrase")
	os.WriteFile(passFile, []byte(testPassphrase), 0600)
	sealed := filepath.Join(dir, "backup.sealed")
	if err := runQueueBackupWithDB(t, live, "backup", "--output", sealed, "--passphrase-file", passFile); err == nil {
		t.Fatal("accepted a passphrase file without --seal")
	}
	if err := sealedBackupCommand(ctx, io.Discard, live, sealed, ""); !errors.Is(err, backupseal.ErrMissingPassphrase) {
		t.Fatalf("sealed without a passphrase: %v", err)
	}
	if _, err := os.Stat(sealed); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote a backup without a passphrase")
	}
	if err := runQueueBackupWithDB(t, live, "backup", "--output", sealed, "--seal", "--passphrase-file", passFile); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(sealed); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("sealed backup: %v, %v", info, err)
	}
	data, _ := os.ReadFile(sealed)
	if !bytes.HasPrefix(data, []byte(backupseal.Magic)) || bytes.Contains(data, []byte("owner/repo")) {
		t.Fatal("backup is not sealed")
	}
	if err := runQueueBackupWithDB(t, live, "backup", "--output", sealed, "--seal", "--passphrase-file", passFile); err == nil {
		t.Fatal("overwrote an existing sealed backup")
	}

	target := filepath.Join(dir, "recovered.sqlite3")
	if err := restoreQueue(ctx, io.Discard, sealed, target); !errors.Is(err, backupseal.ErrMissingPassphrase) {
		t.Fatalf("restored a sealed backup without a passphrase: %v", err)
	}
	wrong := filepath.Join(dir, "wrong")
	os.WriteFile(wrong, []byte("not the passphrase"), 0600)
	if err := runQueueBackupWithDB(t, target, "restore", "--from", sealed, "--passphrase-file", wrong); !errors.Is(err, backupseal.ErrAuthentication) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	damaged := filepath.Join(dir, "damaged.sealed")
	os.WriteFile(damaged, data[:len(data)-1], 0600)
	if err := runQueueBackupWithDB(t, target, "restore", "--from", damaged, "--passphrase-file", passFile); !errors.Is(err, backupseal.ErrAuthentication) {
		t.Fatalf("truncated backup: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.Name() == "recovered.sqlite3" || strings.HasPrefix(entry.Name(), ".runquill-") {
			t.Fatalf("failed restore left %s", entry.Name())
		}
	}
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE", testPassphrase)
	if err := runQueueBackupWithDB(t, target, "restore", "--from", sealed); err != nil {
		t.Fatal(err)
	}
	if err := activateQueue(ctx, io.Discard, target); err != nil {
		t.Fatal(err)
	}
	restored, err := openQueue(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	jobs, err := restored.jobs(ctx, "ORDER BY sequence")
	if err != nil || len(jobs) != 1 || jobs[0].id != 7 {
		t.Fatalf("restored jobs %+v, %v", jobs, err)
	}
}

func runQueueBackupWithDB(t *testing.T, db string, args ...string) error {
	t.Helper()
	t.Setenv("RUNQUILL_DB", db)
	return runQueueBackup(t.Context(), io.Discard, args[0], args[1:])
}
