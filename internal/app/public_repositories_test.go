package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseAllowPublic(t *testing.T) {
	for value, want := range map[string]bool{"": false, " ": false, "false": false, "0": false, "true": true, "1": true, " TRUE ": true} {
		if got, err := parseAllowPublic(value); err != nil || got != want {
			t.Errorf("parseAllowPublic(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	if _, err := parseAllowPublic("yes"); err == nil || !strings.Contains(err.Error(), allowPublicEnv) {
		t.Errorf("an invalid value must name %s: %v", allowPublicEnv, err)
	}
}

func TestWebhookTakesPublicRepositoriesOnlyWhenAllowed(t *testing.T) {
	const (
		private  = `{"id":2,"full_name":"owner/repo","private":true,"owner":{"login":"owner"}}`
		public   = `{"id":3,"full_name":"owner/open","private":false,"visibility":"public","owner":{"login":"owner"}}`
		foreign  = `{"id":4,"full_name":"other/open","private":false,"visibility":"public","owner":{"login":"other"}}`
		internal = `{"id":5,"full_name":"owner/inner","private":true,"visibility":"internal","owner":{"login":"owner"}}`
	)
	for _, allow := range []bool{false, true} {
		q := testQueue(t)
		handler := webhook(q, &github{owner: "owner", allowPublic: allow}, []byte("secret"), nil)
		for id, repo := range []string{private, public, foreign, internal} {
			payload := fmt.Sprintf(`{"action":"queued","installation":{"id":1},"repository":%s,
				"workflow_job":{"id":%d,"run_id":17,"labels":["runquill-17-1-ci"]}}`, repo, id+1)
			mac := hmac.New(sha256.New, []byte("secret"))
			mac.Write([]byte(payload))
			req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
			req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			req.Header.Set("X-GitHub-Event", "workflow_job")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusAccepted {
				t.Fatalf("allow=%v %s: status %d", allow, repo, res.Code)
			}
		}
		jobs, err := q.jobs(t.Context(), "ORDER BY job_id")
		want := []string{"owner/repo"}
		if allow {
			want = append(want, "owner/open")
		}
		var got []string
		for _, j := range jobs {
			got = append(got, j.repository)
		}
		if err != nil || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("allow=%v: recorded %v, want %v (%v)", allow, got, want, err)
		}
	}
}

func TestPollerScansPublicRepositoriesOnlyWhenAllowed(t *testing.T) {
	for _, allow := range []bool{false, true} {
		var publicScans atomic.Int32
		gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/users/owner/installation":
				fmt.Fprint(w, `{"id":1,"account":{"login":"owner","type":"User"}}`)
			case "/app/installations/1/access_tokens":
				fmt.Fprintf(w, `{"token":"poll","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
			case "/installation/repositories":
				fmt.Fprint(w, `{"repositories":[
					{"id":3,"full_name":"owner/open","private":false,"visibility":"public","owner":{"login":"owner"}},
					{"id":4,"full_name":"other/open","private":false,"visibility":"public","owner":{"login":"other"}},
					{"id":5,"full_name":"owner/gone","private":false,"visibility":"public","archived":true,"owner":{"login":"owner"}},
					{"id":6,"full_name":"owner/inner","private":true,"visibility":"internal","owner":{"login":"owner"}}]}`)
			case "/repos/owner/open/actions/runs":
				publicScans.Add(1)
				if r.URL.Query().Get("status") != "queued" {
					fmt.Fprint(w, `{"workflow_runs":[]}`)
					return
				}
				fmt.Fprint(w, `{"workflow_runs":[{"id":17}]}`)
			case "/repos/owner/open/actions/runs/17/jobs":
				fmt.Fprint(w, `{"jobs":[{"id":41,"run_id":17,"status":"queued","labels":["runquill-17-1-ci"]}]}`)
			default:
				t.Errorf("allow=%v: unexpected GitHub API call: %s %s", allow, r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		})
		gh.allowPublic = allow
		q := testQueue(t)
		p := &poller{github: gh, queue: q, cache: make(map[string]pollPage)}
		if err := p.sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
		jobs, err := q.jobs(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}
		if allow && (len(jobs) != 1 || jobs[0].repository != "owner/open" || jobs[0].repositoryID != 3) {
			t.Errorf("allowed public repository job was not recorded: %+v", jobs)
		}
		if !allow && (len(jobs) != 0 || publicScans.Load() != 0) {
			t.Errorf("public repository scanned without opt-in: %+v, %d scans", jobs, publicScans.Load())
		}
	}
}

// publicLaunch runs one scheduler tick for job 9 of owner/repo (ID 2), whose workflow run 17 is
// described by run, and reports the stored job, the runs API calls and JIT registrations.
func publicLaunch(t *testing.T, allow, organization bool, repo, run string) (job, *testRuntime, int32, int32) {
	t.Helper()
	var runCalls, registered atomic.Int32
	gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/owner/repo/installation":
			fmt.Fprint(w, `{"id":1,"account":{"login":"owner","type":"Organization"},"repository_selection":"all"}`)
		case "POST /app/installations/1/access_tokens":
			fmt.Fprint(w, `{"token":"stub"}`)
		case "GET /repos/owner/repo":
			fmt.Fprint(w, repo)
		case "GET /repos/owner/repo/actions/jobs/9":
			fmt.Fprint(w, `{"id":9,"run_id":17,"status":"queued"}`)
		case "GET /repos/owner/repo/actions/runs/17":
			runCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer stub" {
				t.Error("the fork check must use the repository token")
			}
			if run == "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, run)
		case "POST /repos/owner/repo/actions/runners/generate-jitconfig":
			registered.Add(1)
			fmt.Fprint(w, `{"runner":{"id":55},"encoded_jit_config":"test-config"}`)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	gh.allowPublic, gh.organization = allow, organization
	q := testQueue(t)
	now := time.Now()
	recordTestJob(t, q, 9, "runquill-17-1-ci", now)
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: map[string]string{}}}}
	s := &scheduler{queue: q, github: gh, runtime: runtime, limit: 1}
	if err := s.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	return readJob(t, q, 9), runtime, runCalls.Load(), registered.Load()
}

func TestSchedulerRefusesForkRunsOfPublicRepositories(t *testing.T) {
	const (
		public   = `{"id":2,"full_name":"owner/repo","private":false,"visibility":"public","owner":{"login":"owner"}}`
		sameRepo = `{"id":17,"repository":{"id":2},"head_repository":{"id":2,"full_name":"owner/repo"}}`
	)
	for _, test := range []struct {
		name, run, failure, retry string
	}{
		{"fork", `{"id":17,"repository":{"id":2},"head_repository":{"id":99,"full_name":"outsider/repo"}}`,
			"the workflow run comes from the fork outsider/repo; Runquill does not run workflow runs from forks of public repositories", ""},
		{"deleted fork", `{"id":17,"repository":{"id":2},"head_repository":null}`,
			"the workflow run comes from a fork that no longer exists; Runquill does not run workflow runs from forks of public repositories", ""},
		{"missing head", `{"id":17,"repository":{"id":2}}`,
			"the workflow run comes from a fork that no longer exists; Runquill does not run workflow runs from forks of public repositories", ""},
		{"other run", `{"id":18,"repository":{"id":2},"head_repository":{"id":2}}`, "", "different workflow run"},
		{"GitHub error", "", "", "HTTP 500"},
	} {
		got, runtime, runCalls, registered := publicLaunch(t, true, false, public, test.run)
		if runCalls != 1 || registered != 0 || len(runtime.started) != 0 || got.runnerName != "" {
			t.Errorf("%s: a public repository run that is not proven to be from the repository itself must not start a runner: %+v, runs API %d, JIT %d, started %v",
				test.name, got, runCalls, registered, runtime.started)
		}
		if test.failure != "" && (got.phase != completed || got.failure != test.failure) {
			t.Errorf("%s: job must fail with the reason: %+v", test.name, got)
		}
		if test.retry != "" && (got.phase != queued || got.failure != "" || !strings.Contains(got.retryReason, test.retry)) {
			t.Errorf("%s: job must wait and retry: %+v", test.name, got)
		}
	}

	got, runtime, runCalls, registered := publicLaunch(t, true, false, public, sameRepo)
	if runCalls != 1 || registered != 1 || len(runtime.started) != 1 || got.phase != queued || got.failure != "" {
		t.Errorf("a public repository's own run must start a runner: %+v, runs API %d, JIT %d", got, runCalls, registered)
	}
	got, runtime, runCalls, registered = publicLaunch(t, true, true,
		`{"id":2,"full_name":"owner/repo","private":false,"visibility":"public","owner":{"login":"owner"}}`, sameRepo)
	if runCalls != 1 || registered != 1 || len(runtime.started) != 1 || got.failure != "" {
		t.Errorf("an organization's public repository must start a runner: %+v, runs API %d, JIT %d", got, runCalls, registered)
	}
}

func TestSchedulerKeepsOtherRepositoryChecksForPublicRepositories(t *testing.T) {
	for _, test := range []struct {
		allow, organization bool
		repo, failure       string
	}{
		{false, false, `{"id":2,"private":false,"visibility":"public","owner":{"login":"owner"}}`, "repository is public"},
		{true, false, `{"id":2,"private":false,"visibility":"public","owner":{"login":"other"}}`, `repository owner is "other", not "owner"`},
		{true, false, `{"id":2,"private":false,"visibility":"public","archived":true,"owner":{"login":"owner"}}`, "repository is archived"},
		{true, false, `{"id":2,"private":false,"visibility":"public","disabled":true,"owner":{"login":"owner"}}`, "repository is disabled"},
		{true, false, `{"id":3,"private":false,"visibility":"public","owner":{"login":"owner"}}`, "GitHub repository ID is 3, not 2"},
		{true, true, `{"id":2,"private":true,"visibility":"internal","owner":{"login":"owner"}}`, "repository is internal"},
		{true, true, `{"id":2,"private":false,"owner":{"login":"owner"}}`, "GitHub did not report a consistent repository visibility"},
	} {
		got, runtime, runCalls, registered := publicLaunch(t, test.allow, test.organization, test.repo, "")
		want := "repository is no longer eligible: " + test.failure
		if got.phase != completed || got.failure != want || runCalls != 0 || registered != 0 || len(runtime.started) != 0 {
			t.Errorf("%s: %+v, runs API %d, JIT %d; want failure %q", test.repo, got, runCalls, registered, want)
		}
	}
}

func TestSchedulerSkipsForkCheckForPrivateRepositories(t *testing.T) {
	for _, allow := range []bool{false, true} {
		got, runtime, runCalls, registered := publicLaunch(t, allow, false,
			`{"id":2,"full_name":"owner/repo","private":true,"owner":{"login":"owner"}}`, "")
		if runCalls != 0 || registered != 1 || len(runtime.started) != 1 || got.failure != "" {
			t.Errorf("allow=%v: private repository launch: %+v, runs API %d, JIT %d", allow, got, runCalls, registered)
		}
	}
}

func TestOrganizationRunnerGroupPublicRepositories(t *testing.T) {
	const (
		private  = `{"id":9,"full_name":"team/private","private":true,"visibility":"private","owner":{"login":"team"}}`
		public   = `{"id":10,"full_name":"team/open","private":false,"visibility":"public","owner":{"login":"team"}}`
		internal = `{"id":11,"full_name":"team/inner","private":true,"visibility":"internal","owner":{"login":"team"}}`
	)
	for _, test := range []struct {
		allow, groupPublic bool
		members, err       string
	}{
		{false, false, private, ""},
		{false, true, private, "disallow public repositories"},
		{false, false, private + "," + public, "must contain only private repositories"},
		{true, false, private, ""},
		{true, true, private + "," + public, ""},
		{true, false, private + "," + public, "disallows public repositories but contains the public repository team/open"},
		{true, true, private + "," + internal, "must contain only private or public repositories"},
	} {
		gh := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/orgs/team/actions/runner-groups/12":
				fmt.Fprintf(w, `{"id":12,"visibility":"selected","allows_public_repositories":%t,"inherited":false}`, test.groupPublic)
			case "/orgs/team/actions/runner-groups/12/repositories":
				fmt.Fprintf(w, `{"repositories":[%s]}`, test.members)
			case "/installation/repositories":
				fmt.Fprintf(w, `{"repositories":[%s]}`, test.members)
			default:
				t.Errorf("unexpected API request %s %s", r.Method, r.URL)
				w.WriteHeader(http.StatusNotFound)
			}
		})
		gh.owner, gh.organization, gh.runnerGroupID, gh.repositoryScope, gh.allowPublic = "team", true, 12, scopeSelected, test.allow
		repositories, err := gh.runnerGroupRepositories(t.Context(), installationToken{Token: "org"})
		if test.err == "" {
			if err != nil || repositories[9] != "team/private" || strings.Contains(test.members, public) && repositories[10] != "team/open" {
				t.Errorf("allow=%v group public=%v: %v, %v", test.allow, test.groupPublic, repositories, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), test.err) {
			t.Errorf("allow=%v group public=%v: error %v, want %q", test.allow, test.groupPublic, err, test.err)
		}
	}
}
