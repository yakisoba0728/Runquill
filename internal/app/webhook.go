package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxWebhookBytes = 2 << 20

var (
	repositoryName = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9_.-]+$`)
	routeSuffix    = regexp.MustCompile(`^[1-9][0-9]*-[a-z0-9][a-z0-9_-]*$`)
)

type repository struct {
	ID         int64  `json:"id"`
	FullName   string `json:"full_name"`
	Private    bool   `json:"private"`
	Visibility string `json:"visibility"`
	Archived   bool   `json:"archived"`
	Disabled   bool   `json:"disabled"`
	// DefaultBranch decides which runs may write a pool's shared cache; see cache_writes.
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type workflowJob struct {
	ID        int64    `json:"id"`
	RunID     int64    `json:"run_id"`
	Labels    []string `json:"labels"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
	StartedAt string   `json:"started_at"`
	// CompletedAt is set once GitHub reports the job completed.
	CompletedAt string `json:"completed_at"`
}

type workflowEvent struct {
	Action       string      `json:"action"`
	Repository   repository  `json:"repository"`
	Job          workflowJob `json:"workflow_job"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

func webhook(q *queue, api *github, secret []byte, panel *panelState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := q.PingContext(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		report := api.ready.report(time.Now())
		report.Database = "ok"
		if err := q.PingContext(r.Context()); err != nil {
			report.Database, report.Ready = "unavailable", false
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !report.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(report)
	})
	if len(secret) == 0 {
		return mux
	}
	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}
			return
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte(expected)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-GitHub-Event") == "workflow_job" {
			var event workflowEvent
			if err := json.Unmarshal(body, &event); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			job, eligible, err := event.Job.queueJob(api.owner, api.allowPublic, event.Repository, event.Installation.ID, event.Action)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if eligible {
				if api.organization || api.repositoryScope == scopeSelected {
					ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
					err := api.checkRepositoryAccess(ctx, job)
					cancel()
					if errors.Is(err, errRepositoryOutsideInstallation) {
						w.WriteHeader(http.StatusAccepted)
						return
					}
					if transientGitHubError(err) {
						// GitHub does not redeliver a failed webhook. The scheduler repeats this check before it
						// requests a token or registers a runner, so record the job instead of losing it.
						slog.Warn("verify webhook repository access; checking again before launch", "job", job.id, "error", err)
					} else if err != nil {
						slog.Warn("verify webhook repository access", "job", job.id, "error", err)
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
				}
				job.discoverySource = "webhook"
				changed, err := q.record(r.Context(), job, time.Now())
				if err != nil {
					slog.Error("save webhook", "job", job.id, "error", err)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if panel != nil {
					panel.markWebhook(time.Now())
				}
				if job.phase == queued {
					slog.Info("queued job received", "source", "webhook", "repository", job.repository, "job", job.id, "added", changed)
				}
			}
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}

// A successful scheduler tick older than this makes /readyz fail.
const readyTickMaxAge = 2 * time.Minute

// readiness records the outcome of the last scheduler tick and GitHub App authentication.
type readiness struct {
	mu                    sync.Mutex
	tickAt, tickSucceeded time.Time
	tickFailed            bool
	authAt                time.Time
	authFailed            bool
}

type readinessReport struct {
	Ready      bool       `json:"ready"`
	Database   string     `json:"database"`
	Scheduler  string     `json:"scheduler"`
	LastTick   *time.Time `json:"last_successful_tick,omitempty"`
	GitHubAuth string     `json:"github_auth"`
}

func (r *readiness) markTick(now time.Time, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tickAt, r.tickFailed = now, err != nil
	if err == nil {
		r.tickSucceeded = now
	}
}

func (r *readiness) markAuth(now time.Time, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authAt, r.authFailed = now, err != nil
}

// report omits error details because /readyz shares the webhook listener.
func (r *readiness) report(now time.Time) readinessReport {
	report := readinessReport{Ready: true, Scheduler: "unknown", GitHubAuth: "unknown"}
	if r == nil {
		return report
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.tickSucceeded.IsZero() {
		at := r.tickSucceeded.UTC()
		report.LastTick = &at
	}
	switch {
	case r.tickAt.IsZero():
		report.Scheduler, report.Ready = "starting", false
	case r.tickFailed:
		report.Scheduler, report.Ready = "failing", false
	case now.Sub(r.tickSucceeded) > readyTickMaxAge:
		report.Scheduler, report.Ready = "stalled", false
	default:
		report.Scheduler = "ok"
	}
	switch {
	case r.authAt.IsZero():
		// Runquill authenticates on demand; an idle queue has not needed a token yet.
	case r.authFailed:
		report.GitHubAuth, report.Ready = "failing", false
	default:
		report.GitHubAuth = "ok"
	}
	return report
}

func (repo repository) privateFor(owner string) bool {
	return repo.Private && (repo.Visibility == "" || repo.Visibility == "private") && strings.EqualFold(repo.Owner.Login, owner)
}

func (repo repository) publicFor(owner string) bool {
	return !repo.Private && (repo.Visibility == "" || repo.Visibility == "public") && strings.EqualFold(repo.Owner.Login, owner)
}

// acceptedFor reports whether Runquill takes jobs from repo: a private repository of owner, or
// a public one when allowPublic is set. Internal repositories are never accepted.
func (repo repository) acceptedFor(owner string, allowPublic bool) bool {
	return repo.privateFor(owner) || allowPublic && repo.publicFor(owner)
}

func (j workflowJob) queueJob(owner string, allowPublic bool, repo repository, installationID int64, status string) (job, bool, error) {
	p, known := phases[status]
	if !known || !repo.acceptedFor(owner, allowPublic) {
		return job{}, false, nil
	}
	if len(j.Labels) != 1 {
		return job{}, false, nil
	}
	route := strings.ToLower(j.Labels[0])
	prefix := "runquill-" + strconv.FormatInt(j.RunID, 10) + "-"
	suffix, hasPrefix := strings.CutPrefix(route, prefix)
	if !hasPrefix || len(route) > 100 || !routeSuffix.MatchString(suffix) {
		return job{}, false, nil
	}
	name, valid := ownedRepositoryPath(repo.FullName, owner)
	if !valid || j.ID <= 0 || j.RunID <= 0 || repo.ID <= 0 || installationID <= 0 {
		return job{}, false, errors.New("invalid workflow job")
	}
	observed := job{id: j.ID, repositoryID: repo.ID, installationID: installationID,
		repository: name, route: route, phase: p, githubCreatedAt: githubTimestamp(j.CreatedAt)}
	if p != queued {
		observed.githubStartedAt = githubTimestamp(j.StartedAt)
	}
	if p == completed {
		observed.githubCompletedAt = githubTimestamp(j.CompletedAt)
	}
	return observed, true, nil
}

func githubTimestamp(raw string) int64 {
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return 0
	}
	return max(0, at.Unix())
}

func ownedRepositoryPath(fullName, owner string) (string, bool) {
	name := strings.ToLower(fullName)
	login, slug, _ := strings.Cut(name, "/")
	return name, repositoryName.MatchString(name) && slug != "." && slug != ".." && strings.EqualFold(login, owner)
}
