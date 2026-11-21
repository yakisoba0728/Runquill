package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type logFixture struct {
	*panelFixture
	logs  *runnerLogs
	mu    sync.Mutex
	calls [][]string
	shell string
}

func newLogFixture(t *testing.T) *logFixture {
	t.Helper()
	f := &logFixture{panelFixture: newPanelFixture(t), shell: "echo hello"}
	f.logs = newRunnerLogs(f.q.DB)
	f.logs.command = func(ctx context.Context, binary string, args ...string) *exec.Cmd {
		f.mu.Lock()
		f.calls = append(f.calls, append([]string{binary}, args...))
		shell := f.shell
		f.mu.Unlock()
		return exec.CommandContext(ctx, "sh", "-c", shell)
	}
	mux := http.NewServeMux()
	f.logs.register(mux, f.access)
	f.handler = mux
	for i, host := range []string{"local", "ssh://deploy@build-1:2222", "agent://gpu", kubeHost} {
		if _, err := f.q.Exec(`INSERT INTO jobs (job_id, repository, repository_id, installation_id, route, phase, changed_at, runner_name, runner_host)
            VALUES (?, 'owner/repo', 8, 9, 'r', 1, 1, ?, ?)`, i+1, "runquill-"+strconv.Itoa(i+1)+"-abcdef012345", host); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *logFixture) lastCall() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func TestRunnerLogsRequireSignedInOperator(t *testing.T) {
	f := newLogFixture(t)
	path := "/api/runners/runquill-1-abcdef012345/logs"
	f.expect(f.call(http.MethodGet, path, "", "", false), http.StatusUnauthorized)
	f.expect(f.call(http.MethodGet, path, "", "viewer", false), http.StatusForbidden)
	for _, scope := range []string{"fleet:read", "hosts:write"} {
		key := panelSecret()
		digest := panelDigest(key)
		if _, err := f.q.Exec(`INSERT INTO panel_tokens(digest,user_id,name,scope,expires_at,created_at) VALUES (?,101,?,?,?,0)`,
			digest[:], scope, scope, time.Now().Add(time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
		f.expect(f.call(http.MethodGet, path, "", "rqk_"+key, false), http.StatusForbidden)
	}
	if f.lastCall() != nil {
		t.Fatal("docker ran for a denied request")
	}
	for _, who := range []string{"operator", "admin"} {
		rec := f.call(http.MethodGet, path, "", who, false)
		f.expect(rec, http.StatusOK)
		if rec.Body.String() != "hello\n" || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("%s logs: %q", who, rec.Body)
		}
	}
	var audits int
	if err := f.q.QueryRow(`SELECT count(*) FROM panel_audit WHERE action = 'runner.logs' AND target = 'runquill-1-abcdef012345'`).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("log audit entries: %d, %v", audits, err)
	}
}

func TestRunnerLogsOnlyForTrackedRunners(t *testing.T) {
	f := newLogFixture(t)
	for _, name := range []string{"runquill-9-000000000000", "-rm", "a%20b", strings.Repeat("a", 200)} {
		f.expect(f.call(http.MethodGet, "/api/runners/"+name+"/logs", "", "operator", false), http.StatusNotFound)
	}
	if f.lastCall() != nil {
		t.Fatal("docker ran for an untracked runner")
	}
	for name, message := range map[string]string{
		"runquill-3-abcdef012345": "agent:// worker",
		"runquill-4-abcdef012345": "Kubernetes",
	} {
		rec := f.call(http.MethodGet, "/api/runners/"+name+"/logs", "", "operator", false)
		f.expect(rec, http.StatusNotImplemented)
		if !strings.Contains(rec.Body.String(), `"error":"Runner logs are not available for `+message) {
			t.Fatalf("%s: %s", name, rec.Body)
		}
	}
	if f.lastCall() != nil {
		t.Fatal("docker ran for an unsupported backend")
	}
}

func TestRunnerLogsArgumentsAndBounds(t *testing.T) {
	f := newLogFixture(t)
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/api/runners/runquill-1-abcdef012345/logs", []string{"docker", "logs", "--timestamps", "--tail", "200", "runquill-1-abcdef012345"}},
		{"/api/runners/runquill-1-abcdef012345/logs?tail=99999", []string{"docker", "logs", "--timestamps", "--tail", "2000", "runquill-1-abcdef012345"}},
		{"/api/runners/runquill-2-abcdef012345/logs?tail=5&follow=1",
			[]string{"docker", "--host", "ssh://deploy@build-1:2222", "logs", "--timestamps", "--tail", "5", "--follow", "runquill-2-abcdef012345"}},
	} {
		f.expect(f.call(http.MethodGet, tc.path, "", "operator", false), http.StatusOK)
		if got := f.lastCall(); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: docker %q, want %q", tc.path, got, tc.want)
		}
	}
	for _, query := range []string{"tail=-1", "tail=x", "follow=yes"} {
		f.expect(f.call(http.MethodGet, "/api/runners/runquill-1-abcdef012345/logs?"+query, "", "operator", false), http.StatusBadRequest)
	}
}

func TestRunnerLogsUseTheConfiguredEngine(t *testing.T) {
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "podman")
	f := newLogFixture(t)
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/api/runners/runquill-1-abcdef012345/logs", []string{"podman", "logs", "--timestamps", "--tail", "200", "runquill-1-abcdef012345"}},
		{"/api/runners/runquill-2-abcdef012345/logs?tail=5&follow=1", []string{"podman", "--url", "ssh://deploy@build-1:2222/run/podman/podman.sock",
			"logs", "--timestamps", "--tail", "5", "--follow", "runquill-2-abcdef012345"}},
	} {
		f.expect(f.call(http.MethodGet, tc.path, "", "operator", false), http.StatusOK)
		if got := f.lastCall(); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: ran %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestRunnerLogsTailOutlastsServerWriteTimeout(t *testing.T) {
	f := newLogFixture(t)
	f.shell = "sleep 0.5; echo hello"
	server := httptest.NewUnstartedServer(f.handler)
	server.Config.WriteTimeout = 200 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/runners/runquill-1-abcdef012345/logs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(f.cookies["operator"])
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("tail slower than the write timeout: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "hello\n" {
		t.Fatalf("tail: HTTP %d %q, %v", resp.StatusCode, body, err)
	}
}

func TestRunnerLogsFailuresLimitsAndRedaction(t *testing.T) {
	f := newLogFixture(t)
	path := "/api/runners/runquill-1-abcdef012345/logs"

	f.shell = "echo 'Error: No such container' >&2; exit 1"
	f.expect(f.call(http.MethodGet, path, "", "operator", false), http.StatusBadGateway)

	secret := strings.Repeat("QUJD", 100) + "=="
	f.shell = "echo before; echo 'jitconfig " + secret + "' >&2; echo after"
	rec := f.call(http.MethodGet, path, "", "operator", false)
	f.expect(rec, http.StatusOK)
	if body := rec.Body.String(); strings.Contains(body, "QUJDQUJD") || body != "before\njitconfig [redacted]\nafter\n" {
		t.Fatalf("redacted output: %q", body)
	}

	f.shell = "yes 'x y' | head -c 9000000"
	rec = f.call(http.MethodGet, path, "", "operator", false)
	f.expect(rec, http.StatusOK)
	if rec.Body.Len() > runnerLogMaxBytes+len(runnerLogLimitMessage) || !strings.HasSuffix(rec.Body.String(), runnerLogLimitMessage) {
		t.Fatalf("capped output: %d bytes", rec.Body.Len())
	}

	for range runnerLogMaxStreams {
		f.logs.streams <- struct{}{}
	}
	f.expect(f.call(http.MethodGet, path, "", "operator", false), http.StatusTooManyRequests)
}

func TestRunnerLogsFollowCommandExit(t *testing.T) {
	path := "/api/runners/runquill-1-abcdef012345/logs?follow=1"
	for _, tc := range []struct {
		name, shell, body string
		status            int
	}{
		{"empty failure", "exit 1", "Could not read the runner logs.\n", http.StatusBadGateway},
		{"empty success", "exit 0", "", http.StatusOK},
		{"failure after output", "echo hello; exit 1", "hello\n", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLogFixture(t)
			f.shell = tc.shell
			rec := f.call(http.MethodGet, path, "", "operator", false)
			f.expect(rec, tc.status)
			if body := rec.Body.String(); body != tc.body {
				t.Fatalf("body = %q, want %q", body, tc.body)
			}
		})
	}
}
