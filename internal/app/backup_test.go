package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBackupOnlineAndRestoreToFencedQueue(t *testing.T) {
	ctx := t.Context()
	t.Setenv("RUNQUILL_OWNER", "Owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite3")
	archive := filepath.Join(dir, "backup.sqlite3")
	recovered := filepath.Join(dir, "recovered.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()

	if _, err := q.Exec("INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at, runner_name, runner_host, runner_id) VALUES (5, 'owner/repo', 8, 9, 'runquill-12-1-example', 1, 123, 'runquill-5-abcdef012345', 'local', 77)"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec("UPDATE jobs SET jit_requested_ms = 123001, jit_registered_ms = 123020, runner_launch_ms = 123490 WHERE job_id = 5"); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	if _, err := q.Exec("INSERT INTO worker_authorities (host, engine, token_digest) VALUES (?, ?, ?)", "builder", "test-engine", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(workerEnrollmentSchema + "; INSERT INTO worker_enrollment_codes (host, code_digest, expires_at) VALUES ('other', 'test-code', 9999999999)"); err != nil {
		t.Fatal(err)
	}
	if err := writeHostCordon(ctx, q.DB, "local", true); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec("INSERT INTO repository_polls VALUES (9, 8, 123, 123, 0, '')"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec("INSERT INTO operation_settings VALUES (1, 1, ?)",
		`{"slot_limit":4,"poll_interval":"30s","runner_image":"runquill-runner:local","docker_hosts":["local"]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(panelAccessSchema + "INSERT INTO panel_users VALUES (42, 'operator', 'admin', 0); INSERT INTO panel_sessions VALUES (x'aaaa', 42, 9999999999); INSERT INTO panel_tokens (digest,user_id,name,scope,expires_at,created_at) VALUES (x'bbbb',42,'token','fleet:read',9999999999,1)"); err != nil {
		t.Fatal(err)
	}

	if _, err := q.Exec("CREATE TABLE test_worker_credentials (secret TEXT); INSERT INTO test_worker_credentials VALUES ('test-secret'); ALTER TABLE jobs ADD COLUMN test_jit TEXT DEFAULT 'test-secret'"); err != nil {
		t.Fatal(err)
	}

	if err := backupQueue(ctx, io.Discard, live, archive); err != nil {
		t.Fatal(err)
	}
	if _, err := q.record(ctx, job{id: 6, repository: "owner/repo", repositoryID: 8, installationID: 9, route: "runquill-12-1-later"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := restoreQueue(ctx, io.Discard, archive, live); err == nil || !strings.Contains(err.Error(), "lock queue") {
		t.Fatalf("restore ignored active manager lock: %v", err)
	}
	if candidate, err := openQueue(archive); err == nil {
		candidate.close()
		t.Fatal("manager started directly from the archive")
	}
	if err := restoreQueue(ctx, io.Discard, archive, recovered); err != nil {
		t.Fatal(err)
	}
	if candidate, err := openQueue(recovered); err == nil {
		candidate.close()
		t.Fatal("manager started before recovery activation")
	}
	for _, name := range []string{archive, recovered} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0600 {
			t.Fatalf("queue artifact permissions %o", mode)
		}
	}
	t.Setenv("RUNQUILL_APP_ID", "456")
	if err := activateQueue(ctx, io.Discard, recovered); err == nil || !strings.Contains(err.Error(), "another GitHub owner or app") {
		t.Fatalf("activated for a different GitHub App: %v", err)
	}
	t.Setenv("RUNQUILL_APP_ID", "123")
	if err := activateQueue(ctx, io.Discard, recovered); err != nil {
		t.Fatal(err)
	}

	restored, err := openQueue(recovered)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	jobs, err := restored.jobs(ctx, "ORDER BY sequence")
	if err != nil || len(jobs) != 1 || jobs[0].id != 5 || jobs[0].runnerID != 77 || jobs[0].runnerHost != "local" {
		t.Fatalf("snapshot lost a lease or included newer jobs: %+v, %v", jobs, err)
	}
	var requested, registered, launched int64
	if err := restored.QueryRow("SELECT jit_requested_ms, jit_registered_ms, runner_launch_ms FROM jobs WHERE job_id = 5").
		Scan(&requested, &registered, &launched); err != nil || requested != 123001 || registered != 123020 || launched != 123490 {
		t.Fatalf("restored job stages: %d %d %d, %v", requested, registered, launched, err)
	}
	var engine, restoredDigest string
	if err := restored.QueryRow("SELECT engine, token_digest FROM worker_authorities WHERE host = 'builder'").Scan(&engine, &restoredDigest); err != nil || engine != "test-engine" || restoredDigest != digest {
		t.Fatalf("restored worker authority: engine %q, digest matched %t, %v", engine, restoredDigest == digest, err)
	}
	hosts, err := readHostStates(ctx, restored.DB)
	if err != nil || !hosts["local"].cordoned {
		t.Fatalf("restored host state: %+v, %v", hosts, err)
	}
	var credentials, polls, liveJobs int
	if err := restored.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name LIKE 'panel_%' OR name IN ('test_worker_credentials', 'worker_enrollment_codes')").Scan(&credentials); err != nil || credentials != 0 {
		t.Fatalf("restored credentials: %d tables, %v", credentials, err)
	}
	containsJIT, err := hasJobColumn(ctx, restored.DB, "test_jit")
	if err != nil || containsJIT {
		t.Fatalf("restored an unapproved job column: %t, %v", containsJIT, err)
	}
	if err := restored.QueryRow("SELECT count(*) FROM repository_polls").Scan(&polls); err != nil || polls != 1 {
		t.Fatalf("restored repository polls: %d, %v", polls, err)
	}
	if policy, set, err := readOperationPolicy(ctx, restored.DB, operationPolicy{}); err != nil || !set || policy.Limit != 4 || policy.Revision != 1 {
		t.Fatalf("restored operation settings: %+v, %v", policy, err)
	}
	if err := q.QueryRow("SELECT count(*) FROM jobs").Scan(&liveJobs); err != nil || liveJobs != 2 {
		t.Fatalf("original queue was modified: got %d jobs, %v", liveJobs, err)
	}
}

func TestQueueCopiesRejectBadInputsAndExistingDestinations(t *testing.T) {
	ctx := t.Context()
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "120")
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()

	target := filepath.Join(dir, "new.sqlite3")
	if err := restoreQueue(ctx, io.Discard, live, target); err == nil || !strings.Contains(err.Error(), "not a Runquill queue backup") {
		t.Fatalf("accepted ordinary SQLite DB as a restore artifact: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore published a database: %v", err)
	}
	const untouched = "original contents"
	if err := os.WriteFile(target, []byte(untouched), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backupQueue(ctx, io.Discard, live, target); err == nil {
		t.Fatal("overwrote existing file")
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != untouched {
		t.Fatalf("existing file changed: %q, %v", content, err)
	}
	if err := backupQueue(ctx, io.Discard, target, filepath.Join(dir, "bad-backup.sqlite3")); err == nil {
		t.Fatal("backed up non-SQLite data")
	}

	stale := filepath.Join(dir, "stale.sqlite3")
	if err := os.WriteFile(stale+"-wal", []byte(untouched), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backupQueue(ctx, io.Discard, live, stale); err == nil || !strings.Contains(err.Error(), "-wal already exists") {
		t.Fatalf("ignored stale journal: %v", err)
	}

	archive := filepath.Join(dir, "race.sqlite3")
	outcomes := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() { outcomes <- backupQueue(ctx, io.Discard, live, archive) })
	}
	workers.Wait()
	close(outcomes)
	var successes int
	for err := range outcomes {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected one successful concurrent backup, got %d", successes)
	}
	if err := restoreQueue(ctx, io.Discard, archive, filepath.Join(dir, "restored.sqlite3")); err != nil {
		t.Fatalf("concurrent backup produced invalid archive: %v", err)
	}
}

func TestBackupNormalizesLegacyQueue(t *testing.T) {
	ctx := t.Context()
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.sqlite3")
	db, err := openQueueDB(legacy, "rwc")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema + "; INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at) VALUES (1, 'owner/repo', 10, 20, 'runquill-12-1-legacy', 0, 123)"); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "backup.sqlite3")
	if err := backupQueue(ctx, io.Discard, legacy, archive); err != nil {
		t.Fatal(err)
	}
	source, err := openQueueDB(archive, "rw")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	// A version 1 archive from before stage timings and enrollment has neither.
	if _, err := source.Exec(`ALTER TABLE jobs DROP COLUMN jit_requested_ms;
		ALTER TABLE jobs DROP COLUMN jit_registered_ms;
		ALTER TABLE jobs DROP COLUMN runner_launch_ms;
		DROP TABLE worker_authorities`); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "recover.sqlite3")
	t.Setenv("RUNQUILL_OWNER", "other")
	if err := restoreQueue(ctx, io.Discard, archive, target); err == nil || !strings.Contains(err.Error(), "another GitHub owner or app") {
		t.Fatalf("restored archive into another account: %v", err)
	}
	t.Setenv("RUNQUILL_OWNER", "owner")
	if err := restoreQueue(ctx, io.Discard, archive, target); err != nil {
		t.Fatal(err)
	}
	recovered, err := openQueueDB(target, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	jobs, err := (&queue{DB: recovered}).jobs(ctx, "ORDER BY sequence")
	if err != nil || len(jobs) != 1 || jobs[0].id != 1 || jobs[0].phase != queued {
		t.Fatalf("unexpected legacy backup jobs %+v, %v", jobs, err)
	}

	if _, err := source.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if err := restoreQueue(ctx, io.Discard, archive, filepath.Join(dir, "unsupported.sqlite3")); err == nil || !strings.Contains(err.Error(), "unsupported Runquill queue backup version") {
		t.Fatalf("accepted unsupported backup format: %v", err)
	}
}
