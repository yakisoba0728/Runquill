package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPanelMigrateOutlivesServerWriteTimeout(t *testing.T) {
	q := testQueue(t)
	access := panelAccessForTest(t, q)
	fake, api := newFakeMigrationGitHub(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // a slow GitHub response
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(slow.Close)
	api.base = slow.URL
	mux := http.NewServeMux()
	registerMigrateRoutes(mux, access, migrateDeps{api: api})
	server := httptest.NewUnstartedServer(mux)
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)
	admin, err := access.startSession(t.Context(), panelIdentity{GitHubID: 101, Login: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct{ method, path, body, want string }{
		{"GET", "/api/migrate/status", "", `"ready":true`},
		{"POST", "/api/migrate/preview", `{"repository":"owner/repo","mapping":[{"from":["ubuntu-latest"]}]}`, `"hash":`},
	} {
		req, err := http.NewRequestWithContext(t.Context(), call.method, server.URL+call.path, strings.NewReader(call.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = access.cfg.host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", access.cfg.origin)
		req.AddCookie(&http.Cookie{Name: access.cookieName("session"), Value: admin})
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", call.path, err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), call.want) {
			t.Fatalf("%s: HTTP %d %s, %v", call.path, response.StatusCode, body, err)
		}
	}
}

func TestPanelMigrateRoutes(t *testing.T) {
	q := testQueue(t)
	access := panelAccessForTest(t, q)
	fake, api := newFakeMigrationGitHub(t)
	mux := http.NewServeMux()
	registerMigrateRoutes(mux, access, migrateDeps{api: api})
	if _, err := q.Exec(`INSERT INTO panel_users(github_id, role) VALUES (102, 'operator')`); err != nil {
		t.Fatal(err)
	}
	admin, err := access.startSession(t.Context(), panelIdentity{GitHubID: 101, Login: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	operator, err := access.startSession(t.Context(), panelIdentity{GitHubID: 102, Login: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	const tokenKey = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"
	digest := panelDigest(tokenKey)
	if _, err := q.Exec(`INSERT INTO panel_tokens(digest,user_id,name,scope,expires_at,created_at) VALUES (?,101,'ci','hosts:write',9999999999,1)`, digest[:]); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, session string, origin bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, access.cfg.origin+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if origin {
			req.Header.Set("Origin", access.cfg.origin)
		}
		if strings.HasPrefix(session, "rqk_") {
			req.Header.Set("Authorization", "Bearer "+session)
		} else if session != "" {
			req.AddCookie(&http.Cookie{Name: access.cookieName("session"), Value: session})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	expect := func(rec *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if rec.Code != status || code != "" && !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("got HTTP %d %s, want %d %s", rec.Code, rec.Body, status, code)
		}
	}
	previewBody := `{"repository":"owner/repo","mapping":[{"from":["ubuntu-latest"]}]}`

	expect(request("GET", "/api/migrate/status", "", "", false), http.StatusUnauthorized, "")
	expect(request("GET", "/api/migrate/status", "", operator, false), http.StatusForbidden, "")
	expect(request("GET", "/api/migrate/status", "", "rqk_"+tokenKey, false), http.StatusForbidden, "")
	expect(request("POST", "/api/migrate/preview", previewBody, "rqk_"+tokenKey, false), http.StatusForbidden, "")
	expect(request("POST", "/api/migrate/open", previewBody, "rqk_"+tokenKey, false), http.StatusForbidden, "")
	expect(request("POST", "/api/migrate/preview", previewBody, admin, false), http.StatusForbidden, "")

	status := request("GET", "/api/migrate/status", "", admin, false)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"ready":true`) {
		t.Fatalf("status: %d %s", status.Code, status.Body)
	}
	delete(fake.permissions, "workflows")
	status = request("GET", "/api/migrate/status", "", admin, false)
	if !strings.Contains(status.Body.String(), `"ready":false`) || !strings.Contains(status.Body.String(), `"missing":["workflows"]`) {
		t.Fatalf("status without workflows: %s", status.Body)
	}

	expect(request("POST", "/api/migrate/preview", `{"repository":"owner/repo","mapping":[]}`, admin, true), http.StatusBadRequest, "mapping")
	expect(request("POST", "/api/migrate/preview", `{"repository":"other/repo","mapping":[{"from":["ubuntu-latest"]}]}`, admin, true), http.StatusBadRequest, "repository")
	rec := request("POST", "/api/migrate/preview", previewBody, admin, true)
	expect(rec, http.StatusOK, "")
	var preview migrationPreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil || len(preview.Files) != 2 || !preview.Files[0].Changed {
		t.Fatalf("preview: %v %s", err, rec.Body)
	}
	openBody := func(hash string) string {
		return `{"repository":"owner/repo","mapping":[{"from":["ubuntu-latest"]}],"hash":"` + hash + `"}`
	}
	expect(request("POST", "/api/migrate/open", openBody(preview.Hash), admin, true), http.StatusConflict, "permissions")
	fake.permissions["workflows"] = "write"
	expect(request("POST", "/api/migrate/open", openBody("short"), admin, true), http.StatusBadRequest, "hash")
	expect(request("POST", "/api/migrate/open", openBody(preview.Hash), admin, false), http.StatusForbidden, "")
	expect(request("POST", "/api/migrate/open", openBody(strings.Repeat("b", 64)), admin, true), http.StatusConflict, "stale")
	if len(fake.branches) != 1 {
		t.Fatal("a stale open created a branch")
	}

	rec = request("POST", "/api/migrate/open", openBody(preview.Hash), admin, true)
	expect(rec, http.StatusCreated, "")
	var pr migrationPR
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil || pr.URL != "https://github.com/owner/repo/pull/5" {
		t.Fatalf("open: %v %s", err, rec.Body)
	}
	rec = request("POST", "/api/migrate/open", openBody(preview.Hash), admin, true)
	expect(rec, http.StatusConflict, "branch_exists")
	if !strings.Contains(rec.Body.String(), pr.URL) {
		t.Fatalf("branch exists response: %s", rec.Body)
	}

	fake.outside = true
	expect(request("POST", "/api/migrate/preview", previewBody, admin, true), http.StatusForbidden, "scope")

	rows, err := q.Query(`SELECT action, target FROM panel_audit WHERE action LIKE 'migrate.%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var audit []string
	for rows.Next() {
		var action, target string
		if err := rows.Scan(&action, &target); err != nil {
			t.Fatal(err)
		}
		audit = append(audit, action+" "+target)
	}
	want := []string{
		"migrate.request owner/repo", "migrate.failed owner/repo",
		"migrate.request owner/repo", "migrate.failed owner/repo",
		"migrate.request owner/repo", "migrate.open owner/repo " + pr.URL,
		"migrate.request owner/repo", "migrate.failed owner/repo",
	}
	if strings.Join(audit, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit entries:\n%s", strings.Join(audit, "\n"))
	}

	t.Setenv("RUNQUILL_OWNER", "")
	unconfigured := http.NewServeMux()
	registerMigrateRoutes(unconfigured, access, migrateDeps{})
	req := httptest.NewRequest("GET", access.cfg.origin+"/api/migrate/status", nil)
	req.AddCookie(&http.Cookie{Name: access.cookieName("session"), Value: admin})
	out := httptest.NewRecorder()
	unconfigured.ServeHTTP(out, req)
	if out.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d", out.Code)
	}
}
