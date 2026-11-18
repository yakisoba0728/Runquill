package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type poller struct {
	github         *github
	queue          *queue
	installationID int64
	token          installationToken
	cache          map[string]pollPage
}

type pollPage struct {
	etag, next string
	items      json.RawMessage
	usedAt     time.Time
}

func (p *poller) run(ctx context.Context, interval time.Duration) {
	var backoff time.Duration
	for ctx.Err() == nil {
		delay := interval
		err := p.sweep(ctx)
		if ctx.Err() != nil {
			return
		}
		var apiErr *githubError
		if errors.As(err, &apiErr) && !apiErr.retryAt.IsZero() {
			backoff = min(max(time.Minute, 2*backoff), 10*time.Minute)
			delay = max(delay, backoff, time.Until(apiErr.retryAt))
		} else {
			backoff = 0
		}
		if err != nil {
			slog.Warn("GitHub poll failed", "error", err, "retry_in", delay)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *poller) sweep(ctx context.Context) (err error) {
	now := time.Now()
	defer func() {
		var apiErr *githubError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusUnauthorized {
			p.token = installationToken{}
		}
		for path, page := range p.cache {
			if page.usedAt.Before(now.Add(-10 * time.Minute)) {
				delete(p.cache, path)
			}
		}
	}()
	if p.token.Token == "" || !p.token.ExpiresAt.After(now.Add(5*time.Minute)) {
		jwt, err := p.github.jwt(now)
		if err != nil {
			return err
		}
		inst, err := p.github.installation(ctx, jwt)
		if err != nil {
			return err
		}
		// Keep the account-wide discovery token read-only. The scheduler still mints
		// a separate repository-scoped token when it needs to register a runner.
		body := map[string]any{"permissions": map[string]string{"actions": "read"}}
		token, err := p.github.mintToken(ctx, jwt, inst.ID, body)
		if err != nil {
			return err
		}
		if inst.ID != p.installationID {
			clear(p.cache)
		}
		p.installationID, p.token = inst.ID, token
	}
	repos, err := polledPages[repository](ctx, p, "/installation/repositories?per_page=100", "repositories")
	if err != nil {
		return err
	}
	var scanErrors []error
	for _, repo := range repos {
		name, valid := ownedRepositoryPath(repo.FullName, p.github.owner)
		if !valid || !repo.privateFor(p.github.owner) || repo.Archived || repo.Disabled {
			continue
		}
		if err := p.scanRepository(ctx, repo, name); err != nil {
			var apiErr *githubError
			if ctx.Err() != nil || errors.As(err, &apiErr) && (!apiErr.retryAt.IsZero() || apiErr.status == http.StatusUnauthorized) {
				return err
			}
			scanErrors = append(scanErrors, err)
		}
	}
	return errors.Join(scanErrors...)
}

func (p *poller) scanRepository(ctx context.Context, repo repository, name string) error {
	base := "/repos/" + name + "/actions/runs"
	seen := make(map[int64]bool)
	for _, status := range []string{"queued", "in_progress", "requested", "waiting", "pending"} {
		runs, err := polledPages[struct{ ID int64 }](ctx, p, base+"?per_page=100&status="+status, "workflow_runs")
		if err != nil {
			return err
		}
		for _, run := range runs {
			if run.ID <= 0 || seen[run.ID] {
				continue
			}
			seen[run.ID] = true
			path := fmt.Sprintf("%s/%d/jobs?per_page=50&filter=latest", base, run.ID)
			jobs, err := polledPages[workflowJob](ctx, p, path, "jobs")
			if githubNotFound(err) {
				// A workflow run can be deleted between listing runs and reading its jobs.
				continue
			}
			if err != nil {
				return err
			}
			for _, candidate := range jobs {
				if candidate.Status != "queued" || candidate.RunID != run.ID {
					continue
				}
				job, eligible, err := candidate.queueJob(p.github.owner, repo, p.installationID, candidate.Status)
				if err != nil {
					return err
				}
				if !eligible {
					continue
				}
				added, err := p.queue.record(ctx, job, time.Now())
				if err != nil {
					return err
				}
				if added {
					slog.Info("queued job discovered", "source", "poll", "repository", job.repository, "job", job.id)
				}
			}
		}
	}
	return nil
}

func polledPages[T any](ctx context.Context, p *poller, path, key string) ([]T, error) {
	var items []T
	for path != "" {
		page, err := p.page(ctx, path, key)
		if err != nil {
			return nil, err
		}
		var batch []T
		if err := json.Unmarshal(page.items, &batch); err != nil {
			return nil, fmt.Errorf("decode GitHub %s list: %w", key, err)
		}
		items = append(items, batch...)
		path = page.next
	}
	return items, nil
}

func (p *poller) page(ctx context.Context, path, key string) (pollPage, error) {
	page := p.cache[path]
	res, err := p.github.send(ctx, http.MethodGet, path, p.token.Token, nil, page.etag)
	if err != nil {
		return page, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotModified {
		var out map[string]json.RawMessage
		if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&out); err != nil {
			return page, fmt.Errorf("decode GitHub %s page: %w", key, err)
		}
		if len(out[key]) == 0 {
			return page, fmt.Errorf("GitHub response omitted %s", key)
		}
		page = pollPage{etag: res.Header.Get("ETag"), items: out[key]}
		for link := range strings.SplitSeq(res.Header.Get("Link"), ",") {
			address, relation, ok := strings.Cut(strings.TrimSpace(link), ";")
			if !ok || strings.TrimSpace(relation) != `rel="next"` {
				continue
			}
			address = strings.Trim(address, "<>")
			pathOnly, sameOrigin := strings.CutPrefix(address, p.github.base+"/")
			if !sameOrigin || "/"+pathOnly == path {
				return page, errors.New("invalid GitHub pagination link")
			}
			next, err := url.Parse("/" + pathOnly)
			if err != nil || next.Host != "" || next.Scheme != "" {
				return page, errors.New("invalid GitHub pagination URL")
			}
			page.next = next.RequestURI()
			break
		}
	}
	page.usedAt = time.Now()
	p.cache[path] = page
	return page, nil
}
