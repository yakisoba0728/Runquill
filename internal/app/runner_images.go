package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/yakisoba0728/Runquill/internal/route"
)

// A workflow picks its runner OS inside the one per-job label it already uses:
//
//	runs-on: ${{ format('runquill-{0}-{1}-os__ubuntu2204__build', github.run_id, github.run_attempt) }}
//	runs-on: ${{ format('runquill-{0}-{1}-pool__big__os__debian12__build', github.run_id, github.run_attempt) }}
//
// A bare `runs-on: ubuntu-22.04` is deliberately not supported. Ephemeral runners are paired with
// jobs only because each runner is registered with a label no other job uses; a label shared by many
// jobs lets GitHub hand a runner a different job than the one it was started for, and the
// GitHub-hosted names would also make GitHub-hosted runners compete for the same jobs.
//
// Route labels cannot contain dots (routeSuffix in webhook.go), so names are compared by a key
// without dots and hyphens: ubuntu22.04, ubuntu-22.04, ubuntu2204 and ubuntu-22-04 are one entry.
//
// A workflow can only name a catalogue entry, never an image reference: runners may run
// privileged, so which images they start from stays an administrator decision.

const (
	runnerImagesEnv = "RUNQUILL_RUNNER_IMAGES"
	maxRunnerImages = 64
	// imageDemandTTL keeps an on-demand image in the puller's pairs after the last job asked for it.
	imageDemandTTL = time.Hour
	// imageCheckTTL limits engine inspects of on-demand images when Runquill does not pull them.
	imageCheckTTL = 15 * time.Second

	runnerImageSourceRelease    = "release"
	runnerImageSourceDefault    = "default"
	runnerImageSourceConfigured = "configured"
	runnerOSLatest              = "ubuntu-latest"
)

var (
	// runnerImageName is a catalogue name; dots are allowed and dropped in the label form.
	runnerImageName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,39}$`)
	// releaseRunnerOS lists the runner variants each release publishes as <image>-runner:<version>-<os>.
	releaseRunnerOS = []string{"ubuntu22.04", "ubuntu26.04", "debian12", "debian13"}
)

// runnerImageKey folds the spellings of one catalogue name together.
func runnerImageKey(name string) string {
	return strings.NewReplacer(".", "", "-", "").Replace(strings.ToLower(name))
}

// runnerOSLabelName is the form of a catalogue name that fits a route label.
func runnerOSLabelName(name string) string {
	return strings.ReplaceAll(name, ".", "")
}

// routeOS returns the OS NAME a stored route selects, or "" when it selects none.
func routeOS(label string) (string, error) {
	parts := strings.SplitN(label, "-", 4)
	if len(parts) != 4 {
		return "", nil
	}
	suffix := parts[3]
	if requested, ok := strings.CutPrefix(suffix, "pool__"); ok {
		if _, after, found := strings.Cut(requested, "__"); found {
			suffix = after
		}
	}
	if !strings.HasPrefix(suffix, "os__") {
		return "", nil
	}
	_, osName, _, err := route.SplitSuffix(suffix)
	return osName, err
}

type runnerImage struct {
	Name string `json:"name"`
	// Label is the NAME to write in os__NAME__ selectors.
	Label string `json:"label"`
	Image string `json:"image"`
	// Source is release (published with this Runquill version), default (RUNQUILL_RUNNER_IMAGE) or configured.
	Source string `json:"source"`
}

// runnerImages is the catalogue of runner images a workflow may select by name.
type runnerImages struct {
	defaultImage string
	entries      map[string]runnerImage
}

func loadRunnerImages(getenv func(string) string, defaultImage string) (*runnerImages, error) {
	return buildRunnerImages(getenv(runnerImagesEnv), releaseImage, version, defaultImage)
}

// releaseRunnerImage is <release>-runner:<version> for a release build, or "" for a development build.
func releaseRunnerImage(release, ver string) string {
	if release == "" || ver == "" || ver == "dev" || !setupImage.MatchString(release+":"+ver) {
		return ""
	}
	return release + "-runner:" + ver
}

func buildRunnerImages(raw, release, ver, defaultImage string) (*runnerImages, error) {
	c := &runnerImages{defaultImage: defaultImage, entries: make(map[string]runnerImage)}
	add := func(name, image, source string) {
		c.entries[runnerImageKey(name)] = runnerImage{Name: name, Label: runnerOSLabelName(name), Image: image, Source: source}
	}
	add(runnerOSLatest, "", runnerImageSourceDefault)
	if base := releaseRunnerImage(release, ver); base != "" {
		add("ubuntu24.04", base, runnerImageSourceRelease)
		for _, name := range releaseRunnerOS {
			add(name, base+"-"+name, runnerImageSourceRelease)
		}
	} else {
		// A development build publishes no variants; only the default image is known to exist.
		add("ubuntu24.04", "", runnerImageSourceDefault)
	}
	if strings.TrimSpace(raw) == "" {
		return c, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var configured map[string]string
	if err := decoder.Decode(&configured); err != nil || configured == nil {
		return nil, fmt.Errorf("%s must be a JSON object of NAME: image reference", runnerImagesEnv)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must contain exactly one JSON object", runnerImagesEnv)
	}
	if len(configured) > maxRunnerImages {
		return nil, fmt.Errorf("%s may name at most %d images", runnerImagesEnv, maxRunnerImages)
	}
	seen := make(map[string]string)
	for _, name := range slices.Sorted(maps.Keys(configured)) {
		if !runnerImageName.MatchString(name) {
			return nil, fmt.Errorf("%s: invalid name %q (use 1-40 lowercase letters, digits, dots or hyphens)", runnerImagesEnv, name)
		}
		key := runnerImageKey(name)
		if other, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s: names %q and %q are the same label", runnerImagesEnv, other, name)
		}
		seen[key] = name
		if err := checkRunnerImageReference(configured[name]); err != nil {
			return nil, fmt.Errorf("%s: image for %q %w", runnerImagesEnv, name, err)
		}
		add(name, configured[name], runnerImageSourceConfigured)
	}
	return c, nil
}

// checkRunnerImageReference applies the pool image rules and refuses credentials in the reference.
func checkRunnerImageReference(image string) error {
	if !validWorkerImage(image) {
		return errors.New("must be a Docker image reference")
	}
	if strings.Contains(image, "://") || imageUserinfo.MatchString(image) {
		return errors.New("must not contain a scheme or credentials; log the engine in to the registry instead")
	}
	return nil
}

// withDefault follows a live change of the default runner image.
func (c *runnerImages) withDefault(image string) *runnerImages {
	if c == nil {
		return nil
	}
	next := *c
	next.defaultImage = image
	return &next
}

func (c *runnerImages) resolved(entry runnerImage) runnerImage {
	if entry.Source == runnerImageSourceDefault && entry.Image == "" {
		entry.Image = c.defaultImage
	}
	return entry
}

// list returns the catalogue sorted by name, with default entries resolved.
func (c *runnerImages) list() []runnerImage {
	if c == nil {
		return nil
	}
	out := make([]runnerImage, 0, len(c.entries))
	for _, entry := range c.entries {
		out = append(out, c.resolved(entry))
	}
	slices.SortFunc(out, func(a, b runnerImage) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// lookup finds a catalogue entry by any of its spellings.
func (c *runnerImages) lookup(name string) (runnerImage, bool) {
	if c == nil {
		return runnerImage{}, false
	}
	entry, ok := c.entries[runnerImageKey(name)]
	return entry, ok
}

// hostedRunnerOS returns the os__ label that keeps a job on the OS of its GitHub-hosted Ubuntu
// label (ubuntu-22.04, ubuntu-24.04, ...) when the runner image catalogue has that entry.
// The built-in ubuntu-latest is the default image, which a plain route already uses.
func (images *runnerImages) hostedRunnerOS(labels []string) string {
	found := ""
	for _, label := range labels {
		lower := strings.ToLower(label)
		if !strings.HasPrefix(lower, "ubuntu-") {
			continue
		}
		entry, ok := images.lookup(lower)
		if !ok || entry.Source == runnerImageSourceDefault && runnerImageKey(entry.Name) == runnerImageKey(runnerOSLatest) {
			continue
		}
		if found != "" {
			return ""
		}
		found = entry.Label
	}
	return found
}

// forJob resolves a job's OS selector. The returned Image is empty when the job selects no OS and
// runs its host's image (the pool's image, or the default image).
func (c *runnerImages) forJob(route string, pool workerPool) (runnerImage, error) {
	name, err := routeOS(route)
	if err != nil || name == "" {
		return runnerImage{}, err
	}
	entry, ok := c.lookup(name)
	if !ok {
		return runnerImage{}, fmt.Errorf("runner image %q is not in the runner image catalogue; add it to %s", name, runnerImagesEnv)
	}
	if !pool.allowsImage(name) {
		return runnerImage{}, fmt.Errorf("the worker pool does not allow runner image %q", name)
	}
	return c.resolved(entry), nil
}

// effective is the image a job runs or will run, for display.
func (c *runnerImages) effective(route string, pool workerPool) string {
	if c == nil {
		return ""
	}
	selected, err := c.forJob(route, pool)
	switch {
	case err != nil:
		return ""
	case selected.Image != "":
		return selected.Image
	case pool.Image != "":
		return pool.Image
	}
	return c.defaultImage
}

// allowsImage reports whether jobs in the pool may select the catalogue name.
func (p workerPool) allowsImage(name string) bool {
	return p.Images == nil || slices.ContainsFunc(p.Images, func(allowed string) bool { return runnerImageKey(allowed) == runnerImageKey(name) })
}

// checkPoolImageNames validates a pool's optional images list; names are not required to exist,
// so removing a catalogue entry never makes saved settings unloadable.
func checkPoolImageNames(names []string) error {
	// An empty list would mean "allow none" here but is dropped on save, which reads as "allow all".
	if names != nil && len(names) == 0 {
		return errors.New("images must list at least one name or be omitted")
	}
	if len(names) > maxRunnerImages {
		return fmt.Errorf("images may list at most %d names", maxRunnerImages)
	}
	for _, name := range names {
		if !runnerImageName.MatchString(name) {
			return fmt.Errorf("images has an invalid name %q", name)
		}
	}
	return nil
}

type runnerImageKeyCtx struct{}

// withRunnerImage asks the runtime to start a runner from image instead of its host's image.
func withRunnerImage(ctx context.Context, image string) context.Context {
	if image == "" {
		return ctx
	}
	return context.WithValue(ctx, runnerImageKeyCtx{}, image)
}

func runnerImageFrom(ctx context.Context) string {
	image, _ := ctx.Value(runnerImageKeyCtx{}).(string)
	return image
}

// runnerImageRuntime is implemented by runtimes whose hosts cannot all start every image.
// A runtime without it (Kubernetes, where the kubelet pulls) can start any image anywhere.
type runnerImageRuntime interface {
	// canRun is cheap; own reports that image is the host's configured image, gated by its snapshot.
	canRun(host, image string) (eligible, own bool)
	// imageReady may ask the engine.
	imageReady(ctx context.Context, host, image string, now time.Time) imageState
	requestImage(host, image string, now time.Time)
}

// imageState is an on-demand image on one host: busy means a pull is in flight or already requested,
// failed holds the last pull error.
type imageState struct {
	ready, busy bool
	failed      string
}

func unwrapRuntime(runtime runnerRuntime) runnerRuntime {
	if serial, ok := runtime.(*hostSerialRuntime); ok {
		return serial.runnerRuntime
	}
	return runtime
}

// placeJob picks a host for a waiting job. image is "" when the job runs its host's image; wait is the
// image reason to show while no host is ready; err is a selection problem an administrator can fix.
func (s *scheduler) placeJob(ctx context.Context, j job, hosts []hostCapacity, pool string, now time.Time) (host *hostCapacity, image, wait string, err error) {
	selected, err := s.images.forJob(j.route, s.pools[pool])
	if err != nil {
		return nil, "", "", err
	}
	if selected.Image == "" {
		host = availableHost(hosts, pool)
		if waiting := freeHost(hosts, pool, false); host == nil && waiting != nil && waiting.ImageWait != "" {
			wait = imageWaitPrefix + waiting.Image + " on " + waiting.Name
		}
		return host, "", wait, nil
	}
	image = selected.Image
	targets, _ := unwrapRuntime(s.runtime).(runnerImageRuntime)
	var ready, pending *hostCapacity
	var readyOwn, pendingOwn, eligible bool
	var pendingState imageState
	for i := range hosts {
		candidate := &hosts[i]
		if pool != "" && candidate.Pool != pool {
			continue
		}
		canRun, own, state := true, false, imageState{ready: true}
		if targets != nil {
			canRun, own = targets.canRun(candidate.Name, image)
		}
		if !canRun {
			continue
		}
		eligible = true
		if candidate.Containers == nil || candidate.Err != nil || candidate.Cordoned || candidate.Occupied >= candidate.Limit {
			continue
		}
		switch {
		case own:
			state.ready = candidate.ImageWait == ""
		case targets != nil:
			state = targets.imageReady(ctx, candidate.Name, image, now)
		}
		if state.ready {
			if ready == nil || candidate.Occupied < ready.Occupied {
				ready, readyOwn = candidate, own
			}
			continue
		}
		// Keep asking the host that is already pulling, so one job does not pull on every host.
		if pending == nil || state.busy && !pendingState.busy || state.busy == pendingState.busy && candidate.Occupied < pending.Occupied {
			pending, pendingOwn, pendingState = candidate, own, state
		}
	}
	switch {
	case !eligible && pool != "":
		return nil, "", "", fmt.Errorf("no host in worker pool %q can start runner image %s; outbound agent:// workers start only their own image", pool, image)
	case !eligible:
		return nil, "", "", fmt.Errorf("no host can start runner image %s; outbound agent:// workers start only their own image", image)
	case ready != nil:
		if !readyOwn && targets != nil {
			// Keeps the image in the puller's pairs, so a removed image is seen and pulled again.
			targets.requestImage(ready.Name, image, now)
		}
		return ready, image, "", nil
	case pending != nil:
		if !pendingOwn && targets != nil {
			targets.requestImage(pending.Name, image, now)
		}
		wait = imageWaitPrefix + image + " on " + pending.Name
		if pendingState.failed != "" {
			wait += "; pull failed: " + pendingState.failed
		}
		return nil, image, wait, nil
	}
	return nil, image, "", nil
}

func (f *dockerFleet) canRun(name, image string) (eligible, own bool) {
	host, err := f.host(name)
	switch {
	case err != nil:
		return false, false
	case image == host.image:
		return true, true
	}
	// An outbound agent only accepts tasks for the image it was started with.
	return host.agentName() == "", false
}

func (f *dockerFleet) imageReady(ctx context.Context, name, image string, now time.Time) imageState {
	switch {
	case f.images == nil:
		return imageState{ready: true}
	case f.images.enabled():
		return f.images.status(name, image)
	}
	host, err := f.host(name)
	if err != nil {
		return imageState{}
	}
	inspect := f.inspect
	if inspect == nil {
		inspect = runImageCommand
	}
	return imageState{ready: f.images.inspected(ctx, host, image, now, inspect)}
}

func (f *dockerFleet) requestImage(name, image string, now time.Time) {
	if f.images.enabled() {
		f.images.want(name, image, now)
	}
}

// status reports whether the puller has seen image on host, and whether a pull is in flight or requested.
func (r *imageReadiness) status(host, image string) imageState {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := imageKey{host, image}
	var state imageState
	if e := r.entries[key]; e != nil {
		if e.seen && e.present {
			return imageState{ready: true}
		}
		state.busy, state.failed = e.pulling, e.err
	}
	_, requested := r.demand[key]
	state.busy = state.busy || requested
	return state
}

// want records that a waiting job needs image on host; the first request wakes the puller.
func (r *imageReadiness) want(host, image string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.demand == nil {
		r.demand = make(map[imageKey]time.Time)
	}
	key := imageKey{host, image}
	if _, known := r.demand[key]; !known {
		r.notify()
	}
	r.demand[key] = now
}

// demanded lists the on-demand images still wanted, forgetting those no job asked for in imageDemandTTL.
func (r *imageReadiness) demanded(now time.Time) []imageKey {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]imageKey, 0, len(r.demand))
	for key, at := range r.demand {
		if now.Sub(at) > imageDemandTTL {
			// Forget what the puller saw too: the next job that asks inspects the host again.
			delete(r.demand, key)
			if e := r.entries[key]; e != nil && !e.pulling {
				delete(r.entries, key)
			}
			continue
		}
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b imageKey) int { return strings.Compare(a.host+"\x00"+a.image, b.host+"\x00"+b.image) })
	return keys
}

// inspected checks image on host directly when Runquill does not pull, at most once per imageCheckTTL.
func (r *imageReadiness) inspected(ctx context.Context, host docker, image string, now time.Time, inspect imageCommand) bool {
	key := imageKey{host.label(), image}
	r.mu.Lock()
	if r.checked == nil {
		r.checked = make(map[imageKey]imageCheck)
	}
	check, known := r.checked[key]
	r.mu.Unlock()
	if known && now.Sub(check.at) < imageCheckTTL {
		return check.present
	}
	_, err := inspect(ctx, host, dockerHostProbeTimeout, "image", "inspect", image, "--format", "{{.Id}}")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checked[key] = imageCheck{at: now, present: err == nil}
	return err == nil
}

type imageCheck struct {
	at      time.Time
	present bool
}

// describeImages adds each job's OS selector and effective runner image.
func (r *statusReport) describeImages(images *runnerImages, jobs []job) {
	for i, j := range jobs {
		if i >= len(r.Jobs) {
			return
		}
		r.Jobs[i].RunnerOS, _ = routeOS(j.route)
		r.Jobs[i].RunnerImage = images.effective(j.route, r.pools[r.Jobs[i].Pool])
	}
}

// checkRunnerImageCatalogue lists the catalogue for runquill doctor and, with Docker, which entries the
// local engine already holds; a missing entry is only pulled when a job selects it.
func checkRunnerImageCatalogue(ctx context.Context, saved *operationState, defaultImage string, report func(string, error)) {
	var images *runnerImages
	var err error
	if saved != nil {
		images = saved.runners.images
	} else {
		images, err = loadRunnerImages(os.Getenv, cmp.Or(defaultImage, env("RUNQUILL_RUNNER_IMAGE", "runquill-runner:local")))
	}
	report("runner image catalogue ("+runnerImagesEnv+")", err)
	if err != nil {
		return
	}
	var local *docker
	if env("RUNQUILL_BACKEND", "docker") == "docker" {
		if fleet, err := parseDockerFleet("", "local"); err == nil {
			local = &fleet.hosts[0]
		}
	}
	reportRunnerImages(ctx, images, local, report)
}

func reportRunnerImages(ctx context.Context, images *runnerImages, local *docker, report func(string, error)) {
	for _, entry := range images.list() {
		line := fmt.Sprintf("runner image os__%s__ = %s (%s)", entry.Label, entry.Image, entry.Source)
		if local != nil && entry.Image != "" {
			_, err := local.command(ctx, nil, "image", "inspect", entry.Image, "--format", "{{.Id}}")
			switch {
			case err == nil:
				line += ": present on the local host"
			case imageMissing(err):
				line += ": not on the local host; pulled when a job selects it unless RUNQUILL_IMAGE_PULL=never"
			default:
				line += ": the local engine could not be asked"
			}
		}
		report(line, nil)
	}
}
