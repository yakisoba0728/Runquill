package app

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Housekeeping of runner images and cache volumes on local and ssh:// Docker hosts.
//
// runner_assets records, per host, when Runquill last started a runner with an
// image or cache volume, and whether Runquill's own puller pulled the image.
// Only two kinds of object are ever removed:
//   - a cache volume with the io.runquill.cache=1 label whose name is
//     runquill-cache-<digits> or runquill-cache-<digits>-tools-<hash>, mounted by no
//     container, and unused for the retention;
//   - an image Runquill pulled itself on that host (with the image ID it pulled),
//     not configured, used by no container, and unused for the retention.
//
// An object without a row is first seen when the cleanup discovers it, so after
// an upgrade nothing is removed before one full retention period has passed.
const (
	assetKindImage = "image"
	assetKindCache = "cache"

	cacheVolumeLabel = "io.runquill.cache=1"

	defaultCleanupAfter    = 30 * 24 * time.Hour
	defaultCleanupInterval = 6 * time.Hour
	minimumCleanupAfter    = time.Hour

	// assetFlushInterval bounds how long a recorded use stays only in memory.
	assetFlushInterval = 30 * time.Second
	// assetUsageResolution suppresses repeated writes for the same object; it stays far below the 1h minimum retention.
	assetUsageResolution = 10 * time.Minute
	assetCommandTimeout  = 30 * time.Second
	assetRemoveTimeout   = 2 * time.Minute
)

var cacheVolumeName = regexp.MustCompile(`^runquill-cache-[0-9]+(-tools-[a-f0-9]{64})?$`)

// assetPolicy is the retention per kind; zero disables that kind.
type assetPolicy struct {
	Images, Cache, Interval time.Duration
}

func (p assetPolicy) enabled() bool { return p.Images > 0 || p.Cache > 0 }

func (p assetPolicy) String() string {
	if !p.enabled() {
		return "automatic cleanup off"
	}
	kind := func(name string, d time.Duration) string {
		if d == 0 {
			return name + " kept"
		}
		return name + " unused for " + formatRetention(d)
	}
	return fmt.Sprintf("%s, %s, checked every %s", kind("images", p.Images), kind("cache volumes", p.Cache), formatRetention(p.Interval))
}

func formatRetention(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	return d.String()
}

// parseAssetDuration reads a Go duration or a whole number of days such as "30d".
func parseAssetDuration(raw string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.ParseInt(days, 10, 64)
		if err != nil || n < 0 || n > 3650 {
			return 0, errors.New("invalid day count")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(raw)
}

// parseRetention returns 0 for off; any other value must be at least one hour.
func parseRetention(name, raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "off" || raw == "0" {
		return 0, nil
	}
	d, err := parseAssetDuration(raw)
	if err != nil || d < minimumCleanupAfter {
		return 0, fmt.Errorf("%s must be off or a duration of at least 1h, such as 720h or 30d", name)
	}
	return d, nil
}

// loadAssetPolicy reads RUNQUILL_CLEANUP_AFTER (default 30d), its per-kind overrides and RUNQUILL_CLEANUP_INTERVAL.
func loadAssetPolicy(getenv func(string) string) (assetPolicy, error) {
	policy := assetPolicy{Images: defaultCleanupAfter, Cache: defaultCleanupAfter, Interval: defaultCleanupInterval}
	if raw := getenv("RUNQUILL_CLEANUP_AFTER"); strings.TrimSpace(raw) != "" {
		d, err := parseRetention("RUNQUILL_CLEANUP_AFTER", raw)
		if err != nil {
			return policy, err
		}
		policy.Images, policy.Cache = d, d
	}
	for _, override := range []struct {
		name  string
		value *time.Duration
	}{{"RUNQUILL_CLEANUP_IMAGES_AFTER", &policy.Images}, {"RUNQUILL_CLEANUP_CACHE_AFTER", &policy.Cache}} {
		if raw := getenv(override.name); strings.TrimSpace(raw) != "" {
			d, err := parseRetention(override.name, raw)
			if err != nil {
				return policy, err
			}
			*override.value = d
		}
	}
	if raw := strings.TrimSpace(getenv("RUNQUILL_CLEANUP_INTERVAL")); raw != "" {
		d, err := parseAssetDuration(raw)
		if err != nil || d < 5*time.Minute || d > 7*24*time.Hour {
			return policy, errors.New("RUNQUILL_CLEANUP_INTERVAL must be a duration between 5m and 7d")
		}
		policy.Interval = d
	}
	return policy, nil
}

func migrateAssets(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS runner_assets (
    host TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('image', 'cache')),
    name TEXT NOT NULL,
    first_seen INTEGER NOT NULL,
    last_used INTEGER NOT NULL,
    pulled INTEGER NOT NULL DEFAULT 0,
    image_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (host, kind, name)
) WITHOUT ROWID`)
	return err
}

type assetKey struct{ host, kind, name string }

type assetRow struct {
	firstSeen, lastUsed time.Time
	pulled              bool
	imageID             string
}

// idleSince is the later of the first sighting and the last recorded use.
func (r assetRow) idleSince() time.Time {
	if r.lastUsed.After(r.firstSeen) {
		return r.lastUsed
	}
	return r.firstSeen
}

func missingAssetTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// readAssetRows returns the host's rows; a database without the table has none.
func readAssetRows(ctx context.Context, db *sql.DB, host string) (map[assetKey]assetRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT kind, name, first_seen, last_used, pulled, image_id FROM runner_assets WHERE host = ?`, host)
	if missingAssetTable(err) {
		return map[assetKey]assetRow{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := make(map[assetKey]assetRow)
	for rows.Next() {
		var kind, name, id string
		var first, last int64
		var pulled bool
		if err := rows.Scan(&kind, &name, &first, &last, &pulled, &id); err != nil {
			return nil, err
		}
		assets[assetKey{host, kind, name}] = assetRow{time.Unix(first, 0), time.Unix(last, 0), pulled, id}
	}
	return assets, rows.Err()
}

// assetUse is a pending write: a runner start, or a pull when pulled is set.
type assetUse struct {
	at      time.Time
	pulled  bool
	imageID string
}

// assetRecorder batches usage in memory; a launch never waits for, or fails on, the database.
type assetRecorder struct {
	db      *sql.DB
	now     func() time.Time
	mu      sync.Mutex
	pending map[assetKey]assetUse
	written map[assetKey]time.Time // last persisted use, for the debounce
}

func newAssetRecorder(db *sql.DB, now func() time.Time) *assetRecorder {
	if now == nil {
		now = time.Now
	}
	return &assetRecorder{db: db, now: now, pending: make(map[assetKey]assetUse), written: make(map[assetKey]time.Time)}
}

// runnerAssets is the process recorder; nil (CLI commands, worker agents) records nothing.
var runnerAssets atomic.Pointer[assetRecorder]

// noteRunnerAssets records the selected image reference and cache volumes.
func noteRunnerAssets(host, image string, volumes ...string) {
	r := runnerAssets.Load()
	if r == nil {
		return
	}
	if image != "" {
		r.use(assetKey{host, assetKindImage, image})
	}
	for _, volume := range volumes {
		if volume != "" {
			r.use(assetKey{host, assetKindCache, volume})
		}
	}
}

// noteImagePulled records that Runquill's puller pulled image on target, with the image ID it pulled.
func noteImagePulled(ctx context.Context, run imageCommand, target docker, image string) {
	r := runnerAssets.Load()
	if r == nil {
		return
	}
	id, err := run(ctx, target, imageInspectTimeout, "image", "inspect", image, "--format", "{{.Id}}")
	if err != nil {
		// Without the pulled ID the image is never removed, which is the safe side.
		slog.Warn("record pulled runner image", "host", target.label(), "image", logImage(image), "error", pullErrorText(err))
		id = ""
	}
	r.pull(assetKey{target.label(), assetKindImage, image}, strings.TrimSpace(id))
}

func (r *assetRecorder) use(key assetKey) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.pending[key]; ok {
		p.at = later(p.at, now)
		r.pending[key] = p
		return
	}
	if written, ok := r.written[key]; ok && now.Sub(written) < assetUsageResolution {
		return
	}
	r.pending[key] = assetUse{at: now}
}

func (r *assetRecorder) pull(key assetKey, id string) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending[key] = assetUse{at: later(r.pending[key].at, now), pulled: true, imageID: id}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// flush writes pending uses in one short transaction; on failure they stay pending.
func (r *assetRecorder) flush(ctx context.Context) error {
	r.mu.Lock()
	batch := r.pending
	r.pending = make(map[assetKey]assetUse)
	r.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	err := r.write(ctx, batch)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		for key, use := range batch {
			if newer, ok := r.pending[key]; ok {
				use.at = later(use.at, newer.at)
				if newer.pulled {
					use.pulled, use.imageID = true, newer.imageID
				}
			}
			r.pending[key] = use
		}
		return err
	}
	cutoff := r.now().Add(-assetUsageResolution)
	maps.DeleteFunc(r.written, func(_ assetKey, at time.Time) bool { return at.Before(cutoff) })
	for key, use := range batch {
		r.written[key] = use.at
	}
	return nil
}

func (r *assetRecorder) write(ctx context.Context, batch map[assetKey]assetUse) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, use := range batch {
		at := use.at.Unix()
		if _, err := tx.ExecContext(ctx, `INSERT INTO runner_assets (host, kind, name, first_seen, last_used, pulled, image_id)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT (host, kind, name) DO UPDATE SET last_used = max(last_used, excluded.last_used),
                pulled = CASE WHEN excluded.pulled = 1 THEN 1 ELSE pulled END,
                image_id = CASE WHEN excluded.pulled = 1 THEN excluded.image_id ELSE image_id END`,
			key.host, key.kind, key.name, at, at, use.pulled, use.imageID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *assetRecorder) run(ctx context.Context) {
	ticker := time.NewTicker(assetFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.flush(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("record runner image and cache use", "error", err)
			}
		}
	}
}

// assetTopology is what the cleanup needs from the live configuration.
type assetTopology struct {
	hosts      []docker        // local and ssh:// hosts, one entry per host
	skipped    []string        // outbound workers and Kubernetes
	configured map[string]bool // every image the configuration can start or the puller keeps
	images     *imageReadiness
}

func assetTopologyOf(runners runnerConfiguration) assetTopology {
	topology := assetTopology{configured: make(map[string]bool)}
	fleet, ok := runners.runtime.(*dockerFleet)
	if !ok {
		topology.skipped = append(topology.skipped, cmp.Or(runners.kind, "runner backend"))
		return topology
	}
	topology.images = fleet.images
	// The puller's own image set, so every image source it learns about is protected here too.
	for _, pair := range fleet.imagePairs() {
		topology.configured[pair.image] = true
	}
	seen := make(map[string]bool)
	for _, host := range fleet.hosts {
		if host.image != "" {
			topology.configured[host.image] = true
		}
		if seen[host.label()] {
			continue
		}
		seen[host.label()] = true
		if host.agentName() != "" {
			topology.skipped = append(topology.skipped, host.label())
			continue
		}
		topology.hosts = append(topology.hosts, host)
	}
	return topology
}

func (t assetTopology) busy(host, image string) bool {
	return t.images != nil && t.images.busy(host, image)
}

// assetTopologyFrom reads the live topology each run, without holding its lock across engine calls.
func assetTopologyFrom(ops *operations) func() assetTopology {
	return func() assetTopology {
		ops.topology.RLock()
		defer ops.topology.RUnlock()
		return assetTopologyOf(ops.current.Load().runners)
	}
}

// assetCandidate is one object the cleanup would remove.
type assetCandidate struct {
	host, kind, name string
	imageID          string
	idle             time.Duration
	size             int64 // reclaimed bytes when known, otherwise 0
}

// assetCleaner selects and removes unused objects; the loop and runquill gc --unused share it.
type assetCleaner struct {
	run        imageCommand
	db         *sql.DB
	write      bool // record discoveries and drop rows of removed objects
	policy     assetPolicy
	configured map[string]bool
	busy       func(host, image string) bool
	now        time.Time
}

func (c assetCleaner) command(ctx context.Context, target docker, args ...string) (string, error) {
	timeout := assetCommandTimeout
	if slices.Contains(args, "rm") {
		timeout = assetRemoveTimeout
	}
	return c.run(ctx, target, timeout, args...)
}

func (c assetCleaner) pulling(host, image string) bool {
	return c.busy != nil && c.busy(host, image)
}

// plan returns what may be removed from target now; every listed condition was checked.
func (c assetCleaner) plan(ctx context.Context, target docker) ([]assetCandidate, error) {
	host := target.label()
	rows, err := readAssetRows(ctx, c.db, host)
	if err != nil {
		return nil, fmt.Errorf("read runner asset records: %w", err)
	}
	var candidates []assetCandidate
	var failures []error
	if c.policy.Cache > 0 {
		volumes, err := c.planVolumes(ctx, target, rows)
		candidates = append(candidates, volumes...)
		failures = append(failures, err)
	}
	if c.policy.Images > 0 {
		images, err := c.planImages(ctx, target, rows)
		candidates = append(candidates, images...)
		failures = append(failures, err)
	}
	slices.SortFunc(candidates, func(a, b assetCandidate) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.name, b.name))
	})
	return candidates, errors.Join(failures...)
}

func (c assetCleaner) cacheVolumes(ctx context.Context, target docker) ([]string, error) {
	output, err := c.command(ctx, target, "volume", "ls", "--filter", "label="+cacheVolumeLabel, "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Fields(output) {
		if cacheVolumeName.MatchString(name) {
			names = append(names, name)
		}
	}
	return names, nil
}

func (c assetCleaner) planVolumes(ctx context.Context, target docker, rows map[assetKey]assetRow) ([]assetCandidate, error) {
	host := target.label()
	names, err := c.cacheVolumes(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("list cache volumes: %w", err)
	}
	if c.write {
		if err := c.recordVolumes(ctx, host, names, rows); err != nil {
			return nil, fmt.Errorf("record cache volumes: %w", err)
		}
	}
	var candidates []assetCandidate
	var failures []error
	for _, name := range names {
		row, known := rows[assetKey{host, assetKindCache, name}]
		// A volume without a record is first seen now.
		if !known || c.now.Sub(row.idleSince()) <= c.policy.Cache {
			continue
		}
		used, err := c.volumeInUse(ctx, target, name)
		if err != nil {
			failures = append(failures, fmt.Errorf("check cache volume %s: %w", name, err))
			continue
		}
		if !used {
			candidates = append(candidates, assetCandidate{host: host, kind: assetKindCache, name: name, idle: c.now.Sub(row.idleSince())})
		}
	}
	return candidates, errors.Join(failures...)
}

// recordVolumes marks new volumes first seen now and forgets records of volumes that are gone.
func (c assetCleaner) recordVolumes(ctx context.Context, host string, names []string, rows map[assetKey]assetRow) error {
	present := make(map[string]bool)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, name := range names {
		present[name] = true
		if _, ok := rows[assetKey{host, assetKindCache, name}]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO runner_assets (host, kind, name, first_seen, last_used)
            VALUES (?, 'cache', ?, ?, ?)`, host, name, c.now.Unix(), c.now.Unix()); err != nil {
			return err
		}
	}
	for key := range rows {
		if key.kind == assetKindCache && !present[key.name] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM runner_assets WHERE host = ? AND kind = 'cache' AND name = ? AND last_used < ?`,
				host, key.name, c.now.Add(-assetUsageResolution).Unix()); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (c assetCleaner) volumeInUse(ctx context.Context, target docker, name string) (bool, error) {
	output, err := c.command(ctx, target, "ps", "-a", "-q", "--no-trunc", "--filter", "volume="+name)
	return strings.TrimSpace(output) != "", err
}

type imageDetails struct {
	id   string
	size int64
	tags int
}

func (c assetCleaner) inspectImage(ctx context.Context, target docker, image string) (imageDetails, error) {
	output, err := c.command(ctx, target, "image", "inspect", image, "--format", "{{.Id}}|{{.Size}}|{{len .RepoTags}}")
	if err != nil {
		return imageDetails{}, err
	}
	fields := strings.Split(strings.TrimSpace(output), "|")
	if len(fields) != 3 || fields[0] == "" {
		return imageDetails{}, fmt.Errorf("invalid image inspect output for %s", image)
	}
	size, _ := strconv.ParseInt(fields[1], 10, 64)
	tags, _ := strconv.Atoi(fields[2])
	return imageDetails{id: fields[0], size: size, tags: tags}, nil
}

func (c assetCleaner) planImages(ctx context.Context, target docker, rows map[assetKey]assetRow) ([]assetCandidate, error) {
	host := target.label()
	if len(c.configured) == 0 {
		// Without a known configuration nothing can be proved unconfigured.
		return nil, nil
	}
	var protectedIDs map[string]bool
	var candidates []assetCandidate
	var failures []error
	for _, key := range slices.SortedFunc(maps.Keys(rows), func(a, b assetKey) int { return cmp.Compare(a.name, b.name) }) {
		row := rows[key]
		if key.kind != assetKindImage || !row.pulled || c.configured[key.name] || c.pulling(host, key.name) ||
			c.now.Sub(row.idleSince()) <= c.policy.Images {
			continue
		}
		details, err := c.inspectImage(ctx, target, key.name)
		if err != nil {
			if imageMissing(err) {
				c.forget(ctx, key, "")
				continue
			}
			failures = append(failures, fmt.Errorf("inspect image %s: %w", logImage(key.name), err))
			continue
		}
		if row.imageID == "" || details.id != row.imageID {
			// The tag now names an image Runquill did not pull, such as a local rebuild.
			if row.imageID != "" {
				c.disown(ctx, key)
			}
			continue
		}
		if protectedIDs == nil {
			if protectedIDs, err = c.configuredIDs(ctx, target); err != nil {
				return candidates, errors.Join(append(failures, err)...)
			}
		}
		if protectedIDs[details.id] {
			continue
		}
		used, err := c.imageInUse(ctx, target, details.id)
		if err != nil {
			failures = append(failures, fmt.Errorf("check image %s: %w", logImage(key.name), err))
			continue
		}
		if used {
			continue
		}
		size := details.size
		if details.tags > 1 {
			size = 0 // another tag keeps the layers
		}
		candidates = append(candidates, assetCandidate{host: host, kind: assetKindImage, name: key.name, imageID: details.id,
			idle: c.now.Sub(row.idleSince()), size: size})
	}
	return candidates, errors.Join(failures...)
}

// configuredIDs resolves configured images on the host, so a configured image is kept under any other tag too.
func (c assetCleaner) configuredIDs(ctx context.Context, target docker) (map[string]bool, error) {
	ids := make(map[string]bool)
	for _, image := range slices.Sorted(maps.Keys(c.configured)) {
		details, err := c.inspectImage(ctx, target, image)
		if err != nil && !imageMissing(err) {
			return nil, fmt.Errorf("inspect configured image %s: %w", logImage(image), err)
		}
		if err == nil {
			ids[details.id] = true
		}
	}
	return ids, nil
}

func (c assetCleaner) imageInUse(ctx context.Context, target docker, id string) (bool, error) {
	output, err := c.command(ctx, target, "ps", "-a", "-q", "--no-trunc", "--filter", "ancestor="+id)
	return strings.TrimSpace(output) != "", err
}

func (c assetCleaner) forget(ctx context.Context, key assetKey, imageID string) {
	if !c.write {
		return
	}
	// The ID guard keeps a record of an image pulled again since it was selected.
	_, err := c.db.ExecContext(ctx, `DELETE FROM runner_assets WHERE host = ? AND kind = ? AND name = ? AND (? = '' OR image_id = ?)`,
		key.host, key.kind, key.name, imageID, imageID)
	if err != nil {
		slog.Warn("forget runner asset record", "host", key.host, "kind", key.kind, "name", logImage(key.name), "error", err)
	}
}

func (c assetCleaner) disown(ctx context.Context, key assetKey) {
	if !c.write {
		return
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE runner_assets SET pulled = 0, image_id = '' WHERE host = ? AND kind = 'image' AND name = ?`,
		key.host, key.name); err != nil {
		slog.Warn("update runner image record", "host", key.host, "image", logImage(key.name), "error", err)
	}
}

// engineRefused reports an engine answer meaning the object is in use or already gone; neither is a failure.
func engineRefused(err error) bool {
	text := strings.ToLower(err.Error())
	for _, phrase := range []string{"volume is in use", "volume is being used", "is using its referenced image", "image is in use",
		"image used by", "unable to delete", "unable to remove", "no such volume", "no such image", "image not known", "no volume with name"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// remove re-checks a candidate immediately before removing it. kept explains why it stayed; err is
// set only for an engine failure other than a refusal.
func (c assetCleaner) remove(ctx context.Context, target docker, candidate assetCandidate) (kept string, err error) {
	key := assetKey{candidate.host, candidate.kind, candidate.name}
	var args []string
	switch candidate.kind {
	case assetKindCache:
		label, err := c.command(ctx, target, "volume", "inspect", "--format", `{{index .Labels "io.runquill.cache"}}`, candidate.name)
		if err != nil {
			if engineRefused(err) {
				return "already gone", nil
			}
			return "inspect failed", err
		}
		if strings.TrimSpace(label) != "1" || !cacheVolumeName.MatchString(candidate.name) {
			return "not a Runquill cache volume", nil
		}
		if used, err := c.volumeInUse(ctx, target, candidate.name); err != nil {
			return "in-use check failed", err
		} else if used {
			return "in use", nil
		}
		args = []string{"volume", "rm", candidate.name}
	case assetKindImage:
		if c.configured[candidate.name] || c.pulling(candidate.host, candidate.name) {
			return "configured or being pulled", nil
		}
		details, err := c.inspectImage(ctx, target, candidate.name)
		if err != nil {
			if imageMissing(err) {
				c.forget(ctx, key, "")
				return "already gone", nil
			}
			return "inspect failed", err
		}
		if details.id != candidate.imageID {
			return "image changed", nil
		}
		if used, err := c.imageInUse(ctx, target, details.id); err != nil {
			return "in-use check failed", err
		} else if used {
			return "in use", nil
		}
		// By reference and without force: an image a container starts using meanwhile is refused.
		args = []string{"image", "rm", candidate.name}
	default:
		return "unknown kind", nil
	}
	if _, err := c.command(ctx, target, args...); err != nil {
		if engineRefused(err) {
			return "refused by the engine: " + oneLine(err), nil
		}
		return "remove failed", err
	}
	c.forget(ctx, key, candidate.imageID)
	return "", nil
}

// assetCleanupStatus is the in-memory summary other code reads.
type assetCleanupStatus struct {
	LastRun        time.Time `json:"last_run"`
	RemovedImages  int       `json:"removed_images"`
	RemovedVolumes int       `json:"removed_volumes"`
	ReclaimedBytes int64     `json:"reclaimed_bytes"`
	LastError      string    `json:"last_error,omitempty"`
}

type assetCleanupSummary struct {
	mu     sync.Mutex
	status assetCleanupStatus
}

// runnerAssetCleanup accumulates the manager's cleanup results since start.
var runnerAssetCleanup = &assetCleanupSummary{}

func (s *assetCleanupSummary) snapshot() assetCleanupStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *assetCleanupSummary) finish(at time.Time, removed []assetCandidate, failures []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastRun, s.status.LastError = at, ""
	for _, c := range removed {
		if c.kind == assetKindImage {
			s.status.RemovedImages++
		} else {
			s.status.RemovedVolumes++
		}
		s.status.ReclaimedBytes += c.size
	}
	if err := errors.Join(failures...); err != nil {
		s.status.LastError = oneLine(err)
	}
}

type assetCleanupDeps struct {
	Policy   assetPolicy
	DB       *sql.DB
	Recorder *assetRecorder
	Topology func() assetTopology
	Run      imageCommand
	Now      func() time.Time
	Summary  *assetCleanupSummary
}

// runAssetCleanup removes unused runner images and cache volumes on its own ticker until ctx ends.
func runAssetCleanup(ctx context.Context, deps assetCleanupDeps) {
	if !deps.Policy.enabled() {
		return
	}
	deps = deps.withDefaults()
	if skipped := deps.Topology().skipped; len(skipped) > 0 {
		slog.Info("unused runner images and cache volumes are cleaned on local and SSH Docker hosts only; not here", "hosts", skipped)
	}
	slog.Info("runner image and cache volume cleanup enabled", "images_after", formatRetention(deps.Policy.Images),
		"cache_after", formatRetention(deps.Policy.Cache), "interval", formatRetention(deps.Policy.Interval))
	ticker := time.NewTicker(deps.Policy.Interval)
	defer ticker.Stop()
	for {
		// The first pass runs at start, so a manager restarted more often than the interval still cleans.
		deps.cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (deps assetCleanupDeps) withDefaults() assetCleanupDeps {
	if deps.Run == nil {
		deps.Run = runImageCommand
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Summary == nil {
		deps.Summary = runnerAssetCleanup
	}
	return deps
}

// cycle runs one cleanup pass; one host's failure never stops the others.
func (deps assetCleanupDeps) cycle(ctx context.Context) {
	deps = deps.withDefaults()
	var removed []assetCandidate
	var failures []error
	defer func() {
		if err := recover(); err != nil {
			slog.Error("runner asset cleanup failed", "panic", err)
			failures = append(failures, fmt.Errorf("panic: %v", err))
		}
		deps.Summary.finish(deps.Now(), removed, failures)
	}()
	if deps.Recorder != nil {
		// Uses still in memory must count before anything is selected.
		if err := deps.Recorder.flush(ctx); err != nil {
			slog.Warn("runner asset cleanup skipped: usage records could not be saved", "error", err)
			failures = append(failures, err)
			return
		}
	}
	topology := deps.Topology()
	cleaner := assetCleaner{run: deps.Run, db: deps.DB, write: true, policy: deps.Policy, configured: topology.configured,
		busy: topology.busy, now: deps.Now()}
	for _, target := range topology.hosts {
		if ctx.Err() != nil {
			return
		}
		done, err := cleaner.clean(ctx, target, nil)
		removed = append(removed, done...)
		if err != nil {
			slog.Warn("runner asset cleanup", "host", target.label(), "error", oneLine(err))
			failures = append(failures, fmt.Errorf("%s: %w", target.label(), err))
		}
	}
}

// clean plans and removes on one host; report, when set, sees every candidate with the reason it was kept.
func (c assetCleaner) clean(ctx context.Context, target docker, report func(assetCandidate, string, error)) ([]assetCandidate, error) {
	candidates, err := c.plan(ctx, target)
	failures := []error{err}
	var removed []assetCandidate
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		kept, err := c.remove(ctx, target, candidate)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s %s: %w", candidate.kind, logImage(candidate.name), err))
		}
		if report != nil {
			report(candidate, kept, err)
		}
		if kept != "" {
			if err == nil {
				slog.Info("kept runner "+assetKindLabel(candidate.kind), "host", candidate.host, "name", logImage(candidate.name), "reason", kept)
			}
			continue
		}
		removed = append(removed, candidate)
		attrs := []any{"host", candidate.host, "name", logImage(candidate.name), "unused_for", formatIdle(candidate.idle)}
		if candidate.size > 0 {
			attrs = append(attrs, "reclaimed", formatBytes(candidate.size))
		}
		slog.Info("removed unused runner "+assetKindLabel(candidate.kind), attrs...)
	}
	return removed, errors.Join(failures...)
}

func assetKindLabel(kind string) string {
	if kind == assetKindCache {
		return "cache volume"
	}
	return kind
}

func formatIdle(d time.Duration) string {
	if d >= 48*time.Hour {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	return d.Truncate(time.Minute).String()
}

// formatBytes uses the decimal units Docker prints.
func formatBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	value, prefix := float64(n), 0
	for value >= unit && prefix < 4 {
		value /= unit
		prefix++
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + string("kMGTP"[prefix-1]) + "B"
}

// parseDecimalSize reads sizes such as "77.34MB" from docker system df.
func parseDecimalSize(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	for _, unit := range []struct {
		suffix string
		scale  float64
	}{{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1}} {
		if number, ok := strings.CutSuffix(value, unit.suffix); ok {
			n, err := strconv.ParseFloat(number, 64)
			if err != nil || n < 0 {
				return 0, false
			}
			return int64(n * unit.scale), true
		}
	}
	return 0, false
}

// assetHousekeeping is the manager's recorder and cleanup loop.
type assetHousekeeping struct {
	recorder *assetRecorder
	cancel   context.CancelFunc
	done     sync.WaitGroup
}

// startAssetCleanup validates the policy, migrates runner_assets, installs the usage recorder and starts the loop.
func startAssetCleanup(ctx context.Context, db *sql.DB, ops *operations) (*assetHousekeeping, error) {
	policy, err := loadAssetPolicy(os.Getenv)
	if err != nil {
		return nil, err
	}
	if err := migrateAssets(ctx, db); err != nil {
		return nil, fmt.Errorf("migrate runner asset records: %w", err)
	}
	h := &assetHousekeeping{recorder: newAssetRecorder(db, time.Now)}
	runnerAssets.Store(h.recorder)
	ctx, h.cancel = context.WithCancel(ctx)
	h.done.Go(func() { h.recorder.run(ctx) })
	if !policy.enabled() {
		slog.Info("automatic cleanup of unused runner images and cache volumes is off")
		return h, nil
	}
	h.done.Go(func() {
		runAssetCleanup(ctx, assetCleanupDeps{Policy: policy, DB: db, Recorder: h.recorder, Topology: assetTopologyFrom(ops)})
	})
	return h, nil
}

// stop ends the loop and saves pending uses; call it before the database closes.
func (h *assetHousekeeping) stop() {
	h.cancel()
	h.done.Wait()
	runnerAssets.CompareAndSwap(h.recorder, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.recorder.flush(ctx); err != nil {
		slog.Warn("record runner image and cache use", "error", err)
	}
}

// reportRunnerAssets prints, per local and SSH Docker host, the cache volumes and Runquill-pulled images present.
func reportRunnerAssets(ctx context.Context, fleet *dockerFleet, report func(string, error), info func(string)) {
	reportRunnerAssetsWith(ctx, runImageCommand, env("RUNQUILL_DB", "runquill.sqlite3"), fleet, report, info)
}

func reportRunnerAssetsWith(ctx context.Context, run imageCommand, path string, fleet *dockerFleet, report func(string, error), info func(string)) {
	topology := assetTopologyOf(runnerConfiguration{runtime: fleet, kind: "docker"})
	var db *sql.DB
	if _, err := os.Stat(path); err == nil {
		if db, err = openQueueDB(path, "ro"); err == nil {
			defer db.Close()
		}
	}
	cleaner := assetCleaner{run: run}
	for _, target := range topology.hosts {
		name := target.label() + " runner images and cache volumes"
		use, err := measureRunnerDisk(ctx, cleaner, db, target)
		if target.cache && err == nil {
			info(fmt.Sprintf("worker pool %q cache volumes on %s: %d", target.pool, target.label(), use.CacheVolumes))
		}
		if err != nil {
			report(name, err)
			continue
		}
		volumeText := fmt.Sprintf("%d cache volumes", use.CacheVolumes)
		if use.CacheBytes != nil && use.CacheVolumes > 0 {
			volumeText += " (" + formatBytes(*use.CacheBytes) + ")"
		}
		report(fmt.Sprintf("%s: %s, %d Runquill-pulled images (%s)", name, volumeText, use.Images, formatBytes(use.ImageBytes)), nil)
	}
}

// runnerDiskUse is what runner cache volumes and Runquill-pulled images take on one host.
type runnerDiskUse struct {
	CacheVolumes int `json:"cache_volumes"`
	// CacheBytes is absent when the engine cannot size volumes cheaply (Podman) or docker system df failed.
	CacheBytes *int64 `json:"cache_bytes,omitempty"`
	Images     int    `json:"images"`
	ImageBytes int64  `json:"image_bytes"`
}

// measureRunnerDisk runs docker system df -v and one image inspect per pulled image: call it on demand, not in a loop.
// A nil db counts no images.
func measureRunnerDisk(ctx context.Context, cleaner assetCleaner, db *sql.DB, target docker) (runnerDiskUse, error) {
	var use runnerDiskUse
	volumes, err := cleaner.cacheVolumes(ctx, target)
	if err != nil {
		return use, fmt.Errorf("list cache volumes: %w", err)
	}
	use.CacheVolumes = len(volumes)
	if size, ok := volumeSizes(ctx, cleaner, target, volumes); ok {
		use.CacheBytes = &size
	} else if len(volumes) == 0 {
		use.CacheBytes = new(int64)
	}
	if db == nil {
		return use, nil
	}
	rows, err := readAssetRows(ctx, db, target.label())
	if err != nil {
		return use, err
	}
	for key, row := range rows {
		if key.kind != assetKindImage || !row.pulled {
			continue
		}
		if details, err := cleaner.inspectImage(ctx, target, key.name); err == nil && details.id == row.imageID {
			use.Images++
			use.ImageBytes += details.size
		}
	}
	return use, nil
}

// volumeSizes is best effort: docker system df -v walks every volume, which suits doctor but not the loop.
func volumeSizes(ctx context.Context, c assetCleaner, target docker, names []string) (int64, bool) {
	engine, err := target.containerEngine()
	if err != nil || engine.name != "docker" || len(names) == 0 {
		return 0, false
	}
	output, err := c.command(ctx, target, "system", "df", "-v", "--format", "{{json .Volumes}}")
	if err != nil {
		return 0, false
	}
	var volumes []struct{ Name, Size string }
	if json.Unmarshal([]byte(output), &volumes) != nil {
		return 0, false
	}
	wanted := make(map[string]bool)
	for _, name := range names {
		wanted[name] = true
	}
	var total int64
	for _, v := range volumes {
		if wanted[v.Name] {
			size, ok := parseDecimalSize(v.Size)
			if !ok {
				return 0, false
			}
			total += size
		}
	}
	return total, true
}
