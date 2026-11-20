package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

type operationSettingsResponse struct {
	Backend string          `json:"backend"`
	Webhook bool            `json:"webhook_enabled"`
	Policy  operationPolicy `json:"policy"`
	// RunnerImages is the read-only catalogue workflows select with os__NAME__.
	RunnerImages []runnerImage `json:"runner_images,omitempty"`
}

func (p *panelState) operationSettings(w http.ResponseWriter, r *http.Request, _ panelIdentity) {
	state := p.operations.current.Load()
	panelJSON(w, http.StatusOK, operationSettingsResponse{state.runners.kind, p.operations.webhook, state.policy, state.runners.images.list()})
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
	err = a.change(ctx, user, "settings.update", settingsChangeSummary(next.runners.kind, old.policy, next.policy), func(tx *sql.Tx) error {
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

const settingsSummaryLimit = 1000

var secretSettingName = regexp.MustCompile(`(?i)secret|token|password|credential|key`)

// settingsChangeSummary describes what a settings save changed for the audit
// trail: field names with old -> new values. Fields whose names suggest a
// secret are named without their values.
func settingsChangeSummary(backend string, old, next operationPolicy) string {
	var changes []string
	scalar := func(name string, before, after any) {
		if before != after {
			changes = append(changes, fmt.Sprintf("%s %v -> %v", name, before, after))
		}
	}
	scalar("slot_limit", old.Limit, next.Limit)
	scalar("poll_interval", old.PollInterval, next.PollInterval)
	scalar("runner_image", old.Image, next.Image)
	for _, host := range next.Hosts {
		if !slices.Contains(old.Hosts, host) {
			changes = append(changes, "docker_hosts +"+host)
		}
	}
	for _, host := range old.Hosts {
		if !slices.Contains(next.Hosts, host) {
			changes = append(changes, "docker_hosts -"+host)
		}
	}
	for _, host := range sortedUnion(old.HostLimits, next.HostLimits) {
		before, hadBefore := old.HostLimits[host]
		after, hasAfter := next.HostLimits[host]
		if before != after || hadBefore != hasAfter {
			changes = append(changes, fmt.Sprintf("host_limits[%s] %s -> %s", host, optionalLimit(before, hadBefore), optionalLimit(after, hasAfter)))
		}
	}
	for _, name := range sortedUnion(old.Pools, next.Pools) {
		before, hadBefore := old.Pools[name]
		after, hasAfter := next.Pools[name]
		switch {
		case !hadBefore:
			changes = append(changes, "worker_pools +"+name)
		case !hasAfter:
			changes = append(changes, "worker_pools -"+name)
		default:
			changes = append(changes, poolChanges(name, before, after)...)
		}
	}
	summary := backend
	if len(changes) == 0 {
		return summary + ": no changes"
	}
	summary += ": " + strings.Join(changes, "; ")
	if runes := []rune(summary); len(runes) > settingsSummaryLimit {
		summary = string(runes[:settingsSummaryLimit-1]) + "…"
	}
	return summary
}

func sortedUnion[V any](a, b map[string]V) []string {
	keys := slices.Collect(maps.Keys(a))
	for key := range b {
		if _, ok := a[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func optionalLimit(limit int, set bool) string {
	if !set {
		return "unset"
	}
	return fmt.Sprint(limit)
}

// poolChanges compares pools field by field through their JSON form, so
// fields added to workerPool later are summarised without changes here.
func poolChanges(name string, before, after workerPool) []string {
	fields := func(pool workerPool) map[string]json.RawMessage {
		data, _ := json.Marshal(pool)
		var out map[string]json.RawMessage
		json.Unmarshal(data, &out)
		return out
	}
	old, next := fields(before), fields(after)
	var changes []string
	for _, field := range sortedUnion(old, next) {
		a, b := string(old[field]), string(next[field])
		if a == b {
			continue
		}
		label := "worker_pools[" + name + "]." + field
		if secretSettingName.MatchString(field) {
			changes = append(changes, label+" changed")
			continue
		}
		changes = append(changes, label+" "+settingValue(a)+" -> "+settingValue(b))
	}
	return changes
}

func settingValue(raw string) string {
	switch {
	case raw == "" || raw == "null":
		return "default"
	case len(raw) > 80:
		return "(changed)"
	}
	return raw
}
