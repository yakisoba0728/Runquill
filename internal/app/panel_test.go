package app

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yakisoba0728/Runquill/internal/webui"
)

func TestPanelServerBoundsRequestBodyRead(t *testing.T) {
	server := panelHTTPServer(nil, nil)
	if server.ReadTimeout <= server.ReadHeaderTimeout || server.ReadTimeout >= server.WriteTimeout {
		t.Fatalf("request read timeout %v must allow body reads after headers and leave time to respond", server.ReadTimeout)
	}
}

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
		{live, "GET", "http://localhost:8081/demo/jobs.js", "", 404},
		{live, "GET", "http://localhost:8081/app.js", "VIEW CONTRACT", 200},
		{live, "GET", "http://localhost:8081/views/queue.js", "mount", 200},
		{live, "GET", "http://localhost:8081/views/", "", 404},
		{live, "GET", "http://localhost:8081/views/missing.js", "", 404},
		{live, "GET", "http://localhost:8081/views/..%2fpanel.css", "", 400},
		{live, "GET", "http://localhost:8081/wizard.js", "", 404},
		{live, "GET", "http://localhost:8081/index.html", "", 404},
		{live, "POST", "http://localhost:8081/snapshot", "", 405},
		{live, "GET", "http://public.example/snapshot", "", 403},
		{demo, "GET", "http://127.0.0.1:8081/", "data-mode=\"demo\"", 200},
		{demo, "GET", "http://127.0.0.1:8081/demo.js", "", 200},
		{demo, "GET", "http://127.0.0.1:8081/demo/jobs.js", "", 200},
		{demo, "GET", "http://127.0.0.1:8081/views/overview.js", "", 200},
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
// panelTranslations reads panelweb/i18n.js: every key of the words table
// (failing on a key written twice, since the later one silently wins) and the
// templates listed in serverPatterns.
func panelTranslations(t *testing.T) (keys, patterns map[string]bool) {
	t.Helper()
	source, err := fs.ReadFile(webui.Panel(), "i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "const serverPatterns = [")
	end := strings.Index(text[max(start, 0):], "].map(")
	if start < 0 || end < 0 {
		t.Fatal("panelweb/i18n.js has no serverPatterns list")
	}
	entry := regexp.MustCompile(`(?m)^\s*("(?:[^"\\]|\\.)*")[:,]`)
	unquote := func(match []string) string {
		key, err := strconv.Unquote(match[1])
		if err != nil {
			t.Fatalf("i18n key %s: %v", match[1], err)
		}
		return key
	}
	keys, patterns = make(map[string]bool), make(map[string]bool)
	for _, match := range entry.FindAllStringSubmatch(text[:start], -1) {
		key := unquote(match)
		if keys[key] {
			t.Errorf("panelweb/i18n.js defines %q more than once", key)
		}
		keys[key] = true
	}
	for _, match := range entry.FindAllStringSubmatch(text[start:start+end], -1) {
		patterns[unquote(match)] = true
	}
	return keys, patterns
}

// The sample panel must never change anything or open a live connection:
// fixtures answer GETs only, and screens reach the network only through the
// api.js client (which refuses everything else in the sample panel), never
// with their own fetch, EventSource or beacon.
func TestPanelSamplePanelIsReadOnly(t *testing.T) {
	fixture := regexp.MustCompile(`"([A-Z]+) /api/[^"]*"\s*:`)
	network := regexp.MustCompile(`\b(fetch|EventSource|XMLHttpRequest|sendBeacon|WebSocket)\b`)
	assets := webui.Panel()
	for _, pattern := range []string{"demo/*.js", "views/*.js", "demo.js", "fleet.js", "ui.js", "i18n.js"} {
		names, err := fs.Glob(assets, pattern)
		if err != nil || len(names) == 0 {
			t.Fatalf("%s: %v", pattern, err)
		}
		for _, name := range names {
			source, err := fs.ReadFile(assets, name)
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range fixture.FindAllStringSubmatch(string(source), -1) {
				if match[1] != "GET" {
					t.Errorf("%s: sample fixtures answer GET only, not %s", name, match[0])
				}
			}
			for _, line := range strings.Split(string(source), "\n") {
				code, _, _ := strings.Cut(line, "//")
				if network.MatchString(code) {
					t.Errorf("%s: use the ctx.api client instead of %q", name, strings.TrimSpace(line))
				}
			}
		}
	}
}

func TestPanelTranslationsHaveNoDuplicateKeys(t *testing.T) {
	keys, patterns := panelTranslations(t)
	if len(keys) < 500 || len(patterns) < 10 {
		t.Fatalf("read too few keys (%d) or server patterns (%d)", len(keys), len(patterns))
	}
	for pattern := range patterns {
		if !keys[pattern] {
			t.Errorf("serverPatterns lists %q without a translation", pattern)
		}
	}
}

func TestPanelServerMessagesHaveTranslations(t *testing.T) {
	keys, patterns := panelTranslations(t)

	// Each file is scanned where the panel can see its messages: whole files
	// for handlers, the route layer only where the rest of the file is
	// configuration or command-line output.
	files := map[string]func(fn *ast.FuncDecl) bool{
		"panel.go":            nil,
		"panel_controls.go":   nil,
		"panel_settings.go":   nil,
		"panel_operations.go": nil,
		"panel_jobs.go":       nil,
		"panel_team.go":       nil,
		"panel_backups.go":    nil,
		"panel_migrate.go":    nil,
		"events.go":           nil,
		// The rest of usage.go is the rollup, which reports to the log.
		"usage.go": func(fn *ast.FuncDecl) bool {
			return fn == nil || fn.Name.Name == "registerUsageRoutes" || fn.Name.Name == "parseUsageQuery"
		},
		// panel_access.go serves OAuth pages and startup errors outside the
		// panel script; its audit actions still count.
		"panel_access.go": func(*ast.FuncDecl) bool { return false },
		"operations.go":   func(fn *ast.FuncDecl) bool { return fn != nil && fn.Name.Name == "resolve" },
		"elastic.go":      func(fn *ast.FuncDecl) bool { return fn != nil && strings.HasPrefix(fn.Name.Name, "register") },
		// errRunnerLogLimit only ends a copy; the client sees the limit marker.
		"runner_logs.go": func(fn *ast.FuncDecl) bool { return fn != nil },
		// Migrate previews show refusal reasons and mapping errors from these
		// two functions; the sentinel errors are mapped to sentences in
		// writeMigrateFailure.
		"../workflow/rewrite.go": func(fn *ast.FuncDecl) bool {
			return fn != nil && (fn.Name.Name == "ValidateMappings" || fn.Name.Name == "runquillRunsOn")
		},
		"migrate_github.go": func(*ast.FuncDecl) bool { return false },
	}
	// Handlers may send a non-literal message only here, and only when its
	// text is a checked sentinel or a dynamic detail handled by serverPatterns.
	dynamic := map[string]bool{
		"panel_controls.go:setTeamUser":           true,
		"panel_settings.go:saveOperationSettings": true,
		"events.go:serve":                         true, // errPanelEventsFull
		"usage.go:registerUsageRoutes":            true, // errUsageQuery
		"panel_migrate.go:preview":                true, // validateRunsOnMapping
		"panel_migrate.go:open":                   true, // validateRunsOnMapping
		"runner_logs.go:runnerLogUnavailable":     true, // its callers' literals
	}
	// Refusal reasons are shown wherever they are produced, in every file.
	refusalFields := map[string]bool{"Refused": true, "refusal": true}
	roles := []string{"viewer", "operator", "admin"}
	var messages, formats, actions []string
	fset := token.NewFileSet()
	literal := func(expr ast.Expr) (string, bool) {
		lit, ok := expr.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(lit.Value)
		return value, err == nil
	}
	// text collects a literal, or the format of fmt.Sprintf/Errorf, and
	// reports whether expr was either.
	text := func(expr ast.Expr) bool {
		if value, ok := literal(expr); ok {
			messages = append(messages, value)
			return true
		}
		if call, ok := expr.(*ast.CallExpr); ok && len(call.Args) > 0 {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Sprintf" || sel.Sel.Name == "Errorf") {
				if value, ok := literal(call.Args[0]); ok {
					formats = append(formats, value)
					return true
				}
			}
		}
		return false
	}
	for name, scope := range files {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			userFacing := scope == nil || scope(fn)
			function := ""
			if isFunc {
				function = name + ":" + fn.Name.Name
			}
			assigned := make(map[string][]ast.Expr)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					if len(node.Lhs) != len(node.Rhs) {
						return true
					}
					for i, lhs := range node.Lhs {
						switch target := lhs.(type) {
						case *ast.Ident:
							assigned[target.Name] = append(assigned[target.Name], node.Rhs[i])
						case *ast.SelectorExpr:
							if target.Sel.Name == "DockerError" && userFacing || refusalFields[target.Sel.Name] {
								text(node.Rhs[i])
							}
						}
					}
				case *ast.ValueSpec:
					// const reasonX = "..." in the workflow rewriter
					for i, id := range node.Names {
						if strings.HasPrefix(id.Name, "reason") && i < len(node.Values) {
							text(node.Values[i])
						}
					}
				case *ast.CompositeLit:
					// {Error: "..."} and map[string]string{"error": "..."} JSON bodies.
					for _, elt := range node.Elts {
						pair, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, _ := literal(pair.Key)
						if id, ok := pair.Key.(*ast.Ident); ok {
							key = id.Name
						}
						if refusalFields[key] {
							text(pair.Value)
							continue
						}
						if (key == "Error" || key == "error") && userFacing {
							if !text(pair.Value) && !dynamic[function] {
								t.Errorf("%s: a JSON error needs a literal message with an i18n key", fset.Position(pair.Pos()))
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
				case (callee == "http.Error" || callee == "runnerLogUnavailable") && userFacing:
					if !text(call.Args[1]) && !dynamic[function] {
						t.Errorf("%s: %s needs a literal message with an i18n key", position, callee)
					}
				case callee == "errors.New" && userFacing:
					text(call.Args[0])
				case callee == "fmt.Errorf" && userFacing && name == "../workflow/rewrite.go":
					text(call)
				case strings.HasSuffix(callee, ".refuse") || callee == "refuse":
					if len(call.Args) == 2 && !text(call.Args[1]) {
						if id, ok := call.Args[1].(*ast.Ident); !ok || !strings.HasPrefix(id.Name, "reason") {
							t.Errorf("%s: a refusal reason must be a literal or a reason constant", position)
						}
					}
				case callee == "auditPanel" && len(call.Args) == 5, strings.HasSuffix(callee, ".change") && len(call.Args) == 5,
					callee == ".recordAudit" && len(call.Args) == 4:
					arg := call.Args[3]
					if callee != "auditPanel" {
						arg = call.Args[2]
					}
					candidates := []ast.Expr{arg}
					if id, ok := arg.(*ast.Ident); ok {
						if isFunc && (fn.Name.Name == "change" || fn.Name.Name == "recordAudit") {
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
	if len(messages) < 80 || len(formats) < 10 || len(actions) < 15 {
		t.Fatalf("scanned too few messages (%d), formats (%d) or audit actions (%d)", len(messages), len(formats), len(actions))
	}
	for _, value := range append(messages, actions...) {
		if !keys[value] {
			t.Errorf("panelweb/i18n.js has no translation for %q", value)
		}
	}
	// A message with values filled in must match a serverPatterns template:
	// each Go verb stands where the template has a {placeholder}.
	verb := regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)
	placeholder := regexp.MustCompile(`\{[a-z]+\}`)
	normalize := func(value string, wildcard *regexp.Regexp) string {
		return wildcard.ReplaceAllString(value, "\x00")
	}
	templates := make(map[string]bool)
	for pattern := range patterns {
		templates[normalize(pattern, placeholder)] = true
	}
	for _, format := range formats {
		if !templates[normalize(format, verb)] {
			t.Errorf("panelweb/i18n.js serverPatterns has no template for %q", format)
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
