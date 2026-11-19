package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testPassphrase = "correct horse battery staple"

// memoizeSealKey keeps the real KDF but avoids repeating it under -race.
func memoizeSealKey(t *testing.T) {
	t.Helper()
	real := sealKey
	var mu sync.Mutex
	cache := map[string][]byte{}
	sealKey = func(passphrase, salt []byte, iterations int) ([]byte, error) {
		id := string(passphrase) + "\x00" + string(salt) + "\x00" + string(binary.BigEndian.AppendUint32(nil, uint32(iterations)))
		mu.Lock()
		defer mu.Unlock()
		if key, ok := cache[id]; ok {
			return key, nil
		}
		key, err := real(passphrase, salt, iterations)
		if err == nil {
			cache[id] = key
		}
		return key, err
	}
	t.Cleanup(func() { sealKey = real })
}

func sealBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var sealed bytes.Buffer
	if err := sealBackup(&sealed, bytes.NewReader(payload), []byte(testPassphrase)); err != nil {
		t.Fatal(err)
	}
	return sealed.Bytes()
}

func TestSealRoundTripAndFraming(t *testing.T) {
	memoizeSealKey(t)
	for _, size := range []int{0, 1, sealChunkSize, 2*sealChunkSize + 5} {
		payload := make([]byte, size)
		rand.Read(payload)
		sealed := sealBytes(t, payload)
		header := len(sealMagic) + 7 + sealSaltSize + 4
		chunks := max(1, (size+sealChunkSize-1)/sealChunkSize)
		if want := header + size + chunks*sealTagSize; len(sealed) != want {
			t.Fatalf("size %d: sealed length %d, want %d", size, len(sealed), want)
		}
		if !bytes.HasPrefix(sealed, []byte(sealMagic)) {
			t.Fatal("missing magic")
		}
		var opened bytes.Buffer
		if err := unsealBackup(&opened, bytes.NewReader(sealed), []byte(testPassphrase)); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(opened.Bytes(), payload) {
			t.Fatalf("size %d: round trip changed the payload", size)
		}
		if size > 0 && bytes.Contains(sealed, payload[:min(size, 32)]) && size >= 16 {
			t.Fatal("payload appears in the sealed file")
		}
	}
}

func TestSealUsesTheDocumentedKDF(t *testing.T) {
	sealed := sealBytes(t, []byte("payload"))
	if got := binary.BigEndian.Uint32(sealed[10:14]); got != sealIterations || sealIterations < 600_000 {
		t.Fatalf("iterations %d", got)
	}
	if err := unsealBackup(io.Discard, bytes.NewReader(sealed), []byte("wrong passphrase!")); !errors.Is(err, errSealedAuth) {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

func TestUnsealRejectsTamperingInEveryRegion(t *testing.T) {
	memoizeSealKey(t)
	payload := make([]byte, 2*sealChunkSize+100)
	rand.Read(payload)
	sealed := sealBytes(t, payload)
	header := len(sealMagic) + 7 + sealSaltSize + 4
	regions := map[string]int{
		"magic": 0, "version": 8, "kdf": 9, "iterations": 13, "salt length": 14, "salt": 15,
		"chunk size": header - 1, "first chunk": header + 10, "first tag": header + sealChunkSize + 3,
		"middle chunk": header + sealChunkSize + sealTagSize + 50, "last chunk": len(sealed) - sealTagSize - 2, "last tag": len(sealed) - 1,
	}
	for region, offset := range regions {
		damaged := bytes.Clone(sealed)
		damaged[offset] ^= 1
		var opened bytes.Buffer
		if err := unsealBackup(&opened, bytes.NewReader(damaged), []byte(testPassphrase)); err == nil {
			t.Fatalf("%s: accepted a flipped bit", region)
		}
	}
	for name, cut := range map[string][]byte{
		"empty": nil, "half header": sealed[:10], "header only": sealed[:header],
		"chunk boundary": sealed[:header+sealChunkSize+sealTagSize], "mid chunk": sealed[:header+500],
		"last byte": sealed[:len(sealed)-1], "trailing data": append(bytes.Clone(sealed), 0),
		"dropped chunk": append(bytes.Clone(sealed[:header]), sealed[header+sealChunkSize+sealTagSize:]...),
	} {
		if err := unsealBackup(io.Discard, bytes.NewReader(cut), []byte(testPassphrase)); err == nil {
			t.Fatalf("%s: accepted a truncated or extended file", name)
		}
	}
	if err := unsealBackup(io.Discard, bytes.NewReader(sealed), []byte("wrong passphrase!")); !errors.Is(err, errSealedAuth) {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

func TestUnsealBoundsHeaderParameters(t *testing.T) {
	memoizeSealKey(t)
	sealed := sealBytes(t, []byte("x"))
	for name, change := range map[string]func([]byte){
		"low iterations":  func(b []byte) { binary.BigEndian.PutUint32(b[10:], 1000) },
		"high iterations": func(b []byte) { binary.BigEndian.PutUint32(b[10:], 1<<31) },
		"short salt":      func(b []byte) { b[14] = 4 },
		"huge chunk":      func(b []byte) { binary.BigEndian.PutUint32(b[15+sealSaltSize:], 1<<30) },
		"version":         func(b []byte) { b[8] = 2 },
	} {
		damaged := bytes.Clone(sealed)
		change(damaged)
		err := unsealBackup(io.Discard, bytes.NewReader(damaged), []byte(testPassphrase))
		if err == nil || errors.Is(err, errSealedAuth) {
			t.Fatalf("%s: header was not rejected before key derivation: %v", name, err)
		}
	}
}

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
	if err := sealBackup(io.Discard, strings.NewReader("x"), []byte("short")); err == nil {
		t.Fatal("sealed with a short passphrase")
	}
}

func TestSealedBackupRestoresEndToEnd(t *testing.T) {
	memoizeSealKey(t)
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
	if err := sealedBackupCommand(ctx, io.Discard, live, sealed, ""); !errors.Is(err, errSealPassphrase) {
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
	if !bytes.HasPrefix(data, []byte(sealMagic)) || bytes.Contains(data, []byte("owner/repo")) {
		t.Fatal("backup is not sealed")
	}
	if err := runQueueBackupWithDB(t, live, "backup", "--output", sealed, "--seal", "--passphrase-file", passFile); err == nil {
		t.Fatal("overwrote an existing sealed backup")
	}

	target := filepath.Join(dir, "recovered.sqlite3")
	if err := restoreQueue(ctx, io.Discard, sealed, target); !errors.Is(err, errSealPassphrase) {
		t.Fatalf("restored a sealed backup without a passphrase: %v", err)
	}
	wrong := filepath.Join(dir, "wrong")
	os.WriteFile(wrong, []byte("not the passphrase"), 0600)
	if err := runQueueBackupWithDB(t, target, "restore", "--from", sealed, "--passphrase-file", wrong); !errors.Is(err, errSealedAuth) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	damaged := filepath.Join(dir, "damaged.sealed")
	os.WriteFile(damaged, data[:len(data)-1], 0600)
	if err := runQueueBackupWithDB(t, target, "restore", "--from", damaged, "--passphrase-file", passFile); !errors.Is(err, errSealedAuth) {
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
