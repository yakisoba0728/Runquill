package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

const migrationHead = "0123456789abcdef0123456789abcdef01234567"

// fakeMigrationGitHub is a stateful stand-in for the GitHub endpoints migration uses.
// Tests never reach the real GitHub API.
type fakeMigrationGitHub struct {
	t           *testing.T
	mu          sync.Mutex
	permissions map[string]string
	mintStatus  int
	private     bool
	outside     bool
	failPut     bool
	files       map[string]string // workflow name -> contents at the head commit
	branches    map[string]string // branch -> tip SHA
	pulls       map[string]string // branch -> PR URL
	tokenBodies []map[string]any
	puts        []map[string]string
	deleted     []string
	commits     int
}

func newFakeMigrationGitHub(t *testing.T) (*fakeMigrationGitHub, *github) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMigrationGitHub{t: t, private: true,
		permissions: map[string]string{"actions": "read", "administration": "write", "contents": "write", "pull_requests": "write", "workflows": "write"},
		files: map[string]string{
			"ci.yml":    "on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n  win:\n    runs-on: windows-latest\n",
			"lint.yaml": "jobs:\n  lint:\n    runs-on: ${{ matrix.os }}\n",
		},
		branches: map[string]string{"main": migrationHead},
		pulls:    map[string]string{},
	}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return f, &github{client: server.Client(), base: server.URL, key: key, appID: "1", owner: "owner"}
}

func blobSHA(content string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(content)))
}

func (f *fakeMigrationGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.t
	auth := r.Header.Get("Authorization")
	repoToken := auth == "Bearer read-token" || auth == "Bearer write-token"
	install := func() {
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "account": map[string]any{"login": "owner", "type": "User"},
			"repository_selection": "all", "permissions": f.permissions})
	}
	route := r.Method + " " + r.URL.Path
	switch {
	case route == "GET /users/owner/installation":
		install()
	case route == "GET /repos/owner/repo/installation":
		if f.outside {
			http.NotFound(w, r)
			return
		}
		install()
	case route == "POST /app/installations/7/access_tokens":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.tokenBodies = append(f.tokenBodies, body)
		if f.mintStatus != 0 {
			w.WriteHeader(f.mintStatus)
			return
		}
		token := "read-token"
		if perms, _ := body["permissions"].(map[string]any); perms["contents"] == "write" {
			token = "write-token"
		}
		fmt.Fprintf(w, `{"token":%q}`, token)
	case !repoToken:
		t.Errorf("%s used token %q", route, auth)
		w.WriteHeader(http.StatusUnauthorized)
	case route == "GET /repos/owner/repo":
		fmt.Fprintf(w, `{"private":%t,"visibility":%q,"default_branch":"main","owner":{"login":"owner"}}`,
			f.private, map[bool]string{true: "private", false: "public"}[f.private])
	case strings.HasPrefix(route, "GET /repos/owner/repo/git/ref/heads/"):
		tip, ok := f.branches[strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/git/ref/heads/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"object":{"sha":%q}}`, tip)
	case route == "GET /repos/owner/repo/contents/.github/workflows":
		if r.URL.Query().Get("ref") != f.branches["main"] {
			t.Errorf("listing at ref %q", r.URL.Query().Get("ref"))
		}
		entries := []map[string]any{
			{"type": "dir", "name": "nested", "path": ".github/workflows/nested"},
			{"type": "file", "name": "README.md", "path": ".github/workflows/README.md", "sha": "x"},
		}
		for _, name := range slices.Sorted(maps.Keys(f.files)) {
			entries = append(entries, map[string]any{"type": "file", "name": name, "path": ".github/workflows/" + name,
				"sha": blobSHA(f.files[name]), "size": len(f.files[name])})
		}
		json.NewEncoder(w).Encode(entries)
	case strings.HasPrefix(route, "GET /repos/owner/repo/contents/.github/workflows/"):
		name := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/contents/.github/workflows/")
		content, ok := f.files[name]
		if !ok || r.URL.Query().Get("ref") != f.branches["main"] {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"type": "file", "path": ".github/workflows/" + name, "sha": blobSHA(content),
			"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
	case route == "GET /repos/owner/repo/pulls":
		if r.URL.Query().Get("state") != "all" {
			t.Errorf("pull lookup query %v", r.URL.Query())
		}
		branch := strings.TrimPrefix(r.URL.Query().Get("head"), "owner:")
		if url, ok := f.pulls[branch]; ok {
			fmt.Fprintf(w, `[{"html_url":%q}]`, url)
			return
		}
		fmt.Fprint(w, `[]`)
	case route == "POST /repos/owner/repo/git/refs":
		var body struct{ Ref, SHA string }
		json.NewDecoder(r.Body).Decode(&body)
		branch, ok := strings.CutPrefix(body.Ref, "refs/heads/")
		if !ok || !strings.HasPrefix(branch, migrationBranchPrefix) || body.SHA != f.branches["main"] {
			t.Errorf("unexpected branch creation %+v", body)
		}
		if _, exists := f.branches[branch]; exists {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		f.branches[branch] = body.SHA
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	case strings.HasPrefix(route, "PUT /repos/owner/repo/contents/"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.puts = append(f.puts, body)
		name, inWorkflows := strings.CutPrefix(r.URL.Path, "/repos/owner/repo/contents/.github/workflows/")
		_, branchExists := f.branches[body["branch"]]
		if auth != "Bearer write-token" || !inWorkflows || body["branch"] == "main" || !branchExists || body["sha"] != blobSHA(f.files[name]) {
			t.Errorf("unsafe contents write %s %+v", r.URL.Path, body)
			w.WriteHeader(http.StatusConflict)
			return
		}
		if f.failPut {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.commits++
		sha := fmt.Sprintf("%040x", f.commits)
		f.branches[body["branch"]] = sha
		fmt.Fprintf(w, `{"commit":{"sha":%q}}`, sha)
	case route == "POST /repos/owner/repo/pulls":
		var body struct{ Title, Head, Base, Body string }
		json.NewDecoder(r.Body).Decode(&body)
		if body.Base != "main" || !strings.HasPrefix(body.Head, migrationBranchPrefix) {
			t.Errorf("unexpected pull request %+v", body)
		}
		f.pulls[body.Head] = "https://github.com/owner/repo/pull/5"
		f.puts = append(f.puts, map[string]string{"pr_body": body.Body})
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"number":5,"html_url":"https://github.com/owner/repo/pull/5"}`)
	case strings.HasPrefix(route, "DELETE /repos/owner/repo/git/refs/heads/"):
		branch := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/git/refs/heads/")
		if branch == "main" {
			t.Errorf("deleted the default branch")
		}
		delete(f.branches, branch)
		f.deleted = append(f.deleted, branch)
		w.WriteHeader(http.StatusNoContent)
	default:
		t.Errorf("unexpected GitHub request %s", route)
		http.NotFound(w, r)
	}
}

var hostedMapping = []runsOnMapping{{From: []string{"ubuntu-latest"}}}

func TestMigrationPreview(t *testing.T) {
	f, api := newFakeMigrationGitHub(t)
	preview, err := previewMigration(t.Context(), api, "Owner/Repo", hostedMapping)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Repository != "owner/repo" || preview.Base != "main" || preview.Head != migrationHead || len(preview.Files) != 2 {
		t.Fatalf("preview: %+v", preview)
	}
	ci, lint := preview.Files[0], preview.Files[1]
	if !ci.Changed || !strings.Contains(ci.Diff, "+    runs-on: "+routeBuild) || len(ci.Jobs) != 2 ||
		ci.Jobs[1].Code != outcomeUnmapped || lint.Changed || lint.Jobs[0].Code != outcomeExpression || lint.Jobs[0].File != ".github/workflows/lint.yaml" {
		t.Fatalf("files: %+v", preview.Files)
	}
	want := map[string]any{"repositories": []any{"repo"}, "permissions": map[string]any{"contents": "read"}}
	if len(f.tokenBodies) != 1 || !reflect.DeepEqual(f.tokenBodies[0], want) {
		t.Fatalf("preview token request: %+v", f.tokenBodies)
	}
	// An unrelated commit moves the head but leaves the workflow files and the hash alone.
	f.branches["main"] = strings.Repeat("f", 40)
	again, err := previewMigration(t.Context(), api, "owner/repo", hostedMapping)
	if err != nil || again.Hash != preview.Hash || again.Head == preview.Head || !migrationHash.MatchString(preview.Hash) {
		t.Fatalf("preview hash is not stable: %s %s %v", preview.Hash, again.Hash, err)
	}
	if len(api.tokens) != 0 {
		t.Fatal("a migration token entered the scheduler's token cache")
	}
	f.files["lint.yaml"] += "# edited\n"
	changed, err := previewMigration(t.Context(), api, "owner/repo", hostedMapping)
	if err != nil || changed.Hash == preview.Hash {
		t.Fatal("editing a workflow did not change the preview hash")
	}
	other, err := previewMigration(t.Context(), api, "owner/repo", []runsOnMapping{{From: []string{"ubuntu-latest"}, Pool: "big"}})
	if err != nil || other.Hash == changed.Hash {
		t.Fatal("a different mapping kept the preview hash")
	}
}

func TestMigrationRefusals(t *testing.T) {
	f, api := newFakeMigrationGitHub(t)
	if _, err := previewMigration(t.Context(), api, "someone-else/repo", hostedMapping); !errors.Is(err, errMigrationRepository) {
		t.Fatalf("other owner: %v", err)
	}
	if _, err := previewMigration(t.Context(), api, "owner/../x", hostedMapping); !errors.Is(err, errMigrationRepository) {
		t.Fatalf("path traversal: %v", err)
	}
	if _, err := previewMigration(t.Context(), api, "owner/repo", nil); err == nil {
		t.Fatal("empty mapping accepted")
	}
	if len(f.tokenBodies) != 0 {
		t.Fatal("invalid requests minted a token")
	}
	f.outside = true
	if _, err := previewMigration(t.Context(), api, "owner/repo", hostedMapping); !errors.Is(err, errRepositoryOutsideInstallation) {
		t.Fatalf("outside installation: %v", err)
	}
	f.outside, api.repositoryScope = false, scopeSelected
	if _, err := previewMigration(t.Context(), api, "owner/repo", hostedMapping); !errors.Is(err, errMigrationInstallScope) {
		t.Fatalf("installation scope mismatch: %v", err)
	}
	api.repositoryScope, f.private = "", false
	if _, err := previewMigration(t.Context(), api, "owner/repo", hostedMapping); !errors.Is(err, errMigrationPublic) {
		t.Fatalf("public repository: %v", err)
	}
	f.private = true
	delete(f.permissions, "contents")
	before := len(f.tokenBodies)
	if _, err := previewMigration(t.Context(), api, "owner/repo", hostedMapping); !errors.Is(err, errMigrationPermissions) {
		t.Fatalf("no contents permission: %v", err)
	}
	if len(f.tokenBodies) != before {
		t.Fatal("minted a token without the permission")
	}
	f.permissions["contents"] = "read"
	if _, err := openMigration(t.Context(), api, "owner/repo", hostedMapping, strings.Repeat("a", 64)); !errors.Is(err, errMigrationPermissions) ||
		!strings.Contains(err.Error(), "Contents, Pull requests and Workflows") {
		t.Fatalf("read-only installation: %v", err)
	}
	if len(f.tokenBodies) != before {
		t.Fatal("open minted a token without the permissions")
	}
	f.permissions["contents"] = "write"
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusForbidden} {
		f.mintStatus = status
		if _, err := openMigration(t.Context(), api, "owner/repo", hostedMapping, strings.Repeat("a", 64)); !errors.Is(err, errMigrationPermissions) {
			t.Fatalf("mint HTTP %d: %v", status, err)
		}
	}
	if len(f.puts) != 0 || len(f.branches) != 1 {
		t.Fatal("a refused migration wrote to GitHub")
	}
}

func TestOpenMigration(t *testing.T) {
	f, api := newFakeMigrationGitHub(t)
	preview, err := previewMigration(t.Context(), api, "owner/repo", hostedMapping)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openMigration(t.Context(), api, "owner/repo", hostedMapping, strings.Repeat("0", 64)); !errors.Is(err, errMigrationStale) {
		t.Fatalf("wrong hash: %v", err)
	}
	original := f.files["ci.yml"]
	f.files["ci.yml"] = original + "# pushed after the preview\n"
	if _, err := openMigration(t.Context(), api, "owner/repo", hostedMapping, preview.Hash); !errors.Is(err, errMigrationStale) || len(f.branches) != 1 {
		t.Fatalf("edited after preview: %v %v", err, f.branches)
	}
	f.files["ci.yml"] = original
	pr, err := openMigration(t.Context(), api, "owner/repo", hostedMapping, preview.Hash)
	if err != nil {
		t.Fatal(err)
	}
	branch := migrationBranchPrefix + preview.Hash[:12]
	if pr.URL != "https://github.com/owner/repo/pull/5" || pr.Branch != branch || f.branches["main"] != migrationHead {
		t.Fatalf("pull request %+v, branches %v", pr, f.branches)
	}
	writeToken := f.tokenBodies[len(f.tokenBodies)-1]
	want := map[string]any{"repositories": []any{"repo"}, "permissions": map[string]any{"contents": "write", "pull_requests": "write", "workflows": "write"}}
	if !reflect.DeepEqual(writeToken, want) || len(api.tokens) != 0 {
		t.Fatalf("write token request: %+v (cached %d)", writeToken, len(api.tokens))
	}
	if len(f.puts) != 2 {
		t.Fatalf("expected one commit and one pull request, got %+v", f.puts)
	}
	put := f.puts[0]
	content, _ := base64.StdEncoding.DecodeString(put["content"])
	if put["branch"] != branch || put["sha"] != blobSHA(original) ||
		string(content) != strings.Replace(original, "runs-on: ubuntu-latest", "runs-on: "+routeBuild, 1) {
		t.Fatalf("commit: %+v %q", put, content)
	}
	body := f.puts[1]["pr_body"]
	changed, left, _ := strings.Cut(body, "### Jobs left unchanged")
	if !strings.Contains(changed, "job `build` (line 4)") || strings.Contains(changed, "`win`") ||
		!strings.Contains(left, "job `win` (line 6): "+reasonUnmapped) || !strings.Contains(left, "job `lint` (line 3): "+reasonExpression) {
		t.Fatalf("pull request body:\n%s", body)
	}

	// The same preview again finds the branch and reports the existing pull request.
	puts := len(f.puts)
	_, err = openMigration(t.Context(), api, "owner/repo", hostedMapping, preview.Hash)
	var exists *migrationBranchExistsError
	if !errors.As(err, &exists) || exists.PullRequest != pr.URL || len(f.puts) != puts {
		t.Fatalf("branch exists: %v", err)
	}
	delete(f.pulls, branch)
	if _, err := openMigration(t.Context(), api, "owner/repo", hostedMapping, preview.Hash); !errors.As(err, &exists) || exists.PullRequest != "" {
		t.Fatalf("branch without pull request: %v", err)
	}
}

func TestOpenMigrationNothingAndCleanup(t *testing.T) {
	f, api := newFakeMigrationGitHub(t)
	mapping := []runsOnMapping{{From: []string{"macos-latest"}}}
	preview, err := previewMigration(t.Context(), api, "owner/repo", mapping)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openMigration(t.Context(), api, "owner/repo", mapping, preview.Hash); !errors.Is(err, errNothingToMigrate) || len(f.branches) != 1 {
		t.Fatalf("nothing to migrate: %v %v", err, f.branches)
	}
	preview, err = previewMigration(t.Context(), api, "owner/repo", hostedMapping)
	if err != nil {
		t.Fatal(err)
	}
	f.failPut = true
	if _, err := openMigration(t.Context(), api, "owner/repo", hostedMapping, preview.Hash); err == nil || len(f.deleted) != 1 || len(f.branches) != 1 {
		t.Fatalf("failed commit: %v, deleted %v, branches %v", err, f.deleted, f.branches)
	}
}

func TestMigrationPRBodyIsBounded(t *testing.T) {
	jobs := make([]jobOutcome, 3000)
	for i := range jobs {
		jobs[i] = jobOutcome{Job: fmt.Sprintf("job-%d", i), Code: outcomeUnmapped, Reason: reasonUnmapped}
	}
	body := migrationPRBody(migrationPreview{Files: []migrationFile{{Path: ".github/workflows/x.yml", Jobs: jobs}}})
	if len(body) > maxPullRequestBody+200 || !strings.Contains(body, "shortened") {
		t.Fatalf("body length %d", len(body))
	}
}
