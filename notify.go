package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Outbound webhook notifications for operator-relevant fleet events.
//
// The webhook URL is treated as a secret (Slack and similar services embed a
// token in its path): it is never logged and never included in returned errors.

const (
	notifyFormatSlack = "slack" // {"text": ...} plus structured fields (default)
	notifyFormatText  = "text"  // {"text": ...} only, for strict receivers

	notifyDefaultCooldown   = 30 * time.Minute
	notifyDefaultStallAfter = 10 * time.Minute
	notifyDefaultTimeout    = 10 * time.Second
	notifyDefaultAttempts   = 3
	notifyDefaultBackoff    = time.Second
	notifyDefaultQueueSize  = 64
	notifyMaxTracked        = 4096
	notifyUserAgent         = "runquill"
)

type notifyKind string

const (
	notifyQueueStalled   notifyKind = "queue_stalled"
	notifyQueueRecovered notifyKind = "queue_recovered"
	notifyHostOffline    notifyKind = "host_offline"
	notifyHostRecovered  notifyKind = "host_recovered"
	notifyJobFailed      notifyKind = "job_failed"
)

// notifyEvent is one notification. Only the fields relevant to Kind need to be set.
type notifyEvent struct {
	Kind       notifyKind
	Host       string        // host_offline, host_recovered
	Repository string        // job_failed
	Job        string        // job_failed: job name
	JobID      int64         // job_failed: GitHub job id
	Reason     string        // job_failed: short reason
	Waiting    time.Duration // queue_stalled: age of the oldest queued job
	Threshold  time.Duration // queue_stalled: configured stall threshold
	Time       time.Time     // defaults to now when zero
}

// key identifies the condition an event reports, for dedupe and cooldown.
// Recovery events share the key of the problem they resolve.
func (e notifyEvent) key() string {
	switch e.Kind {
	case notifyQueueStalled, notifyQueueRecovered:
		return "queue"
	case notifyHostOffline, notifyHostRecovered:
		return "host/" + e.Host
	case notifyJobFailed:
		return "job/" + e.Repository + "/" + strconv.FormatInt(e.JobID, 10) + "/" + e.Job
	}
	return string(e.Kind)
}

func (e notifyEvent) recovery() bool {
	return e.Kind == notifyQueueRecovered || e.Kind == notifyHostRecovered
}

func (e notifyEvent) text() string {
	var s string
	switch e.Kind {
	case notifyQueueStalled:
		s = fmt.Sprintf("Runquill: queue stalled, oldest queued job has waited %s (threshold %s)",
			e.Waiting.Round(time.Second), e.Threshold.Round(time.Second))
	case notifyQueueRecovered:
		s = "Runquill: queue recovered, queued jobs are starting again"
	case notifyHostOffline:
		s = fmt.Sprintf("Runquill: Docker host %q is offline", e.Host)
	case notifyHostRecovered:
		s = fmt.Sprintf("Runquill: Docker host %q is back online", e.Host)
	case notifyJobFailed:
		s = fmt.Sprintf("Runquill: job %q (%d) in %s failed", e.Job, e.JobID, e.Repository)
		if e.Reason != "" {
			s += ": " + e.Reason
		}
	default:
		s = "Runquill: " + string(e.Kind)
	}
	// Slack mrkdwn control characters.
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

type notifyPayload struct {
	Text       string `json:"text"`
	Source     string `json:"source,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Host       string `json:"host,omitempty"`
	Repository string `json:"repository,omitempty"`
	Job        string `json:"job,omitempty"`
	JobID      int64  `json:"job_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
	WaitingSec int64  `json:"waiting_seconds,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
}

func (e notifyEvent) payload(format string) notifyPayload {
	if format == notifyFormatText {
		return notifyPayload{Text: e.text()}
	}
	return notifyPayload{
		Text: e.text(), Source: "runquill", Kind: string(e.Kind), Host: e.Host,
		Repository: e.Repository, Job: e.Job, JobID: e.JobID, Reason: e.Reason,
		WaitingSec: int64(e.Waiting / time.Second), Timestamp: e.Time.UTC().Format(time.RFC3339),
	}
}

// notifyConfig configures a notifier. Zero values select defaults.
type notifyConfig struct {
	URL        string        // required; https, or http to a loopback host
	Format     string        // notifyFormatSlack (default) or notifyFormatText
	Cooldown   time.Duration // minimum gap between repeats of one condition
	StallAfter time.Duration // queue stall threshold, for the caller's deriveNotifyEvents input
	Timeout    time.Duration // per request
	Attempts   int           // total attempts per event
	Backoff    time.Duration // first retry delay, doubled per attempt
	QueueSize  int
	Client     *http.Client // optional; copied and given a redirect policy
	Logger     *slog.Logger // optional; defaults to slog.Default()

	now func() time.Time // test clock
}

// notifierFromEnv builds a notifier from RUNQUILL_NOTIFY_* variables.
// It returns (nil, nil) when RUNQUILL_NOTIFY_URL is unset; a nil *notifier is
// safe to use and does nothing.
func notifierFromEnv(getenv func(string) string) (*notifier, error) {
	cfg, ok, err := notifyConfigFromEnv(getenv)
	if err != nil || !ok {
		return nil, err
	}
	return newNotifier(cfg)
}

func notifyConfigFromEnv(getenv func(string) string) (notifyConfig, bool, error) {
	cfg := notifyConfig{URL: strings.TrimSpace(getenv("RUNQUILL_NOTIFY_URL")),
		Format: strings.TrimSpace(getenv("RUNQUILL_NOTIFY_FORMAT"))}
	if cfg.URL == "" {
		return notifyConfig{}, false, nil
	}
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{{"RUNQUILL_NOTIFY_COOLDOWN", &cfg.Cooldown}, {"RUNQUILL_NOTIFY_STALL_AFTER", &cfg.StallAfter}} {
		raw := strings.TrimSpace(getenv(d.name))
		if raw == "" {
			continue
		}
		v, err := time.ParseDuration(raw)
		if err != nil || v <= 0 {
			return notifyConfig{}, false, fmt.Errorf("%s must be a positive duration such as 30m", d.name)
		}
		*d.dst = v
	}
	return cfg, true, nil
}

var errNotifyRedirect = errors.New("webhook redirect to a different host refused")

type notifyItem struct {
	ev    notifyEvent
	stamp time.Time // cooldown stamp set at enqueue; zero for recovery events
}

type notifier struct {
	url      string
	cfg      notifyConfig
	client   *http.Client
	log      *slog.Logger
	queue    chan notifyItem
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	dropped  atomic.Uint64
	failed   atomic.Uint64
	sent     atomic.Uint64
	mu       sync.Mutex
	closed   bool
	lastSent map[string]time.Time
}

func newNotifier(cfg notifyConfig) (*notifier, error) {
	if err := notifyValidateURL(cfg.URL); err != nil {
		return nil, err
	}
	switch cfg.Format {
	case "":
		cfg.Format = notifyFormatSlack
	case notifyFormatSlack, notifyFormatText:
	default:
		return nil, fmt.Errorf("notify format must be %q or %q", notifyFormatSlack, notifyFormatText)
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = notifyDefaultCooldown
	}
	if cfg.StallAfter <= 0 {
		cfg.StallAfter = notifyDefaultStallAfter
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = notifyDefaultTimeout
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = notifyDefaultAttempts
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = notifyDefaultBackoff
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = notifyDefaultQueueSize
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := &http.Client{}
	if cfg.Client != nil {
		*client = *cfg.Client
	}
	client.CheckRedirect = notifyCheckRedirect
	ctx, cancel := context.WithCancel(context.Background())
	n := &notifier{url: cfg.URL, cfg: cfg, client: client, log: logger,
		queue: make(chan notifyItem, cfg.QueueSize), ctx: ctx, cancel: cancel,
		done: make(chan struct{}), lastSent: make(map[string]time.Time)}
	n.cfg.URL = ""
	n.cfg.Client = nil
	go n.run()
	return n, nil
}

// notifyValidateURL never mentions the URL in its errors.
func notifyValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("notify URL is not a valid absolute URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && notifyIsLoopback(u.Hostname()):
	default:
		return errors.New("notify URL must use https (http is allowed only for loopback hosts)")
	}
	return nil
}

func notifyIsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func notifyCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errNotifyRedirect
	}
	first := via[0].URL
	if !strings.EqualFold(req.URL.Host, first.Host) || (first.Scheme == "https" && req.URL.Scheme != "https") {
		return errNotifyRedirect
	}
	return nil
}

// StallAfter reports the configured queue stall threshold (0 for a nil notifier).
func (n *notifier) StallAfter() time.Duration {
	if n == nil {
		return 0
	}
	return n.cfg.StallAfter
}

// Notify enqueues ev without blocking. Repeats of a condition inside the
// cooldown are suppressed; a recovery event clears the cooldown so the next
// occurrence notifies immediately, and is itself sent only if the problem was.
// When the queue is full the oldest queued event is dropped.
func (n *notifier) Notify(ev notifyEvent) {
	if n == nil {
		return
	}
	now := n.cfg.now()
	if ev.Time.IsZero() {
		ev.Time = now
	}
	key := ev.key()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		n.dropped.Add(1)
		return
	}
	item := notifyItem{ev: ev}
	if ev.recovery() {
		if _, notified := n.lastSent[key]; !notified {
			return
		}
		delete(n.lastSent, key)
	} else {
		if last, ok := n.lastSent[key]; ok && now.Sub(last) < n.cfg.Cooldown {
			return
		}
		if len(n.lastSent) >= notifyMaxTracked {
			n.pruneLocked(now)
		}
		n.lastSent[key] = now
		item.stamp = now
	}
	for {
		select {
		case n.queue <- item:
			return
		default:
		}
		select {
		case old := <-n.queue:
			n.dropped.Add(1)
			n.forgetLocked(old)
		default:
		}
	}
}

func (n *notifier) pruneLocked(now time.Time) {
	for k, t := range n.lastSent {
		if now.Sub(t) >= n.cfg.Cooldown {
			delete(n.lastSent, k)
		}
	}
}

// forgetLocked clears the cooldown stamp of an undelivered event so the
// condition is reported again on its next occurrence.
func (n *notifier) forgetLocked(item notifyItem) {
	if item.stamp.IsZero() {
		return
	}
	if k := item.ev.key(); n.lastSent[k].Equal(item.stamp) {
		delete(n.lastSent, k)
	}
}

// Dropped counts events discarded because the queue was full or closed.
func (n *notifier) Dropped() uint64 {
	if n == nil {
		return 0
	}
	return n.dropped.Load()
}

// Failed counts events not delivered after all attempts.
func (n *notifier) Failed() uint64 {
	if n == nil {
		return 0
	}
	return n.failed.Load()
}

// Sent counts delivered events.
func (n *notifier) Sent() uint64 {
	if n == nil {
		return 0
	}
	return n.sent.Load()
}

// Close stops accepting events and waits for queued ones to be delivered.
// If ctx ends first, in-flight delivery is cancelled and ctx.Err() returned.
func (n *notifier) Close(ctx context.Context) error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	if !n.closed {
		n.closed = true
		close(n.queue)
	}
	n.mu.Unlock()
	select {
	case <-n.done:
		return nil
	case <-ctx.Done():
		n.cancel()
		<-n.done
		return ctx.Err()
	}
}

func (n *notifier) run() {
	defer close(n.done)
	defer n.cancel()
	for item := range n.queue {
		if err := n.deliver(item.ev); err != nil {
			n.mu.Lock()
			n.forgetLocked(item)
			n.mu.Unlock()
			n.failed.Add(1)
			n.log.Warn("notification not delivered", "kind", string(item.ev.Kind), "error", err)
			continue
		}
		n.sent.Add(1)
	}
}

func (n *notifier) deliver(ev notifyEvent) error {
	body, err := json.Marshal(ev.payload(n.cfg.Format))
	if err != nil {
		return err
	}
	delay := n.cfg.Backoff
	var lastErr error
	for attempt := 1; attempt <= n.cfg.Attempts; attempt++ {
		if attempt > 1 {
			t := time.NewTimer(delay)
			select {
			case <-n.ctx.Done():
				t.Stop()
				return fmt.Errorf("%w (after %v)", n.ctx.Err(), lastErr)
			case <-t.C:
			}
			delay *= 2
		}
		retry, err := n.post(body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			return err
		}
		n.log.Debug("notification attempt failed", "kind", string(ev.Kind), "attempt", attempt, "error", err)
	}
	return lastErr
}

// post sends one request. Errors never contain the webhook URL.
func (n *notifier) post(body []byte) (retry bool, err error) {
	ctx, cancel := context.WithTimeout(n.ctx, n.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return false, errors.New("notify webhook: cannot build request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", notifyUserAgent)
	res, err := n.client.Do(req)
	if err != nil {
		if errors.Is(err, errNotifyRedirect) {
			return false, errors.New("notify webhook: " + errNotifyRedirect.Error())
		}
		return n.ctx.Err() == nil, n.redact(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return false, nil
	}
	err = fmt.Errorf("notify webhook: HTTP %d", res.StatusCode)
	return res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500, err
}

// redact drops the *url.Error wrapper (which embeds the URL) and scrubs any
// remaining occurrence of the URL or its path.
func (n *notifier) redact(err error) error {
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		msg = ue.Err.Error()
		if ue.Timeout() {
			msg = "request timed out"
		}
	}
	msg = strings.ReplaceAll(msg, n.url, "[redacted]")
	if u, perr := url.Parse(n.url); perr == nil {
		if u.RawQuery != "" {
			msg = strings.ReplaceAll(msg, u.RawQuery, "[redacted]")
		}
		if len(u.Path) > 1 {
			msg = strings.ReplaceAll(msg, u.Path, "[redacted]")
		}
	}
	return errors.New("notify webhook: " + msg)
}

// notifyFleetInput is one observation of the fleet for deriveNotifyEvents.
type notifyFleetInput struct {
	Now             time.Time
	OldestQueuedAge time.Duration   // 0 when nothing is queued
	StallAfter      time.Duration   // stall threshold; <= 0 disables stall events
	HostsOnline     map[string]bool // per Docker host or worker; absent hosts are forgotten
}

// notifyFleetState is carried between deriveNotifyEvents calls; start with the zero value.
type notifyFleetState struct {
	Stalled bool
	Offline map[string]bool
}

// deriveNotifyEvents is pure: it returns the events for this observation and
// the next state, without modifying prev or in. Problem events (queue_stalled,
// host_offline) are emitted on every call while the condition persists, so
// the notifier's cooldown decides when to re-notify; recovery events are
// emitted once, on the transition back to healthy. Output is ordered: queue
// first, then hosts sorted by name.
func deriveNotifyEvents(prev notifyFleetState, in notifyFleetInput) ([]notifyEvent, notifyFleetState) {
	var events []notifyEvent
	next := notifyFleetState{Offline: make(map[string]bool)}
	if in.StallAfter > 0 && in.OldestQueuedAge > in.StallAfter {
		next.Stalled = true
		events = append(events, notifyEvent{Kind: notifyQueueStalled, Waiting: in.OldestQueuedAge,
			Threshold: in.StallAfter, Time: in.Now})
	} else if prev.Stalled {
		events = append(events, notifyEvent{Kind: notifyQueueRecovered, Time: in.Now})
	}
	hosts := make([]string, 0, len(in.HostsOnline))
	for host := range in.HostsOnline {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		switch {
		case !in.HostsOnline[host]:
			next.Offline[host] = true
			events = append(events, notifyEvent{Kind: notifyHostOffline, Host: host, Time: in.Now})
		case prev.Offline[host]:
			events = append(events, notifyEvent{Kind: notifyHostRecovered, Host: host, Time: in.Now})
		}
	}
	return events, next
}

// fleetAlerts turns scheduler observations into notifications. The scheduler
// holds it by pointer because every tick runs on a copy of the scheduler.
type fleetAlerts struct {
	notifier *notifier
	state    notifyFleetState
}

func (a *fleetAlerts) observe(ctx context.Context, q *queue, hosts []hostCapacity, now time.Time, capped bool) error {
	if a == nil || a.notifier == nil {
		return nil
	}
	age, err := q.oldestQueuedAge(ctx, now, capped)
	if err != nil {
		return err
	}
	online := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		online[host.Name] = host.Containers != nil
	}
	events, next := deriveNotifyEvents(a.state, notifyFleetInput{Now: now, OldestQueuedAge: age,
		StallAfter: a.notifier.StallAfter(), HostsOnline: online})
	a.state = next
	for _, ev := range events {
		a.notifier.Notify(ev)
	}
	return nil
}

func (a *fleetAlerts) jobFailed(j job, now time.Time, reason string) {
	if a == nil {
		return
	}
	a.notifier.Notify(notifyEvent{Kind: notifyJobFailed, Repository: j.repository, Job: j.route,
		JobID: j.id, Reason: reason, Time: now})
}
