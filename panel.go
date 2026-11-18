package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed panelweb/index.html panelweb/panel.css panelweb/panel.js panelweb/i18n.js panelweb/demo.js panelweb/manage.js
var panelAssets embed.FS

type panelSnapshot struct {
	Fleet         *statusReport    `json:"fleet"`
	Inventory     []panelRunner    `json:"runner_inventory"`
	Recent        []panelCompleted `json:"recent"`
	SampledAt     *time.Time       `json:"sampled_at,omitempty"`
	LastWebhook   *time.Time       `json:"last_webhook,omitempty"`
	LastPoll      *time.Time       `json:"last_poll,omitempty"`
	PollFailed    bool             `json:"poll_failed"`
	Polling       bool             `json:"polling"`
	Webhook       bool             `json:"webhook"`
	FirstJobSeen  bool             `json:"first_job_seen"`
	FirstJobClean *panelCompleted  `json:"first_job_clean,omitempty"`
}

type panelCompleted struct {
	ID         int64            `json:"id"`
	Repository string           `json:"repository"`
	RunnerHost string           `json:"runner_host,omitempty"`
	Pool       string           `json:"pool,omitempty"`
	URL        string           `json:"url,omitempty"`
	Timing     *statusJobTiming `json:"timing,omitempty"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
	CleanedAt  *time.Time       `json:"cleaned_at"`
}

type panelState struct {
	mu              sync.RWMutex
	snapshot        panelSnapshot
	backend         string
	repositoryScope string
	pools           workerPools
}

func newPanel(cfg config) *panelState {
	return &panelState{
		repositoryScope: cfg.repositoryScope,
		snapshot:        panelSnapshot{Polling: cfg.pollInterval > 0, Webhook: cfg.secret != ""},
	}
}

func (p *panelState) markWebhook(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot.LastWebhook = &at
}

func (p *panelState) markPoll(at time.Time, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot.PollFailed = !ok
	if ok {
		p.snapshot.LastPoll = &at
	}
}

func (p *panelState) capture(ctx context.Context, q *queue, hosts []hostCapacity, unassigned, limit int, probeErr error) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	jobs, err := readStatusJobs(ctx, q.DB)
	if err != nil {
		slog.Warn("read panel jobs", "error", err)
		return
	}
	now := time.Now()
	var firstSeen bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (
        SELECT 1 FROM jobs WHERE route GLOB '*-first-job' AND discovered_at IS NOT NULL
    )`).Scan(&firstSeen); err != nil {
		slog.Warn("read first job progress", "error", err)
		return
	}
	recent, firstClean, err := readPanelCompleted(ctx, q, now, p.pools)
	if err != nil {
		slog.Warn("read panel job history", "error", err)
		return
	}
	report := summarizeStatus(jobs, hosts, unassigned, limit, now)
	report.configure(p.backend, p.repositoryScope, p.pools, jobs, hosts)
	inventory := panelRunnerInventory(hosts, report.Jobs)
	if probeErr != nil {
		report.DockerError = "Runner host discovery failed. Check the manager logs."
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot.Fleet = &report
	p.snapshot.Inventory = inventory
	p.snapshot.Recent = recent
	p.snapshot.SampledAt = &now
	p.snapshot.FirstJobSeen = firstSeen
	p.snapshot.FirstJobClean = firstClean
}

func readPanelCompleted(ctx context.Context, q *queue, now time.Time, pools workerPools) ([]panelCompleted, *panelCompleted, error) {
	rows, err := q.QueryContext(ctx, `WITH relevant AS (
        SELECT job_id FROM (
            SELECT job_id FROM jobs WHERE runner_cleaned_at IS NOT NULL
            ORDER BY runner_cleaned_at DESC, sequence DESC LIMIT 8
        )
        UNION SELECT job_id FROM (
            SELECT job_id FROM jobs WHERE runner_cleaned_at IS NOT NULL
            AND github_started_at IS NOT NULL AND route GLOB '*-first-job'
            ORDER BY runner_cleaned_at DESC, sequence DESC LIMIT 1
        )
    )
        SELECT job_id, repository, route, changed_at,
        coalesce(runner_cleaned_host, ''), runner_cleaned_at, coalesce(github_created_at, 0),
        coalesce(discovered_at, 0), coalesce(discovery_source, ''), coalesce(provisioned_at, 0),
        coalesce(github_started_at, 0)
        FROM jobs JOIN relevant USING (job_id)
        ORDER BY runner_cleaned_at DESC, sequence DESC`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	recent := make([]panelCompleted, 0, 8)
	var firstClean *panelCompleted
	for rows.Next() {
		var j job
		var cleanedAt int64
		if err := rows.Scan(&j.id, &j.repository, &j.route, &j.changedAt, &j.runnerHost, &cleanedAt,
			&j.githubCreatedAt, &j.discoveredAt, &j.discoverySource, &j.provisionedAt, &j.githubStartedAt); err != nil {
			return nil, nil, err
		}
		j.phase = completed
		pool, _ := pools.forJob(j)
		record := panelCompleted{ID: j.id, Repository: j.repository, URL: jobURL(j),
			RunnerHost: j.runnerHost, Pool: pool, Timing: jobTiming(j, now),
			FinishedAt: statusTime(j.changedAt), CleanedAt: statusTime(cleanedAt)}
		if len(recent) < cap(recent) {
			recent = append(recent, record)
		}
		if firstClean == nil && j.githubStartedAt != 0 && strings.HasSuffix(j.route, "-first-job") {
			firstClean = &record
		}
	}
	return recent, firstClean, rows.Err()
}

func panelHTTPServer(panel *panelState, access *panelAccess) *http.Server {
	return &http.Server{
		Handler: panelHandler(panel, access), ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
}

func panelHandler(panel *panelState, access *panelAccess) http.Handler {
	assets, err := fs.Sub(panelAssets, "panelweb")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(assets)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if panel == nil {
			files.ServeHTTP(w, r)
			return
		}
		page, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			http.Error(w, "Panel page unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, strings.Replace(string(page), `data-mode="demo"`, `data-mode="live"`, 1))
	})
	mux.Handle("GET /panel.css", files)
	mux.Handle("GET /panel.js", files)
	mux.Handle("GET /i18n.js", files)
	if panel == nil {
		mux.Handle("GET /demo.js", files)
	} else if access != nil {
		mux.Handle("GET /manage.js", files)
		access.register(mux, panel)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		validHost := isPanelLoopback(host)
		if access != nil {
			validHost = strings.EqualFold(r.Host, access.cfg.host)
		}
		if !validHost {
			http.Error(w, "Use the configured panel address.", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		mux.ServeHTTP(w, r)
	})
}

func runPanelDemo(ctx context.Context, port uint) error {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10)))
	if err != nil {
		return err
	}
	server := panelHTTPServer(nil, nil)
	fmt.Printf("Runquill panel demo at http://%s\n", listener.Addr())
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			return errors.Join(err, server.Close())
		}
		return nil
	}
}
