package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type poller struct {
	github         *github
	queue          *queue
	installationID int64
	token          installationToken
	cache          map[string]pollPage
	panel          *panelState
}

type pollPage struct {
	etag, next string
	items      json.RawMessage
	usedAt     time.Time
}

func (p *poller) run(ctx context.Context, ops *operations) {
	var last, rateBlockedUntil time.Time
	var backoff time.Duration
	for ctx.Err() == nil {
		state := ops.current.Load()
		if state.interval == 0 {
			select {
			case <-ctx.Done():
				return
			case <-state.pollEdited:
				continue
			}
		}
		next := last.Add(state.interval)
		if rateBlockedUntil.After(next) {
			next = rateBlockedUntil
		}
		if delay := time.Until(next); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-state.pollEdited:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		err := p.sweep(ctx)
		if ctx.Err() != nil {
			return
		}
		last = time.Now()
		if p.panel != nil {
			p.panel.markPoll(last, err == nil)
		}
		rateBlockedUntil = time.Time{}
		var apiErr *githubError
		if errors.As(err, &apiErr) && !apiErr.retryAt.IsZero() {
			backoff = min(max(time.Minute, 2*backoff), 10*time.Minute)
			rateBlockedUntil = last.Add(max(backoff, time.Until(apiErr.retryAt)))
		} else {
			backoff = 0
		}
		if err != nil {
			slog.Warn("GitHub poll failed", "error", err, "retry_in", max(state.interval, time.Until(rateBlockedUntil)))
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
	// A selected installation can gain repositories before an issued token expires.
	selected := p.github.repositoryScope == scopeSelected
	if selected || p.token.Token == "" || !p.token.ExpiresAt.After(now.Add(5*time.Minute)) {
		jwt, err := p.github.jwt(now)
		if err != nil {
			return err
		}
		inst, err := p.github.installation(ctx, jwt)
		if err != nil {
			return err
		}
		if selected || p.github.organization {
			if err := inst.checkRepositoryScope(p.github.repositoryScope); err != nil {
				return err
			}
		}
		// Keep the account-wide discovery token read-only. The scheduler still mints
		// a separate scoped token when it needs to register a runner.
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
	work, err := p.pollOrder(ctx, repos, now)
	if err != nil {
		return err
	}
	var results []repositoryPoll
	defer func() {
		if len(results) > 0 && ctx.Err() == nil {
			err = errors.Join(err, p.queue.saveRepositoryPolls(ctx, p.installationID, results))
		}
	}()
	var scanErrors []error
	for _, target := range work {
		checkedAt := time.Now()
		hasActiveRun, scanErr := p.scanRepository(ctx, target)
		result := target.previous
		result.repositoryID = target.repo.ID
		result.checkedAt = checkedAt.UnixNano()
		result.lastError = ""
		if target.full {
			result.fullAt = result.checkedAt
		}
		if hasActiveRun || scanErr == nil && target.recentJob {
			result.activeAt = result.checkedAt
		}
		if scanErr != nil {
			result.lastError = retryMessage("scan", scanErr)
		}
		results = append(results, result)
		if scanErr != nil {
			var apiErr *githubError
			if ctx.Err() != nil || errors.As(scanErr, &apiErr) && (!apiErr.retryAt.IsZero() || apiErr.status == http.StatusUnauthorized) {
				return scanErr
			}
			scanErrors = append(scanErrors, scanErr)
		}
	}
	return errors.Join(scanErrors...)
}

const (
	fullPollCycles   = 4
	maxPriorityRepos = 32
	recentActivity   = 5 * time.Minute
)

type repositoryScan struct {
	repo      repository
	name      string
	previous  repositoryPoll
	recentJob bool
	full      bool
}

func (p *poller) pollOrder(ctx context.Context, repos []repository, now time.Time) ([]repositoryScan, error) {
	previous, err := readRepositoryPolls(ctx, p.queue.DB, p.installationID)
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-recentActivity)
	recent, err := p.queue.recentRepositories(ctx, p.installationID, cutoff.Unix())
	if err != nil {
		return nil, err
	}
	work := make([]repositoryScan, 0, len(repos))
	seen := make(map[int64]bool, len(repos))
	for _, repo := range repos {
		name, valid := ownedRepositoryPath(repo.FullName, p.github.owner)
		if !valid || !repo.privateFor(p.github.owner) || repo.Archived || repo.Disabled || repo.ID <= 0 || seen[repo.ID] {
			continue
		}
		seen[repo.ID] = true
		work = append(work, repositoryScan{repo: repo, name: name, previous: previous[repo.ID], recentJob: recent[repo.ID]})
	}

	// Rotate the uncommon states independently of recent activity.
	slices.SortStableFunc(work, func(a, b repositoryScan) int {
		return cmp.Compare(a.previous.fullAt, b.previous.fullAt)
	})
	fullCount := (len(work) + fullPollCycles - 1) / fullPollCycles
	for i := range fullCount {
		work[i].full = true
	}
	order := make([]repositoryScan, 0, len(work))
	rest := make([]repositoryScan, 0, len(work))
	for _, target := range work {
		active := target.recentJob || target.previous.activeAt >= cutoff.UnixNano()
		if active && len(order) < maxPriorityRepos {
			order = append(order, target)
		} else {
			rest = append(rest, target)
		}
	}
	return append(order, rest...), nil
}

func (p *poller) scanRepository(ctx context.Context, target repositoryScan) (bool, error) {
	return p.walkWorkflowJobs(ctx, target.name, target.full, func(candidate workflowJob) error {
		if candidate.Status != "queued" {
			return nil
		}
		job, eligible, err := candidate.queueJob(p.github.owner, target.repo, p.installationID, candidate.Status)
		if err != nil || !eligible {
			return err
		}
		job.discoverySource = "poll"
		added, err := p.queue.record(ctx, job, time.Now())
		if added {
			slog.Info("queued job discovered", "source", "poll", "repository", job.repository, "job", job.id)
		}
		return err
	})
}

func (p *poller) walkWorkflowJobs(ctx context.Context, name string, full bool, visit func(workflowJob) error) (bool, error) {
	base := "/repos/" + name + "/actions/runs"
	seen := make(map[int64]bool)
	statuses := []string{"queued", "in_progress"}
	if full {
		statuses = append(statuses, "requested", "waiting", "pending")
	}
	for _, status := range statuses {
		runs, err := polledPages[struct{ ID int64 }](ctx, p, base+"?per_page=100&status="+status, "workflow_runs")
		if err != nil {
			return len(seen) > 0, err
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
				return true, err
			}
			for _, candidate := range jobs {
				if candidate.RunID != run.ID {
					continue
				}
				if err := visit(candidate); err != nil {
					return true, err
				}
			}
		}
	}
	return len(seen) > 0, nil
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
