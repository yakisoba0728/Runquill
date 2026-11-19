package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadyz(t *testing.T) {
	q := testQueue(t)
	ready := &readiness{}
	handler := webhook(q, &github{owner: "owner", ready: ready}, nil, nil)
	probe := func(path string) (int, readinessReport) {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		var report readinessReport
		if path == "/readyz" {
			if err := json.Unmarshal(res.Body.Bytes(), &report); err != nil {
				t.Fatalf("decode %s: %v", res.Body, err)
			}
		}
		return res.Code, report
	}
	if code, report := probe("/readyz"); code != http.StatusServiceUnavailable || report.Scheduler != "starting" || report.Database != "ok" {
		t.Fatalf("before the first tick: %d %+v", code, report)
	}
	if code, _ := probe("/healthz"); code != http.StatusOK {
		t.Fatalf("liveness must not wait for the scheduler: %d", code)
	}
	ready.markTick(time.Now(), nil)
	if code, report := probe("/readyz"); code != http.StatusOK || !report.Ready || report.GitHubAuth != "unknown" || report.LastTick == nil {
		t.Fatalf("after a successful tick: %d %+v", code, report)
	}
	ready.markAuth(time.Now(), errors.New("bad key"))
	if code, report := probe("/readyz"); code != http.StatusServiceUnavailable || report.GitHubAuth != "failing" {
		t.Fatalf("after failed GitHub authentication: %d %+v", code, report)
	}
	ready.markAuth(time.Now(), nil)
	ready.markTick(time.Now(), errors.New("docker unavailable"))
	if code, report := probe("/readyz"); code != http.StatusServiceUnavailable || report.Scheduler != "failing" ||
		strings.Contains(fmt.Sprint(report), "docker") {
		t.Fatalf("after a failed tick: %d %+v", code, report)
	}
	ready.markTick(time.Now(), nil)
	if report := ready.report(time.Now().Add(readyTickMaxAge + time.Second)); report.Ready || report.Scheduler != "stalled" {
		t.Fatalf("stalled scheduler: %+v", report)
	}
}

func TestGitHubAuthReadiness(t *testing.T) {
	var reject atomic.Bool
	reject.Store(true)
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/1/access_tokens" && reject.Load():
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case r.URL.Path == "/app/installations/2/access_tokens":
			w.WriteHeader(http.StatusUnprocessableEntity)
		case r.URL.Path == "/users/owner/installation":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
		}
	})
	gh.ready = &readiness{}
	j := job{installationID: 1, repositoryID: 2, repository: "owner/repo"}
	if _, err := gh.token(t.Context(), j); err == nil || gh.ready.report(time.Now()).GitHubAuth != "failing" {
		t.Fatalf("rejected App credentials not reported: %v", err)
	}
	reject.Store(false)
	if _, err := gh.token(t.Context(), j); err != nil || gh.ready.report(time.Now()).GitHubAuth != "ok" {
		t.Fatalf("recovered App credentials not reported: %v", err)
	}
	j.installationID = 2
	if _, err := gh.token(t.Context(), j); err == nil || gh.ready.report(time.Now()).GitHubAuth != "ok" {
		t.Fatalf("a repository-scoped refusal is not an authentication failure: %v", err)
	}
	// The poller authenticates with the App JWT before it mints a token.
	p := &poller{github: gh, queue: testQueue(t), cache: make(map[string]pollPage)}
	if err := p.sweep(t.Context()); err == nil || gh.ready.report(time.Now()).GitHubAuth != "failing" {
		t.Fatalf("rejected installation lookup not reported: %v", err)
	}
}

func TestLogHandler(t *testing.T) {
	if handler, level, err := logHandler(nil, "", ""); handler != nil || level != slog.LevelInfo || err != nil {
		t.Fatalf("defaults must keep the standard log output: %v %v %v", handler, level, err)
	}
	if handler, level, err := logHandler(nil, "", "debug"); handler != nil || level != slog.LevelDebug || err != nil {
		t.Fatalf("level without format: %v %v %v", handler, level, err)
	}
	var out bytes.Buffer
	handler, _, err := logHandler(&out, "json", "WARN")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(handler)
	logger.Info("hidden")
	logger.Warn("job failed", "job", 7)
	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil || entry["msg"] != "job failed" || entry["job"] != float64(7) {
		t.Fatalf("json log: %q, %v", out.String(), err)
	}
	out.Reset()
	handler, _, err = logHandler(&out, "text", "")
	if err != nil {
		t.Fatal(err)
	}
	slog.New(handler).Debug("hidden")
	slog.New(handler).Info("shown")
	if !strings.Contains(out.String(), "level=INFO msg=shown") || strings.Contains(out.String(), "hidden") {
		t.Fatalf("text log: %q", out.String())
	}
	for _, invalid := range [][2]string{{"yaml", ""}, {"", "loud"}} {
		if _, _, err := logHandler(nil, invalid[0], invalid[1]); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}
