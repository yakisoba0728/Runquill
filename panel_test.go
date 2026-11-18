package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	p := newPanel(config{pollInterval: 30 * time.Second})
	p.markPoll(now, true)
	host := hostCapacity{dockerHostSnapshot: dockerHostSnapshot{Name: "local", Containers: map[string]string{}}, Limit: 1}
	p.capture(ctx, q, []hostCapacity{host}, 0, 1, nil)

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
		len(got.Fleet.Jobs) != 1 || got.Fleet.Jobs[0].Timing.DiscoverySource != "poll" {
		t.Fatalf("first job progress: %+v", got)
	}

	if _, err := q.ExecContext(ctx, `UPDATE jobs SET phase = ?, github_started_at = ?, runner_cleaned_at = ?,
        runner_cleaned_host = ? WHERE job_id = ?`, completed, now.Unix(), now.Unix(), "local", j.id); err != nil {
		t.Fatal(err)
	}
	p.capture(ctx, q, []hostCapacity{host}, 0, 1, nil)
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
		{webhook(q, "owner", []byte("key"), p), "GET", "http://localhost:8080/snapshot", "", 404},
	} {
		rec := httptest.NewRecorder()
		tc.h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.url, nil))
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.url, rec.Code, tc.code)
		}
	}
}
