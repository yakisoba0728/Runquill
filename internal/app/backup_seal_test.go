package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// Span multiple sealed chunks so late authentication failure happens after plaintext has been written.
	failure := strings.Repeat("x", 128<<10)
	if _, err := q.Exec("INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at, failure) VALUES (7, 'owner/repo', 8, 9, 'runquill-12-1-sealed', 0, 123, ?)", failure); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	if _, err := q.Exec("INSERT INTO worker_authorities (host, engine, token_digest) VALUES ('builder', 'test-engine', ?)", digest); err != nil {
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
	data, err := os.ReadFile(sealed)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(damaged, data[:len(data)-1], 0600); err != nil {
		t.Fatal(err)
	}
	if err := runQueueBackupWithDB(t, target, "restore", "--from", damaged, "--passphrase-file", passFile); !errors.Is(err, backupseal.ErrAuthentication) {
		t.Fatalf("truncated backup: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
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
	if jobs[0].failure != failure {
		t.Fatalf("restored job failure length %d, want %d", len(jobs[0].failure), len(failure))
	}
	var restoredDigest string
	if err := restored.QueryRow("SELECT token_digest FROM worker_authorities WHERE host = 'builder'").Scan(&restoredDigest); err != nil || restoredDigest != digest {
		t.Fatalf("sealed restore lost the worker authority: digest matched %t, %v", restoredDigest == digest, err)
	}
}

func TestPlaintextRestoreNeedsConsentWhileSealing(t *testing.T) {
	ctx := t.Context()
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	fake, _, env := newTestS3(t, true)
	for name, value := range env {
		t.Setenv(name, value)
	}
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE", "")
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE_FILE", "")
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	if _, err := q.Exec("INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at) VALUES (9, 'owner/repo', 8, 9, 'runquill-12-1-plain', 0, 123)"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec("INSERT INTO worker_authorities (host, engine, token_digest) VALUES ('builder', 'attacker-engine', ?)", strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "runquill-queue-20260101T000000Z.sqlite3")
	if err := backupQueue(ctx, io.Discard, live, plain); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	// Anyone who can write to the bucket can plant an unsealed file, even under a sealed name.
	fake.mu.Lock()
	fake.objects["fleet/a/runquill-queue-20260101T000000Z.sqlite3"] = data
	fake.objects["fleet/a/runquill-queue-20260102T000000Z.sqlite3.sealed"] = data
	fake.mu.Unlock()
	passFile := filepath.Join(dir, "passphrase")
	os.WriteFile(passFile, []byte(testPassphrase), 0600)
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, nil, 0600)

	target := filepath.Join(dir, "recovered.sqlite3")
	for _, tc := range []struct {
		env  string
		args []string
	}{
		{"", []string{"--from", plain, "--passphrase-file", passFile}},
		{"", []string{"--from", plain, "--passphrase-file", empty}},
		{"", []string{"--from", plain, "--passphrase-file", filepath.Join(dir, "missing")}},
		{testPassphrase, []string{"--from", plain}},
		{testPassphrase, []string{"--from-s3", "runquill-queue-20260101T000000Z.sqlite3"}},
		{testPassphrase, []string{"--from-s3", "runquill-queue-20260102T000000Z.sqlite3.sealed"}},
	} {
		t.Setenv("RUNQUILL_BACKUP_PASSPHRASE", tc.env)
		if err := runQueueBackupWithDB(t, target, append([]string{"restore"}, tc.args...)...); !errors.Is(err, errPlaintextRestore) {
			t.Fatalf("restore %v with a passphrase: %v", tc.args, err)
		}
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused restore %v wrote the queue", tc.args)
		}
	}
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE", "")
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE_FILE", passFile)
	if err := runQueueBackupWithDB(t, target, "restore", "--from", plain); !errors.Is(err, errPlaintextRestore) {
		t.Fatalf("restore with RUNQUILL_BACKUP_PASSPHRASE_FILE: %v", err)
	}
	if err := runQueueBackupWithDB(t, target, "restore", "--activate", "--allow-plaintext"); err == nil {
		t.Fatal("accepted --allow-plaintext with --activate")
	}

	for i, args := range [][]string{
		{"--from", plain, "--allow-plaintext"},
		{"--from-s3", "runquill-queue-20260101T000000Z.sqlite3", "--allow-plaintext"},
	} {
		target := filepath.Join(dir, fmt.Sprintf("allowed-%d.sqlite3", i))
		if err := runQueueBackupWithDB(t, target, append([]string{"restore"}, args...)...); err != nil {
			t.Fatalf("restore %v: %v", args, err)
		}
		assertPlaintextRestore(t, target)
	}
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE_FILE", "")
	target = filepath.Join(dir, "unconfigured.sqlite3")
	if err := runQueueBackupWithDB(t, target, "restore", "--from", plain); err != nil {
		t.Fatalf("restore without a configured passphrase: %v", err)
	}
	assertPlaintextRestore(t, target)
}

func assertPlaintextRestore(t *testing.T, target string) {
	t.Helper()
	if err := activateQueue(t.Context(), io.Discard, target); err != nil {
		t.Fatal(err)
	}
	restored, err := openQueue(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	var jobs, authorities int
	if err := restored.QueryRow("SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM worker_authorities)").Scan(&jobs, &authorities); err != nil || jobs != 1 || authorities != 0 {
		t.Fatalf("plaintext restore: %d jobs, %d worker authorities, %v", jobs, authorities, err)
	}
}

func TestVerifyLocalBackupUnsealsBesideBackup(t *testing.T) {
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE", testPassphrase)
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE_FILE", "")
	root := t.TempDir()
	live := filepath.Join(root, "live.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	dir := filepath.Join(root, "backups")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := backupObjectName(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), true)
	if err := sealedBackupCommand(t.Context(), io.Discard, live, filepath.Join(dir, name), ""); err != nil {
		t.Fatal(err)
	}
	stale, fresh := filepath.Join(dir, backupVerifyPrefix+"stale"), filepath.Join(dir, backupVerifyPrefix+"fresh")
	for _, d := range []string{stale, fresh} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-3 * panelBackupTimeout)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// An unusable system temporary directory must not matter.
	missing := filepath.Join(root, "no-tmp")
	t.Setenv("TMPDIR", missing)
	t.Setenv("TMP", missing)
	t.Setenv("TEMP", missing)
	if err := verifyLocalBackup(t.Context(), filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	if strings.Join(left, ",") != backupVerifyPrefix+"fresh,"+name {
		t.Fatalf("backup directory after verification: %v", left)
	}
	if names, _ := listLocalBackups(dir); len(names) != 1 {
		t.Fatalf("verification left files counted as backups: %v", names)
	}
}

func runQueueBackupWithDB(t *testing.T, db string, args ...string) error {
	t.Helper()
	t.Setenv("RUNQUILL_DB", db)
	return runQueueBackup(t.Context(), io.Discard, args[0], args[1:])
}
