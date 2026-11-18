package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type panelAction func(http.ResponseWriter, *http.Request, panelIdentity)

func (a *panelAccess) require(scope string, action panelAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := a.identify(r)
		if errors.Is(err, errPanelUnauthorized) {
			http.Error(w, "Sign in to Runquill.", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, "Could not check panel access.", http.StatusServiceUnavailable)
			return
		}
		if !panelPermission(user, scope) {
			http.Error(w, "You do not have access to this operation.", http.StatusForbidden)
			return
		}
		if user.Scope == "" && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Header.Get("Origin") != a.cfg.origin {
			http.Error(w, "The request must come from the panel origin.", http.StatusForbidden)
			return
		}
		action(w, r, user)
	}
}

func panelPermission(user panelIdentity, required string) bool {
	if user.Scope != "" && user.Scope != required {
		return false
	}
	switch required {
	case "fleet:read", "browser":
		return true
	case "hosts:write":
		return user.Role == "operator" || user.Role == "admin"
	case "admin":
		return user.Role == "admin"
	default:
		return false
	}
}

func (a *panelAccess) register(mux *http.ServeMux, panel *panelState) {
	mux.HandleFunc("GET /auth/github", a.githubLogin)
	mux.HandleFunc("GET "+panelCallbackPath, a.githubCallback)
	mux.HandleFunc("GET /api/me", a.require("browser", func(w http.ResponseWriter, r *http.Request, user panelIdentity) {
		panelJSON(w, http.StatusOK, user)
	}))
	mux.HandleFunc("POST /api/logout", a.require("browser", a.logout))
	mux.HandleFunc("GET /api/team", a.require("admin", a.team))
	mux.HandleFunc("PUT /api/team/{id}", a.require("admin", a.setTeamUser))
	mux.HandleFunc("GET /api/tokens", a.require("browser", a.tokens))
	mux.HandleFunc("POST /api/tokens", a.require("browser", a.createToken))
	mux.HandleFunc("DELETE /api/tokens/{id}", a.require("browser", a.revokeToken))
	mux.HandleFunc("GET /api/audit", a.require("admin", a.audit))
	mux.HandleFunc("POST /api/hosts/cordon", a.require("hosts:write", func(w http.ResponseWriter, r *http.Request, user panelIdentity) {
		a.cordon(w, r, user, panel)
	}))
	mux.HandleFunc("GET /snapshot", a.require("fleet:read", func(w http.ResponseWriter, r *http.Request, user panelIdentity) {
		panel.mu.RLock()
		snapshot := panel.snapshot
		panel.mu.RUnlock()
		panelJSON(w, http.StatusOK, snapshot)
	}))
}

func panelJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func decodePanelJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	contentType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if contentType != "application/json" {
		http.Error(w, "Send application/json.", http.StatusUnsupportedMediaType)
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		http.Error(w, "Invalid JSON request.", http.StatusBadRequest)
		return false
	}
	return true
}

func (a *panelAccess) logout(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	cookie, err := r.Cookie(a.cookieName("session"))
	if err != nil {
		http.Error(w, "Sign in to Runquill.", http.StatusUnauthorized)
		return
	}
	digest := panelDigest(cookie.Value)
	if err := a.change(r.Context(), user, "session.logout", "", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), "DELETE FROM panel_sessions WHERE digest = ?", digest[:])
		return err
	}); err != nil {
		http.Error(w, "Could not end the session.", http.StatusServiceUnavailable)
		return
	}
	a.setCookie(w, "session", "", -1)
	w.WriteHeader(http.StatusNoContent)
}

type panelMember struct {
	panelIdentity
	Disabled bool `json:"disabled"`
}

func (a *panelAccess) team(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT github_id, login, role, disabled FROM panel_users ORDER BY github_id`)
	if err != nil {
		http.Error(w, "Could not read team members.", http.StatusServiceUnavailable)
		return
	}
	defer rows.Close()
	members := make([]panelMember, 0)
	for rows.Next() {
		var m panelMember
		if err := rows.Scan(&m.GitHubID, &m.Login, &m.Role, &m.Disabled); err != nil {
			http.Error(w, "Could not read team members.", http.StatusServiceUnavailable)
			return
		}
		members = append(members, m)
	}
	if rows.Err() != nil {
		http.Error(w, "Could not read team members.", http.StatusServiceUnavailable)
		return
	}
	panelJSON(w, http.StatusOK, members)
}

var errLastPanelAdmin = errors.New("At least one active administrator is required.")

func (a *panelAccess) setTeamUser(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	rawID := r.PathValue("id")
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "Use a positive GitHub account ID.", http.StatusBadRequest)
		return
	}
	var change struct {
		Role     string `json:"role"`
		Disabled bool   `json:"disabled"`
	}
	if !decodePanelJSON(w, r, &change) {
		return
	}
	switch change.Role {
	case "viewer", "operator", "admin":
	default:
		http.Error(w, "Choose viewer, operator, or admin.", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	action := "user.set." + change.Role
	if change.Disabled {
		action = "user.disable"
	}
	err = a.change(ctx, user, action, strconv.FormatInt(id, 10), func(tx *sql.Tx) error {
		if change.Disabled || change.Role != "admin" {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO panel_users(github_id,role,disabled)
            VALUES (?,?,?) ON CONFLICT(github_id) DO UPDATE SET role=excluded.role, disabled=excluded.disabled`,
			id, change.Role, change.Disabled); err != nil {
			return err
		}
		if change.Disabled {
			if _, err := tx.ExecContext(ctx, "DELETE FROM panel_sessions WHERE user_id = ?", id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM panel_tokens WHERE user_id = ?", id); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errLastPanelAdmin) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "Could not update the team member.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type panelToken struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	ExpiresAt int64  `json:"expires_at"`
}

func (a *panelAccess) tokens(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,user_id,name,scope,expires_at FROM panel_tokens
        WHERE (user_id = ? OR ?) AND expires_at > ? ORDER BY id DESC LIMIT 100`,
		user.GitHubID, user.Role == "admin", time.Now().Unix())
	if err != nil {
		http.Error(w, "Could not read API tokens.", http.StatusServiceUnavailable)
		return
	}
	defer rows.Close()
	tokens := make([]panelToken, 0)
	for rows.Next() {
		var token panelToken
		if err := rows.Scan(&token.ID, &token.UserID, &token.Name, &token.Scope, &token.ExpiresAt); err != nil {
			http.Error(w, "Could not read API tokens.", http.StatusServiceUnavailable)
			return
		}
		tokens = append(tokens, token)
	}
	if rows.Err() != nil {
		http.Error(w, "Could not read API tokens.", http.StatusServiceUnavailable)
		return
	}
	panelJSON(w, http.StatusOK, tokens)
}

func (a *panelAccess) createToken(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	var input struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
		Days  int    `json:"days"`
	}
	if !decodePanelJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 80 || input.Days < 1 || input.Days > 90 {
		http.Error(w, "Use a token name and an expiry between 1 and 90 days.", http.StatusBadRequest)
		return
	}
	if (input.Scope != "fleet:read" && input.Scope != "hosts:write") || !panelPermission(user, input.Scope) {
		http.Error(w, "Choose a token scope that your role permits.", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	key := panelSecret()
	digest := panelDigest(key)
	now := time.Now()
	token := panelToken{UserID: user.GitHubID, Name: input.Name, Scope: input.Scope,
		ExpiresAt: now.Add(time.Duration(input.Days) * 24 * time.Hour).Unix()}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "Could not create the token.", http.StatusServiceUnavailable)
		return
	}
	defer tx.Rollback()
	if err := verifyPanelActor(ctx, tx, user); err != nil {
		http.Error(w, "Your panel permissions changed. Reload and try again.", http.StatusForbidden)
		return
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO panel_tokens (digest,user_id,name,scope,expires_at,created_at)
        VALUES (?,?,?,?,?,?) RETURNING id`, digest[:], token.UserID, token.Name, token.Scope, token.ExpiresAt, now.Unix()).Scan(&token.ID)
	if err == nil {
		err = auditPanel(ctx, tx, user.GitHubID, "token.create", strconv.FormatInt(token.ID, 10))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		http.Error(w, "Could not create the token.", http.StatusServiceUnavailable)
		return
	}
	panelJSON(w, http.StatusCreated, struct {
		panelToken
		Secret string `json:"secret"`
	}{token, "rqk_" + key})
}

func (a *panelAccess) revokeToken(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	err = a.change(ctx, user, "token.revoke", strconv.FormatInt(id, 10), func(tx *sql.Tx) error {
		var deleted int64
		return tx.QueryRowContext(ctx, `DELETE FROM panel_tokens WHERE id = ? AND (user_id = ? OR ?) RETURNING id`,
			id, user.GitHubID, user.Role == "admin").Scan(&deleted)
	})
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Could not revoke the token.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *panelAccess) audit(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT actor,action,target,occurred_at FROM panel_audit ORDER BY id DESC LIMIT 100`)
	if err != nil {
		http.Error(w, "Could not read audit entries.", http.StatusServiceUnavailable)
		return
	}
	defer rows.Close()
	type event struct {
		Actor      int64  `json:"actor"`
		Action     string `json:"action"`
		Target     string `json:"target"`
		OccurredAt int64  `json:"occurred_at"`
	}
	events := make([]event, 0)
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.Actor, &e.Action, &e.Target, &e.OccurredAt); err != nil {
			http.Error(w, "Could not read audit entries.", http.StatusServiceUnavailable)
			return
		}
		events = append(events, e)
	}
	if rows.Err() != nil {
		http.Error(w, "Could not read audit entries.", http.StatusServiceUnavailable)
		return
	}
	panelJSON(w, http.StatusOK, events)
}

func (a *panelAccess) cordon(w http.ResponseWriter, r *http.Request, user panelIdentity, panel *panelState) {
	var input struct {
		Host     string `json:"host"`
		Cordoned *bool  `json:"cordoned"`
	}
	if !decodePanelJSON(w, r, &input) {
		return
	}
	if input.Host == "" || input.Cordoned == nil {
		http.Error(w, "Choose a host and maintenance state.", http.StatusBadRequest)
		return
	}
	panel.mu.RLock()
	var known bool
	if panel.snapshot.Fleet != nil {
		for _, host := range panel.snapshot.Fleet.Hosts {
			if host.Name == input.Host {
				known = true
				break
			}
		}
	}
	panel.mu.RUnlock()
	if !known {
		http.Error(w, "This host has no current observation.", http.StatusNotFound)
		return
	}
	action := "host.uncordon"
	if *input.Cordoned {
		action = "host.cordon"
	}
	ctx := r.Context()
	err := a.change(ctx, user, action, input.Host, func(tx *sql.Tx) error {
		return writeHostCordon(ctx, tx, input.Host, *input.Cordoned)
	})
	if err != nil {
		http.Error(w, "Could not update the host.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *panelAccess) change(ctx context.Context, user panelIdentity, action, target string, apply func(*sql.Tx) error) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyPanelActor(ctx, tx, user); err != nil {
		return err
	}
	if err := apply(tx); err != nil {
		return err
	}
	if err := auditPanel(ctx, tx, user.GitHubID, action, target); err != nil {
		return err
	}
	return tx.Commit()
}

func verifyPanelActor(ctx context.Context, tx *sql.Tx, user panelIdentity) error {
	table := "panel_sessions"
	if user.Scope != "" {
		table = "panel_tokens"
	}
	var active bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
        SELECT 1 FROM `+table+` JOIN panel_users ON github_id = user_id
        WHERE digest = ? AND user_id = ? AND expires_at > ? AND disabled = 0 AND role = ?)`,
		user.credential[:], user.GitHubID, time.Now().Unix(), user.Role).Scan(&active)
	if err != nil {
		return err
	}
	if !active {
		return errPanelUnauthorized
	}
	return nil
}
