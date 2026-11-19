package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

type imagePullPolicy string

const (
	imagePullMissing imagePullPolicy = "missing"
	imagePullAlways  imagePullPolicy = "always"
	imagePullNever   imagePullPolicy = "never"

	imagePullTimeout     = 15 * time.Minute
	imageInspectTimeout  = 30 * time.Second
	imagePullCycle       = 30 * time.Second
	imagePullBackoff     = 30 * time.Second
	imagePullMaxBackoff  = 10 * time.Minute
	imagePullConcurrency = 2
	imagePullStatusReady = "ready"
)

// loadImagePull reads RUNQUILL_IMAGE_PULL and RUNQUILL_IMAGE_PULL_INTERVAL into an empty readiness store.
func loadImagePull(getenv func(string) string) (*imageReadiness, error) {
	r := &imageReadiness{policy: imagePullMissing, interval: 6 * time.Hour,
		entries: make(map[imageKey]*imageEntry), hosts: make(map[string]bool), wake: make(chan struct{}, 1)}
	switch policy := imagePullPolicy(strings.TrimSpace(getenv("RUNQUILL_IMAGE_PULL"))); policy {
	case "":
	case imagePullMissing, imagePullAlways, imagePullNever:
		r.policy = policy
	default:
		return nil, errors.New("RUNQUILL_IMAGE_PULL must be missing, always or never")
	}
	if raw := strings.TrimSpace(getenv("RUNQUILL_IMAGE_PULL_INTERVAL")); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval < time.Minute || interval > 7*24*time.Hour {
			return nil, errors.New("RUNQUILL_IMAGE_PULL_INTERVAL must be a duration between 1m and 168h")
		}
		r.interval = interval
	}
	return r, nil
}

type imageKey struct{ host, image string }

type imageEntry struct {
	present   bool
	seen      bool
	pulling   bool
	failures  int
	next      time.Time // earliest pull after a failure
	refreshed time.Time // last finished pull of a present image, for "always"
	err       string
}

// imageReadiness is the in-memory runner image state the puller feeds and the scheduler reads.
// A nil store, or the "never" policy, leaves images to the operator.
type imageReadiness struct {
	policy   imagePullPolicy
	interval time.Duration
	mu       sync.Mutex
	entries  map[imageKey]*imageEntry
	hosts    map[string]bool // hosts with a pull in flight
	active   int
	wake     chan struct{}
	pulls    sync.WaitGroup
	// demand holds catalogue images waiting jobs asked for, pulled on demand (runner_images.go).
	demand  map[imageKey]time.Time
	checked map[imageKey]imageCheck
}

func (r *imageReadiness) enabled() bool {
	return r != nil && r.policy != imagePullNever
}

func (r *imageReadiness) entry(host, image string) *imageEntry {
	key := imageKey{host, image}
	e := r.entries[key]
	if e == nil {
		e = &imageEntry{}
		r.entries[key] = e
	}
	return e
}

func (r *imageReadiness) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// observe records an inspect result and returns the host's image status with a reason when it is not ready.
func (r *imageReadiness) observe(host, image string, present bool) (status, wait string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entry(host, image)
	if !present && (e.present || !e.seen) {
		// Wake the puller when an image first goes missing, not on every scheduler tick.
		r.notify()
	}
	e.present, e.seen = present, true
	if present {
		e.failures, e.next, e.err = 0, time.Time{}, ""
		return imagePullStatusReady, ""
	}
	switch {
	case e.pulling:
		return "pulling", fmt.Sprintf("pulling runner image %s", image)
	case e.err != "":
		return "failed", fmt.Sprintf("runner image %s is missing; pull failed: %s", image, e.err)
	}
	return "missing", fmt.Sprintf("runner image %s is missing; waiting to pull it", image)
}

// claim records a puller inspect and reports whether a pull should start now.
func (r *imageReadiness) claim(host, image string, present bool, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entry(host, image)
	e.present, e.seen = present, true
	if present {
		e.failures, e.next, e.err = 0, time.Time{}, ""
	}
	due := !present && !now.Before(e.next) ||
		present && r.policy == imagePullAlways && !now.Before(e.refreshed.Add(r.interval))
	if e.pulling || !due || r.hosts[host] || r.active >= imagePullConcurrency {
		return false
	}
	e.pulling, r.hosts[host] = true, true
	r.active++
	r.pulls.Add(1)
	return true
}

func (r *imageReadiness) finish(host, image string, present bool, err error, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entry(host, image)
	e.pulling = false
	delete(r.hosts, host)
	r.active--
	switch {
	case err == nil:
		e.present, e.failures, e.next, e.err, e.refreshed = true, 0, time.Time{}, "", now
	case present:
		// Keep using the image already on the host and try again at the next refresh.
		e.refreshed = now
	default:
		e.failures++
		e.next = now.Add(min(imagePullBackoff<<min(e.failures-1, 10), imagePullMaxBackoff))
		e.err = pullErrorText(err)
	}
}

func (r *imageReadiness) busy(host, image string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[imageKey{host, image}]
	return e != nil && e.pulling
}

var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]*@`)

// pullErrorText keeps engine output short and drops any credentials embedded in a registry URL.
func pullErrorText(err error) string {
	text := strings.Join(strings.Fields(urlUserinfo.ReplaceAllString(err.Error(), "$1")), " ")
	if runes := []rune(text); len(runes) > 200 {
		text = string(runes[:200]) + "…"
	}
	return text
}

var imageUserinfo = regexp.MustCompile(`^[^/@]*@([^/]+/)`)

// logImage drops credentials typed into a reference as user:password@registry/name; a tag@digest is kept.
func logImage(image string) string {
	return imageUserinfo.ReplaceAllString(image, "$1")
}

// imageMissing reports an engine's "no such image" answer, as opposed to an unreachable engine.
func imageMissing(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such image") || strings.Contains(text, "image not known")
}

// imagePair is one runner image a local or ssh:// host must hold.
type imagePair struct {
	target docker
	image  string
}

func (f *dockerFleet) imagePairs() []imagePair {
	var pairs []imagePair
	seen := make(map[imageKey]bool)
	for _, host := range f.hosts {
		key := imageKey{host.label(), host.image}
		// Outbound agents keep their own preloaded image; the manager cannot pull there.
		if host.agentName() != "" || host.image == "" || seen[key] {
			continue
		}
		seen[key] = true
		pairs = append(pairs, imagePair{target: host, image: host.image})
	}
	// Catalogue images are pulled only on the hosts where a waiting job asked for them.
	for _, key := range f.images.demanded(time.Now()) {
		host, err := f.host(key.host)
		if err != nil || host.agentName() != "" || seen[key] {
			continue
		}
		seen[key] = true
		pairs = append(pairs, imagePair{target: host, image: key.image})
	}
	return pairs
}

// imagePairsFrom reads the live topology each cycle, without holding its lock across engine calls.
func imagePairsFrom(ops *operations) func() []imagePair {
	return func() []imagePair {
		ops.topology.RLock()
		defer ops.topology.RUnlock()
		fleet, ok := ops.current.Load().runners.runtime.(*dockerFleet)
		if !ok {
			return nil
		}
		return fleet.imagePairs()
	}
}

// imageCommand runs one engine command against a host with its own deadline.
type imageCommand func(ctx context.Context, target docker, timeout time.Duration, args ...string) (string, error)

func runImageCommand(ctx context.Context, target docker, timeout time.Duration, args ...string) (string, error) {
	engine, err := target.containerEngine()
	if err != nil {
		return "", err
	}
	if target.host != "" {
		args = append(engine.remoteArgs(target.host), args...)
	}
	return engineCommandTimeout(ctx, timeout, engine.name, nil, args...)
}

type imagePullerDeps struct {
	Images *imageReadiness
	Pairs  func() []imagePair
	Run    imageCommand
	Now    func() time.Time
	Cycle  time.Duration
}

// runImagePuller pulls runner images on local and ssh:// hosts in the background until ctx ends.
func runImagePuller(ctx context.Context, deps imagePullerDeps) {
	if !deps.Images.enabled() {
		return
	}
	if deps.Run == nil {
		deps.Run = runImageCommand
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Cycle == 0 {
		deps.Cycle = imagePullCycle
	}
	slog.Info("runner image pulls enabled", "policy", deps.Images.policy, "refresh_interval", deps.Images.interval)
	ticker := time.NewTicker(deps.Cycle)
	defer ticker.Stop()
	for {
		deps.cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-deps.Images.wake:
		}
	}
}

func (deps imagePullerDeps) cycle(ctx context.Context) {
	defer func() {
		if err := recover(); err != nil {
			slog.Error("runner image pull cycle failed", "panic", err)
		}
	}()
	for _, pair := range deps.Pairs() {
		host := pair.target.label()
		if ctx.Err() != nil {
			return
		}
		if deps.Images.busy(host, pair.image) {
			continue
		}
		// Inspect every cycle, even during backoff, so an image built by hand is ready at once.
		_, err := deps.Run(ctx, pair.target, imageInspectTimeout, "image", "inspect", pair.image, "--format", "{{.Id}}")
		if err != nil && !imageMissing(err) {
			if ctx.Err() == nil {
				slog.Debug("inspect runner image", "host", host, "image", logImage(pair.image), "error", pullErrorText(err))
			}
			continue
		}
		present := err == nil
		if !deps.Images.claim(host, pair.image, present, deps.Now()) {
			continue
		}
		go deps.pull(ctx, pair, present)
	}
}

func (deps imagePullerDeps) pull(ctx context.Context, pair imagePair, present bool) {
	defer deps.Images.pulls.Done()
	host := pair.target.label()
	started := deps.Now()
	slog.Info("pulling runner image", "host", host, "image", logImage(pair.image))
	_, err := deps.Run(ctx, pair.target, imagePullTimeout, "pull", pair.image)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	deps.Images.finish(host, pair.image, present, err, deps.Now())
	deps.Images.notify()
	elapsed := deps.Now().Sub(started).Round(time.Second)
	switch {
	case err == nil:
		noteImagePulled(ctx, deps.Run, pair.target, pair.image)
		slog.Info("pulled runner image", "host", host, "image", logImage(pair.image), "duration", elapsed)
	case present:
		slog.Warn("refresh runner image; keeping the present image", "host", host, "image", logImage(pair.image), "duration", elapsed, "error", pullErrorText(err))
	default:
		slog.Warn("pull runner image", "host", host, "image", logImage(pair.image), "duration", elapsed, "error", pullErrorText(err))
	}
}
