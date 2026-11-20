package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

var errGitHubUserNotFound = errors.New("No GitHub user has this login.")

// lookupGitHubUser resolves a login with GitHub's public, read-only users API.
// It runs before any database transaction: the queue has one connection.
func (a *panelAccess) lookupGitHubUser(ctx context.Context, login string) (int64, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+"/users/"+login, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Runquill")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	resp, err := a.oauth.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, "", errGitHubUserNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	var reply struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&reply); err != nil {
		return 0, "", err
	}
	if reply.Type != "" && reply.Type != "User" {
		// Organizations and bots cannot sign in to the panel.
		return 0, "", errGitHubUserNotFound
	}
	if reply.ID <= 0 || !githubLogin.MatchString(reply.Login) {
		return 0, "", fmt.Errorf("GitHub returned an invalid identity for %q", login)
	}
	return reply.ID, reply.Login, nil
}

// addTeamMember enrolls a teammate by GitHub login and stores the resolved ID.
func (a *panelAccess) addTeamMember(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	var input struct {
		Login string `json:"login"`
		Role  string `json:"role"`
	}
	if !decodePanelJSON(w, r, &input) {
		return
	}
	login := strings.TrimPrefix(strings.TrimSpace(input.Login), "@")
	if !githubLogin.MatchString(login) {
		http.Error(w, "Enter a GitHub login such as octocat.", http.StatusBadRequest)
		return
	}
	switch input.Role {
	case "viewer", "operator", "admin":
	default:
		http.Error(w, "Choose viewer, operator, or admin.", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	id, resolved, err := a.lookupGitHubUser(ctx, login)
	if errors.Is(err, errGitHubUserNotFound) {
		http.Error(w, "No GitHub user has this login.", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Could not look up the GitHub account. Try again or enter the numeric ID.", http.StatusBadGateway)
		return
	}
	action := "user.set." + input.Role
	err = a.change(ctx, user, action, strconv.FormatInt(id, 10), func(tx *sql.Tx) error {
		if input.Role != "admin" {
			var isLastAdmin bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS (
                SELECT 1 FROM panel_users WHERE github_id = ? AND role = 'admin' AND disabled = 0
                AND NOT EXISTS (SELECT 1 FROM panel_users WHERE github_id != ? AND role = 'admin' AND disabled = 0)
            )`, id, id).Scan(&isLastAdmin)
			if err != nil {
				return err
			}
			if isLastAdmin {
				return errLastPanelAdmin
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO panel_users(github_id, login, role, disabled) VALUES (?, ?, ?, 0)
            ON CONFLICT(github_id) DO UPDATE SET login = excluded.login, role = excluded.role, disabled = 0`,
			id, resolved, input.Role)
		return err
	})
	switch {
	case errors.Is(err, errLastPanelAdmin):
		http.Error(w, "At least one active administrator is required.", http.StatusConflict)
	case errors.Is(err, errPanelUnauthorized):
		http.Error(w, "Your panel permissions changed. Reload and try again.", http.StatusForbidden)
	case err != nil:
		http.Error(w, "Could not update the team member.", http.StatusServiceUnavailable)
	default:
		panelJSON(w, http.StatusOK, panelMember{panelIdentity: panelIdentity{GitHubID: id, Login: resolved, Role: input.Role}})
	}
}
