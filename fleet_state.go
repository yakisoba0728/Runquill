package main

import (
	"context"
	"database/sql"
)

type hostState struct {
	cordoned       bool
	untrackedSlots sql.NullInt64
}

type hostCapacity struct {
	dockerHostSnapshot
	Limit, Occupied int
	Cordoned        bool
}

func readHostStates(ctx context.Context, db *sql.DB) (map[string]hostState, error) {
	states := make(map[string]hostState)
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (
        SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'docker_hosts'
    )`).Scan(&exists)
	if err != nil || !exists {
		return states, err
	}
	rows, err := db.QueryContext(ctx, `SELECT host, cordoned, untracked_slots FROM docker_hosts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var state hostState
		if err := rows.Scan(&name, &state.cordoned, &state.untrackedSlots); err != nil {
			return nil, err
		}
		states[name] = state
	}
	return states, rows.Err()
}

func (q *queue) observeHostSlots(ctx context.Context, host string, slots int) error {
	_, err := q.ExecContext(ctx, `
        INSERT INTO docker_hosts(host, untracked_slots) VALUES (?, ?)
        ON CONFLICT(host) DO UPDATE SET untracked_slots = excluded.untracked_slots
        WHERE docker_hosts.untracked_slots IS NOT excluded.untracked_slots
    `, host, slots)
	return err
}

func fleetCapacity(snapshots []dockerHostSnapshot, states map[string]hostState, jobs []job, limits map[string]int, globalLimit int) ([]hostCapacity, int) {
	configured := make(map[string]bool, len(snapshots))
	observed := make(map[string]bool)
	unreachable := false
	for _, host := range snapshots {
		configured[host.Name] = true
		if host.Containers == nil {
			unreachable = true
		}
		for name := range host.Containers {
			observed[name] = true
		}
	}

	leasesOnHost := make(map[string]int)
	var unassigned, legacy int
	for _, j := range jobs {
		if j.runnerName == "" {
			continue
		}
		switch {
		case j.runnerHost == "":
			if !observed[j.runnerName] {
				legacy++
			}
		case configured[j.runnerHost]:
			leasesOnHost[j.runnerHost]++
		default:
			unassigned++
			unreachable = true
		}
	}
	for name, state := range states {
		if !configured[name] && state.untrackedSlots.Int64 > 0 {
			unassigned += int(state.untrackedSlots.Int64)
			unreachable = true
		}
	}
	if unreachable {
		unassigned += legacy
	}

	hosts := make([]hostCapacity, 0, len(snapshots))
	for _, snapshot := range snapshots {
		state := states[snapshot.Name]
		host := hostCapacity{dockerHostSnapshot: snapshot, Limit: globalLimit, Cordoned: state.cordoned}
		if override, ok := limits[host.Name]; ok {
			host.Limit = min(override, globalLimit)
		}
		if host.Containers != nil {
			for _, container := range host.Containers {
				if runnerOccupiesSlot(container) {
					host.Occupied++
				}
			}
		} else if state.untrackedSlots.Valid || host.UnregisteredWorker {
			host.Occupied = leasesOnHost[host.Name] + int(state.untrackedSlots.Int64)
		} else {
			// An engine we have never reached may already be running containers.
			host.Occupied = max(leasesOnHost[host.Name], host.Limit)
		}
		hosts = append(hosts, host)
	}
	return hosts, unassigned
}
