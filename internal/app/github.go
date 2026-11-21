package app

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const githubAPIBase = "https://api.github.com"

const (
	repositoryScopeEnv = "RUNQUILL_REPOSITORY_SCOPE"
	scopeAll           = "all"
	scopeSelected      = "selected"
)

var errRepositoryOutsideInstallation = errors.New("repository is outside the configured GitHub App installation")

type github struct {
	client          *http.Client
	base            string
	appID           string
	key             *rsa.PrivateKey
	owner           string
	repositoryScope string
	organization    bool
	runnerGroupID   int64
	ready           *readiness

	mu           sync.Mutex
	tokens       map[string]installationToken
	blockedUntil time.Time
}

// Installation tokens are refreshed this long before GitHub expires them.
const tokenRefreshMargin = 5 * time.Minute

func (t installationToken) usable(now time.Time) bool {
	return t.Token != "" && t.ExpiresAt.After(now.Add(tokenRefreshMargin))
}

// backoffUntil reports when GitHub's last rate-limit response ends, or zero if it has.
func (g *github) backoffUntil(now time.Time) time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blockedUntil.After(now) {
		return g.blockedUntil
	}
	return time.Time{}
}

func (g *github) backoff(until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until.After(g.blockedUntil) {
		g.blockedUntil = until
	}
}

// mintCached reuses an installation token with the same scope until it nears expiry.
func (g *github) mintCached(ctx context.Context, jwt string, id int64, body any) (installationToken, error) {
	scope, err := json.Marshal(body)
	if err != nil {
		return installationToken{}, err
	}
	key := strconv.FormatInt(id, 10) + " " + string(scope)
	g.mu.Lock()
	cached := g.tokens[key]
	g.mu.Unlock()
	if cached.usable(time.Now()) {
		return cached, nil
	}
	out, err := g.mintToken(ctx, jwt, id, body)
	if err != nil || !out.usable(time.Now()) {
		return out, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tokens == nil {
		g.tokens = make(map[string]installationToken)
	}
	for key, token := range g.tokens {
		if !token.usable(time.Now()) {
			delete(g.tokens, key)
		}
	}
	g.tokens[key] = out
	return out, nil
}

func (g *github) forgetToken(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, cached := range g.tokens {
		if cached.Token == token {
			delete(g.tokens, key)
		}
	}
}

func parseRepositoryScope(value string) (string, error) {
	switch value {
	case "", scopeAll:
		return scopeAll, nil
	case scopeSelected:
		return scopeSelected, nil
	default:
		return "", errors.New("repository scope must be all or selected")
	}
}

func repositoryScopeLabel(scope string) string {
	switch scope {
	case scopeAll:
		return "All repositories"
	case scopeSelected:
		return "Only select repositories"
	default:
		return "unknown repository scope"
	}
}

func parseKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("expected a GitHub App PEM key")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
	}
	return nil, errors.New("expected an RSA private key")
}

func (g *github) jwt(now time.Time) (string, error) {
	claims, err := json.Marshal(struct {
		Issuer   string `json:"iss"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
	}{g.appID, now.Add(-time.Minute).Unix(), now.Add(9 * time.Minute).Unix()})
	if err != nil {
		return "", err
	}
	encoding := base64.RawURLEncoding
	message := encoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(nil, g.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return message + "." + encoding.EncodeToString(signature), nil
}

type githubError struct {
	method, path string
	status       int
	retryAt      time.Time
	deferred     bool
}

func (e *githubError) Error() string {
	if e.deferred {
		return fmt.Sprintf("GitHub %s %s deferred until %s after a GitHub rate limit", e.method, e.path, e.retryAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("GitHub %s %s returned HTTP %d", e.method, e.path, e.status)
}

func githubNotFound(err error) bool {
	var apiErr *githubError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound
}

func (g *github) request(ctx context.Context, method, path, token string, body, result any) error {
	res, err := g.send(ctx, method, path, token, body, "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if result == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(result); err != nil {
		return fmt.Errorf("decode GitHub %s %s response: %w", method, path, err)
	}
	return nil
}

type rateGateKey struct{}

// withRateGate makes requests on ctx fail without calling GitHub while a rate limit is in force.
func withRateGate(ctx context.Context) context.Context {
	return context.WithValue(ctx, rateGateKey{}, true)
}

func (g *github) send(ctx context.Context, method, path, token string, body any, etag string) (*http.Response, error) {
	if gated, _ := ctx.Value(rateGateKey{}).(bool); gated {
		if until := g.backoffUntil(time.Now()); !until.IsZero() {
			return nil, &githubError{method: method, path: path, status: http.StatusTooManyRequests, retryAt: until, deferred: true}
		}
	}
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, input)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Runquill")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	res, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub %s %s: %w", method, path, err)
	}
	if res.StatusCode == http.StatusNotModified && etag != "" {
		return res, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		apiErr := &githubError{method: method, path: path, status: res.StatusCode}
		if res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests {
			apiErr.retryAt = githubRateLimit(res, time.Now())
			g.backoff(apiErr.retryAt)
		}
		if res.StatusCode == http.StatusUnauthorized {
			g.forgetToken(token)
		}
		return nil, apiErr
	}
	return res, nil
}

func githubRateLimit(res *http.Response, now time.Time) time.Time {
	if seconds, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && seconds > 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if res.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(res.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > now.Unix() {
			return time.Unix(reset+1, 0)
		}
		return now.Add(time.Minute)
	}
	var out struct{ Message string }
	json.NewDecoder(io.LimitReader(res.Body, 8<<10)).Decode(&out)
	if res.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(out.Message), "rate limit") {
		return now.Add(time.Minute)
	}
	return time.Time{}
}

type githubInstallation struct {
	ID      int64
	Account struct {
		ID    int64
		Login string
		Type  string
	}
	RepositorySelection string     `json:"repository_selection"`
	SuspendedAt         *time.Time `json:"suspended_at"`
	Permissions         map[string]string
	Events              []string
}

func (inst githubInstallation) checkRepositoryScope(expected string) error {
	if expected == "" {
		expected = scopeAll
	}
	if inst.RepositorySelection != expected {
		return fmt.Errorf("GitHub uses %s. Select %s in the App installation or update %s in the Runquill configuration",
			repositoryScopeLabel(inst.RepositorySelection), repositoryScopeLabel(expected), repositoryScopeEnv)
	}
	return nil
}

func (inst githubInstallation) accessErrors(expected string, organizationGroup bool) (scopeErr, permissionErr error) {
	scopeErr = inst.checkRepositoryScope(expected)
	if actions := inst.Permissions["actions"]; actions != "read" && actions != "write" {
		permissionErr = errors.New("set Actions to at least Read-only")
	}
	switch {
	case organizationGroup && inst.Permissions[organizationRunnerPermission] != "write":
		permissionErr = errors.Join(permissionErr, errors.New("set organization Self-hosted runners to Read and write"))
	case !organizationGroup && inst.Permissions["administration"] != "write":
		permissionErr = errors.Join(permissionErr, errors.New("set repository Administration to Read and write"))
	}
	return scopeErr, permissionErr
}

func (g *github) installation(ctx context.Context, jwt string) (githubInstallation, error) {
	var inst githubInstallation
	accountPath, accountLabel, accountType := "/users/", "personal account", "User"
	if g.organization {
		accountPath, accountLabel, accountType = "/orgs/", "organization", "Organization"
	}
	path := accountPath + url.PathEscape(g.owner) + "/installation"
	err := g.request(ctx, http.MethodGet, path, jwt, nil, &inst)
	g.observeAuth(err)
	if githubNotFound(err) {
		return inst, fmt.Errorf("install the GitHub App on the %s %s: %w", accountLabel, g.owner, err)
	}
	if err != nil {
		return inst, err
	}
	if inst.ID <= 0 || inst.Account.Type != accountType || !strings.EqualFold(inst.Account.Login, g.owner) {
		return inst, fmt.Errorf("GitHub did not return an installation on the configured %s", accountLabel)
	}
	if inst.SuspendedAt != nil {
		return inst, errors.New("the GitHub App installation is suspended")
	}
	return inst, nil
}

func (g *github) repositoryInstallation(ctx context.Context, jwt, name string, installationID int64) (githubInstallation, error) {
	var inst githubInstallation
	err := g.request(ctx, http.MethodGet, "/repos/"+name+"/installation", jwt, nil, &inst)
	g.observeAuth(err)
	if githubNotFound(err) || err == nil && inst.ID != installationID {
		return inst, errRepositoryOutsideInstallation
	}
	if err == nil && g.organization && (inst.Account.Type != "Organization" || !strings.EqualFold(inst.Account.Login, g.owner)) {
		return inst, errRepositoryOutsideInstallation
	}
	if err == nil && inst.SuspendedAt != nil {
		return inst, errors.New("the GitHub App installation is suspended")
	}
	return inst, err
}

func (g *github) checkRepositoryAccess(ctx context.Context, j job) error {
	if !g.organization && g.repositoryScope != scopeSelected {
		return nil
	}
	jwt, err := g.jwt(time.Now())
	if err != nil {
		return err
	}
	inst, err := g.repositoryInstallation(ctx, jwt, j.repository, j.installationID)
	if err != nil {
		return err
	}
	return inst.checkRepositoryScope(g.repositoryScope)
}

type installationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (g *github) mintToken(ctx context.Context, jwt string, id int64, body any) (installationToken, error) {
	var out installationToken
	path := fmt.Sprintf("/app/installations/%d/access_tokens", id)
	err := g.request(ctx, http.MethodPost, path, jwt, body, &out)
	if err == nil && out.Token == "" {
		err = errors.New("GitHub installation token response is empty")
	}
	g.observeAuth(err)
	return out, err
}

// observeAuth records the outcome of a request signed with the App JWT for /readyz.
func (g *github) observeAuth(err error) {
	var apiErr *githubError
	switch {
	case err == nil:
		g.ready.markAuth(time.Now(), nil)
	case errors.As(err, &apiErr) && (apiErr.status == http.StatusUnauthorized ||
		apiErr.status == http.StatusForbidden && apiErr.retryAt.IsZero()):
		// Rate limits, network errors and repository-scoped refusals say nothing about the App credentials.
		g.ready.markAuth(time.Now(), err)
	}
}

func (g *github) token(ctx context.Context, j job) (string, error) {
	jwt, err := g.jwt(time.Now())
	if err != nil {
		return "", err
	}
	body := map[string]any{"repository_ids": []int64{j.repositoryID}}
	if g.runnerGroupID > 0 {
		// Repository reads never use the organization runner-registration token.
		body["permissions"] = map[string]string{"actions": "read"}
	}
	// A fresh mint is how a repository leaving a selected or organization installation is noticed.
	mint := g.mintCached
	if g.organization || g.repositoryScope == scopeSelected {
		mint = g.mintToken
	}
	out, err := mint(ctx, jwt, j.installationID, body)
	var apiErr *githubError
	if (g.organization || g.repositoryScope == scopeSelected) && errors.As(err, &apiErr) &&
		(apiErr.status == http.StatusNotFound || apiErr.status == http.StatusUnprocessableEntity ||
			apiErr.status == http.StatusForbidden && apiErr.retryAt.IsZero()) {
		// A token request can fail while a repository is being removed from the installation.
		if _, lookupErr := g.repositoryInstallation(ctx, jwt, j.repository, j.installationID); errors.Is(lookupErr, errRepositoryOutsideInstallation) {
			return "", lookupErr
		}
	}
	return out.Token, err
}

func (g *github) eligible(ctx context.Context, j job, token string) (repository, bool, error) {
	var repo repository
	err := g.request(ctx, http.MethodGet, "/repos/"+j.repository, token, nil, &repo)
	return repo, g.ineligibility(j, repo) == "", err
}

// ineligibility says why repo may not run j, or is empty when it may.
func (g *github) ineligibility(j job, repo repository) string {
	switch {
	case repo.ID != j.repositoryID:
		return fmt.Sprintf("GitHub repository ID is %d, not %d", repo.ID, j.repositoryID)
	case !strings.EqualFold(repo.Owner.Login, g.owner):
		return fmt.Sprintf("repository owner is %q, not %q", repo.Owner.Login, g.owner)
	case !repo.privateFor(g.owner), g.organization && repo.Visibility != "private":
		if v := repo.Visibility; v != "" && v != "private" {
			return "repository is " + v
		}
		if !repo.Private {
			return "repository is public"
		}
		return "GitHub did not report the repository visibility as private"
	case repo.Archived:
		return "repository is archived"
	case repo.Disabled:
		return "repository is disabled"
	}
	return ""
}

func (g *github) jobDetails(ctx context.Context, j job, token string) (workflowJob, error) {
	var out workflowJob
	path := fmt.Sprintf("/repos/%s/actions/jobs/%d", j.repository, j.id)
	err := g.request(ctx, http.MethodGet, path, token, nil, &out)
	return out, err
}

func (g *github) provision(ctx context.Context, j job, name, token string) (int64, string, error) {
	body := struct {
		Name   string   `json:"name"`
		Group  int64    `json:"runner_group_id"`
		Labels []string `json:"labels"`
	}{name, max(1, g.runnerGroupID), []string{j.route}}
	var out struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		Config string `json:"encoded_jit_config"`
	}
	path := g.runnersPath(j) + "/generate-jitconfig"
	if err := g.request(ctx, http.MethodPost, path, token, body, &out); err != nil {
		return 0, "", err
	}
	if out.Runner.ID <= 0 || out.Config == "" {
		return 0, "", errors.New("incomplete GitHub JIT configuration")
	}
	return out.Runner.ID, out.Config, nil
}

type githubRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// runnerToken mints a token that can manage the runner registrations for a job.
func (g *github) runnerToken(ctx context.Context, j job) (string, error) {
	if g.runnerGroupID > 0 {
		return g.organizationJobToken(ctx, j, false)
	}
	return g.token(ctx, j)
}

// findRunner returns the registration named j.runnerName, or a zero ID when GitHub has none.
func (g *github) findRunner(ctx context.Context, j job, token string) (githubRunner, error) {
	var out struct {
		Runners []githubRunner `json:"runners"`
	}
	err := g.request(ctx, http.MethodGet, g.runnersPath(j)+"?name="+url.QueryEscape(j.runnerName), token, nil, &out)
	if err != nil {
		return githubRunner{}, err
	}
	for _, runner := range out.Runners {
		if runner.Name == j.runnerName && runner.ID > 0 {
			return runner, nil
		}
	}
	return githubRunner{}, nil
}

func (g *github) deleteRunner(ctx context.Context, j job, token string, id int64) error {
	err := g.request(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", g.runnersPath(j), id), token, nil, nil)
	if githubNotFound(err) {
		return nil
	}
	return err
}

func (g *github) remove(ctx context.Context, j job) error {
	token, err := g.runnerToken(ctx, j)
	if err != nil {
		if githubNotFound(err) || errors.Is(err, errRepositoryOutsideInstallation) {
			return nil
		}
		return err
	}
	id := j.runnerID
	if id == 0 {
		// GitHub may have accepted a request before the manager saved the runner ID.
		runner, err := g.findRunner(ctx, j, token)
		if err != nil && !githubNotFound(err) {
			return err
		}
		id = runner.ID
	}
	if id > 0 {
		return g.deleteRunner(ctx, j, token, id)
	}
	return nil
}
