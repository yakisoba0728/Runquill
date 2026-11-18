package main

import (
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
	"time"
)

const maxWebhookBytes = 2 << 20

var (
	repositoryName = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9_.-]+$`)
	routeSuffix    = regexp.MustCompile(`^[1-9][0-9]*-[a-z0-9][a-z0-9_-]*$`)
)

type repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	Disabled bool   `json:"disabled"`
	Owner    struct {
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
}

type workflowEvent struct {
	Action       string      `json:"action"`
	Repository   repository  `json:"repository"`
	Job          workflowJob `json:"workflow_job"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

func webhook(q *queue, owner string, secret []byte) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := q.PingContext(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
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
			job, eligible, err := event.Job.queueJob(owner, event.Repository, event.Installation.ID, event.Action)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if eligible {
				job.discoverySource = "webhook"
				changed, err := q.record(r.Context(), job, time.Now())
				if err != nil {
					slog.Error("save webhook", "job", job.id, "error", err)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
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

func (repo repository) privateFor(owner string) bool {
	return repo.Private && strings.EqualFold(repo.Owner.Login, owner)
}

func (j workflowJob) queueJob(owner string, repo repository, installationID int64, status string) (job, bool, error) {
	p, known := phases[status]
	if !known || !repo.privateFor(owner) {
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
