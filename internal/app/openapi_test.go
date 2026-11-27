package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yakisoba0728/Runquill/internal/workflow"
)

// pendingRoutes lists panel routes that are registered but not yet described
// in api/openapi.yaml, keyed "METHOD /path" with the reason. Add an entry only
// to land a route before its documentation; the test fails once the route is
// documented or removed, so an entry cannot go stale.
var pendingRoutes = map[string]string{}

const openAPIPath = "../../api/openapi.yaml"

var openAPIMethods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

type panelRoute struct {
	method, path, permission, pos string
}

func (r panelRoute) key() string { return r.method + " " + r.path }

type specOperation struct {
	method, path string
	node         *specNode
}

func (o specOperation) key() string { return o.method + " " + o.path }

func TestOpenAPIDocumentsEveryPanelRoute(t *testing.T) {
	spec := loadOpenAPI(t)
	routes := discoverPanelRoutes(t)
	if len(routes) < 20 {
		t.Fatalf("found only %d panel routes; the route discovery in openapi_test.go no longer matches panelHandler", len(routes))
	}
	operations := openAPIOperations(t, spec)
	for _, key := range sortedKeys(routes) {
		route := routes[key]
		op, documented := operations[key]
		_, pending := pendingRoutes[key]
		switch {
		case documented && pending:
			t.Errorf("%s is documented in %s now; remove it from pendingRoutes in openapi_test.go", key, openAPIPath)
		case !documented && !pending:
			t.Errorf("%s (%s, permission %q) is registered but missing from %s. Add paths.%s.%s with operationId, x-runquill-permission: %s, security and responses, or list %q in pendingRoutes in openapi_test.go",
				key, route.pos, route.permission, openAPIPath, route.path, strings.ToLower(route.method), route.permission, key)
		case documented:
			if got := op.node.get("x-runquill-permission").scalar(); got != route.permission {
				t.Errorf("%s:%d %s has x-runquill-permission %q, but %s requires %q", openAPIPath, op.node.line, key, got, route.pos, route.permission)
			}
		}
	}
	for _, key := range sortedKeys(operations) {
		if _, ok := routes[key]; !ok {
			t.Errorf("%s:%d documents %s, which no panel route registers; remove it or register the route", openAPIPath, operations[key].node.line, key)
		}
	}
	for key := range pendingRoutes {
		if _, ok := routes[key]; !ok {
			t.Errorf("pendingRoutes lists %s, which no panel route registers; remove the entry", key)
		}
	}
}

// The route discovery reads source, so check it against the real handler: every
// discovered and documented route must reach a handler, not the router's 404/405.
func TestOpenAPIRoutesAreServed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.sqlite3")
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	access := panelAccessForTest(t, q)
	panel := newPanel(config{})
	panel.operations = &operations{}
	live, demo := panelHandler(panel, access, false), panelHandler(nil, nil, false)
	liveV2, demoV2 := panelHandler(panel, access, true), panelHandler(nil, nil, true)
	served := func(handler http.Handler, origin, method, path string) bool {
		path = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "1")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(method, origin+path, nil))
		return res.Code != http.StatusNotFound && res.Code != http.StatusMethodNotAllowed
	}
	keys := make(map[string]bool)
	for key := range discoverPanelRoutes(t) {
		keys[key] = true
	}
	for key := range openAPIOperations(t, loadOpenAPI(t)) {
		keys[key] = true
	}
	for _, key := range sortedKeys(keys) {
		method, path, _ := strings.Cut(key, " ")
		if !served(live, access.cfg.origin, method, path) && !served(demo, "http://127.0.0.1:8081", method, path) &&
			!served(liveV2, access.cfg.origin, method, path) && !served(demoV2, "http://127.0.0.1:8081", method, path) {
			t.Errorf("%s does not reach a handler in the live or demo panel", key)
		}
	}
}

func TestOpenAPIOperationsAreComplete(t *testing.T) {
	spec := loadOpenAPI(t)
	if got := spec.get("openapi").scalar(); got != "3.1.0" {
		t.Fatalf("openapi = %q, want 3.1.0", got)
	}
	if !strings.Contains(panelAccessSchema, "scope IN ('"+strings.Join(panelTokenScopes, "','")+"')") {
		t.Fatalf("panelTokenScopes %v no longer matches the panel_tokens scope check", panelTokenScopes)
	}
	ids := make(map[string]string)
	for key, op := range openAPIOperations(t, spec) {
		where := fmt.Sprintf("%s:%d %s", openAPIPath, op.node.line, key)
		id := op.node.get("operationId").scalar()
		if id == "" {
			t.Errorf("%s has no operationId", where)
		} else if other, dup := ids[id]; dup {
			t.Errorf("%s reuses operationId %s from %s", where, id, other)
		}
		ids[id] = key
		permission := op.node.get("x-runquill-permission").scalar()
		security := op.node.get("security")
		responses := op.node.get("responses")
		if responses == nil || len(responses.keys) == 0 {
			t.Errorf("%s has no responses", where)
		}
		if permission == "public" {
			if security == nil || security.value != "[]" {
				t.Errorf("%s is public and must declare security: []", where)
			}
		} else {
			var schemes []string
			for _, item := range security.items() {
				schemes = append(schemes, item.keys...)
			}
			slices.Sort(schemes)
			want := []string{"sessionCookie"}
			if panelTokenCanCall(permission) {
				want = []string{"bearerToken", "sessionCookie"}
			}
			if key == "POST /mcp" {
				want = []string{"bearerToken"}
			}
			if !slices.Equal(schemes, want) {
				t.Errorf("%s with permission %s declares security %v, want %v", where, permission, schemes, want)
			}
			for _, status := range []string{"401", "403"} {
				if responses.get(status) == nil {
					t.Errorf("%s lacks the %s response every protected route can return", where, status)
				}
			}
		}
		for _, match := range regexp.MustCompile(`\{([^}]+)\}`).FindAllStringSubmatch(op.path, -1) {
			found := false
			for _, param := range op.node.get("parameters").items() {
				found = found || param.get("name").scalar() == match[1] && param.get("in").scalar() == "path" &&
					param.get("required").scalar() == "true"
			}
			if !found {
				t.Errorf("%s lacks a required path parameter %q", where, match[1])
			}
		}
	}
	var walk func(*specNode)
	walk = func(n *specNode) {
		if n == nil {
			return
		}
		for _, key := range n.keys {
			child := n.m[key]
			if key == "$ref" {
				parts := strings.Split(strings.TrimPrefix(child.value, "#/"), "/")
				if len(parts) != 3 || parts[0] != "components" || spec.get("components").get(parts[1]).get(parts[2]) == nil {
					t.Errorf("%s:%d has an unresolved $ref %s", openAPIPath, child.line, child.value)
				}
			}
			walk(child)
		}
		for _, item := range n.seq {
			walk(item)
		}
	}
	walk(spec)
}

// Schemas named here must list exactly the JSON fields the Go type encodes
// and require exactly those that cannot be omitted.
func TestOpenAPISchemasMatchGoStructs(t *testing.T) {
	spec := loadOpenAPI(t)
	schemas := spec.get("components").get("schemas")
	for name, value := range map[string]any{
		"Identity": panelIdentity{}, "TeamMember": panelMember{}, "Token": panelToken{}, "AuditEvent": panelAuditEvent{},
		"HistoryJob": panelHistoryJob{}, "JobHistoryPage": panelHistoryPageResponse{}, "Snapshot": panelSnapshot{},
		"StatusReport": statusReport{}, "StatusRunners": statusRunners{}, "StatusHost": statusHost{}, "StatusJob": statusJob{},
		"JobTiming": statusJobTiming{}, "JobDiagnostic": jobDiagnostic{}, "Runner": panelRunner{}, "CompletedJob": panelCompleted{},
		"OperationSettings": operationSettingsResponse{}, "OperationPolicy": operationPolicy{}, "WorkerPool": workerPool{},
		"RunnerImage": runnerImage{}, "JobResources": jobResources{}, "RightsizingResource": rightsizingResource{},
		"RightsizingGroup": rightsizingGroup{}, "RightsizingReport": rightsizingResponse{},
		"HostResourcesReport": hostResourcesReport{}, "HostResources": hostResources{},
		"HostReservation": hostReservation{}, "HostUsage": hostUsage{}, "RunnerDiskUse": runnerDiskUse{},
		"ElasticReport": elasticReport{}, "ElasticHostPlan": elasticHostPlan{},
		"ElasticRunner": elasticRunnerView{}, "ElasticDecision": elasticDecision{},
		"UsageReport": usageResponse{}, "UsageTotals": usageTotals{}, "UsageGroup": usageGroup{},
		"MigrationError": migrateFailure{}, "MigrationJob": workflow.JobOutcome{},
		"MigrationFile": migrationFile{}, "MigrationPreview": migrationPreview{}, "MigrationPullRequest": migrationPR{},
		"BackupStatus": backupStatusReport{}, "BackupVerification": backupVerification{},
		"BackupRemote": backupRemoteSummary{}, "BackupRun": backupSnapshot{},
		"JSONRPCResponse": rpcResponse{},
	} {
		checkOpenAPIFields(t, name, schemas, schemas.get(name), value)
	}
	rpcErrorSchema := schemas.get("JSONRPCResponse").get("properties").get("error")
	checkOpenAPIFields(t, "JSONRPCResponse.error", schemas, rpcErrorSchema, rpcError{})
	enrollment := spec.get("paths").get("/api/hosts/{name}/enrollment").get("post").get("responses").get("201")
	checkOpenAPIFields(t, "worker enrollment response", schemas,
		enrollment.get("content").get("application/json").get("schema"), workerEnrollmentCommand{})
}

func checkOpenAPIFields(t *testing.T, name string, schemas, schema *specNode, value any) {
	t.Helper()
	if schema == nil {
		t.Errorf("%s schema is missing", name)
		return
	}
	var properties, documented []string
	seen := make(map[*specNode]bool)
	var collect func(*specNode)
	collect = func(node *specNode) {
		if node == nil || seen[node] {
			return
		}
		seen[node] = true
		if ref, ok := strings.CutPrefix(node.get("$ref").scalar(), "#/components/schemas/"); ok {
			collect(schemas.get(ref))
		}
		for _, item := range node.get("allOf").items() {
			collect(item)
		}
		if props := node.get("properties"); props != nil {
			properties = append(properties, props.keys...)
		}
		documented = append(documented, flowList(node.get("required").scalar())...)
	}
	collect(schema)
	fields, required := jsonFields(reflect.TypeOf(value))
	slices.Sort(properties)
	if !slices.Equal(slices.Compact(properties), fields) {
		t.Errorf("%s properties %v, but %T encodes %v", name, properties, value, fields)
	}
	slices.Sort(documented)
	if !slices.Equal(slices.Compact(documented), required) {
		t.Errorf("%s required %v, but %T always encodes %v", name, documented, value, required)
	}
}

// Other features register routes through helpers that receive the panel mux;
// the discovery must follow them and ignore muxes of other servers.
func TestPanelRouteDiscoveryFollowsHelpers(t *testing.T) {
	routes := panelRoutesFromSource(t, map[string]any{
		"a.go": `package main
const eventsPath = "/api/events"
func panelHandler(panel *panelState, access *panelAccess) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", files)
	access.register(mux, panel)
	registerEvents(mux, access)
	other := http.NewServeMux()
	other.HandleFunc("GET /metrics", nil)
	return mux
}`,
		"b.go": `package main
func (a *panelAccess) register(m *http.ServeMux, panel *panelState) {
	m.HandleFunc("POST /api/runners/{name...}", a.require("hosts:write", nil))
}
func registerEvents(target *http.ServeMux, a *panelAccess) {
	target.HandleFunc("GET "+eventsPath, a.require("fleet:read", nil))
}
func registerUnrelated(mux *http.ServeMux) {
	mux.HandleFunc("GET /webhook", nil)
}`,
	})
	want := map[string]string{"GET /": "public", "POST /api/runners/{name}": "hosts:write", "GET /api/events": "fleet:read"}
	if len(routes) != len(want) {
		t.Fatalf("discovered %v, want %v", sortedKeys(routes), want)
	}
	for key, permission := range want {
		if routes[key].permission != permission {
			t.Errorf("%s permission = %q, want %q", key, routes[key].permission, permission)
		}
	}
}

func TestOpenAPIParserRejectsInvalidYAML(t *testing.T) {
	for _, source := range []string{
		"a:\n\tb: c\n",
		"a: b\na: c\n",
		"a:\n  b: c\n   d: e\n",
		"a: b: c\n",
		"a: #x\n",
	} {
		if _, err := parseSpecYAML(source); err == nil {
			t.Errorf("parseSpecYAML(%q) succeeded", source)
		}
	}
	node, err := parseSpecYAML("a:\n  - x: '1'\n    y: [p, 'q']\n  - \"z\\n\"\nb: 'it''s'\n")
	if err != nil {
		t.Fatal(err)
	}
	items := node.get("a").items()
	if len(items) != 2 || items[0].get("x").scalar() != "1" || !slices.Equal(flowList(items[0].get("y").scalar()), []string{"p", "q"}) ||
		items[1].scalar() != "z\n" || node.get("b").scalar() != "it's" {
		t.Fatalf("unexpected parse: %+v", node)
	}
}

func jsonFields(typ reflect.Type) (fields, required []string) {
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if field.Anonymous && tag == "" {
			embedded, embeddedRequired := jsonFields(field.Type)
			fields, required = append(fields, embedded...), append(required, embeddedRequired...)
			continue
		}
		if !field.IsExported() || tag == "-" {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		fields = append(fields, name)
		flags := strings.Split(options, ",")
		if !slices.Contains(flags, "omitempty") && !slices.Contains(flags, "omitzero") {
			required = append(required, name)
		}
	}
	slices.Sort(fields)
	slices.Sort(required)
	return fields, required
}

func loadOpenAPI(t *testing.T) *specNode {
	t.Helper()
	source, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := parseSpecYAML(string(source))
	if err != nil {
		t.Fatalf("%s: %v", openAPIPath, err)
	}
	return spec
}

func openAPIOperations(t *testing.T, spec *specNode) map[string]specOperation {
	t.Helper()
	operations := make(map[string]specOperation)
	paths := spec.get("paths")
	if paths == nil {
		t.Fatalf("%s has no paths", openAPIPath)
	}
	for _, path := range paths.keys {
		item := paths.m[path]
		for _, method := range item.keys {
			if !slices.Contains(openAPIMethods, method) {
				if method != "parameters" && method != "summary" && method != "description" {
					t.Errorf("%s:%d has an unknown key %s under %s", openAPIPath, item.m[method].line, method, path)
				}
				continue
			}
			op := specOperation{method: strings.ToUpper(method), path: path, node: item.m[method]}
			operations[op.key()] = op
		}
	}
	return operations
}

// discoverPanelRoutes reads every mux.Handle and mux.HandleFunc call reachable
// from panelHandler, following functions that receive the *http.ServeMux, so
// routes other files register through a registerX(mux, ...) helper are found.
// ServeMux cannot list its patterns, and other servers in this package use
// their own muxes, so a plain text search would be wrong.
func discoverPanelRoutes(t *testing.T) map[string]panelRoute {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]any)
	for _, name := range names {
		if !strings.HasSuffix(name, "_test.go") {
			sources[name] = nil
		}
	}
	return panelRoutesFromSource(t, sources)
}

// panelRoutesFromSource parses the named files; a nil source reads the file.
func panelRoutesFromSource(t *testing.T, sources map[string]any) map[string]panelRoute {
	t.Helper()
	fset := token.NewFileSet()
	constants := make(map[string]ast.Expr)
	functions := make(map[string][]*ast.FuncDecl)
	for _, name := range sortedKeys(sources) {
		file, err := parser.ParseFile(fset, name, sources[name], 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				functions[decl.Name.Name] = append(functions[decl.Name.Name], decl)
			case *ast.GenDecl:
				if decl.Tok != token.CONST {
					continue
				}
				for _, spec := range decl.Specs {
					value := spec.(*ast.ValueSpec)
					for i, ident := range value.Names {
						if i < len(value.Values) {
							constants[ident.Name] = value.Values[i]
						}
					}
				}
			}
		}
	}
	var evaluate func(ast.Expr) (string, bool)
	evaluate = func(expr ast.Expr) (string, bool) {
		switch expr := expr.(type) {
		case *ast.BasicLit:
			if expr.Kind == token.STRING {
				value, err := strconv.Unquote(expr.Value)
				return value, err == nil
			}
		case *ast.Ident:
			if value, ok := constants[expr.Name]; ok {
				return evaluate(value)
			}
		case *ast.ParenExpr:
			return evaluate(expr.X)
		case *ast.BinaryExpr:
			if expr.Op == token.ADD {
				left, ok := evaluate(expr.X)
				right, ok2 := evaluate(expr.Y)
				return left + right, ok && ok2
			}
		}
		return "", false
	}
	isServeMux := func(expr ast.Expr) bool {
		star, ok := expr.(*ast.StarExpr)
		if !ok {
			return false
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "ServeMux" && isIdent(sel.X, "http")
	}

	routes := make(map[string]panelRoute)
	visited := make(map[*ast.FuncDecl]bool)
	var visit func(*ast.FuncDecl, string)
	visit = func(fn *ast.FuncDecl, mux string) {
		if visited[fn] || fn.Body == nil {
			return
		}
		visited[fn] = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isIdent(sel.X, mux) &&
				(sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") && len(call.Args) == 2 {
				pos := fset.Position(call.Pos())
				where := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
				pattern, ok := evaluate(call.Args[0])
				method, path, hasMethod := strings.Cut(pattern, " ")
				if !ok || !hasMethod || !strings.HasPrefix(path, "/") {
					t.Errorf("%s registers a panel route whose pattern is not a constant \"METHOD /path\"; openapi_test.go cannot document it", where)
					return true
				}
				path = strings.ReplaceAll(strings.ReplaceAll(path, "{$}", ""), "...}", "}")
				if path == "" {
					path = "/"
				}
				route := panelRoute{method: method, path: path, permission: "public", pos: where}
				if guard, ok := call.Args[1].(*ast.CallExpr); ok && len(guard.Args) > 0 {
					if sel, ok := guard.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "require" {
						if permission, ok := evaluate(guard.Args[0]); ok {
							route.permission = permission
						}
					}
				}
				routes[route.key()] = route
				return true
			}
			for i, arg := range call.Args {
				if !isIdent(arg, mux) {
					continue
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					callee = fun.Name
				case *ast.SelectorExpr:
					callee = fun.Sel.Name
				}
				for _, candidate := range functions[callee] {
					index := 0
					for _, field := range candidate.Type.Params.List {
						for _, name := range field.Names {
							if index == i && isServeMux(field.Type) {
								visit(candidate, name.Name)
							}
							index++
						}
					}
				}
			}
			return true
		})
	}
	for _, fn := range functions["panelHandler"] {
		visit(fn, "mux")
	}
	return routes
}

func isIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// specNode is a block-style YAML node: a mapping, a sequence or a scalar.
// The spec is checked without a YAML dependency, so it keeps to block
// mappings and sequences with single-line scalars and flow lists.
type specNode struct {
	value string
	keys  []string
	m     map[string]*specNode
	seq   []*specNode
	line  int
}

func (n *specNode) get(key string) *specNode {
	if n == nil || n.m == nil {
		return nil
	}
	return n.m[key]
}

func (n *specNode) scalar() string {
	if n == nil {
		return ""
	}
	return n.value
}

func (n *specNode) items() []*specNode {
	if n == nil {
		return nil
	}
	return n.seq
}

type specLine struct {
	indent, number int
	text           string
}

type specParser struct {
	lines []specLine
	pos   int
}

func parseSpecYAML(source string) (*specNode, error) {
	var p specParser
	for i, raw := range strings.Split(source, "\n") {
		if strings.Contains(raw, "\t") {
			return nil, fmt.Errorf("line %d: use spaces, not tabs", i+1)
		}
		text := strings.TrimLeft(raw, " ")
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		p.lines = append(p.lines, specLine{indent: len(raw) - len(text), number: i + 1, text: strings.TrimRight(text, " ")})
	}
	node, err := p.block(0)
	if err == nil && p.pos < len(p.lines) {
		err = fmt.Errorf("line %d: unexpected indentation", p.lines[p.pos].number)
	}
	return node, err
}

func (p *specParser) block(indent int) (*specNode, error) {
	node := &specNode{}
	if p.pos < len(p.lines) {
		node.line = p.lines[p.pos].number
	}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent < indent {
			break
		}
		if line.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", line.number)
		}
		if item, ok := strings.CutPrefix(line.text, "- "); ok {
			if node.m != nil {
				return nil, fmt.Errorf("line %d: a sequence item inside a mapping", line.number)
			}
			if _, _, isKey := splitSpecKey(item); !isKey {
				value, err := specScalar(item, line.number)
				if err != nil {
					return nil, err
				}
				node.seq = append(node.seq, &specNode{value: value, line: line.number})
				p.pos++
				continue
			}
			p.lines[p.pos] = specLine{indent: indent + 2, number: line.number, text: item}
			child, err := p.block(indent + 2)
			if err != nil {
				return nil, err
			}
			node.seq = append(node.seq, child)
			continue
		}
		key, raw, isKey := splitSpecKey(line.text)
		if !isKey || node.seq != nil {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", line.number)
		}
		if node.m == nil {
			node.m = make(map[string]*specNode)
		}
		if _, dup := node.m[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", line.number, key)
		}
		p.pos++
		child := &specNode{line: line.number}
		if raw != "" {
			value, err := specScalar(raw, line.number)
			if err != nil {
				return nil, err
			}
			child.value = value
		} else if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
			nested, err := p.block(p.lines[p.pos].indent)
			if err != nil {
				return nil, err
			}
			nested.line = line.number
			child = nested
		}
		node.m[key] = child
		node.keys = append(node.keys, key)
	}
	return node, nil
}

// splitSpecKey splits "key: value" or "key:"; quoted keys are unquoted.
func splitSpecKey(text string) (key, value string, ok bool) {
	if text != "" && (text[0] == '\'' || text[0] == '"') {
		end := strings.IndexByte(text[1:], text[0])
		if end < 0 {
			return "", "", false
		}
		rest := text[end+2:]
		if rest != ":" && !strings.HasPrefix(rest, ": ") {
			return "", "", false
		}
		return text[1 : end+1], strings.TrimSpace(strings.TrimPrefix(rest, ":")), true
	}
	if key, ok := strings.CutSuffix(text, ":"); ok && !strings.Contains(key, ": ") {
		return key, "", true
	}
	key, value, ok = strings.Cut(text, ": ")
	return key, strings.TrimSpace(value), ok
}

// specScalar unquotes a scalar and rejects plain scalars that real YAML
// parsers would read differently from this one.
func specScalar(raw string, number int) (string, error) {
	switch raw[0] {
	case '\'':
		if len(raw) < 2 || raw[len(raw)-1] != '\'' {
			return "", fmt.Errorf("line %d: unterminated quoted value", number)
		}
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), nil
	case '"':
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", fmt.Errorf("line %d: invalid double-quoted value", number)
		}
		return value, nil
	case '[':
		if raw[len(raw)-1] != ']' {
			return "", fmt.Errorf("line %d: unterminated flow list", number)
		}
		return raw, nil
	}
	if strings.ContainsAny(raw[:1], "-?:,]{}#&*!|>%@`") || strings.Contains(raw, ": ") || strings.Contains(raw, " #") {
		return "", fmt.Errorf("line %d: quote the value %q", number, raw)
	}
	return raw, nil
}

func flowList(raw string) []string {
	inner, ok := strings.CutPrefix(raw, "[")
	inner, ok2 := strings.CutSuffix(inner, "]")
	if !ok || !ok2 || strings.TrimSpace(inner) == "" {
		return nil
	}
	var items []string
	for _, item := range strings.Split(inner, ",") {
		item = strings.TrimSpace(item)
		if value, err := specScalar(item, 0); err == nil {
			item = value
		}
		items = append(items, item)
	}
	return items
}
