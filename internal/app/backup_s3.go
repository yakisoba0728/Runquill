package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	s3EmptyHash       = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	s3MaxDownload     = 64 << 30
	s3MaxListBody     = 8 << 20
	s3MaxListPages    = 100
	s3UploadTimeout   = 2 * time.Hour
	s3RequestTimeout  = time.Minute
	backupStateName   = ".runquill-backup-remote.json"
	backupStateMax    = 1 << 20
	defaultBackupKeep = 7
)

var (
	s3BucketName  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3PrefixChars = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
	s3RegionName  = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	s3ErrorCode   = regexp.MustCompile(`<Code>([A-Za-z0-9.]{1,64})</Code>`)
	// Only files this feature creates match; anything else in the directory or prefix is left alone.
	backupFileName = regexp.MustCompile(`^runquill-queue-([0-9]{8}T[0-9]{6}Z)\.sqlite3(\.sealed)?$`)
)

type s3Config struct {
	endpoint                  *url.URL
	bucket, region, prefix    string
	accessKey, secretKey      string
	pathStyle, allowPlaintext bool
	keep                      int
}

func (c *s3Config) secrets() []string { return []string{c.accessKey, c.secretKey} }

// loadS3Config returns nil when no bucket is configured.
func loadS3Config(getenv func(string) string) (*s3Config, error) {
	endpoint, bucket := strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_ENDPOINT")), strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_BUCKET"))
	if endpoint == "" && bucket == "" {
		return nil, nil
	}
	cfg := &s3Config{bucket: bucket, region: strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_REGION")), keep: defaultBackupKeep,
		accessKey: strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_ACCESS_KEY_ID"))}
	if endpoint == "" || bucket == "" {
		return nil, errors.New("set both RUNQUILL_BACKUP_S3_ENDPOINT and RUNQUILL_BACKUP_S3_BUCKET")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("RUNQUILL_BACKUP_S3_ENDPOINT must be a URL such as https://s3.example.com without a path")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname())) {
		return nil, errors.New("RUNQUILL_BACKUP_S3_ENDPOINT must use https unless it is a loopback address")
	}
	cfg.endpoint = &url.URL{Scheme: u.Scheme, Host: u.Host}
	if !s3BucketName.MatchString(bucket) || strings.Contains(bucket, "..") {
		return nil, errors.New("RUNQUILL_BACKUP_S3_BUCKET is not a valid bucket name")
	}
	if cfg.region == "" {
		cfg.region = "us-east-1"
	}
	if !s3RegionName.MatchString(cfg.region) {
		return nil, errors.New("RUNQUILL_BACKUP_S3_REGION is not a valid region name")
	}
	prefix := strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_PREFIX"))
	if len(prefix) > 512 || !s3PrefixChars.MatchString(prefix) || strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") ||
		slices.Contains(strings.Split(prefix, "/"), "..") {
		return nil, errors.New("RUNQUILL_BACKUP_S3_PREFIX may contain only letters, digits, '.', '_', '-' and '/' separators")
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	cfg.prefix = prefix
	if cfg.accessKey == "" {
		return nil, errors.New("set RUNQUILL_BACKUP_S3_ACCESS_KEY_ID")
	}
	secret, secretFile := getenv("RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY"), getenv("RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY_FILE")
	switch {
	case secret != "" && secretFile != "":
		return nil, errors.New("set only one of RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY and RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY_FILE")
	case secretFile != "":
		data, err := os.ReadFile(secretFile)
		if err != nil {
			return nil, errors.New("could not read RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY_FILE")
		}
		secret = strings.TrimSpace(string(data))
	}
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("set RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY or RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY_FILE")
	}
	cfg.secretKey = strings.TrimSpace(secret)
	for _, flag := range []struct {
		name  string
		value *bool
	}{{"RUNQUILL_BACKUP_S3_PATH_STYLE", &cfg.pathStyle}, {"RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT", &cfg.allowPlaintext}} {
		if raw := strings.TrimSpace(getenv(flag.name)); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, fmt.Errorf("%s must be true or false", flag.name)
			}
			*flag.value = parsed
		}
	}
	if raw := strings.TrimSpace(getenv("RUNQUILL_BACKUP_S3_KEEP")); raw != "" {
		cfg.keep, err = strconv.Atoi(raw)
		if err != nil || cfg.keep < 0 || cfg.keep > 1000 {
			return nil, errors.New("RUNQUILL_BACKUP_S3_KEEP must be an integer from 0 (never prune) to 1000")
		}
	}
	return cfg, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type s3Client struct {
	cfg  *s3Config
	http *http.Client
	now  func() time.Time
}

func newS3Client(cfg *s3Config) *s3Client {
	return &s3Client{cfg: cfg, now: time.Now, http: &http.Client{
		// A redirect would send the signed request somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// s3Escape is the SigV4 URI encoding: everything but unreserved characters.
func s3Escape(value string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || (keepSlash && c == '/') {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func s3CanonicalQuery(query url.Values) string {
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		values := slices.Clone(query[key])
		sort.Strings(values)
		for _, value := range values {
			parts = append(parts, s3Escape(key, false)+"="+s3Escape(value, false))
		}
	}
	return strings.Join(parts, "&")
}

type sigV4Input struct {
	method, path, query string
	headers             map[string]string // lower-case names; must include host
	payloadHash         string
}

func sigV4Key(secret, date, region, service string) []byte {
	key := []byte("AWS4" + secret)
	for _, part := range []string{date, region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	return key
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// sigV4 returns the Authorization header plus the canonical request and string to sign for tests.
func sigV4(in sigV4Input, accessKey, secret, region, service string, at time.Time) (authorization, canonical, stringToSign string) {
	names := make([]string, 0, len(in.headers))
	for name := range in.headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, name := range names {
		headers.WriteString(name + ":" + strings.Join(strings.Fields(in.headers[name]), " ") + "\n")
	}
	signed := strings.Join(names, ";")
	canonical = strings.Join([]string{in.method, in.path, in.query, headers.String(), signed, in.payloadHash}, "\n")
	stamp := at.UTC().Format("20060102T150405Z")
	scope := stamp[:8] + "/" + region + "/" + service + "/aws4_request"
	digest := sha256.Sum256([]byte(canonical))
	stringToSign = "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(digest[:])
	signature := hex.EncodeToString(hmacSHA256(sigV4Key(secret, stamp[:8], region, service), stringToSign))
	authorization = "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + signature
	return authorization, canonical, stringToSign
}

func (c *s3Client) target(key string) (host, escapedPath string) {
	host = c.cfg.endpoint.Host
	escapedPath = "/" + s3Escape(key, true)
	if c.cfg.pathStyle {
		escapedPath = "/" + c.cfg.bucket + escapedPath
		if key == "" {
			escapedPath = "/" + c.cfg.bucket
		}
	} else {
		host = c.cfg.bucket + "." + host
	}
	return host, escapedPath
}

func (c *s3Client) do(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64, payloadHash string) (*http.Response, error) {
	host, escapedPath := c.target(key)
	rawQuery := s3CanonicalQuery(query)
	unescaped, err := url.PathUnescape(escapedPath)
	if err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: c.cfg.endpoint.Scheme, Host: host, Path: unescaped, RawPath: escapedPath, RawQuery: rawQuery}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("build S3 request")
	}
	req.Host = host
	if body != nil {
		req.ContentLength = size
	}
	at := c.now()
	req.Header.Set("X-Amz-Date", at.UTC().Format("20060102T150405Z"))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	auth, _, _ := sigV4(sigV4Input{method: method, path: escapedPath, query: rawQuery, payloadHash: payloadHash, headers: map[string]string{
		"host": host, "x-amz-date": req.Header.Get("X-Amz-Date"), "x-amz-content-sha256": payloadHash,
	}}, c.cfg.accessKey, c.cfg.secretKey, c.cfg.region, "s3", at)
	req.Header.Set("Authorization", auth)
	resp, err := c.http.Do(req)
	if err != nil {
		// Transport errors carry the URL only; the redaction is a second guard.
		return nil, errors.New(redactSecrets(fmt.Sprintf("S3 %s %s: %v", method, key, err), c.cfg.secrets()...))
	}
	return resp, nil
}

// s3Failure reports only the status and S3's error code: error bodies can
// echo the access key ID and the string to sign.
func s3Failure(op, key string, resp *http.Response) error {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	code := ""
	if match := s3ErrorCode.FindSubmatch(data); match != nil {
		code = " " + string(match[1])
	}
	return fmt.Errorf("S3 %s %s failed: HTTP %d%s", op, key, resp.StatusCode, code)
}

func (c *s3Client) key(name string) string { return c.cfg.prefix + name }

// upload PUTs a backup file and confirms its size with HEAD. Unsealed files
// are refused unless RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true.
func (c *s3Client) upload(ctx context.Context, file, name string) error {
	sealed, err := isSealedBackup(file)
	if err != nil {
		return err
	}
	if !sealed {
		if !c.cfg.allowPlaintext {
			return errPlaintextUpload
		}
		slog.Warn("UPLOADING AN UNENCRYPTED QUEUE BACKUP to S3 because RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true. Anyone with bucket access can read repository and job metadata. Set RUNQUILL_BACKUP_PASSPHRASE to seal backups.", "name", name)
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, contextReader{ctx, f})
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s3UploadTimeout)
	defer cancel()
	key := c.key(name)
	resp, err := c.do(ctx, http.MethodPut, key, nil, io.NopCloser(f), size, hex.EncodeToString(hash.Sum(nil)))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return s3Failure("PUT", key, resp)
	}
	resp.Body.Close()
	remote, err := c.head(ctx, key)
	if err != nil {
		return err
	}
	if remote != size {
		return fmt.Errorf("S3 object %s has %d bytes after upload, expected %d", key, remote, size)
	}
	return nil
}

var errPlaintextUpload = errors.New("refusing to upload an unsealed backup. Set RUNQUILL_BACKUP_PASSPHRASE to seal backups, or RUNQUILL_BACKUP_S3_ALLOW_PLAINTEXT=true to accept the risk")

func (c *s3Client) head(ctx context.Context, key string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s3RequestTimeout)
	defer cancel()
	resp, err := c.do(ctx, http.MethodHead, key, nil, nil, 0, s3EmptyHash)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("S3 HEAD %s failed: HTTP %d", key, resp.StatusCode)
	}
	return resp.ContentLength, nil
}

// download streams an object to w, bounded and checked against its length.
func (c *s3Client) download(ctx context.Context, key string, w io.Writer) error {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, 0, s3EmptyHash)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return s3Failure("GET", key, resp)
	}
	defer resp.Body.Close()
	if resp.ContentLength > s3MaxDownload {
		return errors.New("the S3 backup is larger than 64 GiB")
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, s3MaxDownload+1))
	if err != nil {
		return errors.New(redactSecrets(fmt.Sprintf("download S3 backup %s: %v", key, err), c.cfg.secrets()...))
	}
	if n > s3MaxDownload || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		return fmt.Errorf("download S3 backup %s: incomplete or oversized response", key)
	}
	return nil
}

type s3Object struct {
	Key  string
	Size int64
}

func (c *s3Client) list(ctx context.Context) ([]s3Object, error) {
	var objects []s3Object
	token := ""
	for range s3MaxListPages {
		// Listing only this feature's names keeps shared buckets within the page cap.
		query := url.Values{"list-type": {"2"}, "prefix": {c.cfg.prefix + "runquill-queue-"}}
		if token != "" {
			query.Set("continuation-token", token)
		}
		page, err := c.listPage(ctx, query)
		if err != nil {
			return nil, err
		}
		objects = append(objects, page.Contents...)
		if !page.IsTruncated {
			return objects, nil
		}
		if page.NextContinuationToken == "" {
			return nil, errors.New("S3 list response is truncated without a continuation token")
		}
		token = page.NextContinuationToken
	}
	return nil, errors.New("too many S3 list pages under the backup prefix")
}

type s3ListPage struct {
	Contents              []s3Object
	IsTruncated           bool
	NextContinuationToken string
}

func (c *s3Client) listPage(ctx context.Context, query url.Values) (s3ListPage, error) {
	var page s3ListPage
	ctx, cancel := context.WithTimeout(ctx, s3RequestTimeout)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, "", query, nil, 0, s3EmptyHash)
	if err != nil {
		return page, err
	}
	if resp.StatusCode != http.StatusOK {
		return page, s3Failure("LIST", c.cfg.prefix, resp)
	}
	defer resp.Body.Close()
	if err := xml.NewDecoder(io.LimitReader(resp.Body, s3MaxListBody)).Decode(&page); err != nil {
		return page, errors.New("S3 list response is not valid XML")
	}
	return page, nil
}

func (c *s3Client) delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, s3RequestTimeout)
	defer cancel()
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil, 0, s3EmptyHash)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return s3Failure("DELETE", key, resp)
	}
	resp.Body.Close()
	return nil
}

// pruneRemote deletes the oldest objects named by this feature beyond keep.
func (c *s3Client) pruneRemote(ctx context.Context, keep int) error {
	if keep == 0 {
		return nil
	}
	objects, err := c.list(ctx)
	if err != nil {
		return err
	}
	var names []string
	for _, object := range objects {
		name, ok := strings.CutPrefix(object.Key, c.cfg.prefix)
		if ok && backupFileName.MatchString(name) {
			names = append(names, name)
		}
	}
	sortBackupNames(names)
	for len(names) > keep {
		if err := c.delete(ctx, c.key(names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

// sortBackupNames orders oldest first by the fixed-width UTC timestamp.
func sortBackupNames(names []string) {
	sort.Slice(names, func(i, j int) bool {
		a, b := backupFileName.FindStringSubmatch(names[i])[1], backupFileName.FindStringSubmatch(names[j])[1]
		if a != b {
			return a < b
		}
		return names[i] < names[j]
	})
}

// backupRemoteState lists local backups confirmed in the bucket. It lives
// beside the backups so the queue database stays free of backup bookkeeping.
type backupRemoteState struct {
	Version  int                         `json:"version"`
	Uploaded map[string]backupRemoteMark `json:"uploaded"`
}

type backupRemoteMark struct {
	Size int64     `json:"size"`
	At   time.Time `json:"at"`
}

func loadBackupRemoteState(dir string) backupRemoteState {
	state := backupRemoteState{Version: 1, Uploaded: map[string]backupRemoteMark{}}
	f, err := os.Open(filepath.Join(dir, backupStateName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("read backup upload state; uploading every kept backup again", "error", err)
		}
		return state
	}
	defer f.Close()
	var loaded backupRemoteState
	if err := json.NewDecoder(io.LimitReader(f, backupStateMax)).Decode(&loaded); err != nil || loaded.Version != 1 {
		slog.Warn("backup upload state is unreadable; uploading every kept backup again")
		return state
	}
	for name, mark := range loaded.Uploaded {
		if backupFileName.MatchString(name) {
			state.Uploaded[name] = mark
		}
	}
	return state
}

func saveBackupRemoteState(dir string, state backupRemoteState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".runquill-backup-remote-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, backupStateName))
}

type remoteSyncResult struct {
	uploaded []string
	pending  int
}

// syncRemote uploads kept local backups not yet confirmed in the bucket,
// oldest first, then applies remote retention. A failure leaves the rest for
// the next tick.
func (c *s3Client) syncRemote(ctx context.Context, dir string, local []string) (remoteSyncResult, error) {
	var result remoteSyncResult
	state := loadBackupRemoteState(dir)
	for name := range state.Uploaded {
		if !slices.Contains(local, name) {
			delete(state.Uploaded, name)
		}
	}
	candidates := local
	if c.cfg.keep > 0 && len(candidates) > c.cfg.keep {
		// Older files would be pruned from the bucket right after upload.
		candidates = candidates[len(candidates)-c.cfg.keep:]
	}
	var pending []string
	for _, name := range candidates {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if mark, ok := state.Uploaded[name]; !ok || mark.Size != info.Size() {
			pending = append(pending, name)
		}
	}
	var failure error
	for index, name := range pending {
		err := c.upload(ctx, filepath.Join(dir, name), name)
		if errors.Is(err, errPlaintextUpload) {
			// Older unsealed backups must not hold up sealed ones behind them.
			slog.Warn("skip uploading an unsealed backup", "name", name)
			continue
		}
		if err != nil {
			result.pending = len(pending) - index
			failure = fmt.Errorf("upload %s: %w", name, err)
			break
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			result.pending = len(pending) - index
			failure = err
			break
		}
		state.Uploaded[name] = backupRemoteMark{Size: info.Size(), At: c.now().UTC()}
		result.uploaded = append(result.uploaded, name)
	}
	if err := saveBackupRemoteState(dir, state); err != nil {
		failure = errors.Join(failure, fmt.Errorf("save backup upload state: %w", err))
	}
	if failure != nil {
		return result, failure
	}
	if err := c.pruneRemote(ctx, c.cfg.keep); err != nil {
		return result, fmt.Errorf("prune remote backups: %w", err)
	}
	return result, nil
}

func backupObjectName(at time.Time, sealed bool) string {
	name := "runquill-queue-" + at.UTC().Format("20060102T150405Z") + ".sqlite3"
	if sealed {
		name += ".sealed"
	}
	return name
}

// uploadBackupCommand makes a one-off backup and uploads it.
func uploadBackupCommand(ctx context.Context, out io.Writer, database, output string, seal bool, passphraseFile string) error {
	cfg, err := loadS3Config(os.Getenv)
	if err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("set RUNQUILL_BACKUP_S3_ENDPOINT and RUNQUILL_BACKUP_S3_BUCKET to upload backups")
	}
	if !seal && !cfg.allowPlaintext {
		return errPlaintextUpload
	}
	identity, err := currentQueueBackupIdentity()
	if err != nil {
		return err
	}
	name := backupObjectName(time.Now(), seal)
	if output == "" {
		dir, err := uploadStagingDir(database)
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		output = filepath.Join(dir, name)
	}
	if seal {
		passphrase, err := backupPassphrase(os.Getenv, passphraseFile)
		if err != nil {
			return err
		}
		err = sealedQueueBackup(ctx, database, output, identity, passphrase)
		if err != nil {
			return err
		}
	} else if err := backupQueue(ctx, io.Discard, database, output); err != nil {
		return err
	}
	client := newS3Client(cfg)
	if err := client.upload(ctx, output, name); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Uploaded Runquill queue backup to s3://%s/%s.\n", cfg.bucket, client.key(name))
	return err
}

// uploadStagingDir creates a private directory for a one-off upload in RUNQUILL_BACKUP_DIR, or
// beside the queue, so the plaintext copy never lands in a shared or memory-backed temp dir.
// The dot prefix keeps it out of backup retention and remote sync.
func uploadStagingDir(database string) (string, error) {
	parent := strings.TrimSpace(os.Getenv("RUNQUILL_BACKUP_DIR"))
	if parent != "" {
		if err := os.MkdirAll(parent, 0700); err != nil {
			return "", fmt.Errorf("create RUNQUILL_BACKUP_DIR: %w", err)
		}
	} else {
		parent = filepath.Dir(database)
	}
	return os.MkdirTemp(parent, ".runquill-upload-*")
}

// restoreFromS3Command downloads a backup by key, or by name under the
// configured prefix, then restores it like a local file.
func restoreFromS3Command(ctx context.Context, out io.Writer, key, destination, passphraseFile string) error {
	cfg, err := loadS3Config(os.Getenv)
	if err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("set RUNQUILL_BACKUP_S3_ENDPOINT and RUNQUILL_BACKUP_S3_BUCKET to restore from S3")
	}
	if !strings.HasPrefix(key, cfg.prefix) {
		key = cfg.prefix + key
	}
	if strings.TrimPrefix(key, cfg.prefix) != path.Base(key) || !backupFileName.MatchString(path.Base(key)) {
		return errors.New("name a Runquill backup such as runquill-queue-20260101T000000Z.sqlite3.sealed under RUNQUILL_BACKUP_S3_PREFIX")
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if err := checkNewQueuePath(destination); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(destination), ".runquill-download-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, path.Base(key))
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := newS3Client(cfg).download(ctx, key, f); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return restoreQueueFrom(ctx, out, file, destination, passphraseFile)
}
