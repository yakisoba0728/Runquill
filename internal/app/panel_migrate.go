package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/yakisoba0728/Runquill/internal/workflow"
)

// Previews read up to 50 files and opening adds a commit per changed file, which
// takes longer than the panel's 10s write timeout allows.
const panelMigrateTimeout = 90 * time.Second

const panelMigrateStatusTimeout = 15 * time.Second

type migrateDeps struct {
	api *github
}

type migrateFailure struct {
	Error       string `json:"error"`
	Code        string `json:"code"`
	PullRequest string `json:"pull_request,omitempty"`
}

func registerMigrateRoutes(mux *http.ServeMux, a *panelAccess, deps migrateDeps) {
	mux.HandleFunc("GET /api/migrate/status", a.require("admin", deps.status))
	mux.HandleFunc("POST /api/migrate/preview", a.require("admin", deps.preview))
	mux.HandleFunc("POST /api/migrate/open", a.require("admin", func(w http.ResponseWriter, r *http.Request, user panelIdentity) {
		deps.open(w, r, user, a)
	}))
}

func writeMigrateFailure(w http.ResponseWriter, err error) {
	var exists *migrationBranchExistsError
	switch {
	case errors.Is(err, errMigrationPermissions):
		panelJSON(w, http.StatusConflict, migrateFailure{Error: "The GitHub App does not have migration permissions. Grant Contents, Pull requests and Workflows read and write access, then approve them on the installation.", Code: "permissions"})
	case errors.Is(err, errMigrationStale):
		panelJSON(w, http.StatusConflict, migrateFailure{Error: "The workflow files changed since the preview. Preview again before opening the pull request.", Code: "stale"})
	case errors.Is(err, errNothingToMigrate):
		panelJSON(w, http.StatusConflict, migrateFailure{Error: "No job matches the mapping, so there is nothing to change.", Code: "nothing"})
	case errors.As(err, &exists):
		panelJSON(w, http.StatusConflict, migrateFailure{Error: "A migration branch for these changes already exists.", Code: "branch_exists", PullRequest: exists.PullRequest})
	case errors.Is(err, errMigrationRepository):
		panelJSON(w, http.StatusBadRequest, migrateFailure{Error: "Use OWNER/REPO for a repository owned by the configured account.", Code: "repository"})
	case errors.Is(err, errRepositoryOutsideInstallation):
		panelJSON(w, http.StatusForbidden, migrateFailure{Error: "The repository is outside the GitHub App installation.", Code: "scope"})
	case errors.Is(err, errMigrationPublic):
		panelJSON(w, http.StatusBadRequest, migrateFailure{Error: "Workflow migration only changes private repositories.", Code: "public"})
	case errors.Is(err, errMigrationInstallScope):
		panelJSON(w, http.StatusConflict, migrateFailure{Error: "The GitHub App installation's repository access does not match the Runquill configuration.", Code: "installation_scope"})
	case errors.Is(err, errMigrationTooMany):
		panelJSON(w, http.StatusBadRequest, migrateFailure{Error: "The repository has more than 50 workflow files.", Code: "too_many"})
	default:
		slog.Warn("workflow migration failed", "error", err)
		panelJSON(w, http.StatusBadGateway, migrateFailure{Error: "GitHub did not complete the request. Try again shortly.", Code: "github"})
	}
}

// migrateContext serializes migrations with other panel operations and allows them
// more time than the panel's default write timeout.
func migrateContext(w http.ResponseWriter, r *http.Request) (context.Context, func(), bool) {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(panelMigrateTimeout + 5*time.Second))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		http.Error(w, "Could not prepare the request.", http.StatusServiceUnavailable)
		return nil, nil, false
	}
	return panelOperationContext(w, r, panelMigrateTimeout)
}

// client returns the GitHub App client. Without one in deps it reads the manager's
// configuration like runquill diagnose does, so the panel can wire the routes
// without threading the scheduler's client (and its token cache) through.
func (deps migrateDeps) client(w http.ResponseWriter) (*github, bool) {
	if deps.api != nil {
		return deps.api, true
	}
	cfg, err := loadConfig()
	var api *github
	if err == nil {
		api, err = cfg.github()
	}
	if err != nil {
		http.Error(w, "GitHub is not configured.", http.StatusServiceUnavailable)
		return nil, false
	}
	return api, true
}

func (deps migrateDeps) status(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
	api, ok := deps.client(w)
	if !ok {
		return
	}
	// The permission check may outlast the panel's 10s write timeout.
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(panelMigrateStatusTimeout + 5*time.Second))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		http.Error(w, "Could not prepare the request.", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), panelMigrateStatusTimeout)
	defer cancel()
	missing, err := api.migrationPermissionStatus(ctx)
	if err != nil {
		writeMigrateFailure(w, err)
		return
	}
	panelJSON(w, http.StatusOK, struct {
		Ready    bool     `json:"ready"`
		Required []string `json:"required"`
		Missing  []string `json:"missing"`
	}{len(missing) == 0, slices.Sorted(maps.Keys(migrationPermissions)), append([]string{}, missing...)})
}

func (deps migrateDeps) preview(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
	var input struct {
		Repository string             `json:"repository"`
		Mapping    []workflow.Mapping `json:"mapping"`
	}
	if !decodePanelJSONLimit(w, r, &input, 16<<10) {
		return
	}
	if _, err := workflow.ValidateMappings(input.Mapping); err != nil {
		panelJSON(w, http.StatusBadRequest, migrateFailure{Error: err.Error(), Code: "mapping"})
		return
	}
	api, ok := deps.client(w)
	if !ok {
		return
	}
	ctx, done, ok := migrateContext(w, r)
	if !ok {
		return
	}
	defer done()
	preview, err := previewMigration(ctx, api, input.Repository, input.Mapping)
	if err != nil {
		writeMigrateFailure(w, err)
		return
	}
	panelJSON(w, http.StatusOK, preview)
}

func (deps migrateDeps) open(w http.ResponseWriter, r *http.Request, user panelIdentity, a *panelAccess) {
	if user.Scope != "" {
		http.Error(w, "Open migration pull requests from a signed-in panel session.", http.StatusForbidden)
		return
	}
	var input struct {
		Repository string             `json:"repository"`
		Mapping    []workflow.Mapping `json:"mapping"`
		Hash       string             `json:"hash"`
	}
	if !decodePanelJSONLimit(w, r, &input, 16<<10) {
		return
	}
	if _, err := workflow.ValidateMappings(input.Mapping); err != nil {
		panelJSON(w, http.StatusBadRequest, migrateFailure{Error: err.Error(), Code: "mapping"})
		return
	}
	api, ok := deps.client(w)
	if !ok {
		return
	}
	name, valid := ownedRepositoryPath(input.Repository, api.owner)
	if !valid {
		writeMigrateFailure(w, errMigrationRepository)
		return
	}
	if !migrationHash.MatchString(input.Hash) {
		panelJSON(w, http.StatusBadRequest, migrateFailure{Error: "Preview the changes and send the preview hash.", Code: "hash"})
		return
	}
	ctx, done, ok := migrateContext(w, r)
	if !ok {
		return
	}
	defer done()
	// Record the intent first: this also confirms the session is still an active
	// administrator before anything is written to GitHub. No transaction stays open
	// across the GitHub calls.
	if err := a.change(ctx, user, "migrate.request", name, func(*sql.Tx) error { return nil }); err != nil {
		if errors.Is(err, errPanelUnauthorized) {
			http.Error(w, "Sign in to Runquill.", http.StatusUnauthorized)
		} else {
			http.Error(w, "Could not record the request.", http.StatusServiceUnavailable)
		}
		return
	}
	// A closed tab must not cut the writes short: the cleanup path could then delete
	// the branch of a pull request GitHub already created.
	work, cancelWork := context.WithTimeout(context.WithoutCancel(ctx), panelMigrateTimeout)
	defer cancelWork()
	pr, err := openMigration(work, api, name, input.Mapping, input.Hash)
	// The outcome is recorded even if the browser has gone away.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	action, target := "migrate.open", name+" "+pr.URL
	if err != nil {
		action, target = "migrate.failed", name
	}
	if auditErr := a.recordAudit(auditCtx, user, action, target); auditErr != nil {
		slog.Error("record workflow migration audit entry", "error", auditErr, "repository", name)
	}
	if err != nil {
		writeMigrateFailure(w, err)
		return
	}
	panelJSON(w, http.StatusCreated, pr)
}

func (a *panelAccess) recordAudit(ctx context.Context, user panelIdentity, action, target string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := auditPanel(ctx, tx, user.GitHubID, action, target); err != nil {
		return err
	}
	return tx.Commit()
}
