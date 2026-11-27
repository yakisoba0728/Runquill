package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeImage struct {
	id   string
	size int64
}

type fakeContainer struct {
	imageID string
	volumes []string
}

// fakeAssetHost is one engine's volumes, images and containers.
type fakeAssetHost struct {
	volumes    map[string]string // name -> io.runquill.cache label value; "" is unlabelled
	images     map[string]fakeImage
	containers []fakeContainer
	down       bool
	// ignoreFilter lists every volume, as an engine that ignored the label filter would.
	ignoreFilter bool
	// beforeRemove runs before an rm, to model a launch racing the cleanup.
	beforeRemove func(h *fakeAssetHost)
}

type fakeAssetEngine struct {
	mu    sync.Mutex
	hosts map[string]*fakeAssetHost
	calls []string
}

func newFakeAssetEngine() *fakeAssetEngine {
	return &fakeAssetEngine{hosts: make(map[string]*fakeAssetHost)}
}

func (e *fakeAssetEngine) host(label string) *fakeAssetHost {
	h := e.hosts[label]
	if h == nil {
		h = &fakeAssetHost{volumes: make(map[string]string), images: make(map[string]fakeImage)}
		e.hosts[label] = h
	}
	return h
}

func (e *fakeAssetEngine) removals() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, call := range e.calls {
		if strings.Contains(call, " rm ") {
			out = append(out, call)
		}
	}
	return out
}

func (h *fakeAssetHost) mounted(volume string) bool {
	return slices.ContainsFunc(h.containers, func(c fakeContainer) bool { return slices.Contains(c.volumes, volume) })
}

func (h *fakeAssetHost) usedImage(id string) bool {
	return slices.ContainsFunc(h.containers, func(c fakeContainer) bool { return c.imageID == id })
}

func (e *fakeAssetEngine) run(_ context.Context, target docker, _ time.Duration, args ...string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, target.label()+" "+strings.Join(args, " "))
	h := e.host(target.label())
	if h.down {
		return "", errors.New("docker volume: exit status 1: Cannot connect to the Docker daemon")
	}
	command := strings.Join(args[:min(2, len(args))], " ")
	last := args[len(args)-1]
	switch {
	case command == "volume ls":
		var out []string
		filtered := slices.Contains(args, "label="+cacheVolumeLabel) && !h.ignoreFilter
		for name, label := range h.volumes {
			if label == "1" || !filtered {
				out = append(out, name)
			}
		}
		slices.Sort(out)
		return strings.Join(out, "\n") + "\n", nil
	case command == "volume inspect":
		label, ok := h.volumes[last]
		if !ok {
			return "", errors.New("Error response from daemon: get " + last + ": no such volume")
		}
		return label + "\n", nil
	case command == "volume rm":
		if h.beforeRemove != nil {
			h.beforeRemove(h)
		}
		if _, ok := h.volumes[last]; !ok {
			return "", errors.New("Error response from daemon: get " + last + ": no such volume")
		}
		if h.mounted(last) {
			return "", errors.New("Error response from daemon: remove " + last + ": volume is in use - [abc]")
		}
		delete(h.volumes, last)
		return last + "\n", nil
	case args[0] == "ps":
		filter := last
		var out []string
		for i, c := range h.containers {
			if name, ok := strings.CutPrefix(filter, "volume="); ok && slices.Contains(c.volumes, name) ||
				strings.HasPrefix(filter, "ancestor=") && c.imageID == strings.TrimPrefix(filter, "ancestor=") {
				out = append(out, fmt.Sprintf("%064d", i+1))
			}
		}
		return strings.Join(out, "\n"), nil
	case command == "image inspect":
		ref := args[2]
		image, ok := h.images[ref]
		if !ok {
			return "", errors.New("docker image: exit status 1: Error response from daemon: No such image: " + ref)
		}
		if !strings.Contains(last, "|") {
			return image.id + "\n", nil
		}
		tags := 0
		for _, other := range h.images {
			if other.id == image.id {
				tags++
			}
		}
		return fmt.Sprintf("%s|%d|%d\n", image.id, image.size, tags), nil
	case command == "image rm":
		if slices.Contains(args, "-f") || slices.Contains(args, "--force") {
			return "", errors.New("test engine: forced removal")
		}
		if h.beforeRemove != nil {
			h.beforeRemove(h)
		}
		image, ok := h.images[last]
		if !ok {
			return "", errors.New("Error response from daemon: No such image: " + last)
		}
		if h.usedImage(image.id) {
			return "", errors.New("Error response from daemon: conflict: unable to delete " + last + " (must be forced) - container 1 is using its referenced image")
		}
		delete(h.images, last)
		return "Untagged: " + last + "\n", nil
	case args[0] == "pull":
		h.images[last] = fakeImage{id: "sha256:pulled-" + last, size: 4096}
		return "", nil
	case command == "system df":
		var parts []string
		for name := range h.volumes {
			parts = append(parts, fmt.Sprintf(`{"Name":%q,"Size":"1.5MB"}`, name))
		}
		slices.Sort(parts)
		return "[" + strings.Join(parts, ",") + "]\n", nil
	}
	return "", fmt.Errorf("fake engine: unexpected command %q", args)
}

type assetFixture struct {
	db     *sql.DB
	engine *fakeAssetEngine
	now    time.Time
	policy assetPolicy
	hosts  []docker
	config map[string]bool
}

const (
	oldVolume = "runquill-cache-42"
	oldImage  = "registry.example/runner:old"
)

// newAssetFixture holds one host where exactly one cache volume and one image may be removed.
func newAssetFixture(t *testing.T) *assetFixture {
	t.Helper()
	q := testQueue(t)
	if err := migrateAssets(t.Context(), q.DB); err != nil {
		t.Fatal(err)
	}
	f := &assetFixture{db: q.DB, engine: newFakeAssetEngine(), now: time.Unix(1_800_000_000, 0),
		policy: assetPolicy{Images: 30 * 24 * time.Hour, Cache: 30 * 24 * time.Hour, Interval: time.Hour},
		hosts:  []docker{{image: "runquill-runner:local"}}, config: map[string]bool{"runquill-runner:local": true}}
	h := f.engine.host("local")
	h.volumes[oldVolume] = "1"
	h.images[oldImage] = fakeImage{id: "sha256:old", size: 2_000_000}
	h.images["runquill-runner:local"] = fakeImage{id: "sha256:local", size: 9}
	h.containers = append(h.containers, fakeContainer{imageID: "sha256:local"})
	old := f.now.Add(-31 * 24 * time.Hour)
	f.row(t, "local", assetKindCache, oldVolume, old, false, "")
	f.row(t, "local", assetKindImage, oldImage, old, true, "sha256:old")
	f.row(t, "local", assetKindImage, "runquill-runner:local", old, false, "")
	return f
}

func (f *assetFixture) row(t *testing.T, host, kind, name string, at time.Time, pulled bool, id string) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT OR REPLACE INTO runner_assets (host, kind, name, first_seen, last_used, pulled, image_id)
        VALUES (?, ?, ?, ?, ?, ?, ?)`, host, kind, name, at.Unix(), at.Unix(), pulled, id); err != nil {
		t.Fatal(err)
	}
}

func (f *assetFixture) cleaner(write bool) assetCleaner {
	return assetCleaner{run: f.engine.run, db: f.db, write: write, policy: f.policy, configured: f.config, now: f.now}
}

func (f *assetFixture) deps(summary *assetCleanupSummary) assetCleanupDeps {
	return assetCleanupDeps{Policy: f.policy, DB: f.db, Run: f.engine.run, Now: func() time.Time { return f.now }, Summary: summary,
		Topology: func() assetTopology { return assetTopology{hosts: f.hosts, configured: f.config} }}
}

func (f *assetFixture) names(t *testing.T, host string) []string {
	t.Helper()
	candidates, err := f.cleaner(false).plan(t.Context(), docker{host: strings.TrimPrefix(host, "local")})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range candidates {
		names = append(names, c.kind+":"+c.name)
	}
	return names
}

func TestLoadAssetPolicy(t *testing.T) {
	day := 24 * time.Hour
	for _, test := range []struct {
		env                     map[string]string
		images, cache, interval time.Duration
		err                     string
	}{
		{nil, 30 * day, 30 * day, 6 * time.Hour, ""},
		{map[string]string{"RUNQUILL_CLEANUP_AFTER": "720h"}, 30 * day, 30 * day, 6 * time.Hour, ""},
		{map[string]string{"RUNQUILL_CLEANUP_AFTER": " 7d "}, 7 * day, 7 * day, 6 * time.Hour, ""},
		{map[string]string{"RUNQUILL_CLEANUP_AFTER": "off"}, 0, 0, 6 * time.Hour, ""},
		{map[string]string{"RUNQUILL_CLEANUP_AFTER": "0"}, 0, 0, 6 * time.Hour, ""},
		{map[string]string{"RUNQUILL_CLEANUP_AFTER": "1h", "RUNQUILL_CLEANUP_INTERVAL": "15m"}, time.Hour, time.Hour, 15 * time.Minute, ""},
		{map[string]string{"RUNQUILL_CLEANUP_IMAGES_AFTER": "90d", "RUNQUILL_CLEANUP_CACHE_AFTER": "off"}, 90 * day, 0, 6 * time.Hour, ""},
		{map[string]string{"RUNQUILL_CLEANUP_AFTER": "30m"}, 0, 0, 0, "RUNQUILL_CLEANUP_AFTER must be off or a duration of at least 1h, such as 720h or 30d"},
		{map[string]string{"RUNQUILL_CLEANUP_AFTER": "soon"}, 0, 0, 0, "RUNQUILL_CLEANUP_AFTER must be off"},
		{map[string]string{"RUNQUILL_CLEANUP_AFTER": "1.5d"}, 0, 0, 0, "RUNQUILL_CLEANUP_AFTER must be off"},
		{map[string]string{"RUNQUILL_CLEANUP_AFTER": "-30d"}, 0, 0, 0, "RUNQUILL_CLEANUP_AFTER must be off"},
		{map[string]string{"RUNQUILL_CLEANUP_CACHE_AFTER": "5m"}, 0, 0, 0, "RUNQUILL_CLEANUP_CACHE_AFTER must be off"},
		{map[string]string{"RUNQUILL_CLEANUP_INTERVAL": "1m"}, 0, 0, 0, "RUNQUILL_CLEANUP_INTERVAL must be a duration between 5m and 7d"},
	} {
		policy, err := loadAssetPolicy(func(name string) string { return test.env[name] })
		if test.err != "" {
			if err == nil || !strings.HasPrefix(err.Error(), test.err) {
				t.Errorf("%v: error %v, want %q", test.env, err, test.err)
			}
			continue
		}
		if err != nil || policy.Images != test.images || policy.Cache != test.cache || policy.Interval != test.interval {
			t.Errorf("%v: %+v %v", test.env, policy, err)
		}
	}
	if got := (assetPolicy{Images: 30 * day, Cache: 0, Interval: 6 * time.Hour}).String(); got != "images unused for 30d, cache volumes kept, checked every 6h" {
		t.Errorf("policy text %q", got)
	}
	if got := (assetPolicy{Interval: time.Hour}).String(); got != "automatic cleanup off" {
		t.Errorf("off policy text %q", got)
	}
}

func TestMigrateAssetsIsIdempotent(t *testing.T) {
	q := testQueue(t)
	rows, err := readAssetRows(t.Context(), q.DB, "local")
	if err != nil || len(rows) != 0 {
		t.Fatalf("a database without runner_assets has no rows: %v %v", rows, err)
	}
	for range 2 {
		if err := migrateAssets(t.Context(), q.DB); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Exec(`INSERT INTO runner_assets (host, kind, name, first_seen, last_used) VALUES ('local', 'volume', 'x', 1, 1)`); err == nil {
		t.Fatal("unknown kinds must be rejected")
	}
}

func TestAssetRecorderDebouncesUsageWrites(t *testing.T) {
	q := testQueue(t)
	now := time.Unix(1_800_000_000, 0)
	r := newAssetRecorder(q.DB, func() time.Time { return now })
	// The table is missing: the write fails and the uses stay pending.
	r.use(assetKey{"local", assetKindImage, "img"})
	if err := r.flush(t.Context()); err == nil || len(r.pending) != 1 {
		t.Fatalf("a failed flush must keep uses: %v %v", err, r.pending)
	}
	if err := migrateAssets(t.Context(), q.DB); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		now = now.Add(time.Second)
		r.use(assetKey{"local", assetKindImage, "img"})
		r.use(assetKey{"local", assetKindCache, "runquill-cache-" + strconv.Itoa(i%2)})
	}
	if len(r.pending) != 3 {
		t.Fatalf("pending %v", r.pending)
	}
	if err := r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, _ := readAssetRows(t.Context(), q.DB, "local")
	if len(rows) != 3 || !rows[assetKey{"local", assetKindImage, "img"}].lastUsed.Equal(now) {
		t.Fatalf("rows %v", rows)
	}
	// Within the resolution a busy fleet writes nothing more.
	now = now.Add(assetUsageResolution - time.Second)
	r.use(assetKey{"local", assetKindImage, "img"})
	if len(r.pending) != 0 {
		t.Fatalf("use within %s must not be written again: %v", assetUsageResolution, r.pending)
	}
	now = now.Add(2 * time.Second)
	r.use(assetKey{"local", assetKindImage, "img"})
	if len(r.pending) != 1 {
		t.Fatal("a use after the resolution must be written")
	}
	// A pull is always written and records the pulled ID.
	r.pull(assetKey{"local", assetKindImage, "img"}, "sha256:a")
	if err := r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, _ = readAssetRows(t.Context(), q.DB, "local")
	if row := rows[assetKey{"local", assetKindImage, "img"}]; !row.pulled || row.imageID != "sha256:a" || !row.lastUsed.Equal(now) {
		t.Fatalf("pulled row %+v", row)
	}
	// A later start does not clear the pulled mark.
	now = now.Add(time.Hour)
	r.use(assetKey{"local", assetKindImage, "img"})
	if err := r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, _ = readAssetRows(t.Context(), q.DB, "local")
	if row := rows[assetKey{"local", assetKindImage, "img"}]; !row.pulled || row.imageID != "sha256:a" {
		t.Fatalf("pulled mark lost: %+v", row)
	}
}

func installTestRecorder(t *testing.T, db *sql.DB, now func() time.Time) *assetRecorder {
	t.Helper()
	r := newAssetRecorder(db, now)
	previous := runnerAssets.Swap(r)
	t.Cleanup(func() { runnerAssets.Store(previous) })
	return r
}

func TestRunnerStartRecordsImageAndCacheVolumes(t *testing.T) {
	q := testQueue(t)
	if err := migrateAssets(t.Context(), q.DB); err != nil {
		t.Fatal(err)
	}
	noteRunnerAssets("local", "img", "runquill-cache-1") // no recorder: nothing happens
	r := installTestRecorder(t, q.DB, nil)
	tool := "runquill-cache-7-tools-" + strings.Repeat("a", 64)
	noteRunnerAssets("ssh://build", "ghcr.io/o/runner:2", "runquill-cache-7", tool)
	if err := r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, _ := readAssetRows(t.Context(), q.DB, "ssh://build")
	if len(rows) != 3 || rows[assetKey{"ssh://build", assetKindImage, "ghcr.io/o/runner:2"}].pulled ||
		rows[assetKey{"ssh://build", assetKindCache, "runquill-cache-7"}].firstSeen.IsZero() ||
		rows[assetKey{"ssh://build", assetKindCache, tool}].firstSeen.IsZero() {
		t.Fatalf("rows %v", rows)
	}
}

func TestPullerRecordsPulledImage(t *testing.T) {
	q := testQueue(t)
	if err := migrateAssets(t.Context(), q.DB); err != nil {
		t.Fatal(err)
	}
	r := installTestRecorder(t, q.DB, nil)
	engine := newFakeAssetEngine()
	images := testImagePull(t, "missing")
	deps := imagePullerDeps{Images: images, Run: engine.run, Now: time.Now, Pairs: testPairs("local=busybox:1.36")}
	engine.host("local").images["present:1"] = fakeImage{id: "sha256:p"}
	deps.cycle(t.Context())
	images.pulls.Wait()
	if err := r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, _ := readAssetRows(t.Context(), q.DB, "local")
	if row := rows[assetKey{"local", assetKindImage, "busybox:1.36"}]; !row.pulled || row.imageID != "sha256:pulled-busybox:1.36" || len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
}

func TestAssetCleanupRemovesOnlyUnusedRunquillAssets(t *testing.T) {
	day := 24 * time.Hour
	tool := oldVolume + "-tools-" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, f *assetFixture)
		want  []string
	}{
		{"baseline", func(*testing.T, *assetFixture) {}, []string{"cache:" + oldVolume, "image:" + oldImage}},
		{"old tool volume", func(t *testing.T, f *assetFixture) {
			f.engine.host("local").volumes[tool] = "1"
			f.row(t, "local", assetKindCache, tool, f.now.Add(-60*day), false, "")
		}, []string{"cache:" + oldVolume, "cache:" + tool, "image:" + oldImage}},
		{"new tool volume with no record", func(_ *testing.T, f *assetFixture) {
			f.engine.host("local").volumes[tool] = "1"
		}, []string{"cache:" + oldVolume, "image:" + oldImage}},
		{"cache label missing", func(_ *testing.T, f *assetFixture) { f.engine.host("local").volumes[oldVolume] = "" }, []string{"image:" + oldImage}},
		{"cache label other value", func(_ *testing.T, f *assetFixture) { f.engine.host("local").volumes[oldVolume] = "0" }, []string{"image:" + oldImage}},
		{"name pattern mismatch", func(t *testing.T, f *assetFixture) {
			h := f.engine.host("local")
			delete(h.volumes, oldVolume)
			for _, name := range []string{"runquill-cache-", "runquill-cache-4x", "runquill-cache-42-old", "runquill-cache-42-tools-abcd", "my-runquill-cache-42", "runquill-cache-owner/repo"} {
				h.volumes[name] = "1"
				f.row(t, "local", assetKindCache, name, f.now.Add(-60*day), false, "")
			}
		}, []string{"image:" + oldImage}},
		{"volume mounted by a stopped container", func(_ *testing.T, f *assetFixture) {
			h := f.engine.host("local")
			h.containers = append(h.containers, fakeContainer{imageID: "sha256:x", volumes: []string{oldVolume}})
		}, []string{"image:" + oldImage}},
		{"image not pulled by Runquill", func(t *testing.T, f *assetFixture) {
			f.row(t, "local", assetKindImage, oldImage, f.now.Add(-60*day), false, "sha256:old")
		}, []string{"cache:" + oldVolume}},
		{"image without a record", func(t *testing.T, f *assetFixture) {
			if _, err := f.db.Exec(`DELETE FROM runner_assets WHERE kind = 'image'`); err != nil {
				t.Fatal(err)
			}
		}, []string{"cache:" + oldVolume}},
		{"image still configured", func(_ *testing.T, f *assetFixture) { f.config[oldImage] = true }, []string{"cache:" + oldVolume}},
		{"image configured under another tag", func(_ *testing.T, f *assetFixture) {
			f.engine.host("local").images["runquill-runner:local"] = fakeImage{id: "sha256:old"}
		}, []string{"cache:" + oldVolume}},
		{"nothing configured", func(_ *testing.T, f *assetFixture) { clear(f.config) }, []string{"cache:" + oldVolume}},
		{"image used by a stopped container", func(_ *testing.T, f *assetFixture) {
			h := f.engine.host("local")
			h.containers = append(h.containers, fakeContainer{imageID: "sha256:old"})
		}, []string{"cache:" + oldVolume}},
		{"tag rebuilt locally since the pull", func(_ *testing.T, f *assetFixture) {
			f.engine.host("local").images[oldImage] = fakeImage{id: "sha256:rebuilt"}
		}, []string{"cache:" + oldVolume}},
		{"too recent", func(t *testing.T, f *assetFixture) {
			f.row(t, "local", assetKindCache, oldVolume, f.now.Add(-29*day), false, "")
			f.row(t, "local", assetKindImage, oldImage, f.now.Add(-30*day), true, "sha256:old")
		}, nil},
		{"recent use after an old first sighting", func(t *testing.T, f *assetFixture) {
			if _, err := f.db.Exec(`UPDATE runner_assets SET last_used = ?`, f.now.Add(-day).Unix()); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"per-kind retention", func(_ *testing.T, f *assetFixture) { f.policy.Images, f.policy.Cache = 0, 60*day }, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAssetFixture(t)
			test.setup(t, f)
			if got := f.names(t, "local"); !slices.Equal(got, test.want) {
				t.Fatalf("selected %v, want %v", got, test.want)
			}
			summary := &assetCleanupSummary{}
			f.deps(summary).cycle(t.Context())
			if status := summary.snapshot(); status.LastError != "" || status.RemovedImages+status.RemovedVolumes != len(test.want) {
				t.Fatalf("summary %+v", status)
			}
			removals := f.engine.removals()
			if len(removals) != len(test.want) {
				t.Fatalf("removals %v, want %v", removals, test.want)
			}
			for _, call := range removals {
				if strings.Contains(call, "-f") || strings.Contains(call, "prune") {
					t.Fatalf("forced or bulk removal %q", call)
				}
			}
			h := f.engine.host("local")
			if _, ok := h.images["runquill-runner:local"]; !ok {
				t.Fatal("configured image removed")
			}
		})
	}
}

func TestAssetCleanupFirstSeenGraceAfterUpgrade(t *testing.T) {
	f := newAssetFixture(t)
	if _, err := f.db.Exec(`DELETE FROM runner_assets`); err != nil {
		t.Fatal(err)
	}
	summary := &assetCleanupSummary{}
	f.deps(summary).cycle(t.Context())
	if len(f.engine.removals()) != 0 {
		t.Fatal("nothing may be removed before a full retention has been observed")
	}
	rows, _ := readAssetRows(t.Context(), f.db, "local")
	if row, ok := rows[assetKey{"local", assetKindCache, oldVolume}]; !ok || !row.firstSeen.Equal(f.now) {
		t.Fatalf("existing volume not first seen now: %v", rows)
	}
	f.now = f.now.Add(30 * 24 * time.Hour)
	f.deps(summary).cycle(t.Context())
	if len(f.engine.removals()) != 0 {
		t.Fatal("exactly the retention is not more than the retention")
	}
	f.now = f.now.Add(time.Second)
	f.deps(summary).cycle(t.Context())
	if removals := f.engine.removals(); len(removals) != 1 || !strings.Contains(removals[0], "volume rm "+oldVolume) {
		t.Fatalf("removals %v; an image without a pull record must never be removed", removals)
	}
	rows, _ = readAssetRows(t.Context(), f.db, "local")
	if _, ok := rows[assetKey{"local", assetKindCache, oldVolume}]; ok {
		t.Fatal("the removed volume's record must be dropped")
	}
}

func TestAssetCleanupDisabledPolicyDoesNothing(t *testing.T) {
	f := newAssetFixture(t)
	f.policy = assetPolicy{Interval: time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		runAssetCleanup(ctx, f.deps(&assetCleanupSummary{}))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a disabled policy must return at once")
	}
	if len(f.engine.calls) != 0 {
		t.Fatalf("calls %v", f.engine.calls)
	}
}

func TestAssetCleanupRechecksLabelBeforeRemoving(t *testing.T) {
	f := newAssetFixture(t)
	f.policy.Images = 0
	h := f.engine.host("local")
	h.volumes[oldVolume] = ""
	h.ignoreFilter = true
	if got := f.names(t, "local"); !slices.Equal(got, []string{"cache:" + oldVolume}) {
		t.Fatalf("selected %v", got)
	}
	summary := &assetCleanupSummary{}
	f.deps(summary).cycle(t.Context())
	if _, ok := h.volumes[oldVolume]; !ok || len(f.engine.removals()) != 0 || summary.snapshot().RemovedVolumes != 0 {
		t.Fatalf("an unlabelled volume was removed: %v", f.engine.removals())
	}
}

func TestAssetCleanupRunsAtStart(t *testing.T) {
	f := newAssetFixture(t)
	summary := &assetCleanupSummary{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runAssetCleanup(ctx, f.deps(summary))
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for summary.snapshot().LastRun.IsZero() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if status := summary.snapshot(); status.RemovedVolumes != 1 || status.RemovedImages != 1 {
		t.Fatalf("the first pass must run at start: %+v", status)
	}
}

func TestAssetCleanupSkipsImageBeingPulled(t *testing.T) {
	f := newAssetFixture(t)
	images := testImagePull(t, "missing")
	if !images.claim("local", oldImage, false, f.now) {
		t.Fatal("claim")
	}
	deps := f.deps(&assetCleanupSummary{})
	deps.Topology = func() assetTopology { return assetTopology{hosts: f.hosts, configured: f.config, images: images} }
	deps.cycle(t.Context())
	if removals := f.engine.removals(); len(removals) != 1 || !strings.Contains(removals[0], "volume rm") {
		t.Fatalf("removals %v", removals)
	}
	images.finish("local", oldImage, false, nil, f.now)
}

func TestAssetCleanupToleratesEngineRefusal(t *testing.T) {
	f := newAssetFixture(t)
	h := f.engine.host("local")
	// A launch mounts the volume and starts from the image after the re-check.
	h.beforeRemove = func(h *fakeAssetHost) {
		h.containers = append(h.containers, fakeContainer{imageID: "sha256:old", volumes: []string{oldVolume}})
	}
	summary := &assetCleanupSummary{}
	f.deps(summary).cycle(t.Context())
	if status := summary.snapshot(); status.LastError != "" || status.RemovedImages != 0 || status.RemovedVolumes != 0 || status.LastRun.IsZero() {
		t.Fatalf("a refusal is not an error: %+v", status)
	}
	if _, ok := h.volumes[oldVolume]; !ok {
		t.Fatal("volume removed")
	}
	if _, ok := h.images[oldImage]; !ok {
		t.Fatal("image removed")
	}
	rows, _ := readAssetRows(t.Context(), f.db, "local")
	if _, ok := rows[assetKey{"local", assetKindImage, oldImage}]; !ok {
		t.Fatal("a kept image keeps its record")
	}
}

func TestAssetCleanupIsolatesHosts(t *testing.T) {
	f := newAssetFixture(t)
	f.hosts = []docker{{host: "ssh://down"}, {image: "runquill-runner:local"}}
	f.engine.host("ssh://down").down = true
	summary := &assetCleanupSummary{}
	f.deps(summary).cycle(t.Context())
	status := summary.snapshot()
	if status.RemovedImages != 1 || status.RemovedVolumes != 1 || status.ReclaimedBytes != 2_000_000 ||
		!strings.Contains(status.LastError, "ssh://down") || strings.Contains(status.LastError, "local") {
		t.Fatalf("summary %+v", status)
	}
	// A panic on one pass is recovered and recorded.
	deps := f.deps(summary)
	deps.Topology = func() assetTopology { panic("topology") }
	deps.cycle(t.Context())
	if status := summary.snapshot(); !strings.Contains(status.LastError, "panic") {
		t.Fatalf("summary %+v", status)
	}
}

func TestAssetCleanupLoopAndCLIShareSelection(t *testing.T) {
	f := newAssetFixture(t)
	h := f.engine.host("local")
	h.volumes["runquill-cache-43"] = "1" // no record: first seen now
	h.volumes["runquill-cache-44"] = "1"
	h.containers = append(h.containers, fakeContainer{volumes: []string{"runquill-cache-44"}})
	f.row(t, "local", assetKindCache, "runquill-cache-44", f.now.Add(-90*24*time.Hour), false, "")
	topology := assetTopology{hosts: f.hosts, configured: f.config}

	var dry strings.Builder
	if err := writeUnusedAssets(t.Context(), &dry, f.cleaner(false), topology, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HOST   KIND          NAME", "local  cache volume  " + oldVolume, "local  image         " + oldImage,
		"31d", "2.0 MB", "Would remove 1 images and 1 cache volumes (2.0 MB of images)."} {
		if !strings.Contains(dry.String(), want) {
			t.Errorf("dry run output lacks %q:\n%s", want, dry.String())
		}
	}
	if len(f.engine.removals()) != 0 {
		t.Fatal("dry run removed something")
	}
	rows, _ := readAssetRows(t.Context(), f.db, "local")
	if _, ok := rows[assetKey{"local", assetKindCache, "runquill-cache-43"}]; ok {
		t.Fatal("dry run must not write first sightings")
	}
	loop, err := f.cleaner(true).plan(t.Context(), f.hosts[0])
	if err != nil {
		t.Fatal(err)
	}
	cli, _ := f.cleaner(false).plan(t.Context(), f.hosts[0])
	if !slices.EqualFunc(loop, cli, func(a, b assetCandidate) bool { return a == b }) || len(loop) != 2 {
		t.Fatalf("loop %v, CLI %v", loop, cli)
	}
	var wet strings.Builder
	if err := writeUnusedAssets(t.Context(), &wet, f.cleaner(true), topology, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wet.String(), "removed") || !strings.Contains(wet.String(), "Removed 1 images and 1 cache volumes (2.0 MB of images); kept 0.") {
		t.Fatalf("output:\n%s", wet.String())
	}
	if _, ok := h.volumes["runquill-cache-44"]; !ok {
		t.Fatal("mounted volume removed")
	}
	var empty strings.Builder
	topology.skipped = []string{"agent://w1"}
	if err := writeUnusedAssets(t.Context(), &empty, f.cleaner(false), topology, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty.String(), "Skipped agent://w1: image and cache volume cleanup supports local and SSH Docker hosts only.") ||
		!strings.Contains(empty.String(), "No unused runner images or cache volumes found on reachable hosts.") {
		t.Fatalf("output:\n%s", empty.String())
	}
}

func TestAssetTopologyProtectsConfiguredImages(t *testing.T) {
	fleet := &dockerFleet{hosts: []docker{{image: "default:1"}, {image: "pool:2", pool: "big"}, {host: "ssh://b", image: "default:1"},
		{host: "agent://w1", image: "agent:3"}}}
	topology := assetTopologyOf(runnerConfiguration{runtime: fleet, kind: "docker"})
	var hosts []string
	for _, host := range topology.hosts {
		hosts = append(hosts, host.label())
	}
	if !slices.Equal(hosts, []string{"local", "ssh://b"}) || !slices.Equal(topology.skipped, []string{"agent://w1"}) {
		t.Fatalf("hosts %v skipped %v", hosts, topology.skipped)
	}
	for _, image := range []string{"default:1", "pool:2", "agent:3"} {
		if !topology.configured[image] {
			t.Errorf("%s is not protected", image)
		}
	}
	if topology.busy("local", "default:1") {
		t.Fatal("no puller, nothing busy")
	}
	kube := assetTopologyOf(runnerConfiguration{runtime: &kubeRunners{}, kind: "kubernetes"})
	if len(kube.hosts) != 0 || !slices.Equal(kube.skipped, []string{"kubernetes"}) {
		t.Fatalf("kubernetes topology %+v", kube)
	}
}

func TestGCCommandValidatesFlags(t *testing.T) {
	t.Setenv("RUNQUILL_DB", filepath.Join(t.TempDir(), "missing.sqlite3"))
	t.Setenv("RUNQUILL_CLEANUP_AFTER", "")
	for _, test := range []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{"--dry-run", "--remove-orphans"}, 2},
		{[]string{"--unused", "--remove-orphans"}, 2},
		{[]string{"--dry-run", "--older-than", "30d"}, 2},
		{[]string{"--remove-orphans", "--older-than", "30d"}, 2},
		{[]string{"--unused", "--older-than", "30m"}, 2},
		{[]string{"--unused", "--older-than", "off"}, 2},
		{[]string{"--unused", "extra"}, 2},
		{[]string{"--remove-orphans", "--grace", "10s"}, 2},
		{[]string{"--bogus"}, 2},
		{[]string{"-h"}, 0},
		// Valid combinations reach the database, which is missing here.
		{[]string{"--unused", "--dry-run"}, 1},
		{[]string{"--unused", "--dry-run", "--older-than", "720h"}, 1},
	} {
		var stderr strings.Builder
		if code := gcCommand(t.Context(), test.args, &strings.Builder{}, &stderr); code != test.code {
			t.Errorf("%v: exit %d, want %d\n%s", test.args, code, test.code, stderr.String())
		}
		if test.code == 2 && !strings.Contains(stderr.String(), "--unused cannot be combined with --remove-orphans.") {
			t.Errorf("%v: usage missing:\n%s", test.args, stderr.String())
		}
	}
	t.Setenv("RUNQUILL_CLEANUP_AFTER", "off")
	if err := gcUnused(t.Context(), &strings.Builder{}, true, 0); err == nil || !strings.Contains(err.Error(), "--older-than") {
		t.Fatalf("off policy without --older-than: %v", err)
	}
}

func TestDoctorReportsRunnerAssets(t *testing.T) {
	f := newAssetFixture(t)
	path := filepath.Join(t.TempDir(), "q.sqlite3")
	q, err := openQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	defer q.close()
	if err := migrateAssets(t.Context(), q.DB); err != nil {
		t.Fatal(err)
	}
	f.db = q.DB
	f.row(t, "local", assetKindImage, oldImage, f.now, true, "sha256:old")
	f.engine.host("local").volumes["unlabelled"] = ""
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	var out []string
	reportRunnerAssetsWith(t.Context(), f.engine.run, path, &dockerFleet{hosts: f.hosts}, func(name string, err error) {
		out = append(out, fmt.Sprintf("%s %v", name, err))
	}, func(string) {})
	want := "local runner images and cache volumes: 1 cache volumes (1.5 MB), 1 Runquill-pulled images (2.0 MB) <nil>"
	if !slices.Equal(out, []string{want}) {
		t.Fatalf("doctor %q", out)
	}
}

func TestAssetFormatting(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1500: "1.5 kB", 2_000_000: "2.0 MB", 7_340_000_000: "7.3 GB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q", n, got)
		}
	}
	for raw, want := range map[string]int64{"77.34MB": 77_340_000, "0B": 0, "1.2GB": 1_200_000_000, "12kB": 12_000} {
		if got, ok := parseDecimalSize(raw); !ok || got != want {
			t.Errorf("parseDecimalSize(%q) = %d %t", raw, got, ok)
		}
	}
	if _, ok := parseDecimalSize("N/A"); ok {
		t.Error("N/A is not a size")
	}
	for err, refused := range map[string]bool{
		"Error response from daemon: remove v: volume is in use - [abc]":                                       true,
		"conflict: unable to delete busybox:1.36 (must be forced) - container 1 is using its referenced image": true,
		"Error response from daemon: No such image: x":                                                         true,
		"Error: no volume with name \"v\" found: no such volume":                                               true,
		"Cannot connect to the Docker daemon":                                                                  false,
		"ssh: Could not resolve hostname build: no such host":                                                  false,
	} {
		if engineRefused(errors.New(err)) != refused {
			t.Errorf("engineRefused(%q) != %t", err, refused)
		}
	}
}

// TestAssetCleanupOnRealDocker pulls busybox:1.36 through the puller and removes it and an old labelled
// cache volume, while a mounted cache volume, an unlabelled volume and a local image survive.
// It runs only with RUNQUILL_TEST_DOCKER_CLEANUP=1 and removes everything it creates.
func TestAssetCleanupOnRealDocker(t *testing.T) {
	if os.Getenv("RUNQUILL_TEST_DOCKER_CLEANUP") != "1" {
		t.Skip("set RUNQUILL_TEST_DOCKER_CLEANUP=1 to run against the local Docker engine")
	}
	t.Setenv("RUNQUILL_CONTAINER_ENGINE", "docker")
	const pulled = "busybox:1.36"
	dockerRun := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	exists := func(kind, name string) bool {
		return exec.Command("docker", kind, "inspect", name).Run() == nil
	}
	suffix := strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 10)
	// The unlabelled volume has a cache volume's name, so only the missing label saves it.
	oldCache, mountedCache, plain := "runquill-cache-9"+suffix, "runquill-cache-8"+suffix, "runquill-cache-7"+suffix
	local := "runquill-cleanup-test-local:" + suffix
	keep := "runquill-cleanup-test-keep:" + suffix
	for _, name := range []string{oldCache, mountedCache, plain} {
		if exists("volume", name) {
			t.Fatalf("volume %s already exists", name)
		}
	}
	// Keep a busybox:1.36 the machine already had: a second tag holds its layers and restores the tag.
	hadBusybox := exists("image", pulled)
	if hadBusybox {
		if out := dockerRun("ps", "-a", "-q", "--filter", "ancestor="+pulled); out != "" {
			t.Skip("existing containers use " + pulled)
		}
		dockerRun("tag", pulled, keep)
	}
	var container string
	t.Cleanup(func() {
		if container != "" {
			exec.Command("docker", "rm", "-f", container).Run()
		}
		exec.Command("docker", "volume", "rm", oldCache, mountedCache, plain).Run()
		if hadBusybox {
			exec.Command("docker", "tag", keep, pulled).Run()
			exec.Command("docker", "image", "rm", keep).Run()
		} else {
			exec.Command("docker", "image", "rm", pulled).Run()
		}
		exec.Command("docker", "image", "rm", local).Run()
	})
	// A distinct local image that cannot be pulled: an empty filesystem imported from a tar.
	importCmd := exec.Command("docker", "import", "-", local)
	importCmd.Stdin = strings.NewReader(strings.Repeat("\x00", 1024))
	if out, err := importCmd.CombinedOutput(); err != nil {
		t.Fatalf("import: %v %s", err, out)
	}
	dockerRun("volume", "create", "--label", cacheVolumeLabel, oldCache)
	dockerRun("volume", "create", "--label", cacheVolumeLabel, mountedCache)
	dockerRun("volume", "create", plain)
	container = dockerRun("create", "--mount", "type=volume,src="+mountedCache+",dst=/cache", local, "/none")

	q := testQueue(t)
	if err := migrateAssets(t.Context(), q.DB); err != nil {
		t.Fatal(err)
	}
	r := installTestRecorder(t, q.DB, nil)
	images := testImagePull(t, "always")
	puller := imagePullerDeps{Images: images, Run: runImageCommand, Now: time.Now, Pairs: testPairs("local=" + pulled)}
	puller.cycle(t.Context())
	images.pulls.Wait()
	if err := r.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, _ := readAssetRows(t.Context(), q.DB, "local")
	if row := rows[assetKey{"local", assetKindImage, pulled}]; !row.pulled || !strings.HasPrefix(row.imageID, "sha256:") {
		t.Fatalf("pull not recorded: %+v", rows)
	}
	// Back-date everything, including survivors, so exactly one condition saves each of them.
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()
	for _, row := range []struct {
		kind, name string
		pulled     bool
	}{{assetKindCache, oldCache, false}, {assetKindCache, mountedCache, false}, {assetKindCache, plain, false}, {assetKindImage, local, false}} {
		if _, err := q.Exec(`INSERT OR REPLACE INTO runner_assets (host, kind, name, first_seen, last_used, pulled) VALUES ('local', ?, ?, ?, ?, ?)`,
			row.kind, row.name, old, old, row.pulled); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Exec(`UPDATE runner_assets SET first_seen = ?, last_used = ? WHERE name = ?`, old, old, pulled); err != nil {
		t.Fatal(err)
	}
	summary := &assetCleanupSummary{}
	// Only the test's own volumes are in scope: other runquill-cache volumes on this machine have no
	// record in this database, so they are first seen now and kept.
	deps := assetCleanupDeps{Policy: assetPolicy{Images: 30 * 24 * time.Hour, Cache: 30 * 24 * time.Hour, Interval: time.Hour},
		DB: q.DB, Recorder: r, Run: runImageCommand, Summary: summary,
		Topology: func() assetTopology {
			return assetTopology{hosts: []docker{{image: local}}, configured: map[string]bool{"runquill-runner:never-configured-" + suffix: true}}
		}}
	deps.cycle(t.Context())
	status := summary.snapshot()
	if status.LastError != "" || status.RemovedImages != 1 || status.RemovedVolumes != 1 {
		t.Fatalf("summary %+v", status)
	}
	if exists("volume", oldCache) || exists("image", pulled) {
		t.Fatal("the unused cache volume and pulled image must be removed")
	}
	for kind, names := range map[string][]string{"volume": {mountedCache, plain}, "image": {local}} {
		for _, name := range names {
			if !exists(kind, name) {
				t.Errorf("%s %s was removed", kind, name)
			}
		}
	}
	if hadBusybox && !exists("image", keep) {
		t.Fatal("the machine's busybox layers were removed")
	}
}
