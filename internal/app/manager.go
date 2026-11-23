package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	backups, err := loadBackupScheduleConfig(os.Getenv)
	if err != nil {
		return err
	}
	api, err := cfg.github()
	if err != nil {
		return err
	}
	api.ready = &readiness{}
	// The notify URL is a secret: its errors never include it.
	notify, err := notifierFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("configure RUNQUILL_NOTIFY_*: %w", err)
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := notify.Close(closing); err != nil {
			slog.Warn("deliver pending notifications", "error", err)
		}
	}()
	runners, err := configureRunnerRuntime(cfg.runnerImage)
	if err != nil {
		return err
	}
	q, err := openQueue(env("RUNQUILL_DB", "runquill.sqlite3"))
	if err != nil {
		return err
	}
	defer q.close()
	if err := migrateUsage(ctx, q.DB); err != nil {
		return fmt.Errorf("migrate usage statistics: %w", err)
	}
	ops, err := loadOperations(ctx, q.DB, cfg, runners)
	if err != nil {
		return err
	}
	assets, err := startAssetCleanup(ctx, q.DB, ops)
	if err != nil {
		return err
	}
	defer assets.stop()
	elasticConfig, err := loadElasticConfig(os.Getenv)
	if err != nil {
		return err
	}
	elastic := newElasticState(elasticConfig)
	go runElastic(ctx, elasticDeps{Config: elasticConfig, State: elastic, Hosts: elasticHostsFrom(ops)})
	if fleet, ok := runners.runtime.(*dockerFleet); ok {
		go runImagePuller(ctx, imagePullerDeps{Images: fleet.images, Pairs: imagePairsFrom(ops)})
	}
	state := ops.current.Load()
	runners = state.runners
	if fleet, ok := runners.runtime.(*dockerFleet); ok && len(fleet.hosts) == 1 && state.policy.Revision == 0 {
		if err := fleet.prepare(ctx); err != nil {
			return err
		}
	}
	var workerAgents *workerFleet
	if fleet, ok := runners.runtime.(*dockerFleet); ok {
		workerAgents = fleet.workers
		if workerAgents != nil {
			workerAgents.operations = ops
			if err := workerAgents.attach(ctx, q.DB); err != nil {
				return err
			}
		}
	}

	var panel *panelState
	var panelServer *http.Server
	var panelListener net.Listener
	if addr := os.Getenv("RUNQUILL_PANEL_ADDR"); addr != "" {
		settings, err := loadPanelOAuthConfig()
		if err != nil {
			return err
		}
		access, err := newPanelAccess(ctx, q.DB, settings)
		if err != nil {
			return fmt.Errorf("initialize panel access: %w", err)
		}
		panel = newPanel(cfg)
		panel.elastic = elastic
		panel.backend, panel.operations = runners.kind, ops
		panel.snapshot.Polling = state.interval > 0
		panelServer = panelHTTPServer(panel, access)
		panelListener, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen for panel: %w", err)
		}
		defer panelListener.Close()
	}
	s := &scheduler{queue: q, github: api, panel: panel, operations: ops,
		alerts: &fleetAlerts{notifier: notify}, maxPerRepository: cfg.maxPerRepository, workers: cfg.workers}
	metrics, err := openMetrics(os.Getenv("RUNQUILL_METRICS_ADDR"), os.Getenv("RUNQUILL_METRICS_TOKEN_FILE"))
	if err != nil {
		return err
	}
	if metrics != nil {
		defer metrics.listener.Close()
		s.metrics = &metrics.fleetMetrics
		s.metrics.notify = notify
		s.metrics.elastic = elastic
		s.metrics.backend = runners.kind
	}
	server := &http.Server{
		Addr:              net.JoinHostPort(env("RUNQUILL_BIND", "127.0.0.1"), env("RUNQUILL_PORT", "8080")),
		Handler:           workerAgents.route(webhook(q, api, []byte(cfg.secret), panel)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       8 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	slog.Info("Runquill listening", "address", listener.Addr(), "slots", state.policy.Limit, "version", version,
		"webhook", cfg.secret != "", "poll_interval", state.interval, "runner_backend", runners.kind, "runner_hosts", runners.hostCount)
	serverExit := make(chan error, 3)
	go func() { serverExit <- server.Serve(listener) }()
	if panelServer != nil {
		slog.Info("Runquill panel listening", "address", panelListener.Addr())
		go func() { serverExit <- panelServer.Serve(panelListener) }()
	}
	if metrics != nil {
		slog.Info("Runquill metrics listening", "address", metrics.listener.Addr())
		go func() { serverExit <- metrics.server.Serve(metrics.listener) }()
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	backupExit := make(chan struct{})
	go func() {
		defer close(backupExit)
		runBackupSchedule(workerCtx, backupScheduleDeps{config: backups, database: env("RUNQUILL_DB", "runquill.sqlite3")})
	}()
	pollExit := make(chan struct{})
	p := &poller{github: api, queue: q, cache: make(map[string]pollPage), panel: panel}
	go func() {
		defer close(pollExit)
		p.run(workerCtx, ops)
	}()
	workerExit := make(chan struct{})
	go func() {
		defer close(workerExit)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := s.tick(workerCtx, time.Now()); err != nil && workerCtx.Err() == nil {
				slog.Error("scheduler tick", "error", err)
				if s.metrics != nil {
					s.metrics.tickErrors.Add(1)
				}
			}
			select {
			case <-ticker.C:
			case <-workerCtx.Done():
				return
			}
		}
	}()
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serverExit:
	}
	stopWorker()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if metrics != nil {
		if err := metrics.server.Shutdown(shutdown); err != nil {
			metrics.server.Close()
			serveErr = errors.Join(serveErr, err)
		}
	}
	if panelServer != nil {
		if err := panelServer.Shutdown(shutdown); err != nil {
			panelServer.Close()
			serveErr = errors.Join(serveErr, err)
		}
	}
	if err := server.Shutdown(shutdown); err != nil {
		server.Close()
		serveErr = errors.Join(serveErr, err)
	}
	<-workerExit
	<-pollExit
	<-backupExit
	return serveErr
}
