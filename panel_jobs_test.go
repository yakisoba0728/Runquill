package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type panelFixture struct {
	t       *testing.T
	q       *queue
	access  *panelAccess
	handler http.Handler
	cookies map[string]*http.Cookie
}

func newPanelFixture(t *testing.T) *panelFixture {
	t.Helper()
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	access := panelAccessForTest(t, q)
	if _, err := q.Exec(`INSERT INTO panel_users(github_id, role) VALUES (102, 'viewer'), (103, 'operator')`); err != nil {
		t.Fatal(err)
	}
	f := &panelFixture{t: t, q: q, access: access, handler: panelHandler(newPanel(config{}), access), cookies: map[string]*http.Cookie{}}
	for name, id := range map[string]int64{"admin": 101, "viewer": 102, "operator": 103} {
		key, err := access.startSession(t.Context(), panelIdentity{GitHubID: id, Login: "fleet-" + name})
		if err != nil {
			t.Fatal(err)
		}
		f.cookies[name] = &http.Cookie{Name: access.cookieName("session"), Value: key}
	}
	return f
}

func (f *panelFixture) call(method, path, body, who string, origin bool) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, f.access.cfg.origin+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin {
		req.Header.Set("Origin", f.access.cfg.origin)
	}
	if strings.HasPrefix(who, "rqk_") {
		req.Header.Set("Authorization", "Bearer "+who)
	} else if cookie := f.cookies[who]; cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *panelFixture) expect(rec *httptest.ResponseRecorder, code int) {
	f.t.Helper()
	if rec.Code != code {
		f.t.Fatalf("got HTTP %d, want %d: %s", rec.Code, code, rec.Body)
	}
}

func TestPanelJobHistoryPagesAndFilters(t *testing.T) {
	f := newPanelFixture(t)
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
	add(1, "owner/app", "phase = 2, runner_cleaned_at = ?, runner_cleaned_host = 'local', provisioned_at = ?", now.Unix(), now.Add(-50*time.Minute).Unix())
	add(2, "owner/app", "phase = 2, failure = 'not started within 24 hours'")
	add(3, "owner/lib", "retry_at = ?, retry_reason = 'provision runner: boom'", now.Add(time.Hour).Unix())
	add(4, "owner/app", "runner_name = 'runquill-4-x', runner_host = 'local'")
	add(5, "owner/app", "")

	page := func(query string) panelHistoryPageResponse {
		t.Helper()
		rec := f.call("GET", "/api/jobs"+query, "", "viewer", false)
		f.expect(rec, 200)
		var out panelHistoryPageResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := page("?limit=2")
	if len(first.Jobs) != 2 || first.Jobs[0].ID != 5 || first.Jobs[1].ID != 4 || first.Next == 0 ||
		first.Jobs[0].Outcome != "waiting" || first.Jobs[1].Outcome != "running" || first.Jobs[1].Host != "local" {
		t.Fatalf("first page: %+v", first)
	}
	second := page(fmt.Sprintf("?limit=2&before=%d", first.Next))
	if len(second.Jobs) != 2 || second.Jobs[0].ID != 3 || second.Jobs[1].ID != 2 || second.Next == 0 ||
		second.Jobs[0].RetryReason != "provision runner: boom" || second.Jobs[0].RetryAt == nil ||
		second.Jobs[1].Failure != "not started within 24 hours" || second.Jobs[1].FinishedAt == nil {
		t.Fatalf("second page: %+v", second)
	}
	last := page(fmt.Sprintf("?limit=2&before=%d", second.Next))
	if len(last.Jobs) != 1 || last.Next != 0 || last.Jobs[0].Outcome != "finished" || last.Jobs[0].Failure != "" ||
		last.Jobs[0].Host != "local" || last.Jobs[0].WaitSeconds == nil || *last.Jobs[0].WaitSeconds != 600 ||
		last.Jobs[0].URL != "https://github.com/owner/app/actions/runs/901/job/1" {
		t.Fatalf("last page: %+v", last)
	}
	if got := page("?outcome=failed"); len(got.Jobs) != 1 || got.Jobs[0].ID != 2 {
		t.Fatalf("failed filter: %+v", got)
	}
	if got := page("?repository=OWNER/lib"); len(got.Jobs) != 1 || got.Jobs[0].ID != 3 {
		t.Fatalf("repository filter: %+v", got)
	}
	if got := page(fmt.Sprintf("?since=%d", now.Add(time.Minute).Unix())); len(got.Jobs) != 0 {
		t.Fatalf("time filter: %+v", got)
	}
	if got := page("?limit=1000"); len(got.Jobs) != 5 {
		t.Fatalf("capped page: %+v", got)
	}
	for _, query := range []string{"?outcome=bogus", "?limit=0", "?before=-1", "?repository=no%20slash"} {
		f.expect(f.call("GET", "/api/jobs"+query, "", "viewer", false), 400)
	}
	f.expect(f.call("GET", "/api/jobs", "", "", false), 401)
}

func TestPanelJobActionsAreGuardedAndAudited(t *testing.T) {
	f := newPanelFixture(t)
	ctx := t.Context()
	now := time.Now()
	for id, update := range map[int64]string{
		7: "retry_at = " + fmt.Sprint(now.Add(time.Hour).Unix()),
		8: "runner_name = 'runquill-8-x'",
		9: "",
	} {
		if _, err := f.q.record(ctx, job{id: id, repository: "owner/app", route: "runquill-1-1-x"}, now); err != nil {
			t.Fatal(err)
		}
		if update != "" {
			if _, err := f.q.ExecContext(ctx, "UPDATE jobs SET "+update+" WHERE job_id = ?", id); err != nil {
				t.Fatal(err)
			}
		}
	}
	token := func() string {
		rec := f.call("POST", "/api/tokens", `{"name":"ops","scope":"hosts:write","days":1}`, "admin", true)
		f.expect(rec, 201)
		var out struct{ Secret string }
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Secret
	}()
	f.expect(f.call("POST", "/api/jobs/7/retry", "", "viewer", true), 403)
	f.expect(f.call("POST", "/api/jobs/7/retry", "", "operator", false), 403)
	f.expect(f.call("POST", "/api/jobs/7/cancel", "", token, false), 403)
	// A jobs:write token may change jobs, but only for an operator or admin owner.
	f.expect(f.call("POST", "/api/tokens", `{"name":"jobs","scope":"jobs:write","days":1}`, "viewer", true), 403)
	rec := f.call("POST", "/api/tokens", `{"name":"jobs","scope":"jobs:write","days":1}`, "operator", true)
	f.expect(rec, 201)
	var jobsToken struct{ Secret string }
	json.Unmarshal(rec.Body.Bytes(), &jobsToken)
	f.expect(f.call("POST", "/api/hosts/cordon", `{"host":"local","cordoned":true}`, jobsToken.Secret, false), 403)
	f.expect(f.call("POST", "/api/jobs/404/retry", "", jobsToken.Secret, false), 409)
	f.expect(f.call("POST", "/api/jobs/7/retry", "", "operator", true), 204)
	var retryAt int64
	if err := f.q.QueryRow("SELECT retry_at FROM jobs WHERE job_id = 7").Scan(&retryAt); err != nil || retryAt != 0 {
		t.Fatalf("retry delay was not cleared: %d %v", retryAt, err)
	}
	f.expect(f.call("POST", "/api/jobs/7/retry", "", "operator", true), 409)
	f.expect(f.call("POST", "/api/jobs/8/cancel", "", "operator", true), 409)
	f.expect(f.call("POST", "/api/jobs/404/cancel", "", "operator", true), 409)
	f.expect(f.call("POST", "/api/jobs/x/cancel", "", "operator", true), 404)
	f.expect(f.call("POST", "/api/jobs/9/cancel", "", "operator", true), 204)
	f.expect(f.call("POST", "/api/jobs/9/cancel", "", "admin", true), 409)
	cancelled, err := f.q.jobs(ctx, "WHERE job_id = 9")
	if err != nil || len(cancelled) != 1 || cancelled[0].phase != completed || cancelled[0].failure != "cancelled by @fleet-operator" {
		t.Fatalf("cancelled job: %+v %v", cancelled, err)
	}
	// The scheduler's claim uses the same guard, so it cannot lease the cancelled job.
	claim, err := f.q.ExecContext(ctx, `UPDATE jobs SET runner_name = 'late' WHERE job_id = 9 AND phase = ? AND runner_name IS NULL`, queued)
	if n, _ := claim.RowsAffected(); err != nil || n != 0 {
		t.Fatalf("a cancelled job could still be claimed: %d %v", n, err)
	}
	var actions string
	if err := f.q.QueryRow(`SELECT group_concat(action || ':' || target, ',') FROM
        (SELECT action, target FROM panel_audit WHERE action GLOB 'job.*' ORDER BY id)`).Scan(&actions); err != nil ||
		actions != "job.retry:7,job.cancel:9" {
		t.Fatalf("job audit entries: %q %v", actions, err)
	}
}

func TestPanelAuditPagesWithActorLoginAndFilters(t *testing.T) {
	f := newPanelFixture(t)
	for range 3 {
		f.expect(f.call("POST", "/api/hosts/cordon", `{"host":"missing","cordoned":true}`, "operator", true), 404)
	}
	read := func(query string) []panelAuditEvent {
		t.Helper()
		rec := f.call("GET", "/api/audit"+query, "", "admin", false)
		f.expect(rec, 200)
		var events []panelAuditEvent
		if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
			t.Fatal(err)
		}
		return events
	}
	all := read("")
	if len(all) != 4 || all[0].Action != "session.login" || all[0].ActorLogin == "" || all[0].ID <= all[1].ID {
		t.Fatalf("audit entries: %+v", all)
	}
	page := read("?limit=2")
	next := read(fmt.Sprintf("?limit=2&before=%d", page[1].ID))
	if len(page) != 2 || len(next) != 2 || next[0].ID >= page[1].ID {
		t.Fatalf("audit pages: %+v %+v", page, next)
	}
	if got := read("?actor=@FLEET-operator"); len(got) != 1 || got[0].Actor != 103 || got[0].ActorLogin != "fleet-operator" {
		t.Fatalf("actor login filter: %+v", got)
	}
	if got := read("?actor=102"); len(got) != 1 || got[0].Actor != 102 {
		t.Fatalf("actor ID filter: %+v", got)
	}
	if got := read("?action=session."); len(got) != 3 {
		t.Fatalf("action prefix filter: %+v", got)
	}
	if got := read("?action=user.bootstrap"); len(got) != 1 || got[0].Target != "101" {
		t.Fatalf("exact action filter: %+v", got)
	}
	f.expect(f.call("GET", "/api/audit?limit=x", "", "admin", false), 400)
	f.expect(f.call("GET", "/api/audit", "", "operator", false), 403)
}

func TestPanelTeamEnrollsByGitHubLogin(t *testing.T) {
	f := newPanelFixture(t)
	var lookups []string
	f.access.oauth.Transport = panelTransport(func(r *http.Request) (*http.Response, error) {
		lookups = append(lookups, r.URL.String())
		body, status := `{"id":4321,"login":"Octo-Cat","type":"User"}`, http.StatusOK
		switch r.URL.Path {
		case "/users/ghost":
			body, status = `{"message":"Not Found"}`, http.StatusNotFound
		case "/users/acme":
			body = `{"id":99,"login":"acme","type":"Organization"}`
		case "/users/limited":
			body, status = `{}`, http.StatusForbidden
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	f.expect(f.call("POST", "/api/team", `{"login":"@octo-cat","role":"operator"}`, "operator", true), 403)
	f.expect(f.call("POST", "/api/team", `{"login":"bad/login","role":"viewer"}`, "admin", true), 400)
	f.expect(f.call("POST", "/api/team", `{"login":"ghost","role":"viewer"}`, "admin", true), 404)
	f.expect(f.call("POST", "/api/team", `{"login":"acme","role":"viewer"}`, "admin", true), 404)
	f.expect(f.call("POST", "/api/team", `{"login":"limited","role":"viewer"}`, "admin", true), 502)
	rec := f.call("POST", "/api/team", `{"login":"@octo-cat","role":"operator"}`, "admin", true)
	f.expect(rec, 200)
	if !strings.Contains(rec.Body.String(), `"github_id":4321`) || len(lookups) != 4 ||
		lookups[3] != "https://api.github.com/users/octo-cat" {
		t.Fatalf("lookup: %s %v", rec.Body, lookups)
	}
	var login, role string
	if err := f.q.QueryRow("SELECT login, role FROM panel_users WHERE github_id = 4321").Scan(&login, &role); err != nil ||
		login != "Octo-Cat" || role != "operator" {
		t.Fatalf("enrolled member: %q %q %v", login, role, err)
	}
	var audited int
	if err := f.q.QueryRow("SELECT count(*) FROM panel_audit WHERE action = 'user.set.operator' AND target = '4321' AND actor = 101").Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("team audit: %d %v", audited, err)
	}
}
