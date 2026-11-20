package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/yakisoba0728/Runquill/internal/workflow"
)

const (
	migrationWorkflowDir  = ".github/workflows"
	migrationBranchPrefix = "runquill/migrate-runs-on-"
	maxMigrationFiles     = 50
	// GitHub's contents API only inlines files up to 1 MB; smaller keeps previews bounded.
	maxMigrationFileSize = 512 << 10
	maxPullRequestBody   = 60_000
)

var (
	errMigrationPermissions  = errors.New("the GitHub App does not have migration permissions: grant Contents, Pull requests and Workflows read and write access in the App settings, then approve the new permissions on the installation")
	errMigrationStale        = errors.New("the workflow files changed since the preview; preview again before opening the pull request")
	errNothingToMigrate      = errors.New("no job matches the mapping, so there is nothing to change")
	errMigrationRepository   = errors.New("use OWNER/REPO for a repository owned by RUNQUILL_OWNER")
	errMigrationPublic       = errors.New("Runquill only runs jobs from private repositories")
	errMigrationInstallScope = errors.New("the GitHub App installation's repository access does not match the Runquill configuration")
	errMigrationTooMany      = fmt.Errorf("the repository has more than %d workflow files", maxMigrationFiles)
	workflowFileName         = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*\.ya?ml$`)
	migrationHash            = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type migrationBranchExistsError struct {
	Branch, PullRequest string
}

func (e *migrationBranchExistsError) Error() string {
	if e.PullRequest != "" {
		return fmt.Sprintf("the branch %s already exists with pull request %s", e.Branch, e.PullRequest)
	}
	return fmt.Sprintf("the branch %s already exists without a pull request; delete it in GitHub to try again", e.Branch)
}

type migrationFile struct {
	Path    string                `json:"path"`
	Changed bool                  `json:"changed"`
	Diff    string                `json:"diff,omitempty"`
	Refused string                `json:"refused,omitempty"`
	Jobs    []workflow.JobOutcome `json:"jobs"`
	blobSHA string
	output  []byte
}

type migrationPreview struct {
	Repository string          `json:"repository"`
	Base       string          `json:"base"`
	Head       string          `json:"head"`
	Hash       string          `json:"hash"`
	Files      []migrationFile `json:"files"`
}

type migrationPR struct {
	URL    string `json:"url"`
	Number int    `json:"number"`
	Branch string `json:"branch"`
}

type migrationRepo struct {
	api              *github
	name, token      string
	base, head       string
	pullRequestOwner string
}

// openMigrationRepo checks the repository is in scope and mints a token for it alone.
// The token comes from mintToken, never the scheduler's cache, and is dropped after the request.
func openMigrationRepo(ctx context.Context, api *github, repository string, write bool) (*migrationRepo, error) {
	name, valid := ownedRepositoryPath(repository, api.owner)
	if !valid {
		return nil, errMigrationRepository
	}
	jwt, err := api.jwt(time.Now())
	if err != nil {
		return nil, err
	}
	inst, err := api.installation(ctx, jwt)
	if err != nil {
		return nil, err
	}
	if _, err := api.repositoryInstallation(ctx, jwt, name, inst.ID); err != nil {
		return nil, err
	}
	if err := inst.checkRepositoryScope(api.repositoryScope); err != nil {
		return nil, fmt.Errorf("%w: %w", errMigrationInstallScope, err)
	}
	permissions := map[string]string{"contents": "read"}
	if write {
		permissions = migrationPermissions
		if len(missingMigrationPermissions(inst)) > 0 {
			return nil, errMigrationPermissions
		}
	} else if level := inst.Permissions["contents"]; level != "read" && level != "write" {
		return nil, errMigrationPermissions
	}
	owner, slug, _ := strings.Cut(name, "/")
	token, err := api.mintToken(ctx, jwt, inst.ID, map[string]any{"repositories": []string{slug}, "permissions": permissions})
	var apiErr *githubError
	if errors.As(err, &apiErr) && (apiErr.status == http.StatusUnprocessableEntity || apiErr.status == http.StatusForbidden && apiErr.retryAt.IsZero()) {
		return nil, errMigrationPermissions
	}
	if err != nil {
		return nil, err
	}
	repo := &migrationRepo{api: api, name: name, token: token.Token, pullRequestOwner: owner}
	var meta struct {
		Private       bool
		Visibility    string
		DefaultBranch string `json:"default_branch"`
		Owner         struct{ Login string }
	}
	if err := repo.get(ctx, "/repos/"+name, &meta); err != nil {
		return nil, err
	}
	if !meta.Private || meta.Visibility != "" && meta.Visibility != "private" || !strings.EqualFold(meta.Owner.Login, api.owner) {
		return nil, errMigrationPublic
	}
	if meta.DefaultBranch == "" || len(meta.DefaultBranch) > 255 {
		return nil, errors.New("GitHub did not return the repository's default branch")
	}
	var ref struct{ Object struct{ SHA string } }
	if err := repo.get(ctx, "/repos/"+name+"/git/ref/heads/"+escapeRef(meta.DefaultBranch), &ref); err != nil {
		return nil, err
	}
	if !workflow.IsCommitSHA(ref.Object.SHA) {
		return nil, errors.New("GitHub did not return the default branch head")
	}
	repo.base, repo.head = meta.DefaultBranch, ref.Object.SHA
	return repo, nil
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func (m *migrationRepo) get(ctx context.Context, path string, out any) error {
	return m.api.request(ctx, http.MethodGet, path, m.token, nil, out)
}

func (m *migrationRepo) workflowPath(name string) string {
	return "/repos/" + m.name + "/contents/" + migrationWorkflowDir + "/" + url.PathEscape(name)
}

// workflows reads every workflow file at the pinned head commit.
func (m *migrationRepo) workflows(ctx context.Context) ([]migrationFile, map[string][]byte, error) {
	var entries []struct {
		Type, Name, Path, SHA string
		Size                  int
	}
	err := m.get(ctx, "/repos/"+m.name+"/contents/"+migrationWorkflowDir+"?ref="+m.head, &entries)
	if githubNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var files []migrationFile
	sources := make(map[string][]byte)
	for _, entry := range entries {
		if entry.Type != "file" || !workflowFileName.MatchString(entry.Name) || entry.Path != migrationWorkflowDir+"/"+entry.Name {
			continue
		}
		if len(files) == maxMigrationFiles {
			return nil, nil, errMigrationTooMany
		}
		file := migrationFile{Path: entry.Path, blobSHA: entry.SHA}
		if entry.Size > maxMigrationFileSize {
			file.Refused = "the file is too large to rewrite"
			files = append(files, file)
			continue
		}
		var content struct {
			Type, Path, SHA, Encoding, Content string
		}
		if err := m.get(ctx, m.workflowPath(entry.Name)+"?ref="+m.head, &content); err != nil {
			return nil, nil, err
		}
		data, decodeErr := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
		if content.Type != "file" || content.Path != entry.Path || content.Encoding != "base64" || decodeErr != nil || len(data) > maxMigrationFileSize {
			file.Refused = "GitHub did not return the file contents"
			files = append(files, file)
			continue
		}
		file.blobSHA = content.SHA
		sources[file.Path] = data
		files = append(files, file)
	}
	return files, sources, nil
}

func (m *migrationRepo) preview(ctx context.Context, mapping []workflow.Mapping) (migrationPreview, error) {
	if _, err := workflow.ValidateMappings(mapping); err != nil {
		return migrationPreview{}, err
	}
	files, sources, err := m.workflows(ctx)
	if err != nil {
		return migrationPreview{}, err
	}
	// The hash covers what the pull request would contain, not the head commit, so
	// unrelated commits to the default branch neither stale a preview nor fork a second PR.
	// Hosted Ubuntu version labels keep their OS when the manager's runner image catalogue has it.
	images, err := buildRunnerImages(os.Getenv(runnerImagesEnv), releaseImage, version, "")
	if err != nil {
		images = nil
	}
	digest := sha256.New()
	canonical, _ := json.Marshal(mapping)
	fmt.Fprintf(digest, "runquill-migrate-v1\x00%s\x00%s\x00%s\x00", m.name, m.base, canonical)
	for i := range files {
		file := &files[i]
		if src, ok := sources[file.Path]; ok {
			res, err := workflow.Rewrite(src, mapping, images.hostedRunnerOS)
			if err != nil {
				return migrationPreview{}, err
			}
			file.Refused, file.Changed, file.output = res.Refused, res.Changed(), res.Output
			file.Diff = workflow.UnifiedDiff(file.Path, src, res.Output)
			for _, job := range res.Jobs {
				job.File = file.Path
				file.Jobs = append(file.Jobs, job)
			}
		}
		out := sha256.Sum256(file.output)
		fmt.Fprintf(digest, "%s\x00%s\x00%t\x00%x\x00", file.Path, file.blobSHA, file.Changed, out)
	}
	return migrationPreview{Repository: m.name, Base: m.base, Head: m.head,
		Hash: hex.EncodeToString(digest.Sum(nil)), Files: files}, nil
}

func previewMigration(ctx context.Context, api *github, repository string, mapping []workflow.Mapping) (migrationPreview, error) {
	if _, err := workflow.ValidateMappings(mapping); err != nil {
		return migrationPreview{}, err
	}
	repo, err := openMigrationRepo(ctx, api, repository, false)
	if err != nil {
		return migrationPreview{}, err
	}
	return repo.preview(ctx, mapping)
}

// openMigration recomputes the preview with a write token and opens one pull request
// only if it still matches the hash the administrator reviewed.
func openMigration(ctx context.Context, api *github, repository string, mapping []workflow.Mapping, hash string) (migrationPR, error) {
	if _, err := workflow.ValidateMappings(mapping); err != nil {
		return migrationPR{}, err
	}
	if !migrationHash.MatchString(hash) {
		return migrationPR{}, errMigrationStale
	}
	repo, err := openMigrationRepo(ctx, api, repository, true)
	if err != nil {
		return migrationPR{}, err
	}
	preview, err := repo.preview(ctx, mapping)
	if err != nil {
		return migrationPR{}, err
	}
	if preview.Hash != hash {
		return migrationPR{}, errMigrationStale
	}
	var changed []migrationFile
	for _, file := range preview.Files {
		if file.Changed {
			changed = append(changed, file)
		}
	}
	if len(changed) == 0 {
		return migrationPR{}, errNothingToMigrate
	}
	branch := migrationBranchPrefix + hash[:12]
	if branch == repo.base {
		return migrationPR{}, errors.New("the migration branch would be the default branch")
	}
	refPath := "/repos/" + repo.name + "/git/ref/heads/" + escapeRef(branch)
	err = repo.get(ctx, refPath, &struct{}{})
	if err == nil {
		return migrationPR{}, repo.branchExists(ctx, branch)
	}
	if !githubNotFound(err) {
		return migrationPR{}, err
	}
	err = repo.api.request(ctx, http.MethodPost, "/repos/"+repo.name+"/git/refs", repo.token,
		map[string]string{"ref": "refs/heads/" + branch, "sha": repo.head}, nil)
	var apiErr *githubError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusUnprocessableEntity {
		return migrationPR{}, repo.branchExists(ctx, branch)
	}
	if err != nil {
		return migrationPR{}, err
	}
	tip := repo.head
	for _, file := range changed {
		dir, name := path.Split(file.Path)
		if dir != migrationWorkflowDir+"/" || !workflowFileName.MatchString(name) {
			return migrationPR{}, repo.abandon(ctx, branch, tip, errors.New("refusing to write outside .github/workflows"))
		}
		var commit struct{ Commit struct{ SHA string } }
		err := repo.api.request(ctx, http.MethodPut, repo.workflowPath(name), repo.token, map[string]string{
			"message": "Route " + name + " jobs to Runquill",
			"content": base64.StdEncoding.EncodeToString(file.output),
			"sha":     file.blobSHA,
			"branch":  branch,
		}, &commit)
		if err != nil {
			return migrationPR{}, repo.abandon(ctx, branch, tip, fmt.Errorf("commit %s: %w", file.Path, err))
		}
		tip = commit.Commit.SHA
	}
	var pr struct {
		Number  int
		HTMLURL string `json:"html_url"`
	}
	err = repo.api.request(ctx, http.MethodPost, "/repos/"+repo.name+"/pulls", repo.token, map[string]any{
		"title": "Route GitHub Actions jobs to Runquill",
		"head":  branch,
		"base":  repo.base,
		"body":  migrationPRBody(preview),
	}, &pr)
	if err != nil {
		return migrationPR{}, repo.abandon(ctx, branch, tip, fmt.Errorf("open the pull request: %w", err))
	}
	return migrationPR{URL: pr.HTMLURL, Number: pr.Number, Branch: branch}, nil
}

func (m *migrationRepo) branchExists(ctx context.Context, branch string) error {
	var pulls []struct {
		HTMLURL string `json:"html_url"`
	}
	query := url.Values{"head": {m.pullRequestOwner + ":" + branch}, "state": {"all"}, "per_page": {"1"}}
	if err := m.get(ctx, "/repos/"+m.name+"/pulls?"+query.Encode(), &pulls); err != nil {
		return err
	}
	exists := &migrationBranchExistsError{Branch: branch}
	if len(pulls) > 0 {
		exists.PullRequest = pulls[0].HTMLURL
	}
	return exists
}

// abandon removes a branch this call created when it still points at this call's
// last commit, so a retry is not blocked by a half-finished branch.
func (m *migrationRepo) abandon(ctx context.Context, branch, tip string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	refPath := "/repos/" + m.name + "/git/ref/heads/" + escapeRef(branch)
	var ref struct{ Object struct{ SHA string } }
	if m.get(ctx, refPath, &ref) != nil || ref.Object.SHA != tip {
		return fmt.Errorf("%w; the branch %s was left in place", cause, branch)
	}
	if err := m.api.request(ctx, http.MethodDelete, "/repos/"+m.name+"/git/refs/heads/"+escapeRef(branch), m.token, nil, nil); err != nil {
		return fmt.Errorf("%w; the branch %s was left in place", cause, branch)
	}
	return cause
}

func migrationPRBody(preview migrationPreview) string {
	var changed, left strings.Builder
	code := func(s string) string { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }
	for _, file := range preview.Files {
		if file.Refused != "" {
			fmt.Fprintf(&left, "- %s: left unchanged because %s\n", code(file.Path), file.Refused)
		}
		for _, job := range file.Jobs {
			if job.Changed {
				fmt.Fprintf(&changed, "- %s job %s (line %d): %s → %s\n", code(file.Path), code(job.Job), job.Line,
					code(strings.Join(job.Labels, ", ")), code(job.RunsOn))
			} else {
				fmt.Fprintf(&left, "- %s job %s (line %d): %s\n", code(file.Path), code(job.Job), job.Line, job.Reason)
			}
		}
	}
	body := "Runquill rewrote `runs-on` for the jobs below so they run on your Runquill runners. No other line changed.\n\n" +
		"### Changed jobs\n\n" + changed.String() + "\n### Jobs left unchanged\n\n"
	if left.Len() == 0 {
		left.WriteString("None.\n")
	}
	body += left.String() + "\nReview the diff, then merge when the Runquill pools are ready. Preview hash: " + code(preview.Hash) + "\n"
	if len(body) > maxPullRequestBody {
		cut := body[:maxPullRequestBody]
		body = cut[:strings.LastIndexByte(cut, '\n')+1] + "\n…the list was shortened; see the diff for every change.\n"
	}
	return body
}
