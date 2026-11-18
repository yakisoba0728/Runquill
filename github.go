package main

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
	"time"
)

const githubAPIBase = "https://api.github.com"

type github struct {
	client *http.Client
	base   string
	appID  string
	key    *rsa.PrivateKey
	owner  string
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
}

func (e *githubError) Error() string {
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

func (g *github) send(ctx context.Context, method, path, token string, body any, etag string) (*http.Response, error) {
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
		Login string
		Type  string
	}
	RepositorySelection string     `json:"repository_selection"`
	SuspendedAt         *time.Time `json:"suspended_at"`
	Permissions         map[string]string
	Events              []string
}

func (inst githubInstallation) accessErrors() (scopeErr, permissionErr error) {
	if inst.RepositorySelection != "all" {
		scopeErr = errors.New("select All repositories to include new repositories automatically")
	}
	if actions := inst.Permissions["actions"]; actions != "read" && actions != "write" {
		permissionErr = errors.New("set Actions to at least Read-only")
	}
	if inst.Permissions["administration"] != "write" {
		permissionErr = errors.Join(permissionErr, errors.New("set Administration to Read and write"))
	}
	return scopeErr, permissionErr
}

func (g *github) installation(ctx context.Context, jwt string) (githubInstallation, error) {
	var inst githubInstallation
	path := "/users/" + url.PathEscape(g.owner) + "/installation"
	err := g.request(ctx, http.MethodGet, path, jwt, nil, &inst)
	if githubNotFound(err) {
		return inst, fmt.Errorf("install the GitHub App on the personal account %s: %w", g.owner, err)
	}
	if err != nil {
		return inst, err
	}
	if inst.ID <= 0 || inst.Account.Type != "User" || !strings.EqualFold(inst.Account.Login, g.owner) {
		return inst, errors.New("GitHub did not return an installation on the configured personal account")
	}
	if inst.SuspendedAt != nil {
		return inst, errors.New("the GitHub App installation is suspended")
	}
	return inst, nil
}

type installationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (g *github) mintToken(ctx context.Context, jwt string, id int64, body any) (installationToken, error) {
	var out installationToken
	path := fmt.Sprintf("/app/installations/%d/access_tokens", id)
	if err := g.request(ctx, http.MethodPost, path, jwt, body, &out); err != nil {
		return out, err
	}
	if out.Token == "" {
		return out, errors.New("GitHub installation token response is empty")
	}
	return out, nil
}

func (g *github) token(ctx context.Context, j job) (string, error) {
	jwt, err := g.jwt(time.Now())
	if err != nil {
		return "", err
	}
	body := struct {
		RepositoryIDs []int64 `json:"repository_ids"`
	}{[]int64{j.repositoryID}}
	out, err := g.mintToken(ctx, jwt, j.installationID, body)
	return out.Token, err
}

func (g *github) eligible(ctx context.Context, j job, token string) (bool, error) {
	var repo repository
	err := g.request(ctx, http.MethodGet, "/repos/"+j.repository, token, nil, &repo)
	return repo.Private && repo.ID == j.repositoryID && strings.EqualFold(repo.Owner.Login, g.owner), err
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
		Group  int      `json:"runner_group_id"`
		Labels []string `json:"labels"`
	}{name, 1, []string{j.route}}
	var out struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		Config string `json:"encoded_jit_config"`
	}
	path := "/repos/" + j.repository + "/actions/runners/generate-jitconfig"
	if err := g.request(ctx, http.MethodPost, path, token, body, &out); err != nil {
		return 0, "", err
	}
	if out.Runner.ID <= 0 || out.Config == "" {
		return 0, "", errors.New("incomplete GitHub JIT configuration")
	}
	return out.Runner.ID, out.Config, nil
}

func (g *github) remove(ctx context.Context, j job) error {
	token, err := g.token(ctx, j)
	if err != nil {
		if githubNotFound(err) {
			return nil
		}
		return err
	}
	path := "/repos/" + j.repository + "/actions/runners"
	id := j.runnerID
	if id == 0 {
		// GitHub may have accepted a request before the manager saved the runner ID.
		var out struct {
			Runners []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"runners"`
		}
		err := g.request(ctx, http.MethodGet, path+"?name="+url.QueryEscape(j.runnerName), token, nil, &out)
		if err != nil && !githubNotFound(err) {
			return err
		}
		for _, runner := range out.Runners {
			if runner.Name == j.runnerName {
				id = runner.ID
				break
			}
		}
	}
	if id > 0 {
		err := g.request(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", path, id), token, nil, nil)
		if err != nil && !githubNotFound(err) {
			return err
		}
	}
	return nil
}
