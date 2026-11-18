package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"time"
)

type operationSettingsResponse struct {
	Backend string          `json:"backend"`
	Webhook bool            `json:"webhook_enabled"`
	Policy  operationPolicy `json:"policy"`
}

func (p *panelState) operationSettings(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
	state := p.operations.current.Load()
	panelJSON(w, http.StatusOK, operationSettingsResponse{state.runners.kind, p.operations.webhook, state.policy})
}

var errSettingConflict = errors.New("Settings were changed in another session. Reload them and try again.")
var errWorkerInUse = errors.New("Pause assignments and let the worker finish its jobs before removing it.")

func (a *panelAccess) saveOperationSettings(w http.ResponseWriter, r *http.Request, user panelIdentity, panel *panelState) {
	var policy operationPolicy
	if !decodePanelJSONLimit(w, r, &policy, 64<<10) {
		return
	}
	ops := panel.operations
	if !ops.updates.TryLock() {
		http.Error(w, "Another settings update is in progress. Try again shortly.", http.StatusConflict)
		return
	}
	defer ops.updates.Unlock()
	old := ops.current.Load()
	if policy.Revision != old.policy.Revision {
		http.Error(w, errSettingConflict.Error(), http.StatusConflict)
		return
	}
	next, err := ops.resolve(policy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var removed []string
	if !slices.Equal(old.policy.Hosts, next.policy.Hosts) {
		if !ops.topology.TryLock() {
			http.Error(w, "The scheduler is using the current worker list. Try again shortly.", http.StatusConflict)
			return
		}
		defer ops.topology.Unlock()
		removed, err = verifyWorkerRemoval(ctx, old, next)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	if err := checkChangedWorkers(ctx, old, next); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	next.policy.Revision++
	payload, err := ops.encodeOverrides(next.policy)
	if err != nil {
		http.Error(w, "Could not save operation settings.", http.StatusServiceUnavailable)
		return
	}
	err = a.change(ctx, user, "settings.update", next.runners.kind, func(tx *sql.Tx) error {
		for _, host := range removed {
			var active bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs
				WHERE runner_name IS NOT NULL AND (runner_host = ? OR coalesce(runner_host, '') = ''))
				OR EXISTS(SELECT 1 FROM docker_hosts WHERE host = ? AND untracked_slots > 0)`, host, host).Scan(&active)
			if err != nil {
				return err
			}
			if active {
				return errWorkerInUse
			}
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO operation_settings (id, revision, policy) VALUES (1, ?, ?)
		  ON CONFLICT(id) DO UPDATE SET revision=excluded.revision, policy=excluded.policy WHERE operation_settings.revision=?`,
			next.policy.Revision, string(payload), old.policy.Revision)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err == nil && count == 0 {
			return errSettingConflict
		}
		return err
	})
	switch {
	case errors.Is(err, errSettingConflict), errors.Is(err, errWorkerInUse):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errPanelUnauthorized):
		http.Error(w, "You do not have access to this operation.", http.StatusForbidden)
	case err != nil:
		http.Error(w, "Could not save operation settings.", http.StatusServiceUnavailable)
	default:
		ops.publish(old, next)
		panel.mu.Lock()
		panel.snapshot.Polling = next.interval > 0
		if next.interval == 0 {
			panel.snapshot.PollFailed = false
		}
		panel.mu.Unlock()
		panel.operationSettings(w, r, user)
	}
}

func verifyWorkerRemoval(ctx context.Context, old, next *operationState) ([]string, error) {
	var removed []string
	for _, host := range old.policy.Hosts {
		if !slices.Contains(next.policy.Hosts, host) {
			removed = append(removed, host)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	observations, err := old.runners.runtime.snapshot(ctx)
	if err != nil {
		return nil, errors.New("Check that the worker is reachable before removing it.")
	}
	for _, name := range removed {
		index := slices.IndexFunc(observations, func(host dockerHostSnapshot) bool { return host.Name == name })
		if index < 0 || observations[index].Containers == nil {
			return nil, errors.New("Check that the worker is reachable before removing it.")
		}
		if len(observations[index].Containers) > 0 {
			return nil, errWorkerInUse
		}
	}
	return removed, nil
}

func checkChangedWorkers(ctx context.Context, old, next *operationState) error {
	fleet, ok := next.runners.runtime.(*dockerFleet)
	if !ok {
		return nil
	}
	previous := old.runners.runtime.(*dockerFleet)
	var changes []string
	for _, host := range fleet.hosts {
		prior, err := previous.host(host.label())
		if err != nil || prior.image != host.image {
			changes = append(changes, host.label())
		}
	}
	if len(changes) == 0 {
		return nil
	}
	snapshots, err := fleet.snapshot(ctx)
	if err != nil {
		return err
	}
	for _, host := range snapshots {
		if slices.Contains(changes, host.Name) {
			if host.Err != nil {
				return host.Err
			}
			if host.Containers == nil {
				return errors.New("Connect the worker and make its runner image available before saving.")
			}
		}
	}
	return nil
}
