package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAccessKey = "AKIATESTACCESSKEY123"
	testSecretKey = "test/secret+key/value-0123456789"
)

// TestSigV4MatchesAWSExamples uses the inputs of the S3 header-auth examples
// in the AWS documentation. The expected signatures were reproduced with the
// independent AWS SDK for Go v2 signer, outside this module.
func TestSigV4MatchesAWSExamples(t *testing.T) {
	const (
		access = "AKIAIOSFODNN7EXAMPLE"
		secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		host   = "examplebucket.s3.amazonaws.com"
	)
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	headers := func(extra ...string) map[string]string {
		h := map[string]string{"host": host, "x-amz-date": "20130524T000000Z", "x-amz-content-sha256": s3EmptyHash}
		for i := 0; i < len(extra); i += 2 {
			h[extra[i]] = extra[i+1]
		}
		return h
	}
	auth, canonical, toSign := sigV4(sigV4Input{method: "GET", path: "/test.txt", headers: headers("range", "bytes=0-9"), payloadHash: s3EmptyHash},
		access, secret, "us-east-1", "s3", at)
	wantCanonical := "GET\n/test.txt\n\nhost:examplebucket.s3.amazonaws.com\nrange:bytes=0-9\nx-amz-content-sha256:" + s3EmptyHash +
		"\nx-amz-date:20130524T000000Z\n\nhost;range;x-amz-content-sha256;x-amz-date\n" + s3EmptyHash
	if canonical != wantCanonical {
		t.Fatalf("canonical request:\n%s\nwant:\n%s", canonical, wantCanonical)
	}
	digest := sha256.Sum256([]byte(wantCanonical))
	if want := "AWS4-HMAC-SHA256\n20130524T000000Z\n20130524/us-east-1/s3/aws4_request\n" + hex.EncodeToString(digest[:]); toSign != want {
		t.Fatalf("string to sign:\n%s", toSign)
	}
	if want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"; auth != want {
		t.Fatalf("GET object authorization %s", auth)
	}
	for query, signature := range map[string]string{
		s3CanonicalQuery(url.Values{"max-keys": {"2"}, "prefix": {"J"}}): "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		s3CanonicalQuery(url.Values{"lifecycle": {""}}):                  "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
	} {
		auth, _, _ := sigV4(sigV4Input{method: "GET", path: "/", query: query, headers: headers(), payloadHash: s3EmptyHash}, access, secret, "us-east-1", "s3", at)
		if !strings.HasSuffix(auth, "Signature="+signature) {
			t.Fatalf("query %q: %s", query, auth)
		}
	}
	if got := s3CanonicalQuery(url.Values{"prefix": {"a b/c"}, "list-type": {"2"}}); got != "list-type=2&prefix=a%20b%2Fc" {
		t.Fatalf("canonical query %s", got)
	}
	if got := s3Escape("dir/a b+~", true); got != "dir/a%20b%2B~" {
		t.Fatalf("escaped path %s", got)
	}
}

type fakeS3 struct {
	t         *testing.T
	pathStyle bool
	mu        sync.Mutex
	objects   map[string][]byte
	failPuts  int
	echoAuth  bool
	methods   []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods = append(f.methods, r.Method)
	if f.echoAuth {
		// Real S3 echoes the access key ID and string to sign on signature errors.
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "<Error><Code>SignatureDoesNotMatch</Code><AWSAccessKeyId>%s</AWSAccessKeyId><StringToSign>%s</StringToSign></Error>", testAccessKey, r.Header.Get("Authorization"))
		return
	}
	payload, date := r.Header.Get("X-Amz-Content-Sha256"), r.Header.Get("X-Amz-Date")
	at, err := time.Parse("20060102T150405Z", date)
	if err != nil {
		f.t.Errorf("bad x-amz-date %q", date)
	}
	want, _, _ := sigV4(sigV4Input{method: r.Method, path: r.URL.EscapedPath(), query: s3CanonicalQuery(r.URL.Query()), payloadHash: payload,
		headers: map[string]string{"host": r.Host, "x-amz-date": date, "x-amz-content-sha256": payload}}, testAccessKey, testSecretKey, "eu-test-1", "s3", at)
	if r.Header.Get("Authorization") != want {
		f.t.Errorf("%s %s: authorization %q, want %q", r.Method, r.URL, r.Header.Get("Authorization"), want)
		http.Error(w, "<Error><Code>SignatureDoesNotMatch</Code></Error>", http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	if f.pathStyle {
		var ok bool
		if key, ok = strings.CutPrefix(key, "bucket"); !ok {
			f.t.Errorf("path-style request without bucket: %s", r.URL.Path)
		}
		key = strings.TrimPrefix(key, "/")
	} else if !strings.HasPrefix(r.Host, "bucket.") {
		f.t.Errorf("virtual-hosted request to %s", r.Host)
	}
	switch {
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != payload || r.ContentLength != int64(len(body)) {
			f.t.Errorf("PUT payload hash or length mismatch")
		}
		if f.failPuts > 0 {
			f.failPuts--
			http.Error(w, "<Error><Code>SlowDown</Code></Error>", http.StatusServiceUnavailable)
			return
		}
		f.objects[key] = body
	case r.Method == http.MethodHead || r.Method == http.MethodGet && key != "":
		body, ok := f.objects[key]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body)
	case r.Method == http.MethodGet:
		if r.URL.Query().Get("list-type") != "2" {
			f.t.Errorf("list without list-type=2")
		}
		var keys []string
		for key := range f.objects {
			if strings.HasPrefix(key, r.URL.Query().Get("prefix")) && key > r.URL.Query().Get("continuation-token") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		// Two keys per page exercises continuation.
		truncated := len(keys) > 2
		if truncated {
			keys = keys[:2]
		}
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
		for _, key := range keys {
			fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", key, len(f.objects[key]))
		}
		fmt.Fprintf(w, "<IsTruncated>%t</IsTruncated>", truncated)
		if truncated {
			fmt.Fprintf(w, "<NextContinuationToken>%s</NextContinuationToken>", keys[1])
		}
		fmt.Fprint(w, "</ListBucketResult>")
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for key := range f.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func testS3Env(endpoint string, pathStyle bool) map[string]string {
	return map[string]string{
		"RUNQUILL_BACKUP_S3_ENDPOINT": endpoint, "RUNQUILL_BACKUP_S3_BUCKET": "bucket", "RUNQUILL_BACKUP_S3_REGION": "eu-test-1",
		"RUNQUILL_BACKUP_S3_PREFIX": "fleet/a", "RUNQUILL_BACKUP_S3_ACCESS_KEY_ID": testAccessKey,
		"RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY": testSecretKey, "RUNQUILL_BACKUP_S3_PATH_STYLE": fmt.Sprint(pathStyle), "RUNQUILL_BACKUP_S3_KEEP": "2",
	}
}

func newTestS3(t *testing.T, pathStyle bool) (*fakeS3, *s3Client, map[string]string) {
	t.Helper()
	fake := &fakeS3{t: t, pathStyle: pathStyle, objects: map[string][]byte{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	env := testS3Env("http://localhost:"+port, pathStyle)
	cfg, err := loadS3Config(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	client := newS3Client(cfg)
	// Virtual-hosted names such as bucket.localhost resolve to the fake server.
	client.http.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}
	return fake, client, env
}

func writeSealedStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(sealMagic+body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestS3UploadVerifiesAndRestoresInBothAddressingStyles(t *testing.T) {
	for _, pathStyle := range []bool{true, false} {
		fake, client, _ := newTestS3(t, pathStyle)
		dir := t.TempDir()
		name := backupObjectName(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), true)
		writeSealedStub(t, dir, name, "payload")
		if err := client.upload(t.Context(), filepath.Join(dir, name), name); err != nil {
			t.Fatalf("path style %t: %v", pathStyle, err)
		}
		if keys := fake.keys(); len(keys) != 1 || keys[0] != "fleet/a/runquill-queue-20260102T030405Z.sqlite3.sealed" {
			t.Fatalf("uploaded keys %v", keys)
		}
		if fake.methods[0] != http.MethodPut || fake.methods[1] != http.MethodHead {
			t.Fatalf("upload was not verified with HEAD: %v", fake.methods)
		}
		var downloaded bytes.Buffer
		if err := client.download(t.Context(), client.key(name), &downloaded); err != nil || downloaded.String() != sealMagic+"payload" {
			t.Fatalf("download %q, %v", downloaded.String(), err)
		}
	}
}

func TestS3UploadRefusesPlaintextUnlessAllowed(t *testing.T) {
	fake, client, _ := newTestS3(t, true)
	dir := t.TempDir()
	name := backupObjectName(time.Now(), false)
	os.WriteFile(filepath.Join(dir, name), []byte("SQLite format 3\x00plain"), 0600)
	if err := client.upload(t.Context(), filepath.Join(dir, name), name); !errors.Is(err, errPlaintextUpload) {
		t.Fatalf("uploaded plaintext: %v", err)
	}
	if len(fake.keys()) != 0 {
		t.Fatal("plaintext reached the bucket")
	}
	logs := captureLogs(t)
	client.cfg.allowPlaintext = true
	if err := client.upload(t.Context(), filepath.Join(dir, name), name); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "UNENCRYPTED") || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("allowed plaintext upload without a loud warning: %s", logs.String())
	}
}

func TestS3CatchUpAndRemoteRetention(t *testing.T) {
	fake, client, _ := newTestS3(t, false)
	dir := t.TempDir()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var names []string
	for i := range 3 {
		name := backupObjectName(base.Add(time.Duration(i)*time.Hour), true)
		writeSealedStub(t, dir, name, fmt.Sprint(i))
		names = append(names, name)
	}
	fake.objects["fleet/a/notes.txt"] = []byte("keep")
	fake.objects["fleet/a/runquill-queue-old.sqlite3"] = []byte("keep")
	fake.objects["fleet/a/runquill-queue-20200101T000000Z.sqlite3.sealed"] = []byte("old")
	fake.objects["fleet/b/runquill-queue-20200101T000000Z.sqlite3.sealed"] = []byte("other prefix")
	fake.failPuts = 1

	result, err := client.syncRemote(t.Context(), dir, names)
	if err == nil || result.pending != 2 || len(result.uploaded) != 0 {
		t.Fatalf("failed upload: %+v, %v", result, err)
	}
	if !strings.Contains(err.Error(), "HTTP 503 SlowDown") {
		t.Fatalf("error lacks the S3 code: %v", err)
	}
	result, err = client.syncRemote(t.Context(), dir, names)
	if err != nil || len(result.uploaded) != 2 || result.pending != 0 {
		t.Fatalf("catch-up: %+v, %v", result, err)
	}
	want := []string{"fleet/a/notes.txt", "fleet/a/" + names[1], "fleet/a/" + names[2], "fleet/a/runquill-queue-old.sqlite3",
		"fleet/b/runquill-queue-20200101T000000Z.sqlite3.sealed"}
	if got := fake.keys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("remote keys after retention:\n%v\nwant\n%v", got, want)
	}
	puts := 0
	result, err = client.syncRemote(t.Context(), dir, names)
	for _, method := range fake.methods {
		if method == http.MethodPut {
			puts++
		}
	}
	if err != nil || len(result.uploaded) != 0 || puts != 3 {
		t.Fatalf("re-uploaded confirmed backups: %+v, %d puts, %v", result, puts, err)
	}
	os.WriteFile(filepath.Join(dir, backupStateName), []byte("{corrupt"), 0600)
	if result, err := client.syncRemote(t.Context(), dir, names); err != nil || len(result.uploaded) != 2 {
		t.Fatalf("corrupt state was not tolerated: %+v, %v", result, err)
	}
}

func TestS3ConfigValidation(t *testing.T) {
	base := testS3Env("https://s3.example.com", false)
	load := func(changes map[string]string) (*s3Config, error) {
		return loadS3Config(func(name string) string {
			if value, ok := changes[name]; ok {
				return value
			}
			return base[name]
		})
	}
	if cfg, err := load(nil); err != nil || cfg.prefix != "fleet/a/" || cfg.keep != 2 {
		t.Fatalf("valid config: %+v, %v", cfg, err)
	}
	if cfg, err := loadS3Config(func(string) string { return "" }); cfg != nil || err != nil {
		t.Fatal("empty config enabled S3")
	}
	for name, changes := range map[string]map[string]string{
		"plain http":    {"RUNQUILL_BACKUP_S3_ENDPOINT": "http://s3.example.com"},
		"endpoint path": {"RUNQUILL_BACKUP_S3_ENDPOINT": "https://s3.example.com/bucket"},
		"userinfo":      {"RUNQUILL_BACKUP_S3_ENDPOINT": "https://user:pw@s3.example.com"},
		"bucket":        {"RUNQUILL_BACKUP_S3_BUCKET": "Bad_Bucket"},
		"prefix":        {"RUNQUILL_BACKUP_S3_PREFIX": "../escape"},
		"no secret":     {"RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY": ""},
		"two secrets":   {"RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY_FILE": "/dev/null"},
		"keep":          {"RUNQUILL_BACKUP_S3_KEEP": "-1"},
		"plaintext":     {"RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT": "maybe"},
	} {
		if _, err := load(changes); err == nil {
			t.Fatalf("%s: accepted invalid config", name)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:9000", "http://[::1]:9000", "http://localhost:9000"} {
		if _, err := load(map[string]string{"RUNQUILL_BACKUP_S3_ENDPOINT": endpoint}); err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
	}
	secretFile := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secretFile, []byte(testSecretKey+"\n"), 0600)
	if cfg, err := load(map[string]string{"RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY": "", "RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY_FILE": secretFile}); err != nil || cfg.secretKey != testSecretKey {
		t.Fatalf("secret file: %v", err)
	}
	if _, err := load(map[string]string{"RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY": "", "RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY_FILE": "/nonexistent/" + testSecretKey}); err == nil || strings.Contains(err.Error(), testSecretKey) {
		t.Fatalf("secret file error: %v", err)
	}
}

func TestS3ErrorsAndLogsNeverContainSecrets(t *testing.T) {
	memoizeSealKey(t)
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	fake, client, env := newTestS3(t, true)
	fake.echoAuth = true
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	backups := filepath.Join(dir, "backups")
	env["RUNQUILL_BACKUP_INTERVAL"] = "1h"
	env["RUNQUILL_BACKUP_DIR"] = backups
	env["RUNQUILL_BACKUP_PASSPHRASE"] = testPassphrase
	cfg, err := loadBackupScheduleConfig(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	status := &backupStatus{}
	clock := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	deps := backupScheduleDeps{config: cfg, database: live, status: status, s3: client,
		now: func() time.Time { clock = clock.Add(time.Hour); return clock }}
	runScheduledBackup(t.Context(), deps)

	// A transport failure reports the endpoint, never the credentials.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	broken := newS3Client(cfg.s3)
	broken.cfg = &s3Config{}
	*broken.cfg = *cfg.s3
	broken.cfg.endpoint, _ = url.Parse(closed.URL)
	deps.s3 = broken
	runScheduledBackup(t.Context(), deps)

	snapshot := status.Snapshot()
	if snapshot.Remote != "failed" || snapshot.RemoteError == "" || snapshot.Error != "" || !snapshot.Sealed {
		t.Fatalf("status %+v", snapshot)
	}
	_, putErr := client.syncRemote(t.Context(), backups, []string{snapshot.Name})
	everything := logs.String() + snapshot.RemoteError + fmt.Sprint(putErr)
	for _, secret := range []string{testAccessKey, testSecretKey, testPassphrase} {
		if strings.Contains(everything, secret) {
			t.Fatalf("secret leaked: %s", everything)
		}
	}
	if !strings.Contains(everything, "SignatureDoesNotMatch") {
		t.Fatalf("error lost the S3 code: %s", everything)
	}
}

func TestUploadCommandScheduleAndRestoreFromS3(t *testing.T) {
	memoizeSealKey(t)
	ctx := t.Context()
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "123")
	fake, _, env := newTestS3(t, true)
	for name, value := range env {
		t.Setenv(name, value)
	}
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE", testPassphrase)
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	if _, err := q.Exec("INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at) VALUES (11, 'owner/repo', 8, 9, 'runquill-12-1-remote', 0, 123)"); err != nil {
		t.Fatal(err)
	}
	if err := runQueueBackupWithDB(t, live, "backup", "--upload"); !errors.Is(err, errPlaintextUpload) {
		t.Fatalf("uploaded an unsealed one-off backup: %v", err)
	}
	if err := runQueueBackupWithDB(t, live, "backup", "--upload", "--seal"); err != nil {
		t.Fatal(err)
	}
	keys := fake.keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "fleet/a/runquill-queue-") || !strings.HasSuffix(keys[0], ".sealed") {
		t.Fatalf("uploaded keys %v", keys)
	}

	cfg, err := loadBackupScheduleConfig(func(name string) string {
		if name == "RUNQUILL_BACKUP_INTERVAL" {
			return "1h"
		}
		if name == "RUNQUILL_BACKUP_DIR" {
			return filepath.Join(dir, "backups")
		}
		return os.Getenv(name)
	})
	if err != nil {
		t.Fatal(err)
	}
	// An older unsealed backup must not block the sealed ones behind it.
	writeUnsealed := filepath.Join(cfg.dir, "runquill-queue-20200101T000000Z.sqlite3")
	os.WriteFile(writeUnsealed, []byte("SQLite format 3\x00"), 0600)
	status := &backupStatus{}
	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	deps := backupScheduleDeps{config: cfg, database: live, status: status, now: func() time.Time { return clock }}
	fake.failPuts = 1
	runScheduledBackup(ctx, deps)
	snapshot := status.Snapshot()
	if snapshot.Error != "" || snapshot.Remote != "failed" || snapshot.RemotePending != 1 || len(fake.keys()) != 1 {
		t.Fatalf("failed scheduled upload %+v, keys %v", snapshot, fake.keys())
	}
	clock = clock.Add(time.Hour)
	runScheduledBackup(ctx, deps)
	snapshot = status.Snapshot()
	// Remote retention (keep 2) prunes the older one-off upload.
	if snapshot.Error != "" || snapshot.Remote != "uploaded" || snapshot.RemotePending != 0 || len(fake.keys()) != 2 || slices.Contains(fake.keys(), keys[0]) {
		t.Fatalf("scheduled catch-up %+v, keys %v", snapshot, fake.keys())
	}
	for _, key := range fake.keys() {
		if !strings.HasSuffix(key, ".sealed") {
			t.Fatalf("uploaded unsealed %s", key)
		}
	}

	target := filepath.Join(dir, "recovered.sqlite3")
	if err := runQueueBackupWithDB(t, target, "restore", "--from-s3", "../escape.sqlite3"); err == nil {
		t.Fatal("restored an arbitrary key")
	}
	if err := runQueueBackupWithDB(t, target, "restore", "--from-s3", snapshot.Name); err != nil {
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
	if jobs, err := restored.jobs(ctx, "ORDER BY sequence"); err != nil || len(jobs) != 1 || jobs[0].id != 11 {
		t.Fatalf("restored jobs %+v, %v", jobs, err)
	}
	if err := runQueueBackupWithDB(t, filepath.Join(dir, "again.sqlite3"), "restore", "--from-s3", keys[0], "--from", "x"); err == nil {
		t.Fatal("accepted two restore sources")
	}
}

func TestLiveS3BackupRestore(t *testing.T) {
	if os.Getenv("RUNQUILL_TEST_BACKUP_S3") != "1" {
		t.Skip("set RUNQUILL_TEST_BACKUP_S3=1 and configure a test S3 bucket")
	}
	t.Setenv("RUNQUILL_BACKUP_S3_KEEP", "0")
	t.Setenv("RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT", "false")
	base, err := loadS3Config(os.Getenv)
	if err != nil || base == nil {
		t.Fatalf("configure a test S3 bucket: %v", err)
	}
	t.Setenv("RUNQUILL_BACKUP_S3_PREFIX", base.prefix+"runquill-smoke-"+strings.ToLower(rand.Text()))
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE", rand.Text())
	t.Setenv("RUNQUILL_BACKUP_PASSPHRASE_FILE", "")
	t.Setenv("RUNQUILL_BACKUP_INTERVAL", "1h")
	dir := t.TempDir()
	t.Setenv("RUNQUILL_BACKUP_DIR", filepath.Join(dir, "backups"))
	t.Setenv("RUNQUILL_OWNER", "fixture")
	t.Setenv("RUNQUILL_APP_ID", "123")
	cfg, err := loadBackupScheduleConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	client := newS3Client(cfg.s3)
	at := time.Now().UTC()
	name := backupObjectName(at, true)
	key := client.key(name)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := client.delete(ctx, key); err != nil {
			t.Errorf("clean up the smoke-test backup object: %s", redactSecrets(err.Error(), cfg.secrets()...))
		}
	})

	live := filepath.Join(dir, "live.sqlite3")
	q, err := openQueue(live)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	const jobID, runnerID = 42, 43
	if _, err := q.Exec(`INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at, runner_name, runner_host, runner_id)
		VALUES (?, 'fixture/repo', 8, 9, 'runquill-12-1-s3-smoke', 1, 123, 'runquill-42-abcdef012345', 'local', ?)`, jobID, runnerID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	status := &backupStatus{}
	runScheduledBackup(ctx, backupScheduleDeps{config: cfg, database: live, status: status, s3: client, now: func() time.Time { return at }})
	snapshot := status.Snapshot()
	if snapshot.Name != name || !snapshot.Sealed || snapshot.Error != "" || snapshot.Remote != "uploaded" || snapshot.RemotePending != 0 {
		t.Fatalf("sealed S3 backup did not complete: %+v", snapshot)
	}
	recovered := filepath.Join(dir, "recovered.sqlite3")
	if err := restoreFromS3Command(ctx, io.Discard, name, recovered, ""); err != nil {
		t.Fatalf("restore from S3: %s", redactSecrets(err.Error(), cfg.secrets()...))
	}
	if err := checkQueueReady(recovered); err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("restored queue did not require activation: %v", err)
	}
	if err := activateQueue(ctx, io.Discard, recovered); err != nil {
		t.Fatal(err)
	}
	restored, err := openQueue(recovered)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	jobs, err := restored.jobs(ctx, "ORDER BY sequence")
	if err != nil || len(jobs) != 1 || jobs[0].id != jobID || jobs[0].runnerID != runnerID || jobs[0].runnerHost != "local" {
		t.Fatalf("restored queue did not preserve the seeded lease: %+v, %v", jobs, err)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}
