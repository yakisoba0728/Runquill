package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

const mainUsage = "Usage: runquill [--version] | runquill setup | runquill wizard | runquill workflow {init|check [--json] [--strict] [PATH...]} | runquill doctor [--repo OWNER/REPO] [--json] | runquill diagnose OWNER/REPO | runquill status [--json] | runquill gc (--dry-run | --remove-orphans [--grace DURATION] | --unused [--dry-run] [--older-than DURATION]) | runquill host {cordon|uncordon} HOST | runquill panel --demo | runquill worker | runquill backup (--output FILE | --upload [--output FILE]) [--seal [--passphrase-file FILE]] | runquill restore (--from FILE | --from-s3 KEY) [--passphrase-file FILE] [--allow-plaintext] | runquill restore --activate | runquill {fleet|jobs|runners|pools|hosts|audit} ... --help | runquill usage [--group day|repository|pool|host] [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--repo OWNER/NAME] [--json] | runquill usage --rightsizing [--group pool|job] [--days 1-7] [--json] | runquill elastic [--json] | runquill mcp"

// Main runs the manager or the selected Runquill subcommand.
func Main(buildVersion, registryImage string) {
	version, releaseImage = buildVersion, registryImage
	showVersion := flag.Bool("version", false, "print the Runquill version")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), mainUsage)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	handler, level, err := logHandler(os.Stderr, os.Getenv("RUNQUILL_LOG_FORMAT"), os.Getenv("RUNQUILL_LOG_LEVEL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if handler != nil {
		slog.SetDefault(slog.New(handler))
	} else {
		slog.SetLogLoggerLevel(level)
	}
	if flag.Arg(0) == "mcp" {
		os.Exit(mcpCommand(flag.Args()[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	if handled, code := remoteCommand(flag.Args(), os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if flag.Arg(0) == "worker" {
		if err := workerCommand(ctx, flag.Args()[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			slog.Error("runquill worker", "error", err)
			os.Exit(1)
		}
		return
	}
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
		os.Exit(workflowCommand(flag.Args()[1:], os.Stdout, os.Stderr))
	}
	if flag.Arg(0) == "wizard" {
		if err := wizardCommand(ctx, flag.Args()[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
			slog.Error("runquill wizard", "error", err)
			os.Exit(1)
		}
		return
	}
	if flag.Arg(0) == "setup" {
		flags := flag.NewFlagSet("runquill setup", flag.ExitOnError)
		var options setupOptions
		flags.StringVar(&options.webhookURL, "webhook-url", "", "public HTTPS URL ending in /webhook (otherwise use polling)")
		flags.IntVar(&options.port, "listen-port", 0, "local browser setup port (0 selects a free port)")
		flags.BoolVar(&options.panel, "panel", false, "guide GitHub OAuth and web panel configuration")
		flags.StringVar(&options.panelURL, "panel-url", "", "panel origin (defaults to http://127.0.0.1:8081)")
		flags.BoolVar(&options.kubernetes, "k3s", false, "print Kubernetes deployment instructions after setup")
		flags.StringVar(&options.repositoryScope, "repository-scope", "", "GitHub App repository access: all (default for new setups) or selected")
		flags.StringVar(&options.organization, "organization", "", "register the GitHub App in this organization")
		flags.Int64Var(&options.runnerGroupID, "runner-group-id", 0, "register organization runners in this GitHub runner group (defaults to repository runners)")
		migrate := flags.Bool("migrate", false, "also request Contents, Pull requests and Workflows write for workflow migration")
		flags.Usage = func() {
			fmt.Fprintln(flags.Output(), "Usage: runquill setup [--migrate] [--webhook-url URL] [--listen-port PORT] [--k3s] [--repository-scope all|selected] [--organization ORG [--runner-group-id ID]] [--panel [--panel-url URL]]")
			fmt.Fprintln(flags.Output(), "Connect a GitHub App and create .env and github-app.pem in the current directory.")
			flags.PrintDefaults()
		}
		flags.Parse(flag.Args()[1:])
		if flags.NArg() != 0 {
			flags.Usage()
			os.Exit(2)
		}
		if err := setup(withSetupMigrate(ctx, *migrate), os.Stdout, ".", &http.Client{Timeout: 10 * time.Second}, githubAPIBase, options); err != nil {
			slog.Error("runquill setup", "error", err)
			os.Exit(1)
		}
		return
	}
	if command := flag.Arg(0); command == "backup" || command == "restore" {
		if err := runQueueBackup(ctx, os.Stdout, command, flag.Args()[1:]); err != nil {
			slog.Error("runquill "+command, "error", err)
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
		os.Exit(gcCommand(ctx, flag.Args()[1:], os.Stdout, os.Stderr))
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
		asJSON := flags.Bool("json", false, "print the checks as JSON")
		flags.Parse(flag.Args()[1:])
		if flags.NArg() != 0 {
			flags.Usage()
			os.Exit(2)
		}
		if err := doctor(ctx, os.Stdout, *repo, *asJSON); err != nil {
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

// logHandler returns a nil handler when the standard log output should be kept.
func logHandler(w io.Writer, format, level string) (slog.Handler, slog.Level, error) {
	var minimum slog.Level
	if level != "" {
		if err := minimum.UnmarshalText([]byte(level)); err != nil {
			return nil, 0, errors.New("RUNQUILL_LOG_LEVEL must be debug, info, warn or error")
		}
	}
	options := &slog.HandlerOptions{Level: minimum}
	switch format {
	case "":
		return nil, minimum, nil
	case "text":
		return slog.NewTextHandler(w, options), minimum, nil
	case "json":
		return slog.NewJSONHandler(w, options), minimum, nil
	default:
		return nil, 0, errors.New("RUNQUILL_LOG_FORMAT must be text or json")
	}
}
