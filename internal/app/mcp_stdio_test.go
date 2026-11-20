package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// mcpStdioServer serves registerMCPRoutes over real HTTP and records the
// headers the proxy sent.
func mcpStdioServer(t *testing.T) (*mcpFixture, *httptest.Server, func() []http.Header) {
	t.Helper()
	f := newMCPFixture(t)
	var mu sync.Mutex
	var seen []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return f, server, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func runMCPStdio(t *testing.T, input string, args ...string) (int, map[string]mcpReply, []string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := mcpCommand(args, strings.NewReader(input), &stdout, &stderr)
	byID := map[string]mcpReply{}
	var anonymous []string
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var reply mcpReply
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			t.Fatalf("stdout line is not JSON-RPC: %q", line)
		}
		if reply.ID == nil {
			anonymous = append(anonymous, line)
			continue
		}
		byID[string(reply.ID)] = reply
	}
	return code, byID, anonymous, stderr.String()
}

func mcpLine(id any, method string, params map[string]any) string {
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		message["id"] = id
	}
	if params != nil {
		message["params"] = params
	}
	line, _ := json.Marshal(message)
	return string(line) + "\n"
}

func TestMCPStdioProxyRoundTrip(t *testing.T) {
	f, server, headers := mcpStdioServer(t)
	t.Setenv("RUNQUILL_URL", server.URL)
	t.Setenv("RUNQUILL_TOKEN", f.read)
	modern := func(extra map[string]any) map[string]any {
		params := map[string]any{"_meta": mcpMeta(mcpVersion)}
		for k, v := range extra {
			params[k] = v
		}
		return params
	}
	input := mcpLine("d", "server/discover", modern(nil)) +
		mcpLine(2, "tools/list", modern(nil)) +
		"\n" +
		mcpLine(3, "tools/call", modern(map[string]any{"name": "fleet_status", "arguments": map[string]any{}})) +
		mcpLine(4, "tools/call", modern(map[string]any{"name": "get_job", "arguments": map[string]any{"id": 1}})) +
		mcpLine(5, "tools/call", modern(map[string]any{"name": "nope\nname"})) +
		mcpLine(6, "no/such/method", modern(nil)) +
		"{not json\n" +
		`{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{"pad":"` + strings.Repeat("a", mcpStdioMaxLine) + `"}}` + "\n" +
		mcpLine(nil, "notifications/initialized", nil) +
		// A legacy client: initialize, then requests without _meta.
		mcpLine(8, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "old", "version": "1"}}) +
		mcpLine(9, "tools/list", nil) +
		mcpLine(10, "tools/call", map[string]any{"name": "list_jobs", "arguments": map[string]any{"limit": 500}})
	code, replies, anonymous, stderr := runMCPStdio(t, strings.TrimSuffix(input, "\n"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Contains(stderr, f.read) {
		t.Fatal("stderr leaks the token")
	}
	if r := replies[`"d"`]; r.Result["resultType"] != "complete" || r.Result["supportedVersions"] == nil {
		t.Fatalf("discover: %+v", r)
	}
	if r := replies["2"]; len(r.Result["tools"].([]any)) != len(mcpTools) {
		t.Fatalf("tools/list: %+v", r)
	}
	if r := replies["3"]; r.Result["isError"] != false || r.Result["structuredContent"] == nil {
		t.Fatalf("fleet_status: %+v", r)
	}
	if r := replies["4"]; r.Result["isError"] != true {
		t.Fatalf("get_job of a missing job: %+v", r)
	}
	if r := replies["5"]; r.Error == nil || r.Error.Code != rpcInvalidParams {
		t.Fatalf("unknown tool with an unsafe name: %+v", r)
	}
	if r := replies["6"]; r.Error == nil || r.Error.Code != rpcMethodNotFound {
		t.Fatalf("unknown method: %+v", r)
	}
	if r := replies["8"]; r.Result["protocolVersion"] != mcpLegacyVersion {
		t.Fatalf("initialize: %+v", r)
	}
	if r := replies["9"]; r.Error != nil || len(r.Result["tools"].([]any)) != len(mcpTools) {
		t.Fatalf("legacy tools/list: %+v", r)
	}
	if r := replies["10"]; r.Result["isError"] != true {
		t.Fatalf("legacy bounded call: %+v", r)
	}
	if len(replies) != 9 || len(anonymous) != 2 || !strings.Contains(anonymous[0]+anonymous[1], `"code":-32700`) ||
		!strings.Contains(anonymous[0]+anonymous[1], "larger than 64 KiB") {
		t.Fatalf("replies %d, anonymous %q", len(replies), anonymous)
	}
	var unsafeName, legacyHeader bool
	for _, h := range headers() {
		if h.Get("Authorization") != "Bearer "+f.read || h.Get("Mcp-Method") == "" || h.Get("Origin") != "" {
			t.Fatalf("proxy headers: %v", h)
		}
		unsafeName = unsafeName || h.Get("Mcp-Name") == mcpEncodeHeader("nope\nname") && strings.HasPrefix(h.Get("Mcp-Name"), "=?base64?")
		legacyHeader = legacyHeader || h.Get("Mcp-Method") == "tools/list" && h.Get("MCP-Protocol-Version") == mcpLegacyVersion
	}
	if !unsafeName || !legacyHeader {
		t.Fatalf("missing encoded Mcp-Name (%v) or legacy version header (%v)", unsafeName, legacyHeader)
	}
}

func TestMCPStdioFailuresAndFlags(t *testing.T) {
	f, server, _ := mcpStdioServer(t)
	t.Setenv("RUNQUILL_URL", server.URL)
	t.Setenv("RUNQUILL_TOKEN", "")

	if code, _, _, stderr := runMCPStdio(t, "", "--token", f.read); code != 2 || !strings.Contains(stderr, "RUNQUILL_TOKEN or --token-file") {
		t.Fatalf("--token: %d %s", code, stderr)
	}
	if code, _, _, stderr := runMCPStdio(t, ""); code != 2 || !strings.Contains(stderr, "RUNQUILL_TOKEN") {
		t.Fatalf("no token: %d %s", code, stderr)
	}
	if code, _, _, _ := runMCPStdio(t, "", "extra"); code != 2 {
		t.Fatalf("positional argument accepted: %d", code)
	}
	if code, _, _, _ := runMCPStdio(t, "", "--help"); code != 0 {
		t.Fatalf("--help: %d", code)
	}

	// A token the panel rejects becomes a JSON-RPC error on the request, not a crash.
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("rqk_"+strings.Repeat("B", 43)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discover := mcpLine(1, "server/discover", map[string]any{"_meta": mcpMeta(mcpVersion)})
	code, replies, _, stderr := runMCPStdio(t, discover, "--token-file", tokenFile)
	if r := replies["1"]; code != 0 || r.Error == nil || r.Error.Code != rpcInternalError || !strings.Contains(r.Error.Message, "HTTP 401") {
		t.Fatalf("rejected token: %d %+v %s", code, r, stderr)
	}

	// An unreachable panel also answers the request, and EOF still exits cleanly.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	t.Setenv("RUNQUILL_TOKEN", f.read)
	code, replies, _, stderr = runMCPStdio(t, discover, "--url", closed.URL)
	if r := replies["1"]; code != 0 || r.Error == nil || r.Error.Code != rpcInternalError || stderr == "" || strings.Contains(stderr, f.read) {
		t.Fatalf("unreachable panel: %d %+v %q", code, r, stderr)
	}
}

// notifications/cancelled stops a request and suppresses its reply.
func TestMCPStdioCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	t.Setenv("RUNQUILL_URL", server.URL)
	t.Setenv("RUNQUILL_TOKEN", "rqk_"+strings.Repeat("C", 43))
	reader, writer := io.Pipe()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- mcpCommand(nil, reader, &stdout, &stderr) }()
	io.WriteString(writer, mcpLine(1, "tools/list", map[string]any{"_meta": mcpMeta(mcpVersion)}))
	time.Sleep(50 * time.Millisecond)
	io.WriteString(writer, mcpLine(nil, "notifications/cancelled", map[string]any{"requestId": 1}))
	time.Sleep(50 * time.Millisecond)
	writer.Close()
	select {
	case code := <-done:
		if code != 0 || stdout.Len() != 0 {
			t.Fatalf("exit %d, stdout %q", code, stdout.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not exit after EOF")
	}
}

// An id-less panel error, such as the busy answer, still answers the request.
func TestMCPStdioAnswersIDlessErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpWriteError(w, http.StatusTooManyRequests, nil, rpcInternalError, "Too many MCP requests are running. Try again shortly.", nil)
	}))
	defer server.Close()
	t.Setenv("RUNQUILL_URL", server.URL)
	t.Setenv("RUNQUILL_TOKEN", "rqk_"+strings.Repeat("D", 43))
	code, replies, anonymous, _ := runMCPStdio(t, mcpLine("q", "tools/list", map[string]any{"_meta": mcpMeta(mcpVersion)}))
	r := replies[`"q"`]
	if code != 0 || len(anonymous) != 0 || r.Error == nil || r.Error.Code != rpcInternalError || !strings.Contains(r.Error.Message, "HTTP 429: Too many MCP requests") {
		t.Fatalf("busy panel: %d %+v %q", code, r, anonymous)
	}
}
