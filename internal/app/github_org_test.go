package app

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOrganizationRunnerGroupLifecycle(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const repo = `{"id":9,"full_name":"team/private","private":true,"visibility":"private","owner":{"login":"team"}}`
	const install = `{"id":7,"account":{"login":"team","type":"Organization"},"repository_selection":"selected","permissions":{"actions":"read","organization_self_hosted_runners":"write"}}`
	var public, restricted, revoked, excess, outside atomic.Bool
	var registered, removed atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch r.Method + " " + r.URL.Path {
		case "GET /orgs/team/installation":
			fmt.Fprint(w, install)
		case "GET /repos/team/private/installation":
			if revoked.Load() {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, install)
		case "POST /app/installations/7/access_tokens":
			var body struct {
				Permissions   map[string]string
				RepositoryIDs []int64 `json:"repository_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Permissions) != 1 {
				t.Errorf("unexpected token scope: %+v, %v", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch {
			case body.Permissions[organizationRunnerPermission] == "write" && len(body.RepositoryIDs) == 0:
				fmt.Fprint(w, `{"token":"org-runners"}`)
			case body.Permissions[organizationRunnerPermission] == "read" && len(body.RepositoryIDs) == 0:
				fmt.Fprint(w, `{"token":"org-group-read"}`)
			case body.Permissions["actions"] == "read" && slices.Equal(body.RepositoryIDs, []int64{9}):
				if revoked.Load() {
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				fmt.Fprint(w, `{"token":"repo-read"}`)
			case body.Permissions["actions"] == "read" && len(body.RepositoryIDs) == 0:
				fmt.Fprintf(w, `{"token":"poll-read","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
			default:
				t.Errorf("unexpected token scope: %+v", body)
				w.WriteHeader(http.StatusBadRequest)
			}
		case "GET /orgs/team/actions/runner-groups/12":
			if auth != "Bearer org-runners" && auth != "Bearer org-group-read" {
				t.Errorf("runner group fetched with wrong token: %q", auth)
			}
			fmt.Fprintf(w, `{"id":12,"visibility":"selected","allows_public_repositories":%t,"inherited":false,"restricted_to_workflows":%t}`, public.Load(), restricted.Load())
		case "GET /orgs/team/actions/runner-groups/12/repositories":
			const peer = `{"id":10,"full_name":"team/peer","private":true,"visibility":"private","owner":{"login":"team"}}`
			if excess.Load() {
				fmt.Fprintf(w, `{"repositories":[%s,%s]}`, repo, peer)
			} else if outside.Load() {
				fmt.Fprintf(w, `{"repositories":[%s]}`, peer)
			} else {
				fmt.Fprintf(w, `{"repositories":[%s]}`, repo)
			}
		case "GET /installation/repositories":
			if auth == "Bearer org-runners" && outside.Load() {
				fmt.Fprint(w, `{"repositories":[{"id":10,"full_name":"team/peer","private":true,"visibility":"private","owner":{"login":"team"}}]}`)
			} else {
				fmt.Fprintf(w, `{"repositories":[%s]}`, repo)
			}
		case "GET /repos/team/private/actions/runs":
			if auth != "Bearer poll-read" {
				t.Error("discovery requires the read-only installation token")
			}
			if r.URL.Query().Get("status") == "queued" {
				fmt.Fprint(w, `{"workflow_runs":[{"id":17}]}`)
			} else {
				fmt.Fprint(w, `{"workflow_runs":[]}`)
			}
		case "GET /repos/team/private/actions/runs/17/jobs":
			fmt.Fprint(w, `{"jobs":[{"id":41,"run_id":17,"status":"queued","labels":["runquill-17-1-ci"]}]}`)
		case "GET /repos/team/private":
			if auth != "Bearer repo-read" {
				t.Error("repository verification requires a repo-scoped read token")
			}
			fmt.Fprint(w, repo)
		case "GET /repos/team/private/actions/jobs/41":
			if auth != "Bearer repo-read" {
				t.Error("job lookup requires a repo-scoped read token")
			}
			fmt.Fprint(w, `{"status":"queued"}`)
		case "POST /orgs/team/actions/runners/generate-jitconfig":
			var body struct {
				Name   string
				Group  int64 `json:"runner_group_id"`
				Labels []string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Group != 12 ||
				!slices.Equal(body.Labels, []string{"runquill-17-1-ci"}) || !strings.HasPrefix(body.Name, "runquill-41-") ||
				auth != "Bearer org-runners" || public.Load() || restricted.Load() || excess.Load() || outside.Load() {
				t.Errorf("unexpected JIT request: %+v, %v", body, err)
			}
			registered.Add(1)
			fmt.Fprint(w, `{"runner":{"id":55},"encoded_jit_config":"test-config"}`)
		case "DELETE /orgs/team/actions/runners/55":
			if auth != "Bearer org-runners" {
				t.Error("runner cleanup requires the organization runner token")
			}
			removed.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)

	ctx := t.Context()
	q, err := openQueue(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	api := &github{client: mock.Client(), base: mock.URL, appID: "123", key: key, owner: "team",
		repositoryScope: scopeSelected, organization: true, runnerGroupID: 12}
	poll := &poller{github: api, queue: q, cache: make(map[string]pollPage)}
	if err := poll.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, err := q.jobs(ctx, "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("organization discovery returned %+v, %v", jobs, err)
	}
	j := jobs[0]
	if inst, err := waitForSetupInstallation(ctx, &strings.Builder{}, api); err != nil || inst.ID != 7 {
		t.Fatalf("organization group setup: %+v, %v", inst, err)
	}
	containers := map[string]string{}
	runtime := &testRuntime{hosts: []dockerHostSnapshot{{Name: "local", Containers: containers}}}
	sched := &scheduler{github: api, queue: q, runtime: runtime, limit: 1}
	now := time.Now()
	for _, failure := range []struct {
		flag *atomic.Bool
		want string
	}{
		{&public, "disallow public repositories"},
		{&restricted, "restricts workflows"},
		{&excess, "repositories outside the selected"},
		{&outside, "runner group 12 does not allow team/private"},
	} {
		failure.flag.Store(true)
		claimed, err := sched.launch(ctx, j, "local", "", now)
		failure.flag.Store(false)
		if claimed || err == nil || !strings.Contains(err.Error(), failure.want) || registered.Load() != 0 {
			t.Fatalf("group policy %q: claimed %v, registrations %d, error %v", failure.want, claimed, registered.Load(), err)
		}
	}
	if err := sched.tick(ctx, now); err != nil || len(containers) != 1 || registered.Load() != 1 {
		t.Fatalf("launch organization runner: containers=%v registrations=%d error=%v", containers, registered.Load(), err)
	}
	revoked.Store(true)
	for name := range containers {
		containers[name] = "exited"
	}
	if err := sched.tick(ctx, now.Add(time.Second)); err != nil || len(containers) != 0 || removed.Load() != 1 {
		t.Fatalf("clean organization runner after access revocation: containers=%v removals=%d error=%v", containers, removed.Load(), err)
	}
	if jobs, err := q.jobs(ctx, ""); err != nil || len(jobs) != 0 {
		t.Fatalf("unexpected organization jobs after cleanup: %+v, %v", jobs, err)
	}
}

func TestOrganizationWithoutGroupUsesRepositoryRunner(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var provisioned atomic.Bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /orgs/team/installation", "GET /repos/team/private/installation":
			fmt.Fprint(w, `{"id":7,"account":{"login":"team","type":"Organization"},"repository_selection":"all","permissions":{"actions":"read","administration":"write"}}`)
		case "POST /repos/team/private/actions/runners/generate-jitconfig":
			var body struct {
				Group int64 `json:"runner_group_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Group != 1 {
				t.Errorf("expected repository runner group 1, got %+v, %v", body, err)
			}
			provisioned.Store(true)
			fmt.Fprint(w, `{"runner":{"id":42},"encoded_jit_config":"test-config"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)
	api := &github{client: mock.Client(), base: mock.URL, appID: "123", key: key, owner: "team", organization: true}
	ctx := t.Context()
	inst, err := waitForSetupInstallation(ctx, &strings.Builder{}, api)
	if err != nil || inst.ID != 7 {
		t.Fatalf("organization installation: %+v, %v", inst, err)
	}
	j := job{repositoryID: 9, repository: "team/private", installationID: 8, route: "runquill-17-1-ci"}
	if err := api.checkRepositoryAccess(ctx, j); !errors.Is(err, errRepositoryOutsideInstallation) {
		t.Fatalf("different installation was accepted: %v", err)
	}
	j.installationID = 7
	if err := api.checkRepositoryAccess(ctx, j); err != nil {
		t.Fatal(err)
	}
	if id, _, err := api.provision(ctx, j, "runquill-test", "fixture"); err != nil || id != 42 || !provisioned.Load() {
		t.Fatalf("provision repository runner: id %d, error %v", id, err)
	}
}
