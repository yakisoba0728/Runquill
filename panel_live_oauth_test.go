package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This manual check needs a dedicated GitHub OAuth App configured for the loopback
// RUNQUILL_PANEL_URL, the usual RUNQUILL_PANEL_* credentials, and the two users' IDs.
//
//	RUNQUILL_TEST_GITHUB_OAUTH=1 RUNQUILL_TEST_PANEL_VIEWER_ID=123 \
//	  go test -v -timeout=12m -run '^TestLiveGitHubPanelRoles$'
func TestLiveGitHubPanelRoles(t *testing.T) {
	if os.Getenv("RUNQUILL_TEST_GITHUB_OAUTH") != "1" {
		t.Skip("set RUNQUILL_TEST_GITHUB_OAUTH=1 to run the two-account browser check")
	}
	cfg, err := loadPanelOAuthConfig()
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := strconv.ParseInt(os.Getenv("RUNQUILL_TEST_PANEL_VIEWER_ID"), 10, 64)
	if err != nil || viewerID < 1 || cfg.bootstrapID < 1 || viewerID == cfg.bootstrapID {
		t.Fatal("set RUNQUILL_PANEL_BOOTSTRAP_ID and RUNQUILL_TEST_PANEL_VIEWER_ID to distinct GitHub account IDs")
	}

	panelURL, err := url.Parse(cfg.origin)
	if err != nil || panelURL.Scheme != "http" || panelURL.Port() == "" || !net.ParseIP(panelURL.Hostname()).IsLoopback() {
		t.Fatal("set RUNQUILL_PANEL_URL to a loopback IP and an explicit port, such as http://127.0.0.1:18861")
	}
	listener, err := net.Listen("tcp", panelURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	access, err := newPanelAccess(t.Context(), q.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	panel := newPanel(config{})
	panel.snapshot.Fleet = &statusReport{Hosts: []statusHost{{Name: "fixture"}}}
	handler := panelHandler(panel, access)
	cookies := make(chan *http.Cookie, 2)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/auth/github/callback" {
				handler.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			for header, values := range rec.Header() {
				w.Header()[header] = values
			}
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			if rec.Code == http.StatusSeeOther {
				for _, cookie := range (&http.Response{Header: rec.Header()}).Cookies() {
					if cookie.Name == access.cookieName("session") {
						select {
						case cookies <- cookie:
						default:
						}
					}
				}
			}
		}),
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	defer server.Close()

	request := func(method, path, body string, cookie *http.Cookie, expected int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, cfg.origin+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", cfg.origin)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != expected {
			t.Fatalf("%s %s returned HTTP %d, want %d", method, path, rec.Code, expected)
		}
		return rec
	}
	login := func(identityID int64, role string) *http.Cookie {
		t.Helper()
		t.Logf("Visit %s/auth/github in the %s account's browser profile. Callback: %s/auth/github/callback", cfg.origin, role, cfg.origin)
		timer := time.NewTimer(4 * time.Minute)
		defer timer.Stop()
		select {
		case cookie := <-cookies:
			response := request("GET", "/api/me", "", cookie, 200)
			var identity panelIdentity
			if err := json.NewDecoder(response.Body).Decode(&identity); err != nil || identity.GitHubID != identityID || identity.Role != role {
				t.Fatalf("callback did not log in the configured %s identity", role)
			}
			return cookie
		case err := <-serverDone:
			t.Fatalf("panel stopped before the %s signed in: %v", role, err)
		case <-timer.C:
			t.Fatalf("timed out waiting for the %s; check the OAuth callback and use a separate browser profile", role)
		}
		return nil
	}

	admin := login(cfg.bootstrapID, "admin")
	request("PUT", fmt.Sprintf("/api/team/%d", viewerID), `{"role":"viewer"}`, admin, 204)
	request("GET", "/api/team", "", admin, 200)
	request("POST", "/api/hosts/cordon", `{"host":"fixture","cordoned":true}`, admin, 204)

	viewer := login(viewerID, "viewer")
	request("GET", "/snapshot", "", viewer, 200)
	request("GET", "/api/team", "", viewer, 403)
	request("POST", "/api/hosts/cordon", `{"host":"fixture","cordoned":false}`, viewer, 403)
	request("POST", "/api/logout", "", viewer, 204)
	request("POST", "/api/logout", "", admin, 204)
}
