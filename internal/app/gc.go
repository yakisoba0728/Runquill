package app

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// defaultOrphanGrace is how long a runner must exist before gc --remove-orphans considers it.
const defaultOrphanGrace = 10 * time.Minute

func gc(ctx context.Context, out io.Writer, removeOrphanRunners bool, grace time.Duration) error {
	db, err := openQueueDB(env("RUNQUILL_DB", "runquill.sqlite3"), "ro")
	if err != nil {
		return err
	}
	defer db.Close()
	runners, err := gcRunners(ctx, db, "")
	if err != nil {
		return err
	}
	loadAPI := sync.OnceValues(func() (*github, error) {
		cfg, err := loadConfig()
		if err != nil {
			return nil, err
		}
		return cfg.github()
	})
	if removeOrphanRunners {
		var hosts []orphanHost
		fleet, ok := runners.runtime.(*dockerFleet)
		if !ok {
			fmt.Fprintf(out, "Skipped %s runners: orphan removal supports Docker hosts only.\n", runners.kind)
		} else {
			for _, host := range fleet.hosts {
				if host.agentName() != "" {
					fmt.Fprintf(out, "Skipped %s: orphan removal is not supported on outbound workers.\n", host.label())
					continue
				}
				hosts = append(hosts, host)
			}
		}
		return removeOrphans(ctx, out, db, hosts, loadAPI, grace, time.Now())
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hosts, err := runners.runtime.snapshot(ctx)
	if err != nil {
		return err
	}
	lookup := func(ctx context.Context, j job) (string, error) {
		api, err := loadAPI()
		if err != nil {
			return "", err
		}
		return api.readRunnerJobStatus(ctx, j)
	}
	return writeGCInventory(ctx, out, db, hosts, lookup)
}

// gcRunners reads the runner configuration the manager uses, saved settings first.
func gcRunners(ctx context.Context, db *sql.DB, image string) (runnerConfiguration, error) {
	runners, err := configureRunnerRuntime(image)
	if err != nil {
		return runners, err
	}
	if _, saved, err := readOperationPolicy(ctx, db, operationPolicy{}); err != nil {
		return runners, err
	} else if saved {
		state, err := appliedRunnerSettings(ctx, db, env("RUNQUILL_RUNNER_IMAGE", "runquill-runner:local"), 1)
		if err != nil {
			return runners, err
		}
		runners = state.runners
	}
	return runners, nil
}

func writeGCInventory(ctx context.Context, out io.Writer, db *sql.DB, hosts []dockerHostSnapshot, lookup func(context.Context, job) (string, error)) error {
	leases, queueErr := readRunnerLeases(ctx, db)
	if queueErr != nil {
		fmt.Fprintf(out, "Queue unavailable. Runner leases are unknown (%s)\n", strings.Join(strings.Fields(queueErr.Error()), " "))
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "HOST\tRUNNER\tSTATE\tLEASE\tGITHUB JOB")
	var failures []error
	listed := 0
	for _, host := range hosts {
		if host.Containers == nil {
			if host.Err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", host.Name, host.Err))
			} else {
				failures = append(failures, fmt.Errorf("%s: runner state unavailable", host.Name))
			}
			fmt.Fprintf(table, "%s\t-\toffline\tunknown\tunknown\n", host.Name)
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(host.Containers)) {
			owner, tracked := leases[name]
			if tracked && (owner == host.Name || owner == "") {
				continue
			}
			description := "untracked"
			switch {
			case queueErr != nil:
				description = "unknown"
			case tracked:
				description = "recorded on " + owner
			}
			githubJob := "unknown"
			if queueErr == nil {
				if id, matches := runnerJobID(name); matches {
					j, err := jobByID(ctx, db, id)
					if err == nil {
						status, err := lookup(ctx, j)
						switch {
						case err != nil:
							githubJob = "unavailable"
						case status != "":
							githubJob = status
						}
						if url := jobURL(j); url != "" {
							githubJob += " " + url
						}
					} else if !errors.Is(err, sql.ErrNoRows) {
						failures = append(failures, fmt.Errorf("read queue job %d: %w", id, err))
					}
				}
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", host.Name, name, host.Containers[name], description, githubJob)
			listed++
		}
	}
	if listed == 0 && queueErr == nil {
		fmt.Fprintln(table, "No untracked runners found on reachable hosts.")
	}
	return errors.Join(table.Flush(), queueErr, errors.Join(failures...), ctx.Err())
}

func readRunnerLeases(ctx context.Context, db *sql.DB) (map[string]string, error) {
	hasHost, err := hasJobColumn(ctx, db, "runner_host")
	if err != nil {
		return nil, err
	}
	host := "''"
	if hasHost {
		host = "coalesce(runner_host, '')"
	}
	rows, err := db.QueryContext(ctx, "SELECT runner_name, "+host+" FROM jobs WHERE runner_name IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	leases := make(map[string]string)
	for rows.Next() {
		var runner, owner string
		if err := rows.Scan(&runner, &owner); err != nil {
			return nil, err
		}
		leases[runner] = owner
	}
	return leases, rows.Err()
}

func runnerJobID(name string) (int64, bool) {
	remainder, hasPrefix := strings.CutPrefix(name, "runquill-")
	number, suffix, hasSuffix := strings.Cut(remainder, "-")
	id, err := strconv.ParseInt(number, 10, 64)
	if !hasPrefix || !hasSuffix || err != nil || id <= 0 || strconv.FormatInt(id, 10) != number || len(suffix) != 12 {
		return 0, false
	}
	_, err = hex.DecodeString(suffix)
	return id, err == nil
}

func jobByID(ctx context.Context, db *sql.DB, id int64) (job, error) {
	j := job{id: id}
	err := db.QueryRowContext(ctx, `SELECT repository, repository_id, installation_id, route
        FROM jobs WHERE job_id = ?`, id).Scan(&j.repository, &j.repositoryID, &j.installationID, &j.route)
	return j, err
}

func (g *github) readRunnerJobStatus(ctx context.Context, j job) (string, error) {
	if _, valid := ownedRepositoryPath(j.repository, g.owner); !valid {
		return "", errors.New("job is not associated with a repository owned by RUNQUILL_OWNER")
	}
	jwt, err := g.jwt(time.Now())
	if err != nil {
		return "", err
	}
	token, err := g.mintToken(ctx, jwt, j.installationID, map[string]any{
		"repository_ids": []int64{j.repositoryID},
		"permissions":    map[string]string{"actions": "read"},
	})
	if err != nil {
		return "", err
	}
	_, eligible, err := g.eligible(ctx, j, token.Token)
	if err != nil {
		return "", err
	}
	if !eligible {
		return "", errors.New("job repository is no longer eligible for Runquill")
	}
	details, err := g.jobDetails(ctx, j, token.Token)
	if githubNotFound(err) {
		return "job not found", nil
	}
	if _, known := phases[details.Status]; !known {
		return "", err
	}
	return details.Status, err
}

type orphanHost interface {
	label() string
	ownedRunners(context.Context) ([]runnerContainer, error)
	removeContainer(context.Context, string) error
}

type runnerContainer struct {
	name, id, state string
	owned           bool
	worker          string
	created         time.Time
}

const runnerInspectFormat = `{{.Name}}|{{.Id}}|{{index .Config.Labels "io.runquill.runner"}}|{{index .Config.Labels "io.runquill.worker"}}|{{.Created}}|{{.State.Status}}`

func (d docker) ownedRunners(ctx context.Context) ([]runnerContainer, error) {
	ids, err := d.command(ctx, nil, "ps", "-a", "-q", "--no-trunc", "--filter", "label="+runnerLabel)
	if err != nil || strings.TrimSpace(ids) == "" {
		return nil, err
	}
	output, err := d.command(ctx, nil, append([]string{"inspect", "--format", runnerInspectFormat}, strings.Fields(ids)...)...)
	if err != nil {
		return nil, err
	}
	return parseRunnerInspect(output)
}

func parseRunnerInspect(output string) ([]runnerContainer, error) {
	var runners []runnerContainer
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 6 || !validHex32(fields[1]) {
			return nil, errors.New("invalid Docker inspect output")
		}
		created, err := time.Parse(time.RFC3339Nano, fields[4])
		if err != nil {
			return nil, fmt.Errorf("invalid Docker creation time for %s: %w", fields[0], err)
		}
		runners = append(runners, runnerContainer{name: strings.TrimPrefix(fields[0], "/"), id: fields[1],
			owned: fields[2] == "1", worker: fields[3], created: created, state: fields[5]})
	}
	return runners, nil
}

func (d docker) removeContainer(ctx context.Context, id string) error {
	// Remove by ID so a newer container that reuses the name is never touched.
	return d.remove(ctx, id)
}

// removeOrphans deletes untracked runner containers that cannot be running a GitHub job.
// Anything it cannot prove idle is skipped with the reason printed.
func removeOrphans(ctx context.Context, out io.Writer, db *sql.DB, hosts []orphanHost, loadAPI func() (*github, error), grace time.Duration, now time.Time) error {
	type candidate struct {
		host orphanHost
		runnerContainer
	}
	var candidates []candidate
	var failures []error
	for _, host := range hosts {
		listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		runners, err := host.ownedRunners(listCtx)
		cancel()
		if err != nil {
			fmt.Fprintf(out, "Skipped %s: %s\n", host.label(), oneLine(err))
			failures = append(failures, fmt.Errorf("%s: %w", host.label(), err))
			continue
		}
		for _, runner := range runners {
			candidates = append(candidates, candidate{host, runner})
		}
	}
	// Read leases after listing: a runner launched meanwhile is then tracked or inside the grace period.
	leases, err := readRunnerLeases(ctx, db)
	if err != nil {
		return fmt.Errorf("read runner leases, nothing removed: %w", err)
	}
	var removed, skipped int
	for _, c := range candidates {
		owner, tracked := leases[c.name]
		if tracked && (owner == c.host.label() || owner == "") {
			continue
		}
		reason, err := removeOrphan(ctx, db, c.host, c.runnerContainer, tracked, owner, loadAPI, grace, now)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s %s: %w", c.host.label(), c.name, err))
		}
		if reason != "" {
			fmt.Fprintf(out, "Skipped %s %s: %s\n", c.host.label(), c.name, reason)
			skipped++
			continue
		}
		fmt.Fprintf(out, "Removed %s %s\n", c.host.label(), c.name)
		removed++
	}
	fmt.Fprintf(out, "Removed %d orphaned runners; skipped %d.\n", removed, skipped)
	return errors.Join(errors.Join(failures...), ctx.Err())
}

// removeOrphan returns a non-empty reason when it left the runner in place.
func removeOrphan(ctx context.Context, db *sql.DB, host orphanHost, c runnerContainer, tracked bool, owner string,
	loadAPI func() (*github, error), grace time.Duration, now time.Time) (string, error) {
	switch {
	case !c.owned:
		return "missing the Runquill ownership label", nil
	case c.worker != "":
		return "belongs to outbound worker " + c.worker, nil
	case isWarmRunner(c.name):
		return "idle warm runner; the manager replaces or removes it", nil
	case tracked:
		return "lease recorded on " + owner, nil
	case c.created.IsZero() || now.Sub(c.created) < grace:
		return fmt.Sprintf("created %s ago, inside the %s grace period", now.Sub(c.created).Round(time.Second), grace), nil
	}
	id, valid := runnerJobID(c.name)
	if !valid {
		return "name does not match a Runquill runner", nil
	}
	running := runnerOccupiesSlot(c.state)
	j, err := jobByID(ctx, db, id)
	if errors.Is(err, sql.ErrNoRows) {
		if running {
			return fmt.Sprintf("no queue record for job %d, so GitHub cannot confirm the runner is idle", id), nil
		}
		// A stopped container cannot run a job, and GitHub expires its offline JIT registration.
		if err := host.removeContainer(ctx, c.id); err != nil {
			return "remove container: " + oneLine(err), err
		}
		return "", nil
	}
	if err != nil {
		return "read queue: " + oneLine(err), err
	}
	j.runnerName = c.name
	api, err := loadAPI()
	if err != nil {
		return "GitHub unavailable: " + oneLine(err), err
	}
	if running {
		token, err := api.token(ctx, j)
		if err != nil {
			return "read GitHub job: " + oneLine(err), nil
		}
		details, err := api.jobDetails(ctx, j, token)
		if err != nil && !githubNotFound(err) {
			return "read GitHub job: " + oneLine(err), nil
		}
		if err == nil && details.Status != "completed" {
			return fmt.Sprintf("GitHub job %d is %s", j.id, cmp.Or(details.Status, "in an unknown state")), nil
		}
	}
	token, err := api.runnerToken(ctx, j)
	if err != nil {
		return "look up GitHub runner: " + oneLine(err), nil
	}
	registration, err := api.findRunner(ctx, j, token)
	if err != nil {
		return "look up GitHub runner: " + oneLine(err), nil
	}
	if registration.Busy {
		return "GitHub reports the runner busy", nil
	}
	if registration.ID > 0 {
		// Delete the registration first so the runner cannot accept a job while it is removed.
		if err := api.deleteRunner(ctx, j, token, registration.ID); err != nil {
			return "delete GitHub runner registration: " + oneLine(err), nil
		}
	}
	if err := host.removeContainer(ctx, c.id); err != nil {
		return "remove container: " + oneLine(err), err
	}
	return "", nil
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

const gcUsage = `Usage: runquill gc (--dry-run | --remove-orphans [--grace DURATION] | --unused [--dry-run] [--older-than DURATION])
  --dry-run                  list runners without a queue lease; changes nothing
  --remove-orphans           remove untracked idle runners and their GitHub registrations
  --grace DURATION           with --remove-orphans, keep runners created more recently (default 10m, at least 1m)
  --unused                   remove runner images Runquill pulled itself and Runquill cache volumes unused for
                             RUNQUILL_CLEANUP_AFTER (default 30d); the same selection as the automatic cleanup
  --unused --dry-run         only list what --unused would remove
  --older-than DURATION      with --unused, use this retention instead (at least 1h, such as 720h or 30d);
                             required when RUNQUILL_CLEANUP_AFTER=off
--unused cannot be combined with --remove-orphans.`

// gcCommand parses runquill gc arguments and returns the process exit code.
func gcCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("runquill gc", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dryRun := flags.Bool("dry-run", false, "inspect runners and leases without changes; with --unused, list without removing")
	removeOrphans := flags.Bool("remove-orphans", false, "remove untracked idle runners on local and SSH Docker hosts and delete their GitHub registrations")
	grace := flags.Duration("grace", defaultOrphanGrace, "with --remove-orphans, keep runners created more recently than this (at least 1m)")
	unused := flags.Bool("unused", false, "remove runner images Runquill pulled and Runquill cache volumes unused for the cleanup retention")
	olderThan := flags.String("older-than", "", "with --unused, the retention instead of RUNQUILL_CLEANUP_AFTER, such as 720h or 30d (at least 1h)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), gcUsage)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var retention time.Duration
	valid := flags.NArg() == 0 && *grace >= time.Minute
	if *unused {
		valid = valid && !*removeOrphans
		if valid && *olderThan != "" {
			var err error
			if retention, err = parseRetention("--older-than", *olderThan); err != nil || retention == 0 {
				fmt.Fprintln(stderr, "--older-than must be a duration of at least 1h, such as 720h or 30d")
				valid = false
			}
		}
	} else {
		valid = valid && *olderThan == "" && *dryRun != *removeOrphans
	}
	if !valid {
		flags.Usage()
		return 2
	}
	var err error
	if *unused {
		err = gcUnused(ctx, stdout, *dryRun, retention)
	} else {
		err = gc(ctx, stdout, *removeOrphans, *grace)
	}
	if err != nil {
		slog.Error("runquill gc", "error", err)
		return 1
	}
	return 0
}

// gcUnused lists or removes what the automatic cleanup would remove, with the same selection.
func gcUnused(ctx context.Context, out io.Writer, dryRun bool, olderThan time.Duration) error {
	policy, err := loadAssetPolicy(os.Getenv)
	if err != nil {
		return err
	}
	if olderThan > 0 {
		policy.Images, policy.Cache = olderThan, olderThan
	} else if !policy.enabled() {
		return errors.New("automatic cleanup is off (RUNQUILL_CLEANUP_AFTER=off); pass --older-than DURATION")
	}
	mode := "rw"
	if dryRun {
		mode = "ro"
	}
	db, err := openQueueDB(env("RUNQUILL_DB", "runquill.sqlite3"), mode)
	if err != nil {
		return err
	}
	defer db.Close()
	if !dryRun {
		if err := migrateAssets(ctx, db); err != nil {
			return fmt.Errorf("migrate runner asset records: %w", err)
		}
	}
	runners, err := gcRunners(ctx, db, env("RUNQUILL_RUNNER_IMAGE", "runquill-runner:local"))
	if err != nil {
		return err
	}
	topology := assetTopologyOf(runners)
	cleaner := assetCleaner{run: runImageCommand, db: db, write: !dryRun, policy: policy, configured: topology.configured, now: time.Now()}
	return writeUnusedAssets(ctx, out, cleaner, topology, dryRun)
}

func writeUnusedAssets(ctx context.Context, out io.Writer, cleaner assetCleaner, topology assetTopology, dryRun bool) error {
	for _, name := range topology.skipped {
		fmt.Fprintf(out, "Skipped %s: image and cache volume cleanup supports local and SSH Docker hosts only.\n", name)
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	header := "HOST\tKIND\tNAME\tUNUSED FOR\tSIZE"
	if !dryRun {
		header += "\tRESULT"
	}
	fmt.Fprintln(table, header)
	row := func(c assetCandidate) string {
		size := "-"
		if c.size > 0 {
			size = formatBytes(c.size)
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s", c.host, assetKindLabel(c.kind), logImage(c.name), formatIdle(c.idle), size)
	}
	var failures []error
	var images, volumes, kept, listed int
	var reclaimed int64
	count := func(c assetCandidate) {
		if c.kind == assetKindImage {
			images++
		} else {
			volumes++
		}
		reclaimed += c.size
	}
	for _, target := range topology.hosts {
		var err error
		if dryRun {
			var candidates []assetCandidate
			candidates, err = cleaner.plan(ctx, target)
			for _, c := range candidates {
				fmt.Fprintln(table, row(c))
				count(c)
				listed++
			}
		} else {
			_, err = cleaner.clean(ctx, target, func(c assetCandidate, reason string, err error) {
				result := "removed"
				switch {
				case err != nil:
					result = "kept: " + reason + ": " + oneLine(err)
					kept++
				case reason != "":
					result = "kept: " + reason
					kept++
				default:
					count(c)
				}
				fmt.Fprintln(table, row(c)+"\t"+result)
				listed++
			})
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", target.label(), err))
		}
	}
	if listed == 0 {
		fmt.Fprintln(table, "No unused runner images or cache volumes found on reachable hosts.")
	}
	flushErr := table.Flush()
	for _, failure := range failures {
		fmt.Fprintf(out, "Failed %s\n", oneLine(failure))
	}
	if dryRun {
		fmt.Fprintf(out, "Would remove %d images and %d cache volumes (%s of images).\n", images, volumes, formatBytes(reclaimed))
	} else {
		fmt.Fprintf(out, "Removed %d images and %d cache volumes (%s of images); kept %d.\n", images, volumes, formatBytes(reclaimed), kept)
	}
	return errors.Join(flushErr, errors.Join(failures...), ctx.Err())
}
