package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func panelAccessForTest(t *testing.T, q *queue) *panelAccess {
	t.Helper()
	cfg, err := parsePanelOAuthConfig("http://localhost:8081", "test-client", "test-secret", "101")
	if err != nil {
		t.Fatal(err)
	}
	access, err := newPanelAccess(t.Context(), q.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return access
}

type panelTransport func(*http.Request) (*http.Response, error)

func (transport panelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func TestGitHubPanelLoginRolesAndTokens(t *testing.T) {
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
	panel := newPanel(config{})
	panel.snapshot.Fleet = &statusReport{Hosts: []statusHost{{Name: "local"}}}
	handler := panelHandler(panel, access)
	request := func(method, path, body, origin, bearer string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, access.cfg.origin+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	assertStatus := func(rec *httptest.ResponseRecorder, expected int) {
		t.Helper()
		if rec.Code != expected {
			t.Fatalf("got HTTP %d, want %d: %s", rec.Code, expected, rec.Body)
		}
	}
	assertStatus(request("GET", "/snapshot", "", "", ""), 401)
	redirect := request("GET", "/auth/github", "", "", "")
	assertStatus(redirect, 302)
	authorize, err := url.Parse(redirect.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	pkce, state := authorize.Query().Get("code_challenge"), authorize.Query().Get("state")
	if authorize.Host != "github.com" || len(pkce) != 43 || len(state) != 43 ||
		authorize.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("invalid GitHub login redirect: %s", authorize)
	}
	stateCookie := redirect.Result().Cookies()[0]
	if !stateCookie.HttpOnly || stateCookie.SameSite != http.SameSiteLaxMode || stateCookie.Value != state {
		t.Fatalf("invalid state cookie: %v", stateCookie)
	}
	calls := 0
	profile := `{"id":101,"login":"fleet-admin"}`
	access.oauth.Transport = panelTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := profile
		switch r.URL.Host + r.URL.Path {
		case "github.com/login/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			digest := panelDigest(r.Form.Get("code_verifier"))
			if base64.RawURLEncoding.EncodeToString(digest[:]) != pkce ||
				r.Form.Get("redirect_uri") != access.cfg.origin+"/auth/github/callback" {
				t.Fatal("OAuth exchange did not verify the original PKCE challenge")
			}
			body = `{"access_token":"ephemeral-github-token"}`
		case "api.github.com/user":
			if r.Header.Get("Authorization") != "Bearer ephemeral-github-token" {
				t.Fatal("OAuth exchange did not verify the logged-in account")
			}
		default:
			t.Fatalf("unexpected OAuth URL: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	callback := "/auth/github/callback?code=test&state=" + state
	assertStatus(request("GET", callback+"wrong", "", "", "", stateCookie), 400)
	if calls != 0 {
		t.Fatal("mismatched state reached GitHub")
	}
	result := request("GET", callback, "", "", "", stateCookie)
	assertStatus(result, 303)
	if calls != 2 {
		t.Fatalf("GitHub calls: %d", calls)
	}
	assertStatus(request("GET", callback, "", "", "", stateCookie), 400)
	var adminCookie *http.Cookie
	for _, cookie := range result.Result().Cookies() {
		if cookie.Name == access.cookieName("session") {
			adminCookie = cookie
		}
	}
	if adminCookie == nil || !adminCookie.HttpOnly || adminCookie.Value == "ephemeral-github-token" {
		t.Fatal("no usable panel session")
	}
	assertStatus(request("GET", "/snapshot", "", "", "", adminCookie), 200)
	assertStatus(request("PUT", "/api/team/102", `{"role":"viewer","disabled":false}`,
		"https://foreign.invalid", "", adminCookie), 403)
	assertStatus(request("PUT", "/api/team/101", `{"role":"viewer","disabled":false}`,
		access.cfg.origin, "", adminCookie), 409)
	assertStatus(request("PUT", "/api/team/102", `{"role":"viewer","disabled":false}`,
		access.cfg.origin, "", adminCookie), 204)
	profile = `{"id":102,"login":"fleet-viewer"}`
	redirect = request("GET", "/auth/github", "", "", "")
	assertStatus(redirect, 302)
	authorize, err = url.Parse(redirect.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	pkce, state = authorize.Query().Get("code_challenge"), authorize.Query().Get("state")
	result = request("GET", "/auth/github/callback?code=viewer&state="+state, "", "", "", redirect.Result().Cookies()[0])
	assertStatus(result, 303)
	var viewerCookie *http.Cookie
	for _, cookie := range result.Result().Cookies() {
		if cookie.Name == access.cookieName("session") {
			viewerCookie = cookie
		}
	}
	if viewerCookie == nil || viewerCookie.Value == adminCookie.Value || calls != 4 {
		t.Fatal("viewer did not complete a separate OAuth sign-in")
	}
	assertStatus(request("GET", "/api/me", "", "", "", viewerCookie), 200)
	host := `{"host":"local","cordoned":true}`
	assertStatus(request("GET", "/api/team", "", "", "", viewerCookie), 403)
	assertStatus(request("POST", "/api/hosts/cordon", host, access.cfg.origin, "", viewerCookie), 403)
	assertStatus(request("POST", "/api/tokens", `{"name":"denied","scope":"hosts:write","days":7}`,
		access.cfg.origin, "", viewerCookie), 403)

	assertStatus(request("POST", "/api/backup", "", access.cfg.origin, "", viewerCookie), 403)
	assertStatus(request("POST", "/api/diagnose", `{"repository":"owner/private"}`, access.cfg.origin, "", viewerCookie), 403)
	assertStatus(request("POST", "/api/backup", "", "https://foreign.invalid", "", adminCookie), 403)
	backup := request("POST", "/api/backup", "", access.cfg.origin, "", adminCookie)
	assertStatus(backup, 200)
	if !strings.HasPrefix(backup.Body.String(), "SQLite format 3") ||
		backup.Header().Get("Content-Disposition") != "attachment; filename=runquill-queue-backup.sqlite3" {
		t.Fatal("backup did not return a downloadable SQLite file")
	}

	token := func(input string, cookie *http.Cookie) (int64, string) {
		t.Helper()
		rec := request("POST", "/api/tokens", input, access.cfg.origin, "", cookie)
		assertStatus(rec, 201)
		var result struct {
			ID     int64  `json:"id"`
			Secret string `json:"secret"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.ID == 0 || !strings.HasPrefix(result.Secret, "rqk_") {
			t.Fatal("token was not issued")
		}
		list := request("GET", "/api/tokens", "", "", "", cookie)
		if strings.Contains(list.Body.String(), result.Secret) {
			t.Fatal("token list returned the secret")
		}
		return result.ID, result.Secret
	}
	id, read := token(`{"name":"monitor","scope":"fleet:read","days":7}`, viewerCookie)
	assertStatus(request("GET", "/snapshot", "", "", read), 200)
	assertStatus(request("POST", "/api/hosts/cordon", host, "", read), 403)
	assertStatus(request("GET", "/api/tokens", "", "", read), 403)
	_, write := token(`{"name":"maintenance","scope":"hosts:write","days":7}`, adminCookie)
	assertStatus(request("POST", "/api/hosts/cordon", host, "", write), 204)
	assertStatus(request("GET", "/snapshot", "", "", write), 403)
	hosts, err := readHostStates(t.Context(), q.DB)
	if err != nil || !hosts["local"].cordoned {
		t.Fatalf("host maintenance was not persisted: %v, %v", hosts, err)
	}
	audit := request("GET", "/api/audit", "", "", "", adminCookie)
	assertStatus(audit, 200)
	if !strings.Contains(audit.Body.String(), "host.cordon") {
		t.Fatal("maintenance action is missing from the audit history")
	}
	assertStatus(request("DELETE", fmt.Sprintf("/api/tokens/%d", id), "", access.cfg.origin, "", viewerCookie), 204)
	assertStatus(request("GET", "/snapshot", "", "", read), 401)
	assertStatus(request("PUT", "/api/team/102", `{"role":"viewer","disabled":true}`,
		access.cfg.origin, "", adminCookie), 204)
	assertStatus(request("GET", "/snapshot", "", "", "", viewerCookie), 401)
}
