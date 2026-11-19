package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPanelRoutesAndFirstJobEvidence(t *testing.T) {
	ctx := t.Context()
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	now := time.Now().UTC().Truncate(time.Second)
	j := job{id: 13, repository: "owner/repository", route: "runquill-12-1-first-job", discoverySource: "poll"}
	if _, err := q.record(ctx, j, now); err != nil {
		t.Fatal(err)
	}
	p := newPanel(config{pollInterval: 30 * time.Second, repositoryScope: scopeSelected})
	p.markPoll(now, true)
	host := hostCapacity{dockerHostSnapshot: dockerHostSnapshot{Name: "local", Containers: map[string]string{"orphan": "exited"}}, Limit: 1, Cordoned: true}
	p.capture(ctx, q, []hostCapacity{host}, 0, 1, nil, nil)

	rec := httptest.NewRecorder()
	access := panelAccessForTest(t, q)
	key, err := access.startSession(ctx, panelIdentity{GitHubID: 101, Login: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	live := panelHandler(p, access)
	request := httptest.NewRequest(http.MethodGet, "http://localhost:8081/snapshot", nil)
	request.AddCookie(&http.Cookie{Name: access.cookieName("session"), Value: key})
	live.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("snapshot response: %d %v", rec.Code, rec.Header())
	}
	var got panelSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.LastPoll == nil || !got.FirstJobSeen || got.FirstJobClean != nil ||
		got.Fleet.RepositoryScope != scopeSelected || len(got.Fleet.Jobs) != 1 ||
		got.Fleet.Jobs[0].Timing.DiscoverySource != "poll" || got.Fleet.Jobs[0].RunnerLabel != j.route ||
		got.Fleet.Jobs[0].Diagnostic == nil || got.Fleet.Jobs[0].Diagnostic.Code != "workers_cordoned" ||
		len(got.Inventory) != 1 || got.Inventory[0].Name != "orphan" || got.Inventory[0].RuntimeState != "exited" {
		t.Fatalf("first job progress: %+v", got)
	}

	if _, err := q.ExecContext(ctx, `UPDATE jobs SET phase = ?, github_started_at = ?, runner_cleaned_at = ?,
        runner_cleaned_host = ? WHERE job_id = ?`, completed, now.Unix(), now.Unix(), "local", j.id); err != nil {
		t.Fatal(err)
	}
	p.capture(ctx, q, []hostCapacity{host}, 0, 1, nil, nil)
	if got := p.snapshot; len(got.Fleet.Jobs) != 0 || len(got.Recent) != 1 ||
		got.FirstJobClean == nil || got.FirstJobClean.URL != "https://github.com/owner/repository/actions/runs/12/job/13" {
		t.Fatalf("first job cleanup: %+v", got)
	}

	demo := panelHandler(nil, nil)
	for _, tc := range []struct {
		h                 http.Handler
		method, url, body string
		code              int
	}{
		{live, "GET", "http://localhost:8081/", "data-mode=\"live\"", 200},
		{live, "GET", "http://localhost:8081/demo.js", "", 404},
		{live, "POST", "http://localhost:8081/snapshot", "", 405},
		{live, "GET", "http://public.example/snapshot", "", 403},
		{demo, "GET", "http://127.0.0.1:8081/", "data-mode=\"demo\"", 200},
		{demo, "GET", "http://127.0.0.1:8081/demo.js", "", 200},
		{demo, "GET", "http://127.0.0.1:8081/snapshot", "", 404},
		{webhook(q, &github{owner: "owner"}, []byte("key"), p), "GET", "http://localhost:8080/snapshot", "", 404},
	} {
		rec := httptest.NewRecorder()
		tc.h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.url, nil))
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.url, rec.Code, tc.code)
		}
	}
}

// Server messages reach the browser as plain text and are translated by using
// the English sentence as a key in panelweb/i18n.js. A message or audit action
// without a key would silently show untranslated text.
func TestPanelServerMessagesHaveTranslations(t *testing.T) {
	source, err := os.ReadFile("panelweb/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]bool)
	for _, match := range regexp.MustCompile(`(?m)^\s*("(?:[^"\\]|\\.)*"):`).FindAllStringSubmatch(string(source), -1) {
		key, err := strconv.Unquote(match[1])
		if err != nil {
			t.Fatalf("i18n key %s: %v", match[1], err)
		}
		keys[key] = true
	}

	// Handlers may pass a non-literal message to http.Error only here, and only
	// for errors whose text is either a checked sentinel or a dynamic detail
	// handled by serverPatterns in i18n.js.
	dynamic := map[string]bool{"setTeamUser": true, "saveOperationSettings": true}
	roles := []string{"viewer", "operator", "admin"}
	var messages, actions []string
	fset := token.NewFileSet()
	for _, name := range []string{"panel.go", "panel_controls.go", "panel_settings.go", "panel_operations.go", "panel_jobs.go", "panel_team.go", "panel_access.go", "operations.go"} {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		literal := func(expr ast.Expr) (string, bool) {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return "", false
			}
			value, err := strconv.Unquote(lit.Value)
			return value, err == nil
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			// panel_access.go serves OAuth pages and startup errors outside the
			// panel script, and operations.go only exposes resolve to the panel.
			userFacing := name != "panel_access.go" && (name != "operations.go" || isFunc && fn.Name.Name == "resolve")
			assigned := make(map[string][]ast.Expr)
			ast.Inspect(decl, func(n ast.Node) bool {
				if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == len(assign.Rhs) {
					for i, lhs := range assign.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							assigned[id.Name] = append(assigned[id.Name], assign.Rhs[i])
						}
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "DockerError" && userFacing {
							if value, ok := literal(assign.Rhs[i]); ok {
								messages = append(messages, value)
							}
						}
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := fun.X.(*ast.Ident); ok {
						callee = pkg.Name + "." + fun.Sel.Name
					} else {
						callee = "." + fun.Sel.Name
					}
				case *ast.Ident:
					callee = fun.Name
				}
				position := fset.Position(call.Pos())
				switch {
				case callee == "http.Error" && userFacing:
					if value, ok := literal(call.Args[1]); ok {
						messages = append(messages, value)
					} else if !isFunc || !dynamic[fn.Name.Name] {
						t.Errorf("%s: http.Error needs a literal message with an i18n key", position)
					}
				case callee == "errors.New" && userFacing:
					if value, ok := literal(call.Args[0]); ok {
						messages = append(messages, value)
					}
				case callee == "auditPanel" && len(call.Args) == 5, strings.HasSuffix(callee, ".change") && len(call.Args) == 5:
					arg := call.Args[3]
					if callee != "auditPanel" {
						arg = call.Args[2]
					}
					candidates := []ast.Expr{arg}
					if id, ok := arg.(*ast.Ident); ok {
						if isFunc && fn.Name.Name == "change" {
							return true // forwards its callers' actions
						}
						candidates = assigned[id.Name]
					}
					if len(candidates) == 0 {
						t.Errorf("%s: audit action must be a string literal", position)
					}
					for _, expr := range candidates {
						if value, ok := literal(expr); ok {
							actions = append(actions, value)
							continue
						}
						if sum, ok := expr.(*ast.BinaryExpr); ok {
							if prefix, ok := literal(sum.X); ok && prefix == "user.set." {
								for _, role := range roles {
									actions = append(actions, prefix+role)
								}
								continue
							}
						}
						t.Errorf("%s: audit action must be a string literal", position)
					}
				}
				return true
			})
		}
	}
	if len(messages) < 30 || len(actions) < 10 {
		t.Fatalf("scanned too few messages (%d) or audit actions (%d)", len(messages), len(actions))
	}
	for _, value := range append(messages, actions...) {
		if !keys[value] {
			t.Errorf("panelweb/i18n.js has no translation for %q", value)
		}
	}
}

func TestPanelBackupOutlivesServerWriteTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	t.Setenv("RUNQUILL_DB", path)
	t.Setenv("RUNQUILL_OWNER", "owner")
	t.Setenv("RUNQUILL_APP_ID", "1")
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	access := panelAccessForTest(t, q)
	key, err := access.startSession(t.Context(), panelIdentity{GitHubID: 101, Login: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	handler := panelHandler(newPanel(config{}), access)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stands in for a queue large enough to outlast the server write timeout.
		time.Sleep(300 * time.Millisecond)
		handler.ServeHTTP(w, r)
	}))
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/backup", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = access.cfg.host
	request.Header.Set("Origin", access.cfg.origin)
	request.AddCookie(&http.Cookie{Name: access.cookieName("session"), Value: key})
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("backup request failed: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "SQLite format 3") ||
		response.ContentLength != int64(len(body)) {
		t.Fatalf("backup download: status %d, %d of %d bytes, %v", response.StatusCode, len(body), response.ContentLength, err)
	}

	var actions int
	if err := q.QueryRowContext(t.Context(), "SELECT count(*) FROM panel_audit WHERE action = 'queue.backup'").Scan(&actions); err != nil || actions != 1 {
		t.Fatalf("backup audit entries: %d, %v", actions, err)
	}
	select {
	case panelOperation <- struct{}{}:
		<-panelOperation
	default:
		t.Fatal("the backup kept the operation slot after it finished")
	}
}

func TestPanelRunnerInventoryKeepsUnobservedLeases(t *testing.T) {
	hosts := []hostCapacity{
		{dockerHostSnapshot: dockerHostSnapshot{Name: "online", Pool: "build", Containers: map[string]string{"tracked": "running", "legacy": "created"}}},
		{dockerHostSnapshot: dockerHostSnapshot{Name: "offline", Pool: "build"}},
	}
	jobs := []statusJob{
		{ID: 10, Runner: "tracked", RunnerHost: "online"},
		{ID: 20, Runner: "leased", RunnerHost: "offline", Pool: "build"},
		{ID: 30, Runner: "legacy"},
	}
	rows := panelRunnerInventory(hosts, jobs)
	if len(rows) != 3 || rows[0].JobID != 20 || rows[0].RuntimeState != "" ||
		rows[1].JobID != 30 || rows[1].RuntimeState != "created" || rows[2].JobID != 10 ||
		rows[2].RuntimeState != "running" || rows[2].Pool != "build" {
		t.Fatalf("unexpected runner inventory: %+v", rows)
	}
}
