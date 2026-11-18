package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

func gc(ctx context.Context, out io.Writer) error {
	fleet, err := newDockerFleet("", env("RUNQUILL_DOCKER_HOSTS", "local"))
	if err != nil {
		return err
	}
	db, err := openQueueDB(env("RUNQUILL_DB", "runquill.sqlite3"), "ro")
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hosts, err := fleet.snapshot(ctx)
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
	lookup := func(ctx context.Context, j job) (string, error) {
		api, err := loadAPI()
		if err != nil {
			return "", err
		}
		return api.readRunnerJobStatus(ctx, j)
	}
	return writeGCInventory(ctx, out, db, hosts, lookup)
}

func writeGCInventory(ctx context.Context, out io.Writer, db *sql.DB, hosts []dockerHostSnapshot, lookup func(context.Context, job) (string, error)) error {
	leases, queueErr := readRunnerLeases(ctx, db)
	if queueErr != nil {
		fmt.Fprintf(out, "Queue unavailable. Container leases are unknown (%s)\n", strings.Join(strings.Fields(queueErr.Error()), " "))
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "HOST\tCONTAINER\tDOCKER STATE\tLEASE\tGITHUB JOB")
	var failures []error
	listed := 0
	for _, host := range hosts {
		if host.Containers == nil {
			if host.Err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", host.Name, host.Err))
			} else {
				failures = append(failures, fmt.Errorf("%s: Docker state unavailable", host.Name))
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
	eligible, err := g.eligible(ctx, j, token.Token)
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
