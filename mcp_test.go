package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

type mcpFixture struct {
	*panelFixture
	panel       *panelState
	read, write string
}

func mcpToken(t *testing.T, q *queue, user int64, scope string) string {
	t.Helper()
	key := panelSecret()
	digest := panelDigest(key)
	if _, err := q.Exec(`INSERT INTO panel_tokens(digest,user_id,name,scope,expires_at,created_at) VALUES (?,?,?,?,?,0)`,
		digest[:], user, scope, scope, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	return "rqk_" + key
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	f := &mcpFixture{panelFixture: newPanelFixture(t), panel: newPanel(config{})}
	mux := http.NewServeMux()
	registerMCPRoutes(mux, f.access, f.panel)
	f.handler = mux
	f.read, f.write = mcpToken(t, f.q, 102, "fleet:read"), mcpToken(t, f.q, 103, "hosts:write")
	return f
}

// post sends body with the read token unless headers override Authorization.
func (f *mcpFixture) post(body string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, f.access.cfg.origin+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+f.read)
	for name, value := range headers {
		if value == "" {
			req.Header.Del(name)
		} else {
			req.Header.Set(name, value)
		}
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func mcpMeta(version string) map[string]any {
	return map[string]any{mcpMetaVersion: version, mcpMetaCapabilities: map[string]any{},
		"io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}
}

// modern sends a 2026-07-28 request with matching headers.
func (f *mcpFixture) modern(id any, method string, params map[string]any) *httptest.ResponseRecorder {
	f.t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	if _, ok := params["_meta"]; !ok {
		params["_meta"] = mcpMeta(mcpVersion)
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	headers := map[string]string{"MCP-Protocol-Version": mcpVersion, "Mcp-Method": method}
	if name, ok := params["name"].(string); ok {
		headers["Mcp-Name"] = name
	}
	return f.post(string(body), headers)
}

type mcpReply struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *rpcError       `json:"error"`
}

func decodeMCP(t *testing.T, rec *httptest.ResponseRecorder, status int) mcpReply {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("got HTTP %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type %q: %s", got, rec.Body)
	}
	var reply mcpReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return reply
}

func expectRPCError(t *testing.T, rec *httptest.ResponseRecorder, status, code int) mcpReply {
	t.Helper()
	reply := decodeMCP(t, rec, status)
	if reply.Error == nil || reply.Error.Code != code {
		t.Fatalf("want error %d, got %s", code, rec.Body)
	}
	return reply
}

type mcpCallResult struct {
	Content []struct {
		Type, Text string
	} `json:"content"`
	Structured json.RawMessage `json:"structuredContent"`
	IsError    bool            `json:"isError"`
	ResultType string          `json:"resultType"`
}

func (f *mcpFixture) tool(name string, args any) mcpCallResult {
	f.t.Helper()
	rec := f.modern(7, "tools/call", map[string]any{"name": name, "arguments": args})
	if rec.Code != http.StatusOK {
		f.t.Fatalf("%s: HTTP %d %s", name, rec.Code, rec.Body)
	}
	var reply struct {
		Result mcpCallResult `json:"result"`
		Error  *rpcError     `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Error != nil {
		f.t.Fatalf("%s: %s %v", name, rec.Body, err)
	}
	if reply.Result.ResultType != "complete" || len(reply.Result.Content) != 1 || reply.Result.Content[0].Type != "text" {
		f.t.Fatalf("%s: malformed result %s", name, rec.Body)
	}
	return reply.Result
}

func (f *mcpFixture) toolOK(name string, args any, target any) {
	f.t.Helper()
	result := f.tool(name, args)
	if result.IsError {
		f.t.Fatalf("%s(%v) failed: %s", name, args, result.Content[0].Text)
	}
	// The text block carries the same JSON for clients without structured content.
	if !json.Valid([]byte(result.Content[0].Text)) || string(result.Structured) != result.Content[0].Text {
		f.t.Fatalf("%s: text %q does not mirror %s", name, result.Content[0].Text, result.Structured)
	}
	if err := json.Unmarshal(result.Structured, target); err != nil {
		f.t.Fatal(err)
	}
}

func (f *mcpFixture) toolFails(name string, args any, want string) {
	f.t.Helper()
	result := f.tool(name, args)
	if !result.IsError || !strings.Contains(result.Content[0].Text, want) || result.Structured != nil {
		f.t.Fatalf("%s(%v): want tool error containing %q, got %+v", name, args, want, result)
	}
}

func TestMCPDiscoveryAndVersionNegotiation(t *testing.T) {
	f := newMCPFixture(t)
	reply := decodeMCP(t, f.modern("d1", "server/discover", nil), 200)
	if string(reply.ID) != `"d1"` || reply.Result["resultType"] != "complete" || reply.Result["cacheScope"] != "private" ||
		reply.Result["ttlMs"] == nil || fmt.Sprint(reply.Result["supportedVersions"]) != "[2026-07-28 2025-11-25]" ||
		fmt.Sprint(reply.Result["capabilities"]) != "map[tools:map[]]" {
		t.Fatalf("discover: %+v", reply)
	}
	if info := reply.Result["_meta"].(map[string]any)[mcpMetaServerInfo].(map[string]any); info["name"] != "runquill" {
		t.Fatalf("server info: %v", info)
	}

	// Legacy clients open with initialize; no session id is issued.
	for requested, want := range map[string]string{"2025-11-25": "2025-11-25", "2024-11-05": "2025-11-25"} {
		rec := f.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+requested+
			`","capabilities":{},"clientInfo":{"name":"old","version":"1"}}}`, nil)
		reply := decodeMCP(t, rec, 200)
		if reply.Result["protocolVersion"] != want || reply.Result["serverInfo"] == nil || rec.Header().Get("Mcp-Session-Id") != "" {
			t.Fatalf("initialize %s: %s", requested, rec.Body)
		}
	}
	if rec := f.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); rec.Code != 202 || rec.Body.Len() != 0 {
		t.Fatalf("notification: %d %s", rec.Code, rec.Body)
	}
	legacy := decodeMCP(t, f.post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{"MCP-Protocol-Version": mcpLegacyVersion}), 200)
	if len(legacy.Result["tools"].([]any)) != len(mcpTools) {
		t.Fatalf("legacy tools/list: %+v", legacy)
	}
	if ping := decodeMCP(t, f.post(`{"jsonrpc":"2.0","id":3,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": mcpLegacyVersion}), 200); ping.Result == nil {
		t.Fatalf("legacy ping: %+v", ping)
	}
	// A request without the header would mean 2025-03-26, which is not served.
	missing := expectRPCError(t, f.post(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, nil), 400, mcpUnsupportedVersion)
	if data := missing.Error.Data.(map[string]any); data["requested"] != "2025-03-26" {
		t.Fatalf("missing header: %+v", missing.Error)
	}
	expectRPCError(t, f.post(`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`, map[string]string{"MCP-Protocol-Version": mcpVersion}), 400, rpcInvalidParams)

	unsupported := map[string]any{"_meta": mcpMeta("1900-01-01")}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 6, "method": "tools/list", "params": unsupported})
	reply = expectRPCError(t, f.post(string(body), map[string]string{"MCP-Protocol-Version": "1900-01-01", "Mcp-Method": "tools/list"}), 400, mcpUnsupportedVersion)
	if data := reply.Error.Data.(map[string]any); data["requested"] != "1900-01-01" || fmt.Sprint(data["supported"]) != "[2026-07-28 2025-11-25]" || string(reply.ID) != "6" {
		t.Fatalf("unsupported version: %+v", reply)
	}
	noCapabilities := map[string]any{"_meta": map[string]any{mcpMetaVersion: mcpVersion}}
	expectRPCError(t, f.modern(8, "tools/list", noCapabilities), 400, rpcInvalidParams)

	// Headers must mirror the body.
	body, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/list", "params": map[string]any{"_meta": mcpMeta(mcpVersion)}})
	for _, headers := range []map[string]string{
		{"MCP-Protocol-Version": "2025-11-25", "Mcp-Method": "tools/list"},
		{"MCP-Protocol-Version": mcpVersion},
		{"MCP-Protocol-Version": mcpVersion, "Mcp-Method": "server/discover"},
	} {
		expectRPCError(t, f.post(string(body), headers), 400, mcpHeaderMismatch)
	}
	call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 10, "method": "tools/call",
		"params": map[string]any{"name": "fleet_status", "_meta": mcpMeta(mcpVersion)}})
	base := map[string]string{"MCP-Protocol-Version": mcpVersion, "Mcp-Method": "tools/call"}
	expectRPCError(t, f.post(string(call), base), 400, mcpHeaderMismatch)
	base["Mcp-Name"] = "list_hosts"
	expectRPCError(t, f.post(string(call), base), 400, mcpHeaderMismatch)
	base["Mcp-Name"] = "=?base64?" + base64.StdEncoding.EncodeToString([]byte("fleet_status")) + "?="
	if reply := decodeMCP(t, f.post(string(call), base), 200); reply.Error != nil {
		t.Fatalf("base64 Mcp-Name: %+v", reply.Error)
	}
}

func TestMCPToolsListSchemas(t *testing.T) {
	f := newMCPFixture(t)
	reply := decodeMCP(t, f.modern(1, "tools/list", nil), 200)
	if reply.Result["resultType"] != "complete" || reply.Result["cacheScope"] != "private" || reply.Result["ttlMs"] == nil {
		t.Fatalf("tools/list: %+v", reply.Result)
	}
	var tools []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Annotations map[string]any `json:"annotations"`
		InputSchema struct {
			Type                 string                    `json:"type"`
			AdditionalProperties *bool                     `json:"additionalProperties"`
			Properties           map[string]map[string]any `json:"properties"`
			Required             []string                  `json:"required"`
		} `json:"inputSchema"`
	}
	raw, _ := json.Marshal(reply.Result["tools"])
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
		schema := tool.InputSchema
		if !regexp.MustCompile(`^[a-z_]{1,64}$`).MatchString(tool.Name) || tool.Description == "" || schema.Type != "object" ||
			schema.AdditionalProperties == nil || *schema.AdditionalProperties ||
			tool.Annotations["readOnlyHint"] != true || tool.Annotations["destructiveHint"] != false {
			t.Errorf("tool %s is not a closed read-only object schema: %s", tool.Name, raw)
		}
		for _, name := range schema.Required {
			if schema.Properties[name] == nil {
				t.Errorf("%s requires undeclared %s", tool.Name, name)
			}
		}
		for name, property := range schema.Properties {
			switch property["type"] {
			case "string":
				if property["enum"] == nil && (property["pattern"] == nil || property["maxLength"] == nil) {
					t.Errorf("%s.%s is an unbounded string", tool.Name, name)
				}
				if pattern, ok := property["pattern"].(string); ok {
					regexp.MustCompile(pattern)
				}
			case "integer":
				if property["minimum"] == nil || property["maximum"] == nil {
					t.Errorf("%s.%s is an unbounded integer", tool.Name, name)
				}
			default:
				t.Errorf("%s.%s has type %v", tool.Name, name, property["type"])
			}
		}
		if mcpHandlers[tool.Name] == nil {
			t.Errorf("%s has no handler", tool.Name)
		}
	}
	want := []string{"fleet_status", "list_jobs", "get_job", "list_hosts", "usage_summary", "elastic_decisions"}
	if !slices.Equal(names, want) || len(mcpHandlers) != len(want) {
		t.Fatalf("tools %v, want %v in that order", names, want)
	}
	// Runner logs need a signed-in operator; a fleet:read token cannot reach them.
	unknown := expectRPCError(t, f.modern(2, "tools/call", map[string]any{"name": "runner_log_tail", "arguments": map[string]any{"name": "x"}}), 200, rpcInvalidParams)
	if !strings.Contains(unknown.Error.Message, "runner_log_tail") {
		t.Fatalf("unknown tool: %+v", unknown.Error)
	}
	expectRPCError(t, f.modern(3, "resources/list", nil), 404, rpcMethodNotFound)
	expectRPCError(t, f.modern(4, "ping", nil), 404, rpcMethodNotFound)
	expectRPCError(t, f.post(`{"jsonrpc":"2.0","id":5,"method":"prompts/list"}`, map[string]string{"MCP-Protocol-Version": mcpLegacyVersion}), 200, rpcMethodNotFound)
}

func TestMCPToolsReadPanelData(t *testing.T) {
	f := newMCPFixture(t)
	ctx := t.Context()
	now := time.Now()
	add := func(id int64, repository, update string, args ...any) {
		t.Helper()
		j := job{id: id, repository: repository, route: fmt.Sprintf("runquill-%d-1-build", 900+id), discoverySource: "webhook"}
		if _, err := f.q.record(ctx, j, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if update != "" {
			if _, err := f.q.ExecContext(ctx, "UPDATE jobs SET "+update+" WHERE job_id = ?", append(args, id)...); err != nil {
				t.Fatal(err)
			}
		}
	}

	var status mcpFleetStatus
	f.toolOK("fleet_status", nil, &status)
	if status.SampledAt != nil || len(status.Problems) != 1 || status.Problems[0].Kind != "snapshot" {
		t.Fatalf("status before a snapshot: %+v", status)
	}
	f.toolFails("list_hosts", map[string]any{}, "not sampled")

	add(1, "owner/app", "phase = 2, runner_cleaned_at = ?, runner_cleaned_host = 'local', provisioned_at = ?", now.Unix(), now.Add(-50*time.Minute).Unix())
	add(2, "owner/app", "phase = 2, failure = 'not started within 24 hours'")
	add(3, "owner/lib", "retry_at = ?, retry_reason = 'provision runner: boom'", now.Add(time.Hour).Unix())
	add(4, "owner/app", `runner_name = 'runquill-4-x', runner_host = 'local',
        jit_requested_ms = ?, jit_registered_ms = ?, runner_launch_ms = ?`,
		now.Add(-5*time.Second).UnixMilli(), now.Add(-4900*time.Millisecond).UnixMilli(), now.Add(-4*time.Second).UnixMilli())
	add(5, "owner/app", "")
	hosts := []hostCapacity{
		{dockerHostSnapshot: dockerHostSnapshot{Name: "local", Containers: map[string]string{"runquill-4-x": "running"}}, Limit: 2, Occupied: 1},
		{dockerHostSnapshot: dockerHostSnapshot{Name: "ssh://w2", Err: errors.New("dial ssh://w2: refused")}, Limit: 3, Cordoned: true},
	}
	f.panel.capture(ctx, f.q, hosts, 0, 5, nil, nil)

	f.toolOK("fleet_status", map[string]any{}, &status)
	if status.SampledAt == nil || status.QueueDepth != 2 || status.Retrying != 1 || status.Running != 1 || status.SlotLimit != 5 ||
		status.Hosts.Total != 2 || status.Hosts.Up != 1 || status.Hosts.Offline != 1 || status.Hosts.Cordoned != 1 ||
		status.Hosts.SlotLimit != 5 || status.Hosts.OccupiedSlots != 1 {
		t.Fatalf("fleet status: %+v", status)
	}
	kinds := map[string]bool{}
	for _, p := range status.Problems {
		kinds[p.Kind+":"+p.Subject] = true
	}
	if !kinds["host:ssh://w2"] || !kinds["job:5 owner/app"] || !kinds["job:3 owner/lib"] {
		t.Fatalf("problems: %+v", status.Problems)
	}
	f.toolFails("fleet_status", map[string]any{"verbose": true}, `Unknown argument "verbose"`)

	var hostList struct {
		Hosts []mcpHost `json:"hosts"`
	}
	f.toolOK("list_hosts", nil, &hostList)
	if len(hostList.Hosts) != 2 || hostList.Hosts[0].Name != "local" || hostList.Hosts[0].RunnerCount != 1 ||
		hostList.Hosts[0].Runners[0].Name != "runquill-4-x" || hostList.Hosts[0].Runners[0].JobID != 4 ||
		hostList.Hosts[1].Status != "OFFLINE" || len(hostList.Hosts[1].Runners) != 0 {
		t.Fatalf("hosts: %+v", hostList)
	}

	var page panelHistoryPageResponse
	f.toolOK("list_jobs", map[string]any{"limit": 2}, &page)
	if len(page.Jobs) != 2 || page.Jobs[0].ID != 5 || page.Jobs[1].ID != 4 || page.Next == 0 {
		t.Fatalf("first page: %+v", page)
	}
	if row := page.Jobs[1]; row.Diagnostic == nil || row.Diagnostic.Code != "waiting_for_github" ||
		row.Timing == nil || row.Timing.JITMillis == nil || *row.Timing.JITMillis != 100 {
		t.Fatalf("active job stage and reason missing: %+v", row)
	}
	f.toolOK("list_jobs", map[string]any{"limit": 2, "before": page.Next}, &page)
	if len(page.Jobs) != 2 || page.Jobs[0].ID != 3 {
		t.Fatalf("second page: %+v", page)
	}
	f.toolOK("list_jobs", map[string]any{"outcome": "failed"}, &page)
	if len(page.Jobs) != 1 || page.Jobs[0].ID != 2 || page.Jobs[0].Failure == "" {
		t.Fatalf("failed: %+v", page)
	}
	f.toolOK("list_jobs", map[string]any{"repository": "OWNER/lib", "since": "2h"}, &page)
	if len(page.Jobs) != 1 || page.Jobs[0].ID != 3 {
		t.Fatalf("repository: %+v", page)
	}
	f.toolOK("list_jobs", map[string]any{"since": "30m"}, &page)
	if len(page.Jobs) != 0 {
		t.Fatalf("since: %+v", page)
	}
	for args, want := range map[string]string{
		`{"limit":101}`:           "limit must be an integer between 1 and 100",
		`{"limit":0}`:             "limit must be",
		`{"limit":2.5}`:           "limit must be",
		`{"limit":"2"}`:           "limit must be",
		`{"outcome":"bogus"}`:     "outcome must be",
		`{"repository":"x"}`:      "repository must be OWNER/NAME",
		`{"repository":7}`:        "repository must be a string",
		`{"since":"8d"}`:          "at most 7d",
		`{"since":"-1h"}`:         "since must be",
		`{"before":0}`:            "before must be",
		`{"cursor":"x"}`:          `Unknown argument "cursor"`,
		`[1]`:                     "must be a JSON object",
		`{"repository":"a/b\nc"}`: "repository must be OWNER/NAME",
	} {
		f.toolFails("list_jobs", json.RawMessage(args), want)
	}

	var got mcpJob
	f.toolOK("get_job", map[string]any{"id": 3}, &got)
	if got.ID != 3 || got.Outcome != "waiting" || got.RetryReason != "provision runner: boom" || got.Status != "RETRY" ||
		got.Diagnostic == nil || got.URL != "https://github.com/owner/lib/actions/runs/903/job/3" {
		t.Fatalf("get_job 3: %+v", got)
	}
	got = mcpJob{}
	f.toolOK("get_job", map[string]any{"id": 2}, &got)
	if got.Outcome != "failed" || got.Failure != "not started within 24 hours" || got.Status != "" {
		t.Fatalf("get_job 2: %+v", got)
	}
	f.toolFails("get_job", map[string]any{"id": 99}, "Job 99 is not in the job history")
	f.toolFails("get_job", map[string]any{}, "id is required")
	f.toolFails("get_job", map[string]any{"id": 0}, "id must be")
	f.toolFails("get_job", map[string]any{"id": "3"}, "id must be")

	var usage struct {
		Window string `json:"window"`
		Jobs   int64  `json:"jobs"`
		Failed int64  `json:"failed"`
	}
	f.toolOK("usage_summary", nil, &usage)
	if usage.Window != "24h" || usage.Jobs != 2 || usage.Failed != 1 {
		t.Fatalf("usage 24h: %+v", usage)
	}
	f.toolOK("usage_summary", map[string]any{"window": "7d"}, &usage)
	if usage.Window != "7d" || usage.Jobs != 2 {
		t.Fatalf("usage 7d: %+v", usage)
	}
	f.toolFails("usage_summary", map[string]any{"window": "30d"}, "window must be 24h or 7d")

	var elastic struct {
		Mode      elasticMode       `json:"mode"`
		Decisions []elasticDecision `json:"decisions"`
		Total     int               `json:"total"`
	}
	f.toolOK("elastic_decisions", nil, &elastic)
	if elastic.Mode != elasticOff || len(elastic.Decisions) != 0 {
		t.Fatalf("elastic off: %+v", elastic)
	}
	f.panel.elastic = newElasticState(elasticConfig{Mode: elasticObserve, Interval: time.Minute})
	for i := range 3 {
		f.panel.elastic.record(elasticDecision{At: now.Add(time.Duration(i) * time.Second),
			elasticChange: elasticChange{Host: "local", Runner: fmt.Sprintf("r%d", i), Resource: "cpu", From: 1, To: 2, Reason: "busy"}})
	}
	f.toolOK("elastic_decisions", map[string]any{"limit": 2}, &elastic)
	if elastic.Mode != elasticObserve || elastic.Total != 3 || len(elastic.Decisions) != 2 || elastic.Decisions[0].Runner != "r2" {
		t.Fatalf("elastic: %+v", elastic)
	}
	f.toolFails("elastic_decisions", map[string]any{"limit": 201}, "limit must be")

	// Legacy clients call the same tools; results carry no modern fields.
	rec := f.post(`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"get_job","arguments":{"id":3}}}`,
		map[string]string{"MCP-Protocol-Version": mcpLegacyVersion})
	legacy := decodeMCP(t, rec, 200)
	if legacy.Result["isError"] != false || legacy.Result["structuredContent"] == nil || legacy.Result["resultType"] != nil {
		t.Fatalf("legacy call: %s", rec.Body)
	}
}

func TestMCPAuthOriginAndTransportErrors(t *testing.T) {
	f := newMCPFixture(t)
	discover, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover", "params": map[string]any{"_meta": mcpMeta(mcpVersion)}})
	headers := func(extra map[string]string) map[string]string {
		out := map[string]string{"MCP-Protocol-Version": mcpVersion, "Mcp-Method": "server/discover"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	if rec := f.post(string(discover), headers(nil)); rec.Code != 200 {
		t.Fatalf("read token: %d %s", rec.Code, rec.Body)
	}
	if rec := f.post(string(discover), headers(map[string]string{"Origin": f.access.cfg.origin})); rec.Code != 200 {
		t.Fatalf("panel origin: %d %s", rec.Code, rec.Body)
	}
	expectRPCError(t, f.post(string(discover), headers(map[string]string{"Authorization": ""})), 401, rpcInvalidRequest)

	// A browser session, even same-origin, cannot use the endpoint.
	req := httptest.NewRequest(http.MethodPost, f.access.cfg.origin+"/mcp", strings.NewReader(string(discover)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", f.access.cfg.origin)
	for k, v := range headers(nil) {
		req.Header.Set(k, v)
	}
	req.AddCookie(f.cookies["admin"])
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie session: %d %s", rec.Code, rec.Body)
	}

	if rec := f.post(string(discover), headers(map[string]string{"Authorization": "Bearer " + f.write})); rec.Code != http.StatusForbidden {
		t.Fatalf("hosts:write token: %d", rec.Code)
	}
	if rec := f.post(string(discover), headers(map[string]string{"Authorization": "Bearer rqk_" + panelSecret()})); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", rec.Code)
	}
	for _, origin := range []string{"https://evil.example", "null", "http://localhost:8082"} {
		reply := expectRPCError(t, f.post(string(discover), headers(map[string]string{"Origin": origin})), 403, rpcInvalidRequest)
		if reply.ID != nil {
			t.Fatalf("origin error carries an id: %+v", reply)
		}
	}

	expectRPCError(t, f.post(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"`+strings.Repeat("a", mcpMaxBody)+`"}}`, nil), 413, rpcInvalidRequest)
	expectRPCError(t, f.post(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, nil), 400, rpcInvalidRequest)
	expectRPCError(t, f.post(`{"jsonrpc":"2.0","id":1,`, nil), 400, rpcParseError)
	expectRPCError(t, f.post(`{"jsonrpc":"1.0","id":1,"method":"tools/list"}`, nil), 400, rpcInvalidRequest)
	expectRPCError(t, f.post(`{"jsonrpc":"2.0","id":1,"result":{}}`, nil), 400, rpcInvalidRequest)
	expectRPCError(t, f.post(`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, nil), 400, rpcInvalidRequest)
	expectRPCError(t, f.post(`{"jsonrpc":"2.0","id":1.5,"method":"tools/list"}`, nil), 400, rpcInvalidRequest)
	expectRPCError(t, f.post(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[1]}`, nil), 400, rpcInvalidParams)
	expectRPCError(t, f.post(string(discover), headers(map[string]string{"Content-Type": "text/plain"})), 415, rpcInvalidRequest)

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, f.access.cfg.origin+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+f.read)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /mcp: %d", method, rec.Code)
		}
	}

	// Requests beyond the in-flight bound are refused rather than queued on the one DB connection.
	server := &mcpServer{slots: make(chan struct{}, 1), now: time.Now}
	server.slots <- struct{}{}
	rec = httptest.NewRecorder()
	server.serve(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(discover))), panelIdentity{Scope: "fleet:read"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("busy: %d", rec.Code)
	}
}

// Once wired into panelHandler, openapi_test.go's route discovery must see
// POST /mcp with the fleet:read permission.
func TestMCPRouteDiscoveredWithPermission(t *testing.T) {
	routes := panelRoutesFromSource(t, map[string]any{
		"mcp.go": nil,
		"wiring.go": `package main
func panelHandler(panel *panelState, access *panelAccess) http.Handler {
	mux := http.NewServeMux()
	registerMCPRoutes(mux, access, panel)
	return mux
}`,
	})
	if len(routes) != 1 || routes["POST /mcp"].permission != "fleet:read" {
		t.Fatalf("discovered %+v", routes)
	}
}
