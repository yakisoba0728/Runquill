package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type remoteFixture struct {
	t      *testing.T
	q      *queue
	origin string
	now    time.Time
}

// newRemoteFixture serves the real panel handler on loopback with a sampled
// fleet and two jobs, so the CLI is tested against the actual API.
func newRemoteFixture(t *testing.T) *remoteFixture {
	t.Helper()
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	cfg, err := parsePanelOAuthConfig(origin, "client", "secret", "101")
	if err != nil {
		t.Fatal(err)
	}
	access, err := newPanelAccess(t.Context(), q.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(`INSERT INTO panel_users(github_id, login, role) VALUES (102, 'viewer', 'viewer'), (103, 'operator', 'operator')`); err != nil {
		t.Fatal(err)
	}
	panel := newPanel(config{})
	panel.snapshot.Fleet = &statusReport{Hosts: []statusHost{
		{Name: "build-1", Pool: "build", Status: "UP", SlotLimit: 4, OccupiedSlots: 1},
		{Name: "build-2", Pool: "build", Status: "OFFLINE", Cordoned: true, SlotLimit: 2, ReservedSlots: 2},
		{Name: "local", Status: "UNAVAILABLE", SlotLimit: 1, Error: "docker\x1b[2J down"},
	}}
	panel.snapshot.Inventory = []panelRunner{{Name: "runquill-7", Host: "build-1", Pool: "build", RuntimeState: "running", JobID: 7}}
	server.Config.Handler = panelHandler(panel, access)
	server.Start()
	t.Cleanup(server.Close)

	now := time.Now().Truncate(time.Second)
	for _, id := range []int64{7, 8} {
		j := job{id: id, repository: "owner/app", route: fmt.Sprintf("runquill-%d-1-build", id), discoverySource: "webhook"}
		if id == 8 {
			j.repository = "owner/lib"
		}
		if _, err := q.record(t.Context(), j, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Exec(`UPDATE jobs SET phase = ?, failure = 'image pull failed', changed_at = ? WHERE job_id = 8`, completed, now.Unix()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNQUILL_URL", origin)
	t.Setenv("RUNQUILL_TOKEN", "")
	previous := remoteNow
	remoteNow = func() time.Time { return now }
	t.Cleanup(func() { remoteNow = previous })
	return &remoteFixture{t: t, q: q, origin: origin, now: now}
}

func (f *remoteFixture) token(user int64, scope string) string {
	f.t.Helper()
	key := panelSecret()
	digest := panelDigest(key)
	if _, err := f.q.Exec(`INSERT INTO panel_tokens(digest, user_id, name, scope, expires_at, created_at) VALUES (?, ?, 'cli', ?, ?, ?)`,
		digest[:], user, scope, time.Now().Add(time.Hour).Unix(), time.Now().Unix()); err != nil {
		f.t.Fatal(err)
	}
	return "rqk_" + key
}

func runRemote(t *testing.T, args ...string) (handled bool, code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	handled, code = remoteCommand(args, &out, &errOut)
	return handled, code, out.String(), errOut.String()
}

// rows splits table output into whitespace-separated fields per line.
func rows(output string) [][]string {
	var result [][]string
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		result = append(result, strings.Fields(line))
	}
	return result
}

func TestRemoteCommandNamesDoNotShadowMainCommands(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var existing []string
	ast.Inspect(file, func(n ast.Node) bool {
		if compare, ok := n.(*ast.BinaryExpr); ok && compare.Op == token.EQL {
			if lit, ok := compare.Y.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				value, _ := strconv.Unquote(lit.Value)
				existing = append(existing, value)
			}
		}
		return true
	})
	for _, name := range []string{"status", "host", "gc", "worker", "backup"} {
		if !slices.Contains(existing, name) {
			t.Fatalf("main.go no longer dispatches %q; update this test's reading of main.go (found %v)", name, existing)
		}
	}
	for _, group := range remoteGroups {
		if slices.Contains(existing, group) {
			t.Errorf("remote command %q shadows a main.go subcommand", group)
		}
	}
	for _, name := range existing {
		if handled, _, _, _ := runRemote(t, name); handled {
			t.Errorf("remoteCommand handled main.go's %q", name)
		}
	}
	if handled, _, _, _ := runRemote(t); handled {
		t.Error("remoteCommand handled an empty command line")
	}
}

func TestRemoteCommandValidatesArguments(t *testing.T) {
	newRemoteFixture(t)
	t.Setenv("RUNQUILL_TOKEN", testAPIToken)
	for _, test := range []struct {
		args    []string
		message string
	}{
		{[]string{"jobs"}, "Usage:"},
		{[]string{"jobs", "delete"}, "Usage:"},
		{[]string{"jobs", "list", "--repo", "not a repo"}, "--repo must be OWNER/NAME"},
		{[]string{"jobs", "list", "--outcome", "lost"}, "--outcome must be waiting, running, finished or failed"},
		{[]string{"jobs", "list", "--since", "-1h"}, "--since must be a positive duration"},
		{[]string{"jobs", "list", "--since", "0d"}, "--since must be a positive duration"},
		{[]string{"jobs", "list", "--since", "soon"}, "--since must be a positive duration"},
		{[]string{"jobs", "list", "--limit", "0"}, "--limit must be between 1 and 100"},
		{[]string{"jobs", "list", "--limit", "101"}, "--limit must be between 1 and 100"},
		{[]string{"jobs", "list", "extra"}, "Usage: runquill jobs list"},
		{[]string{"jobs", "list", "--bogus"}, "flag provided but not defined"},
		{[]string{"jobs", "cancel"}, "Usage: runquill jobs cancel JOB_ID"},
		{[]string{"jobs", "cancel", "abc"}, "JOB_ID must be a positive GitHub job ID"},
		{[]string{"jobs", "retry", "0"}, "JOB_ID must be a positive GitHub job ID"},
		{[]string{"hosts", "cordon", " "}, "HOST must be a host name"},
		{[]string{"hosts", "uncordon", "a", "b"}, "Usage: runquill hosts uncordon HOST"},
		{[]string{"audit", "tail", "--limit", "500"}, "--limit must be between 1 and 200"},
		{[]string{"hosts", "list", "--token", "x"}, "pass the API token with RUNQUILL_TOKEN or --token-file"},
		{[]string{"hosts", "list", "-token=x"}, "pass the API token with RUNQUILL_TOKEN or --token-file"},
		{[]string{"hosts", "list", "--url", "http://panel.example"}, "HTTPS origin"},
		{[]string{"hosts", "list", "--token-file", filepath.Join(t.TempDir(), "missing")}, "could not open the token file"},
	} {
		handled, code, stdout, stderr := runRemote(t, test.args...)
		if !handled || code != 2 || stdout != "" || !strings.Contains(stderr, test.message) {
			t.Errorf("%v: handled=%v code=%d stdout=%q stderr=%q, want exit 2 with %q", test.args, handled, code, stdout, stderr, test.message)
		}
	}
	t.Setenv("RUNQUILL_TOKEN", "")
	if _, code, _, stderr := runRemote(t, "hosts", "list"); code != 2 || !strings.Contains(stderr, "set RUNQUILL_TOKEN or --token-file") {
		t.Errorf("missing token: code %d, %q", code, stderr)
	}
	t.Setenv("RUNQUILL_TOKEN", "secret-value")
	if _, code, _, stderr := runRemote(t, "hosts", "list"); code != 2 || !strings.Contains(stderr, "not a Runquill token") || strings.Contains(stderr, "secret-value") {
		t.Errorf("malformed token: code %d, %q", code, stderr)
	}
	t.Setenv("RUNQUILL_URL", "")
	if _, code, _, stderr := runRemote(t, "hosts", "list"); code != 2 || !strings.Contains(stderr, "set RUNQUILL_URL or --url") {
		t.Errorf("missing URL: code %d, %q", code, stderr)
	}
	if _, code, stdout, _ := runRemote(t, "hosts", "list", "-h"); code != 0 || stdout != "" {
		t.Errorf("help: code %d", code)
	}
}

// Tokens cannot read the audit log, so the CLI refuses before sending anything
// instead of asking the server to grant more.
func TestRemoteCommandsTokensCannotCallExplainWhy(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	t.Setenv("RUNQUILL_URL", server.URL)
	t.Setenv("RUNQUILL_TOKEN", testAPIToken)
	for _, args := range [][]string{{"audit", "tail"}} {
		_, code, stdout, stderr := runRemote(t, args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "not to API tokens") {
			t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	if requests != 0 {
		t.Fatalf("the CLI sent %d requests for commands tokens cannot call", requests)
	}
	if panelTokenCanCall("admin") || panelTokenCanCall("browser") ||
		!panelTokenCanCall("fleet:read") || !panelTokenCanCall("hosts:write") || !panelTokenCanCall("jobs:write") {
		t.Fatal("panelTokenCanCall disagrees with panelPermission")
	}
}

func TestRemoteFleetCommandsAgainstPanel(t *testing.T) {
	f := newRemoteFixture(t)
	t.Setenv("RUNQUILL_TOKEN", f.token(102, "fleet:read"))

	_, code, stdout, stderr := runRemote(t, "hosts", "list")
	want := [][]string{
		{"HOST", "POOL", "STATUS", "CORDONED", "SLOTS", "USED", "RESERVED", "ERROR"},
		{"build-1", "build", "UP", "no", "4", "1", "0", "-"},
		{"build-2", "build", "OFFLINE", "yes", "2", "0", "2", "-"},
		{"local", "-", "UNAVAILABLE", "no", "1", "0", "0", "docker", "[2J", "down"},
	}
	if code != 0 || stderr != "" || !slices.EqualFunc(rows(stdout), want, slices.Equal) {
		t.Fatalf("hosts list: code %d, stderr %q\n%s", code, stderr, stdout)
	}
	_, code, stdout, _ = runRemote(t, "hosts", "list", "--json")
	var hosts []statusHost
	if code != 0 || json.Unmarshal([]byte(stdout), &hosts) != nil || len(hosts) != 3 || !hosts[1].Cordoned {
		t.Fatalf("hosts list --json: code %d\n%s", code, stdout)
	}

	_, code, stdout, _ = runRemote(t, "pools", "list")
	want = [][]string{
		{"POOL", "HOSTS", "UP", "CORDONED", "SLOTS", "USED", "RESERVED"},
		{"(default)", "1", "0", "0", "1", "0", "0"},
		{"build", "2", "1", "1", "6", "1", "2"},
	}
	if code != 0 || !slices.EqualFunc(rows(stdout), want, slices.Equal) {
		t.Fatalf("pools list: code %d\n%s", code, stdout)
	}
	_, code, stdout, _ = runRemote(t, "pools", "list", "--json")
	var pools []remotePool
	if code != 0 || json.Unmarshal([]byte(stdout), &pools) != nil || len(pools) != 2 || pools[1] != (remotePool{"build", 2, 1, 1, 6, 1, 2}) {
		t.Fatalf("pools list --json: code %d\n%s", code, stdout)
	}

	_, code, stdout, _ = runRemote(t, "runners", "list")
	want = [][]string{{"RUNNER", "HOST", "POOL", "STATE", "JOB"}, {"runquill-7", "build-1", "build", "running", "7"}}
	if code != 0 || !slices.EqualFunc(rows(stdout), want, slices.Equal) {
		t.Fatalf("runners list: code %d\n%s", code, stdout)
	}
	_, code, stdout, _ = runRemote(t, "runners", "list", "--json")
	if code != 0 || strings.TrimSpace(stdout) != `[{"name":"runquill-7","host":"build-1","pool":"build","runtime_state":"running","job_id":7}]` {
		t.Fatalf("runners list --json: code %d\n%s", code, stdout)
	}

	// A fleet:read token cannot cordon; the server's refusal is shown as is.
	_, code, _, stderr = runRemote(t, "hosts", "cordon", "build-1")
	if code != 1 || !strings.Contains(stderr, "HTTP 403: You do not have access to this operation.") {
		t.Fatalf("cordon with a read token: code %d, %q", code, stderr)
	}
}

func TestRemoteJobsListAgainstPanel(t *testing.T) {
	f := newRemoteFixture(t)
	t.Setenv("RUNQUILL_TOKEN", f.token(102, "fleet:read"))
	_, code, stdout, stderr := runRemote(t, "jobs", "list")
	lines := rows(stdout)
	if code != 0 || stderr != "" || len(lines) != 3 || !slices.Equal(lines[0], []string{"JOB", "REPOSITORY", "OUTCOME", "POOL", "HOST", "QUEUED", "WAIT", "DETAIL"}) ||
		!slices.Equal(lines[1], []string{"8", "owner/lib", "failed", "-", "-", "2h", "ago", "2h", "image", "pull", "failed"}) ||
		!slices.Equal(lines[2][:6], []string{"7", "owner/app", "waiting", "-", "-", "2h"}) {
		t.Fatalf("jobs list: code %d, stderr %q\n%s", code, stderr, stdout)
	}
	_, code, stdout, _ = runRemote(t, "jobs", "list", "--repo", "Owner/App", "--outcome", "waiting", "--since", "3h", "--json")
	var page panelHistoryPageResponse
	if code != 0 || json.Unmarshal([]byte(stdout), &page) != nil || len(page.Jobs) != 1 || page.Jobs[0].ID != 7 {
		t.Fatalf("jobs list --json: code %d\n%s", code, stdout)
	}
	_, code, stdout, _ = runRemote(t, "jobs", "list", "--since", "1h")
	if code != 0 || strings.TrimSpace(stdout) != "No jobs match." {
		t.Fatalf("jobs list --since 1h: code %d\n%s", code, stdout)
	}
	_, code, stdout, stderr = runRemote(t, "jobs", "list", "--limit", "1")
	if code != 0 || len(rows(stdout)) != 2 || !strings.Contains(stderr, "More jobs match") {
		t.Fatalf("jobs list --limit 1: code %d, %q\n%s", code, stderr, stdout)
	}
}

func TestRemoteHostCordonAgainstPanel(t *testing.T) {
	f := newRemoteFixture(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(f.token(103, "hosts:write")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, code, stdout, stderr := runRemote(t, "hosts", "cordon", "build-1", "--token-file", tokenFile)
	if code != 0 || stderr != "" || strings.TrimSpace(stdout) != "Cordoned build-1; it takes no new runners." {
		t.Fatalf("cordon: code %d, %q, %q", code, stdout, stderr)
	}
	var cordoned bool
	if err := f.q.QueryRow(`SELECT cordoned FROM docker_hosts WHERE host = 'build-1'`).Scan(&cordoned); err != nil || !cordoned {
		t.Fatalf("host not cordoned: %v", err)
	}
	var audited int
	if err := f.q.QueryRow(`SELECT count(*) FROM panel_audit WHERE action = 'host.cordon' AND target = 'build-1' AND actor = 103`).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("cordon audit rows = %d, %v", audited, err)
	}
	if err := os.Chmod(tokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	_, code, stdout, stderr = runRemote(t, "hosts", "uncordon", "--token-file", tokenFile, "build-1", "--json")
	if code != 0 || strings.TrimSpace(stdout) != `{"host":"build-1","cordoned":false}` || !strings.Contains(stderr, "readable by other users") {
		t.Fatalf("uncordon --json: code %d, %q, %q", code, stdout, stderr)
	}
	_, code, _, stderr = runRemote(t, "hosts", "cordon", "ghost", "--token-file", tokenFile)
	if code != 1 || !strings.Contains(stderr, "HTTP 404: This host has no current observation.") {
		t.Fatalf("cordon unknown host: code %d, %q", code, stderr)
	}
}

// If the panel ever grants tokens these permissions, the commands already work.
func TestRemoteAuditAndJobChangesWhenTokensMayCall(t *testing.T) {
	previous := remoteTokenCanCall
	remoteTokenCanCall = func(string) bool { return true }
	t.Cleanup(func() { remoteTokenCanCall = previous })
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/audit":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"id":2,"actor":103,"actor_login":"operator","action":"host.cordon","target":"build-1","occurred_at":1700000060},{"id":1,"actor":101,"action":"token.create","target":"4","occurred_at":1700000000}]`)
		case "/api/jobs/7/cancel":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "This job has no retry delay to clear.", http.StatusConflict)
		}
	}))
	defer server.Close()
	t.Setenv("RUNQUILL_URL", server.URL)
	t.Setenv("RUNQUILL_TOKEN", testAPIToken)

	_, code, stdout, _ := runRemote(t, "audit", "tail", "--limit", "2")
	want := [][]string{
		{"ID", "TIME", "ACTOR", "ACTION", "TARGET"},
		{"1", "2023-11-14T22:13:20Z", "101", "token.create", "4"},
		{"2", "2023-11-14T22:14:20Z", "@operator", "host.cordon", "build-1"},
	}
	if code != 0 || !slices.EqualFunc(rows(stdout), want, slices.Equal) {
		t.Fatalf("audit tail: code %d\n%s", code, stdout)
	}
	_, code, stdout, _ = runRemote(t, "audit", "tail", "--json")
	if code != 0 || !strings.HasPrefix(stdout, `[{"id":2,`) {
		t.Fatalf("audit tail --json: code %d\n%s", code, stdout)
	}
	_, code, stdout, _ = runRemote(t, "jobs", "cancel", "7")
	if code != 0 || strings.TrimSpace(stdout) != "Cancelled job 7." {
		t.Fatalf("jobs cancel: code %d, %q", code, stdout)
	}
	_, code, _, stderr := runRemote(t, "jobs", "retry", "7", "--json")
	if code != 1 || !strings.Contains(stderr, "HTTP 409: This job has no retry delay to clear.") {
		t.Fatalf("jobs retry: code %d, %q", code, stderr)
	}
	wantCalls := []string{"GET /api/audit?limit=2", "GET /api/audit?limit=20", "POST /api/jobs/7/cancel", "POST /api/jobs/7/retry"}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
}
