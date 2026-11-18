package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	panelCallbackPath    = "/auth/github/callback"
	panelSessionLifetime = 12 * time.Hour
	panelLoginLifetime   = 10 * time.Minute
	panelPendingLimit    = 256
)

var errPanelUnauthorized = errors.New("panel sign-in required")

type panelOAuthConfig struct {
	origin, host, clientID, clientSecret string
	secure                               bool
	bootstrapID                          int64
}

type panelIdentity struct {
	GitHubID   int64  `json:"github_id"`
	Login      string `json:"login"`
	Role       string `json:"role"`
	Scope      string `json:"-"`
	credential [32]byte
}

type panelLogin struct {
	verifier string
	expires  time.Time
}

type panelAccess struct {
	db     *sql.DB
	cfg    panelOAuthConfig
	oauth  *http.Client
	mu     sync.Mutex
	logins map[[32]byte]panelLogin
}

const panelAccessSchema = `
CREATE TABLE IF NOT EXISTS panel_users (
    github_id INTEGER PRIMARY KEY CHECK (github_id > 0),
    login TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL CHECK (role IN ('viewer','operator','admin')),
    disabled INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1))
);
CREATE TABLE IF NOT EXISTS panel_sessions (
    digest BLOB PRIMARY KEY,
    user_id INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS panel_sessions_expiry ON panel_sessions(expires_at);
CREATE TABLE IF NOT EXISTS panel_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    digest BLOB NOT NULL UNIQUE,
    user_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('fleet:read','hosts:write')),
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS panel_tokens_user ON panel_tokens(user_id);
CREATE TABLE IF NOT EXISTS panel_audit (
    id INTEGER PRIMARY KEY,
    actor INTEGER NOT NULL,
    action TEXT NOT NULL,
    target TEXT NOT NULL,
    occurred_at INTEGER NOT NULL
);
`

func loadPanelOAuthConfig() (panelOAuthConfig, error) {
	return parsePanelOAuthConfig(
		os.Getenv("RUNQUILL_PANEL_URL"), os.Getenv("RUNQUILL_PANEL_GITHUB_CLIENT_ID"),
		os.Getenv("RUNQUILL_PANEL_GITHUB_CLIENT_SECRET"), os.Getenv("RUNQUILL_PANEL_BOOTSTRAP_ID"),
	)
}

func parsePanelOAuthConfig(origin, id, secret, bootstrap string) (panelOAuthConfig, error) {
	u, err := parsePanelURL(origin)
	if err != nil {
		return panelOAuthConfig{}, err
	}
	if id == "" || secret == "" {
		return panelOAuthConfig{}, errors.New("set RUNQUILL_PANEL_GITHUB_CLIENT_ID and RUNQUILL_PANEL_GITHUB_CLIENT_SECRET")
	}
	var bootstrapID int64
	if bootstrap != "" {
		bootstrapID, err = strconv.ParseInt(bootstrap, 10, 64)
		if err != nil || bootstrapID < 1 {
			return panelOAuthConfig{}, errors.New("RUNQUILL_PANEL_BOOTSTRAP_ID must be a positive GitHub user ID")
		}
	}
	return panelOAuthConfig{origin: u.String(), host: u.Host, clientID: id, clientSecret: secret,
		secure: u.Scheme == "https", bootstrapID: bootstrapID}, nil
}

func parsePanelURL(origin string) (*url.URL, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || strings.ContainsAny(origin, "#$\\'\"") || (u.EscapedPath() != "" && u.EscapedPath() != "/") ||
		(u.Scheme != "https" && !(u.Scheme == "http" && isPanelLoopback(u.Hostname()))) {
		return nil, errors.New("set RUNQUILL_PANEL_URL to its HTTPS origin or an HTTP loopback origin")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("RUNQUILL_PANEL_URL has an invalid port")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("RUNQUILL_PANEL_URL has an invalid port")
		}
		if u.Scheme == "https" && n == 443 || u.Scheme == "http" && n == 80 {
			u.Host = strings.TrimSuffix(u.Host, ":"+port)
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.Path, u.RawPath = "", ""
	return u, nil
}

func (cfg panelOAuthConfig) callbackURL() string {
	return cfg.origin + panelCallbackPath
}

func isPanelLoopback(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
}

func newPanelAccess(ctx context.Context, db *sql.DB, cfg panelOAuthConfig) (*panelAccess, error) {
	if _, err := db.ExecContext(ctx, panelAccessSchema); err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var empty bool
	if err := tx.QueryRowContext(ctx, "SELECT NOT EXISTS(SELECT 1 FROM panel_users)").Scan(&empty); err != nil {
		return nil, err
	}
	if empty {
		if cfg.bootstrapID == 0 {
			return nil, errors.New("set RUNQUILL_PANEL_BOOTSTRAP_ID to the numeric GitHub ID of the first administrator")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO panel_users(github_id, role) VALUES (?, 'admin')", cfg.bootstrapID); err != nil {
			return nil, err
		}
		if err := auditPanel(ctx, tx, cfg.bootstrapID, "user.bootstrap", strconv.FormatInt(cfg.bootstrapID, 10)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &panelAccess{db: db, cfg: cfg, logins: make(map[[32]byte]panelLogin), oauth: &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func panelSecret() string {
	var key [32]byte
	rand.Read(key[:])
	return base64.RawURLEncoding.EncodeToString(key[:])
}

func panelDigest(key string) [32]byte {
	return sha256.Sum256([]byte(key))
}

func (a *panelAccess) cookieName(suffix string) string {
	name := "rq_" + suffix
	if a.cfg.secure {
		return "__Host-" + name
	}
	return name
}

func (a *panelAccess) setCookie(w http.ResponseWriter, suffix, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(suffix), Value: value, Path: "/",
		Secure: a.cfg.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: seconds})
}

func (a *panelAccess) registerLogin(state, verifier string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, login := range a.logins {
		if !login.expires.After(now) {
			delete(a.logins, key)
		}
	}
	if len(a.logins) >= panelPendingLimit {
		return false
	}
	a.logins[panelDigest(state)] = panelLogin{verifier: verifier, expires: now.Add(panelLoginLifetime)}
	return true
}

func (a *panelAccess) consumeLogin(state string, now time.Time) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := panelDigest(state)
	login, ok := a.logins[key]
	delete(a.logins, key)
	return login.verifier, ok && login.expires.After(now)
}

func (a *panelAccess) githubLogin(w http.ResponseWriter, r *http.Request) {
	state, verifier := panelSecret(), panelSecret()
	if !a.registerLogin(state, verifier, time.Now()) {
		http.Error(w, "Too many sign-in attempts. Try again later.", http.StatusTooManyRequests)
		return
	}
	a.setCookie(w, "oauth", state, int(panelLoginLifetime.Seconds()))
	challenge := panelDigest(verifier)
	query := url.Values{"client_id": {a.cfg.clientID}, "redirect_uri": {a.cfg.callbackURL()},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"}, "allow_signup": {"false"}}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+query.Encode(), http.StatusFound)
}

func (a *panelAccess) githubCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	cookie, err := r.Cookie(a.cookieName("oauth"))
	a.setCookie(w, "oauth", "", -1)
	if err != nil || len(state) != 43 || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		http.Error(w, "Sign-in expired. Return to the panel and try again.", http.StatusBadRequest)
		return
	}
	verifier, ok := a.consumeLogin(state, time.Now())
	if !ok || code == "" || len(code) > 512 {
		http.Error(w, "Sign-in expired or cancelled. Return to the panel and try again.", http.StatusBadRequest)
		return
	}
	var user panelIdentity
	if err := a.exchangeGitHubIdentity(ctx, code, verifier, &user); err != nil {
		http.Error(w, "Could not verify the GitHub login. Try again.", http.StatusBadGateway)
		return
	}
	key, err := a.startSession(ctx, user)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Your GitHub account is not enrolled in this Runquill panel.", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "Could not start a panel session.", http.StatusServiceUnavailable)
		return
	}
	a.setCookie(w, "session", key, int(panelSessionLifetime.Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *panelAccess) exchangeGitHubIdentity(ctx context.Context, code, verifier string, user *panelIdentity) error {
	form := url.Values{"client_id": {a.cfg.clientID}, "client_secret": {a.cfg.clientSecret},
		"code": {code}, "code_verifier": {verifier}, "redirect_uri": {a.cfg.callbackURL()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var token struct {
		Access string `json:"access_token"`
	}
	if err := a.readGitHub(req, &token); err != nil {
		return err
	}
	if token.Access == "" {
		return errors.New("GitHub returned no access token")
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token.Access)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	var reply struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := a.readGitHub(req, &reply); err != nil {
		return err
	}
	if reply.ID <= 0 || reply.Login == "" || len(reply.Login) > 100 {
		return errors.New("GitHub returned an invalid identity")
	}
	user.GitHubID, user.Login = reply.ID, reply.Login
	return nil
}

func (a *panelAccess) readGitHub(req *http.Request, value any) error {
	resp, err := a.oauth.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(value)
}

func (a *panelAccess) startSession(ctx context.Context, user panelIdentity) (string, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var role string
	err = tx.QueryRowContext(ctx, `UPDATE panel_users SET login = ?
        WHERE github_id = ? AND disabled = 0 RETURNING role`, user.Login, user.GitHubID).Scan(&role)
	if err != nil {
		return "", err
	}
	key := panelSecret()
	digest := panelDigest(key)
	now := time.Now()
	if _, err := tx.ExecContext(ctx, "DELETE FROM panel_sessions WHERE expires_at <= ?", now.Unix()); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO panel_sessions(digest,user_id,expires_at) VALUES(?,?,?)",
		digest[:], user.GitHubID, now.Add(panelSessionLifetime).Unix()); err != nil {
		return "", err
	}
	if err := auditPanel(ctx, tx, user.GitHubID, "session.login", ""); err != nil {
		return "", err
	}
	return key, tx.Commit()
}

func (a *panelAccess) identify(r *http.Request) (panelIdentity, error) {
	var identity panelIdentity
	now := time.Now().Unix()
	if header := r.Header.Get("Authorization"); header != "" {
		key, ok := strings.CutPrefix(header, "Bearer rqk_")
		if !ok || len(key) != 43 {
			return identity, errPanelUnauthorized
		}
		digest := panelDigest(key)
		identity.credential = digest
		err := a.db.QueryRowContext(r.Context(), `SELECT github_id, login, role, scope
            FROM panel_tokens JOIN panel_users ON user_id = github_id
            WHERE digest = ? AND expires_at > ? AND disabled = 0`, digest[:], now).
			Scan(&identity.GitHubID, &identity.Login, &identity.Role, &identity.Scope)
		if errors.Is(err, sql.ErrNoRows) {
			return identity, errPanelUnauthorized
		}
		return identity, err
	}
	cookie, err := r.Cookie(a.cookieName("session"))
	if err != nil || len(cookie.Value) != 43 {
		return identity, errPanelUnauthorized
	}
	digest := panelDigest(cookie.Value)
	identity.credential = digest
	err = a.db.QueryRowContext(r.Context(), `SELECT github_id,login,role
        FROM panel_sessions JOIN panel_users ON user_id = github_id
        WHERE digest = ? AND expires_at > ? AND disabled = 0`, digest[:], now).
		Scan(&identity.GitHubID, &identity.Login, &identity.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return identity, errPanelUnauthorized
	}
	return identity, err
}

func auditPanel(ctx context.Context, tx *sql.Tx, actor int64, action, target string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO panel_audit(actor,action,target,occurred_at) VALUES (?,?,?,?)`,
		actor, action, target, time.Now().Unix())
	return err
}
