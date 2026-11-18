package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the Runquill version")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: runquill [--version] | runquill setup | runquill workflow init | runquill doctor | runquill diagnose OWNER/REPO | runquill status [--json] | runquill gc --dry-run | runquill host {cordon|uncordon} HOST | runquill panel --demo")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if flag.Arg(0) == "panel" {
		flags := flag.NewFlagSet("runquill panel", flag.ExitOnError)
		demo := flags.Bool("demo", false, "serve the panel with example data")
		port := flags.Uint("port", 8081, "loopback HTTP port (0 selects a free port)")
		flags.Parse(flag.Args()[1:])
		if flags.NArg() != 0 || !*demo || *port > 65535 {
			flags.Usage()
			os.Exit(2)
		}
		if err := runPanelDemo(ctx, *port); err != nil {
			slog.Error("runquill panel", "error", err)
			os.Exit(1)
		}
		return
	}
	if flag.Arg(0) == "workflow" {
		if flag.NArg() != 2 || flag.Arg(1) != "init" {
			fmt.Fprintln(os.Stderr, "Usage: runquill workflow init")
			os.Exit(2)
		}
		if err := initWorkflow("."); err != nil {
			slog.Error("runquill workflow init", "error", err)
			os.Exit(1)
		}
		fmt.Printf("Created %s. Commit it to your private repository's default branch, then run Runquill first job from the GitHub Actions tab.\n", firstWorkflowPath)
		return
	}
	if flag.Arg(0) == "setup" {
		flags := flag.NewFlagSet("runquill setup", flag.ExitOnError)
		var options setupOptions
		flags.StringVar(&options.webhookURL, "webhook-url", "", "public HTTPS URL ending in /webhook (otherwise use polling)")
		flags.IntVar(&options.port, "listen-port", 0, "local browser setup port (0 selects a free port)")
		flags.Usage = func() {
			fmt.Fprintln(flags.Output(), "Usage: runquill setup [--webhook-url URL] [--listen-port PORT]")
			fmt.Fprintln(flags.Output(), "Connect a personal GitHub App and create .env and github-app.pem in the current directory.")
			flags.PrintDefaults()
		}
		flags.Parse(flag.Args()[1:])
		if flags.NArg() != 0 {
			flags.Usage()
			os.Exit(2)
		}
		if err := setup(ctx, os.Stdout, ".", &http.Client{Timeout: 10 * time.Second}, githubAPIBase, options); err != nil {
			slog.Error("runquill setup", "error", err)
			os.Exit(1)
		}
		return
	}
	if flag.Arg(0) == "status" {
		flags := flag.NewFlagSet("runquill status", flag.ExitOnError)
		jsonOutput := flags.Bool("json", false, "write the status snapshot as JSON")
		flags.Parse(flag.Args()[1:])
		if flags.NArg() != 0 {
			flags.Usage()
			os.Exit(2)
		}
		if err := status(ctx, os.Stdout, *jsonOutput); err != nil {
			slog.Error("runquill status", "error", err)
			os.Exit(1)
		}
		return
	}
	if flag.Arg(0) == "host" {
		action := flag.Arg(1)
		if flag.NArg() != 3 || (action != "cordon" && action != "uncordon") {
			fmt.Fprintln(os.Stderr, "Usage: runquill host {cordon|uncordon} HOST")
			os.Exit(2)
		}
		if err := setHostCordon(ctx, os.Stdout, flag.Arg(2), action == "cordon"); err != nil {
			slog.Error("runquill host", "error", err)
			os.Exit(1)
		}
		return
	}
	if flag.Arg(0) == "gc" {
		flags := flag.NewFlagSet("runquill gc", flag.ExitOnError)
		dryRun := flags.Bool("dry-run", false, "inspect runners and leases without changes")
		flags.Parse(flag.Args()[1:])
		if !*dryRun || flags.NArg() != 0 {
			flags.Usage()
			os.Exit(2)
		}
		if err := gc(ctx, os.Stdout); err != nil {
			slog.Error("runquill gc", "error", err)
			os.Exit(1)
		}
		return
	}
	if flag.Arg(0) == "diagnose" {
		if flag.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "Usage: runquill diagnose OWNER/REPO")
			os.Exit(2)
		}
		if err := diagnose(ctx, os.Stdout, flag.Arg(1)); err != nil {
			slog.Error("runquill diagnose", "error", err)
			os.Exit(1)
		}
		return
	}
	if flag.Arg(0) == "doctor" {
		flags := flag.NewFlagSet("runquill doctor", flag.ExitOnError)
		repo := flags.String("repo", "", "also check whether the App covers this repository")
		flags.Parse(flag.Args()[1:])
		if flags.NArg() != 0 {
			flags.Usage()
			os.Exit(2)
		}
		if err := doctor(ctx, os.Stdout, *repo); err != nil {
			os.Exit(1)
		}
		return
	}
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(ctx); err != nil {
		slog.Error("runquill", "error", err)
		os.Exit(1)
	}
}

type config struct {
	owner, appID, secret, keyPath, runnerImage string
	limit                                      int
	pollInterval                               time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		owner:       strings.TrimSpace(os.Getenv("RUNQUILL_OWNER")),
		appID:       strings.TrimSpace(os.Getenv("RUNQUILL_APP_ID")),
		secret:      os.Getenv("RUNQUILL_WEBHOOK_SECRET"),
		keyPath:     os.Getenv("RUNQUILL_KEY_FILE"),
		runnerImage: env("RUNQUILL_RUNNER_IMAGE", "runquill-runner:local"),
	}
	var missing []string
	for _, setting := range []struct{ name, value string }{
		{"RUNQUILL_OWNER", cfg.owner}, {"RUNQUILL_APP_ID", cfg.appID},
		{"RUNQUILL_KEY_FILE", cfg.keyPath},
	} {
		if setting.value == "" {
			missing = append(missing, setting.name)
		}
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("set %s", strings.Join(missing, ", "))
	}
	limit, err := runnerLimit()
	if err != nil {
		return cfg, err
	}
	cfg.limit = limit
	cfg.pollInterval, err = time.ParseDuration(env("RUNQUILL_POLL_INTERVAL", "30s"))
	if err != nil || cfg.pollInterval < 0 {
		return cfg, errors.New("RUNQUILL_POLL_INTERVAL must be a duration such as 30s, or 0 to disable polling")
	}
	if cfg.pollInterval == 0 && cfg.secret == "" {
		return cfg, errors.New("set RUNQUILL_WEBHOOK_SECRET or enable GitHub polling")
	}
	return cfg, nil
}

func runnerLimit() (int, error) {
	limit, err := strconv.Atoi(env("RUNQUILL_MAX_RUNNERS", "2"))
	if err != nil || limit < 1 {
		return 0, errors.New("RUNQUILL_MAX_RUNNERS must be a positive integer")
	}
	return limit, nil
}

func (cfg config) github() (*github, error) {
	keyData, err := os.ReadFile(cfg.keyPath)
	if err != nil {
		return nil, fmt.Errorf("read GitHub App key: %w", err)
	}
	key, err := parseKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App key: %w", err)
	}
	return &github{client: &http.Client{Timeout: 10 * time.Second}, base: githubAPIBase,
		appID: cfg.appID, key: key, owner: cfg.owner}, nil
}

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	api, err := cfg.github()
	if err != nil {
		return err
	}
	runtime, err := newDockerFleet(cfg.runnerImage, env("RUNQUILL_DOCKER_HOSTS", "local"))
	if err != nil {
		return err
	}
	hostLimits, err := runtime.limits(os.Getenv("RUNQUILL_DOCKER_HOST_LIMITS"))
	if err != nil {
		return err
	}
	pools, err := runtime.configurePools(os.Getenv("RUNQUILL_WORKER_POOLS"))
	if err != nil {
		return err
	}
	if len(runtime.hosts) == 1 {
		if err := runtime.prepare(ctx); err != nil {
			return err
		}
	}
	q, err := openQueue(env("RUNQUILL_DB", "runquill.sqlite3"))
	if err != nil {
		return err
	}
	defer q.close()

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
		panelServer = panelHTTPServer(panel, access)
		panelListener, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen for panel: %w", err)
		}
		defer panelListener.Close()
	}
	s := &scheduler{queue: q, github: api, runtime: runtime, limit: cfg.limit, hostLimits: hostLimits, panel: panel, pools: pools}
	server := &http.Server{
		Addr:              net.JoinHostPort(env("RUNQUILL_BIND", "127.0.0.1"), env("RUNQUILL_PORT", "8080")),
		Handler:           webhook(q, cfg.owner, []byte(cfg.secret), panel),
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
	slog.Info("Runquill listening", "address", listener.Addr(), "slots", cfg.limit, "version", version,
		"webhook", cfg.secret != "", "poll_interval", cfg.pollInterval, "docker_hosts", len(runtime.hosts))
	serverExit := make(chan error, 2)
	go func() { serverExit <- server.Serve(listener) }()
	if panelServer != nil {
		slog.Info("Runquill panel listening", "address", panelListener.Addr())
		go func() { serverExit <- panelServer.Serve(panelListener) }()
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	var pollExit chan struct{}
	if cfg.pollInterval > 0 {
		pollExit = make(chan struct{})
		p := &poller{github: api, queue: q, cache: make(map[string]pollPage), panel: panel}
		go func() {
			defer close(pollExit)
			p.run(workerCtx, cfg.pollInterval)
		}()
	}
	workerExit := make(chan struct{})
	go func() {
		defer close(workerExit)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := s.tick(workerCtx, time.Now()); err != nil && workerCtx.Err() == nil {
				slog.Error("scheduler tick", "error", err)
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
	if pollExit != nil {
		<-pollExit
	}
	return serveErr
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
