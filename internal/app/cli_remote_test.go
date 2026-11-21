package app

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
	panel  *panelState
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
	if err := migrateUsage(t.Context(), q.DB); err != nil {
		t.Fatal(err)
	}
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
	panel.elastic = newElasticState(elasticConfig{Mode: elasticObserve, Interval: 10 * time.Second})
	panel.elastic.observe(elasticHost{Name: "local", Runners: []elasticRunner{{ID: "c1", Name: "runquill-7", CPULimit: 2, GuaranteeCPUs: 1,
		MemoryLimitMiB: 2048, GuaranteeMemoryMiB: 1024, CPUUsage: 1.234, MemoryUsageMiB: 900.5}}},
		elasticHostPlan{Host: "local", CPUs: 8, MemoryMiB: 16384, HeldSlots: 1, SurplusCPUs: 3.5, LentCPUs: 1, SurplusMemoryMiB: 4096, ExtraMemoryMiB: 1024})
	for i, reason := range []string{"cpu busy", "memory near limit"} {
		panel.elastic.record(elasticDecision{At: time.Unix(1700000000+int64(i)*60, 0), Error: []string{"", "\x1b[31mrejected"}[i],
			elasticChange: elasticChange{Host: "local", ID: "c1", Runner: "runquill-7", Resource: []string{"cpu", "memory"}[i], From: 1, To: 1.5, Reason: reason}})
	}
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
	return &remoteFixture{t: t, q: q, panel: panel, origin: origin, now: now}
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
	file, err := parser.ParseFile(token.NewFileSet(), "cli.go", nil, 0)
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
			t.Fatalf("cli.go no longer dispatches %q; update this test's reading of cli.go (found %v)", name, existing)
		}
	}
	for _, group := range remoteGroups {
		if slices.Contains(existing, group) {
			t.Errorf("remote command %q shadows a cli.go subcommand", group)
		}
	}
	for _, name := range existing {
		if handled, _, _, _ := runRemote(t, name); handled {
			t.Errorf("remoteCommand handled cli.go's %q", name)
		}
	}
	if handled, _, _, _ := runRemote(t); handled {
		t.Error("remoteCommand handled an empty command line")
	}
}

// The top-level usage spells out the remote commands that are a group by
// themselves exactly as the group help does.
func TestMainUsageListsSingleRemoteCommands(t *testing.T) {
	for name := range remoteCommands {
		if strings.Contains(name, " ") {
			continue
		}
		var line string
		for l := range strings.SplitSeq(remoteUsage, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "runquill "+name+" ") {
				line = strings.TrimSpace(l)
			}
		}
		if line == "" || !strings.Contains(mainUsage, "| "+line+" |") {
			t.Errorf("top-level usage does not show %q as the group help does", line)
		}
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
		{[]string{"jobs", "show"}, "Usage: runquill jobs show JOB_ID"},
		{[]string{"jobs", "show", "1.5"}, "JOB_ID must be a positive GitHub job ID"},
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
	// Help for one command goes to standard output too.
	for _, test := range []struct {
		args  []string
		usage string
	}{
		{[]string{"hosts", "list", "-h"}, "Usage: runquill hosts list [flags]"},
		{[]string{"usage", "--help"}, "Usage: runquill usage [flags]"},
		{[]string{"elastic", "-help"}, "Usage: runquill elastic [flags]"},
		{[]string{"jobs", "show", "--help"}, "Usage: runquill jobs show JOB_ID [flags]"},
		{[]string{"jobs", "list", "--repo", "owner/app", "-h"}, "Usage: runquill jobs list [flags]"},
	} {
		_, code, stdout, stderr := runRemote(t, test.args...)
		if code != 0 || stderr != "" || !strings.HasPrefix(stdout, test.usage+"\n") || !strings.Contains(stdout, "-token-file") {
			t.Errorf("%v: code %d, stdout %q, stderr %q", test.args, code, stdout, stderr)
		}
	}
	// Help for a group is asked for, so it goes to standard output and succeeds.
	for _, args := range [][]string{{"hosts", "--help"}, {"jobs", "-h"}, {"fleet", "help"}, {"audit", "-help"}} {
		handled, code, stdout, stderr := runRemote(t, args...)
		if !handled || code != 0 || stderr != "" || !strings.HasPrefix(stdout, "Usage:") ||
			!strings.Contains(stdout, "runquill fleet status") || !strings.Contains(stdout, "runquill usage") {
			t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
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
	at := f.now.Add(-time.Hour).UnixMilli()
	if _, err := f.q.Exec(`UPDATE jobs SET jit_requested_ms = ?, jit_registered_ms = ?, runner_launch_ms = ? WHERE job_id = 7`, at, at+250, at+750); err != nil {
		t.Fatal(err)
	}
	_, code, stdout, stderr = runRemote(t, "jobs", "list", "--repo", "owner/app")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "JIT registration 250ms") || !strings.Contains(stdout, "runtime launch 500ms") {
		t.Fatalf("jobs list with launch stages: code %d, stderr %q\n%s", code, stderr, stdout)
	}
}

func TestRemoteFleetStatusAgainstPanel(t *testing.T) {
	f := newRemoteFixture(t)
	t.Setenv("RUNQUILL_TOKEN", f.token(102, "fleet:read"))
	_, code, stdout, stderr := runRemote(t, "fleet", "status")
	want := [][]string{
		{"SAMPLED", "-"},
		{"BACKEND", "-"},
		{"QUEUED", "0"},
		{"RUNNING", "0"},
		{"HOSTS", "1", "of", "3", "up", "(1", "offline,", "1", "unavailable,", "1", "cordoned)"},
		{"SLOTS", "1", "used,", "2", "reserved", "of", "7"},
		{"LAST_WEBHOOK", "-"},
		{"LAST_POLL", "-"},
		{"PROBLEMS", "2"},
		{"KIND", "SUBJECT", "CODE", "MESSAGE"},
		{"host", "build-2", "offline", "-"},
		{"host", "local", "unavailable", "docker", "[2J", "down"},
	}
	got := slices.DeleteFunc(rows(stdout), func(fields []string) bool { return len(fields) == 0 })
	if code != 0 || stderr != "" || !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("fleet status: code %d, stderr %q\n%s", code, stderr, stdout)
	}
	// --check prints the same summary and fails on a problem.
	_, code, checked, stderr := runRemote(t, "fleet", "status", "--check")
	if code != 1 || checked != stdout || strings.TrimSpace(stderr) != "runquill fleet status: 2 problems need attention" {
		t.Fatalf("fleet status --check: code %d, stderr %q\n%s", code, stderr, checked)
	}
	// --json is the MCP fleet_status result, byte for byte.
	f.panel.mu.RLock()
	expected, _ := json.Marshal(summarizeFleet(f.panel.snapshot))
	f.panel.mu.RUnlock()
	_, code, stdout, _ = runRemote(t, "fleet", "status", "--json", "--check")
	if code != 1 || strings.TrimSpace(stdout) != string(expected) {
		t.Fatalf("fleet status --json: code %d\n%s\nwant %s", code, stdout, expected)
	}

	f.panel.mu.Lock()
	f.panel.snapshot.Fleet = &statusReport{Backend: "docker", SlotLimit: 4, Hosts: []statusHost{{Name: "build-1", Status: "UP", SlotLimit: 4, OccupiedSlots: 1}},
		Jobs: []statusJob{{ID: 7, Status: "RUNNING"}, {ID: 9, Status: "RETRY"}}}
	f.panel.mu.Unlock()
	_, code, stdout, stderr = runRemote(t, "fleet", "status", "--check")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "QUEUED        1 (1 retrying)") || !strings.Contains(stdout, "PROBLEMS      0") ||
		strings.Contains(stdout, "KIND") {
		t.Fatalf("healthy fleet status --check: code %d, stderr %q\n%s", code, stderr, stdout)
	}
}

func TestRemoteUsageAgainstPanel(t *testing.T) {
	f := newRemoteFixture(t)
	t.Setenv("RUNQUILL_TOKEN", f.token(102, "fleet:read"))
	_, code, stdout, stderr := runRemote(t, "usage", "--group", "repository")
	want := [][]string{
		{"REPOSITORY", "JOBS", "FAILED", "RUNNER_TIME", "WAIT"},
		{"owner/lib", "1", "1", "0s", "2h"},
		{"(total)", "1", "1", "0s", "2h"},
	}
	if code != 0 || stderr != "" || !slices.EqualFunc(rows(stdout), want, slices.Equal) {
		t.Fatalf("usage --group repository: code %d, stderr %q\n%s", code, stderr, stdout)
	}
	_, code, stdout, _ = runRemote(t, "usage", "--group", "pool", "--repo", "Owner/Lib")
	if lines := rows(stdout); code != 0 || len(lines) != 3 || lines[0][0] != "POOL" || lines[1][0] != "(default)" {
		t.Fatalf("usage --group pool: code %d\n%s", code, stdout)
	}
	today := f.now.UTC().Format(time.DateOnly)
	_, code, stdout, _ = runRemote(t, "usage", "--from", today, "--json")
	var report usageResponse
	if code != 0 || json.Unmarshal([]byte(stdout), &report) != nil || report.Group != "day" || report.From != today ||
		len(report.Series) != 1 || report.Totals.Jobs != 1 || report.Totals.WaitP95Seconds == 0 {
		t.Fatalf("usage --json: code %d\n%s", code, stdout)
	}
	_, code, stdout, _ = runRemote(t, "usage", "--group", "host", "--repo", "owner/none")
	if code != 0 || !strings.HasPrefix(stdout, "No finished jobs from ") {
		t.Fatalf("usage without jobs: code %d\n%s", code, stdout)
	}
	// Each mistake names its flag.
	for _, test := range []struct {
		args    []string
		message string
	}{
		{[]string{"usage", "--group", "week"}, "--group must be day, repository, pool or host"},
		{[]string{"usage", "--from", "2026-13-01"}, "--from must be a date as YYYY-MM-DD"},
		{[]string{"usage", "--to", "yesterday"}, "--to must be a date as YYYY-MM-DD"},
		{[]string{"usage", "--from", "2026-02-01", "--to", "2026-01-01"}, "--from must not be after --to"},
		{[]string{"usage", "--from", "2020-01-01", "--to", "2026-01-01"}, "--from and --to must cover at most 400 days"},
		{[]string{"usage", "--repo", "nobody"}, "--repo must be OWNER/NAME"},
		{[]string{"usage", "extra"}, "Usage: runquill usage"},
	} {
		if _, code, stdout, stderr := runRemote(t, test.args...); code != 2 || stdout != "" || !strings.Contains(stderr, test.message) {
			t.Errorf("%v: code %d, stdout %q, stderr %q, want %q", test.args, code, stdout, stderr, test.message)
		}
	}
	// 400 days counting both ends is the widest range.
	if _, code, _, stderr := runRemote(t, "usage", "--from", "2025-01-01", "--to", "2026-02-04"); code != 0 {
		t.Errorf("400-day range: code %d, stderr %q", code, stderr)
	}
}

func TestRemoteElasticAgainstPanel(t *testing.T) {
	f := newRemoteFixture(t)
	t.Setenv("RUNQUILL_TOKEN", f.token(102, "fleet:read"))
	_, code, stdout, stderr := runRemote(t, "elastic")
	want := [][]string{
		{"Elastic", "limits:", "observe,", "every", "10s."},
		{"HOST", "CPUS", "SURPLUS_CPUS", "LENT_CPUS", "MEMORY_MIB", "SURPLUS_MIB", "EXTRA_MIB", "HELD_SLOTS"},
		{"local", "8", "3.5", "1", "16384", "4096", "1024", "1"},
		{"RUNNER", "HOST", "CPUS", "GUARANTEE_CPUS", "CPU_USED", "MEMORY_MIB", "GUARANTEE_MIB", "MEMORY_USED_MIB"},
		{"runquill-7", "local", "2", "1", "1.23", "2048", "1024", "900.5"},
		{"TIME", "HOST", "RUNNER", "RESOURCE", "FROM", "TO", "APPLIED", "REASON"},
		{"2023-11-14T22:14:20Z", "local", "runquill-7", "memory", "1", "1.5", "failed", "memory", "near", "limit:", "[31mrejected"},
		{"2023-11-14T22:13:20Z", "local", "runquill-7", "cpu", "1", "1.5", "no", "cpu", "busy"},
	}
	got := slices.DeleteFunc(rows(stdout), func(fields []string) bool { return len(fields) == 0 })
	if code != 0 || stderr != "" || !slices.EqualFunc(got, want, slices.Equal) || strings.Contains(stdout, "\x1b") {
		t.Fatalf("elastic: code %d, stderr %q\n%s", code, stderr, stdout)
	}
	_, code, stdout, _ = runRemote(t, "elastic", "--json")
	var report elasticReport
	if code != 0 || json.Unmarshal([]byte(stdout), &report) != nil || report.Mode != elasticObserve || len(report.Decisions) != 2 {
		t.Fatalf("elastic --json: code %d\n%s", code, stdout)
	}

	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode((*elasticState)(nil).report())
	}))
	defer off.Close()
	t.Setenv("RUNQUILL_URL", off.URL)
	_, code, stdout, _ = runRemote(t, "elastic")
	if code != 0 || strings.TrimSpace(stdout) != "Elastic limits are off." {
		t.Fatalf("elastic off: code %d\n%s", code, stdout)
	}
}

func TestRemoteJobsShowAgainstPanel(t *testing.T) {
	f := newRemoteFixture(t)
	t.Setenv("RUNQUILL_TOKEN", f.token(102, "fleet:read"))
	_, code, stdout, stderr := runRemote(t, "jobs", "show", "8")
	fields := map[string]string{}
	for _, line := range rows(stdout) {
		fields[line[0]] = strings.Join(line[1:], " ")
	}
	queued := f.now.Add(-2*time.Hour).UTC().Format(time.RFC3339) + " (2h ago)"
	if code != 0 || stderr != "" || fields["JOB"] != "8" || fields["REPOSITORY"] != "owner/lib" || fields["OUTCOME"] != "failed" ||
		fields["LABEL"] != "runquill-8-1-build" || fields["QUEUED"] != queued || fields["WAIT"] != "2h" ||
		fields["FAILURE"] != "image pull failed" || fields["RETRY_AT"] != "-" ||
		fields["URL"] != "https://github.com/owner/lib/actions/runs/8/job/8" {
		t.Fatalf("jobs show 8: code %d, stderr %q\n%s", code, stderr, stdout)
	}
	// --json prints the panel's row, the same one jobs list --json pages.
	_, code, stdout, _ = runRemote(t, "jobs", "show", "8", "--json")
	_, _, listed, _ := runRemote(t, "jobs", "list", "--outcome", "failed", "--json")
	var row panelHistoryJob
	var page panelHistoryPageResponse
	if code != 0 || json.Unmarshal([]byte(stdout), &row) != nil || json.Unmarshal([]byte(listed), &page) != nil || len(page.Jobs) != 1 {
		t.Fatalf("jobs show --json: code %d\n%s\n%s", code, stdout, listed)
	}
	shown, _ := json.Marshal(row)
	if want, _ := json.Marshal(page.Jobs[0]); string(shown) != string(want) {
		t.Fatalf("jobs show --json: code %d\n%s\n%s", code, stdout, listed)
	}
	_, code, _, stderr = runRemote(t, "jobs", "show", "99")
	if code != 1 || strings.TrimSpace(stderr) != "runquill jobs show: the panel answered HTTP 404: This job is not in the job history." {
		t.Fatalf("jobs show 99: code %d, %q", code, stderr)
	}
	// A jobs:write token cannot read the fleet.
	t.Setenv("RUNQUILL_TOKEN", f.token(103, "jobs:write"))
	_, code, _, stderr = runRemote(t, "jobs", "show", "8")
	if code != 1 || !strings.Contains(stderr, "HTTP 403") {
		t.Fatalf("jobs show with a write token: code %d, %q", code, stderr)
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
