package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// panelTokenScopes are the scopes POST /api/tokens can grant.
var panelTokenScopes = []string{"fleet:read", "hosts:write", "jobs:write"}

// panelTokenCanCall reports whether any token scope reaches a permission, using
// the same check the panel applies, so the CLI never offers what tokens cannot do.
func panelTokenCanCall(permission string) bool {
	for _, scope := range panelTokenScopes {
		if panelPermission(panelIdentity{Role: "admin", Scope: scope}, permission) {
			return true
		}
	}
	return false
}

// remoteTokenCanCall is replaced in tests to exercise commands tokens cannot call yet.
var remoteTokenCanCall = panelTokenCanCall

var remoteNow = time.Now

const remoteUsage = `Usage:
  runquill jobs list [--repo OWNER/NAME] [--outcome waiting|running|finished|failed] [--since 24h] [--limit N] [--json]
  runquill jobs show JOB_ID [--json]
  runquill jobs cancel JOB_ID [--json]
  runquill jobs retry JOB_ID [--json]
  runquill runners list [--json]
  runquill pools list [--json]
  runquill hosts list [--json]
  runquill hosts cordon HOST [--json]
  runquill hosts uncordon HOST [--json]
  runquill audit tail [--limit N] [--json]
Each command reads the panel origin from --url or RUNQUILL_URL and the API token
from --token-file or RUNQUILL_TOKEN.`

type remoteCommandSpec struct {
	permission string
	args       []string
	define     func(*flag.FlagSet, *remoteOptions)
	validate   func(*remoteOptions, []string) error
	run        func(context.Context, *apiClient, *remoteOptions, []string, io.Writer) error
}

type remoteOptions struct {
	url, tokenFile       string
	json                 bool
	repo, outcome, since string
	limit                int
	sinceTime            time.Time
	jobID                int64
	stderr               io.Writer
}

var remoteGroups = []string{"jobs", "runners", "pools", "hosts", "audit"}

var remoteCommands = map[string]remoteCommandSpec{
	"jobs list": {permission: "fleet:read", define: func(f *flag.FlagSet, o *remoteOptions) {
		f.StringVar(&o.repo, "repo", "", "only jobs of this OWNER/NAME repository")
		f.StringVar(&o.outcome, "outcome", "", "waiting, running, finished or failed")
		f.StringVar(&o.since, "since", "", "only jobs queued within this duration, such as 24h or 7d")
		f.IntVar(&o.limit, "limit", 50, "number of jobs to show (1-100)")
	}, validate: validateJobsList, run: runJobsList},
	"jobs show": {permission: "fleet:read", args: []string{"JOB_ID"}, validate: validateJobID, run: runJobsShow},
	"jobs cancel": {permission: "jobs:write", args: []string{"JOB_ID"}, validate: validateJobID, run: func(ctx context.Context, c *apiClient, o *remoteOptions, _ []string, out io.Writer) error {
		return runJobChange(ctx, c, o, out, "cancel", "Cancelled job %d.\n")
	}},
	"jobs retry": {permission: "jobs:write", args: []string{"JOB_ID"}, validate: validateJobID, run: func(ctx context.Context, c *apiClient, o *remoteOptions, _ []string, out io.Writer) error {
		return runJobChange(ctx, c, o, out, "retry", "Cleared the retry delay of job %d.\n")
	}},
	"runners list": {permission: "fleet:read", run: runRunnersList},
	"pools list":   {permission: "fleet:read", run: runPoolsList},
	"hosts list":   {permission: "fleet:read", run: runHostsList},
	"hosts cordon": {permission: "hosts:write", args: []string{"HOST"}, validate: validateHost, run: func(ctx context.Context, c *apiClient, o *remoteOptions, args []string, out io.Writer) error {
		return runHostCordon(ctx, c, o, args[0], true, out)
	}},
	"hosts uncordon": {permission: "hosts:write", args: []string{"HOST"}, validate: validateHost, run: func(ctx context.Context, c *apiClient, o *remoteOptions, args []string, out io.Writer) error {
		return runHostCordon(ctx, c, o, args[0], false, out)
	}},
	"audit tail": {permission: "admin", define: func(f *flag.FlagSet, o *remoteOptions) {
		f.IntVar(&o.limit, "limit", 20, "number of entries to show (1-200)")
	}, validate: func(o *remoteOptions, _ []string) error {
		if o.limit < 1 || o.limit > 200 {
			return errors.New("--limit must be between 1 and 200")
		}
		return nil
	}, run: runAuditTail},
}

// remoteCommand runs the remote panel commands. handled is false for any other
// subcommand, so main can fall through to its own dispatch.
func remoteCommand(args []string, stdout, stderr io.Writer) (handled bool, code int) {
	if len(args) == 0 || !slices.Contains(remoteGroups, args[0]) {
		return false, 0
	}
	name := args[0]
	if len(args) > 1 {
		name += " " + args[1]
	}
	spec, ok := remoteCommands[name]
	if !ok {
		fmt.Fprintln(stderr, remoteUsage)
		return true, 2
	}
	return true, spec.execute(name, args[2:], stdout, stderr)
}

func (spec remoteCommandSpec) execute(name string, args []string, stdout, stderr io.Writer) int {
	for _, arg := range args {
		flagName, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if strings.HasPrefix(arg, "-") && flagName == "token" {
			fmt.Fprintln(stderr, "runquill: pass the API token with RUNQUILL_TOKEN or --token-file; a --token argument would be visible to other local users.")
			return 2
		}
	}
	usage := "Usage: runquill " + name
	for _, arg := range spec.args {
		usage += " " + arg
	}
	options := &remoteOptions{stderr: stderr}
	flags := flag.NewFlagSet("runquill "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&options.url, "url", "", "panel origin (defaults to RUNQUILL_URL)")
	flags.StringVar(&options.tokenFile, "token-file", "", "file holding the API token (defaults to RUNQUILL_TOKEN)")
	flags.BoolVar(&options.json, "json", false, "print the panel's JSON instead of a table")
	if spec.define != nil {
		spec.define(flags, options)
	}
	flags.Usage = func() {
		fmt.Fprintln(stderr, usage+" [flags]")
		flags.PrintDefaults()
	}
	positional, err := parseInterspersed(flags, args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	if len(positional) != len(spec.args) {
		flags.Usage()
		return 2
	}
	if spec.validate != nil {
		if err := spec.validate(options, positional); err != nil {
			fmt.Fprintf(stderr, "runquill %s: %v\n", name, err)
			return 2
		}
	}
	if !remoteTokenCanCall(spec.permission) {
		fmt.Fprintf(stderr, "runquill %s needs the %s permission, which the panel grants only to signed-in users, not to API tokens. Use the web panel instead.\n", name, spec.permission)
		return 1
	}
	client, err := options.client()
	if err != nil {
		fmt.Fprintf(stderr, "runquill %s: %v\n", name, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiClientTimeout)
	defer cancel()
	if err := spec.run(ctx, client, options, positional, stdout); err != nil {
		fmt.Fprintf(stderr, "runquill %s: %v\n", name, err)
		return 1
	}
	return 0
}

// parseInterspersed accepts flags before and after positional arguments.
func parseInterspersed(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		args = flags.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional, args = append(positional, args[0]), args[1:]
	}
}

func (o *remoteOptions) client() (*apiClient, error) {
	origin := cmp.Or(o.url, os.Getenv("RUNQUILL_URL"))
	if origin == "" {
		return nil, errors.New("set RUNQUILL_URL or --url to the panel origin")
	}
	token := os.Getenv("RUNQUILL_TOKEN")
	if o.tokenFile != "" {
		file, err := os.Open(o.tokenFile)
		if err != nil {
			return nil, fmt.Errorf("could not open the token file: %w", err)
		}
		defer file.Close()
		if info, err := file.Stat(); err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			fmt.Fprintf(o.stderr, "warning: %s is readable by other users; run chmod 600 on it.\n", o.tokenFile)
		}
		data, err := io.ReadAll(io.LimitReader(file, 1<<10))
		if err != nil {
			return nil, fmt.Errorf("could not read the token file: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return nil, errors.New("set RUNQUILL_TOKEN or --token-file to a panel API token")
	}
	return newAPIClient(origin, token)
}

func validateJobsList(o *remoteOptions, _ []string) error {
	o.repo = strings.ToLower(strings.TrimSpace(o.repo))
	if o.repo != "" && !repositoryName.MatchString(o.repo) {
		return errors.New("--repo must be OWNER/NAME")
	}
	switch o.outcome {
	case "", "waiting", "running", "finished", "failed":
	default:
		return errors.New("--outcome must be waiting, running, finished or failed")
	}
	if o.since != "" {
		age, err := parseRemoteAge(o.since)
		if err != nil {
			return err
		}
		o.sinceTime = remoteNow().Add(-age)
	}
	if o.limit < 1 || o.limit > panelHistoryMaxPage {
		return fmt.Errorf("--limit must be between 1 and %d", panelHistoryMaxPage)
	}
	return nil
}

// parseRemoteAge reads a Go duration or a whole number of days such as 7d.
func parseRemoteAge(raw string) (time.Duration, error) {
	age, err := time.ParseDuration(raw)
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, dayErr := strconv.Atoi(days)
		age, err = time.Duration(n)*24*time.Hour, dayErr
		if n > 3650 {
			err = errors.New("too long")
		}
	}
	if err != nil || age <= 0 {
		return 0, errors.New("--since must be a positive duration such as 24h or 7d")
	}
	return age, nil
}

func validateJobID(o *remoteOptions, args []string) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id < 1 {
		return errors.New("JOB_ID must be a positive GitHub job ID")
	}
	o.jobID = id
	return nil
}

func validateHost(_ *remoteOptions, args []string) error {
	if strings.TrimSpace(args[0]) == "" || len(args[0]) > 255 {
		return errors.New("HOST must be a host name from runquill hosts list")
	}
	return nil
}

func runJobsList(ctx context.Context, c *apiClient, o *remoteOptions, _ []string, out io.Writer) error {
	page, raw, err := c.jobHistory(ctx, jobHistoryQuery{repository: o.repo, outcome: o.outcome, since: o.sinceTime, limit: o.limit})
	if err != nil {
		return err
	}
	if o.json {
		return writeRawJSON(out, raw)
	}
	if len(page.Jobs) == 0 {
		_, err := fmt.Fprintln(out, "No jobs match.")
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "JOB\tREPOSITORY\tOUTCOME\tPOOL\tHOST\tQUEUED\tWAIT\tDETAIL")
	for _, job := range page.Jobs {
		wait := "-"
		if job.WaitSeconds != nil {
			wait = remoteDuration(time.Duration(*job.WaitSeconds) * time.Second)
		}
		detail := cmp.Or(job.Failure, job.RetryReason)
		if job.Diagnostic != nil {
			detail = cmp.Or(job.Failure, job.Diagnostic.Message, job.RetryReason)
		}
		if job.Timing != nil && job.Timing.JITRequestedAt != nil {
			if timing := job.Timing.text(); timing != "" {
				if detail != "" {
					detail += " · "
				}
				detail += timing
			}
		}
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", job.ID, cell(job.Repository), cell(job.Outcome), cell(job.Pool),
			cell(job.Host), remoteAgo(job.QueuedAt), wait, cell(detail))
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if page.Next != 0 {
		fmt.Fprintf(o.stderr, "More jobs match; showing the newest %d.\n", len(page.Jobs))
	}
	return nil
}

func runJobsShow(ctx context.Context, c *apiClient, o *remoteOptions, _ []string, out io.Writer) error {
	row, raw, err := c.job(ctx, o.jobID)
	if err != nil {
		return err
	}
	if o.json {
		return writeRawJSON(out, raw)
	}
	wait, diagnostic, timing := "-", "", ""
	if row.WaitSeconds != nil {
		wait = remoteDuration(time.Duration(*row.WaitSeconds) * time.Second)
	}
	if row.Diagnostic != nil {
		diagnostic = row.Diagnostic.Message
	}
	if row.Timing != nil {
		timing = row.Timing.text()
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, field := range [][2]string{
		{"JOB", strconv.FormatInt(row.ID, 10)},
		{"REPOSITORY", cell(row.Repository)},
		{"OUTCOME", cell(row.Outcome)},
		{"LABEL", cell(row.RunnerLabel)},
		{"POOL", cell(row.Pool)},
		{"OS", cell(row.RunnerOS)},
		{"HOST", cell(row.Host)},
		{"QUEUED", remoteTime(row.QueuedAt)},
		{"STARTED", remoteTime(row.StartedAt)},
		{"FINISHED", remoteTime(row.FinishedAt)},
		{"CLEANED", remoteTime(row.CleanedAt)},
		{"WAIT", wait},
		{"FAILURE", cell(row.Failure)},
		{"RETRY", cell(row.RetryReason)},
		{"RETRY_AT", remoteTime(row.RetryAt)},
		{"DIAGNOSTIC", cell(diagnostic)},
		{"TIMING", cell(timing)},
		{"URL", cell(row.URL)},
	} {
		fmt.Fprintf(table, "%s\t%s\n", field[0], field[1])
	}
	return table.Flush()
}

func runJobChange(ctx context.Context, c *apiClient, o *remoteOptions, out io.Writer, action, done string) error {
	if err := c.changeJob(ctx, o.jobID, action); err != nil {
		return err
	}
	if o.json {
		return json.NewEncoder(out).Encode(struct {
			JobID  int64  `json:"job_id"`
			Action string `json:"action"`
		}{o.jobID, action})
	}
	_, err := fmt.Fprintf(out, done, o.jobID)
	return err
}

func runRunnersList(ctx context.Context, c *apiClient, o *remoteOptions, _ []string, out io.Writer) error {
	snapshot, raw, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	if o.json {
		var fields struct {
			Inventory json.RawMessage `json:"runner_inventory"`
		}
		json.Unmarshal(raw, &fields)
		return writeRawJSON(out, nullAsEmptyList(fields.Inventory))
	}
	if len(snapshot.Inventory) == 0 {
		_, err := fmt.Fprintln(out, "No runners.")
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "RUNNER\tHOST\tPOOL\tSTATE\tJOB")
	for _, runner := range snapshot.Inventory {
		job := "-"
		if runner.JobID != 0 {
			job = strconv.FormatInt(runner.JobID, 10)
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", cell(runner.Name), cell(runner.Host), cell(runner.Pool), cell(runner.RuntimeState), job)
	}
	return table.Flush()
}

func fleetHosts(ctx context.Context, c *apiClient) ([]statusHost, json.RawMessage, error) {
	snapshot, raw, err := c.snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	if snapshot.Fleet == nil {
		return nil, nil, errors.New("the manager has not sampled the fleet yet; try again shortly")
	}
	var fields struct {
		Fleet struct {
			Hosts json.RawMessage `json:"hosts"`
		} `json:"fleet"`
	}
	json.Unmarshal(raw, &fields)
	return snapshot.Fleet.Hosts, nullAsEmptyList(fields.Fleet.Hosts), nil
}

func runHostsList(ctx context.Context, c *apiClient, o *remoteOptions, _ []string, out io.Writer) error {
	hosts, raw, err := fleetHosts(ctx, c)
	if err != nil {
		return err
	}
	if o.json {
		return writeRawJSON(out, raw)
	}
	if len(hosts) == 0 {
		_, err := fmt.Fprintln(out, "No runner hosts.")
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "HOST\tPOOL\tSTATUS\tCORDONED\tSLOTS\tUSED\tRESERVED\tERROR")
	for _, host := range hosts {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n", cell(host.Name), cell(host.Pool), cell(host.Status),
			yesNo(host.Cordoned), host.SlotLimit, host.OccupiedSlots, host.ReservedSlots, cell(host.Error))
	}
	return table.Flush()
}

// remotePool summarizes hosts by pool; the panel has no pool endpoint that
// tokens can read, so --json prints this summary rather than a panel response.
type remotePool struct {
	Pool          string `json:"pool"`
	Hosts         int    `json:"hosts"`
	Up            int    `json:"up"`
	Cordoned      int    `json:"cordoned"`
	SlotLimit     int    `json:"slot_limit"`
	OccupiedSlots int    `json:"occupied_slots"`
	ReservedSlots int    `json:"reserved_slots"`
}

func runPoolsList(ctx context.Context, c *apiClient, o *remoteOptions, _ []string, out io.Writer) error {
	hosts, _, err := fleetHosts(ctx, c)
	if err != nil {
		return err
	}
	pools := make([]remotePool, 0)
	for _, host := range hosts {
		index := slices.IndexFunc(pools, func(p remotePool) bool { return p.Pool == host.Pool })
		if index < 0 {
			index = len(pools)
			pools = append(pools, remotePool{Pool: host.Pool})
		}
		p := &pools[index]
		p.Hosts++
		p.SlotLimit += host.SlotLimit
		p.OccupiedSlots += host.OccupiedSlots
		p.ReservedSlots += host.ReservedSlots
		if host.Status == "UP" {
			p.Up++
		}
		if host.Cordoned {
			p.Cordoned++
		}
	}
	slices.SortFunc(pools, func(a, b remotePool) int { return cmp.Compare(a.Pool, b.Pool) })
	if o.json {
		return json.NewEncoder(out).Encode(pools)
	}
	if len(pools) == 0 {
		_, err := fmt.Fprintln(out, "No runner hosts.")
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "POOL\tHOSTS\tUP\tCORDONED\tSLOTS\tUSED\tRESERVED")
	for _, p := range pools {
		fmt.Fprintf(table, "%s\t%d\t%d\t%d\t%d\t%d\t%d\n", poolLabel(p.Pool), p.Hosts, p.Up, p.Cordoned,
			p.SlotLimit, p.OccupiedSlots, p.ReservedSlots)
	}
	return table.Flush()
}

func runHostCordon(ctx context.Context, c *apiClient, o *remoteOptions, host string, cordoned bool, out io.Writer) error {
	if err := c.setHostCordon(ctx, host, cordoned); err != nil {
		return err
	}
	if o.json {
		return json.NewEncoder(out).Encode(struct {
			Host     string `json:"host"`
			Cordoned bool   `json:"cordoned"`
		}{host, cordoned})
	}
	message := "Cordoned %s; it takes no new runners.\n"
	if !cordoned {
		message = "Uncordoned %s; it takes new runners again.\n"
	}
	_, err := fmt.Fprintf(out, message, cell(host))
	return err
}

func runAuditTail(ctx context.Context, c *apiClient, o *remoteOptions, _ []string, out io.Writer) error {
	events, raw, err := c.auditEvents(ctx, o.limit)
	if err != nil {
		return err
	}
	if o.json {
		return writeRawJSON(out, raw)
	}
	if len(events) == 0 {
		_, err := fmt.Fprintln(out, "No audit entries.")
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tTIME\tACTOR\tACTION\tTARGET")
	// The panel pages newest first; a tail reads oldest to newest.
	for _, event := range slices.Backward(events) {
		actor := strconv.FormatInt(event.Actor, 10)
		if event.ActorLogin != "" {
			actor = "@" + event.ActorLogin
		}
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\n", event.ID, time.Unix(event.OccurredAt, 0).UTC().Format(time.RFC3339),
			cell(actor), cell(event.Action), cell(event.Target))
	}
	return table.Flush()
}

func writeRawJSON(out io.Writer, raw []byte) error {
	raw = []byte(strings.TrimRight(string(raw), "\n"))
	_, err := fmt.Fprintf(out, "%s\n", raw)
	return err
}

func nullAsEmptyList(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("[]")
	}
	return raw
}

// cell makes panel text safe for one tabwriter cell.
func cell(text string) string {
	text = strings.ReplaceAll(terminalSafe(text, 120), "\t", " ")
	return cmp.Or(text, "-")
}

func poolLabel(pool string) string {
	if pool == "" {
		return "(default)"
	}
	return cell(pool)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func remoteAgo(at *time.Time) string {
	if at == nil {
		return "-"
	}
	return remoteDuration(remoteNow().Sub(*at)) + " ago"
}

// remoteTime prints a panel time in UTC with its age, or in for a future time.
func remoteTime(at *time.Time) string {
	if at == nil {
		return "-"
	}
	stamp := at.UTC().Format(time.RFC3339)
	if age := remoteNow().Sub(*at); age < 0 {
		return stamp + " (in " + remoteDuration(-age) + ")"
	}
	return stamp + " (" + remoteAgo(at) + ")"
}

func remoteDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}
