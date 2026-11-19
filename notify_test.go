package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type notifyTestSink struct {
	mu       sync.Mutex
	bodies   []map[string]any
	agents   []string
	requests atomic.Int64
}

func (s *notifyTestSink) record(r *http.Request) {
	s.requests.Add(1)
	var body map[string]any
	data, _ := io.ReadAll(r.Body)
	json.Unmarshal(data, &body)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.agents = append(s.agents, r.Header.Get("User-Agent"))
	s.mu.Unlock()
}

func (s *notifyTestSink) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.bodies {
		k, _ := b["kind"].(string)
		out = append(out, k)
	}
	return out
}

func notifyTestServer(t *testing.T, handler func(*notifyTestSink, http.ResponseWriter, *http.Request)) (*httptest.Server, *notifyTestSink) {
	t.Helper()
	sink := &notifyTestSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink.record(r)
		if handler != nil {
			handler(sink, w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, sink
}

func notifyTestNew(t *testing.T, cfg notifyConfig) *notifier {
	t.Helper()
	if cfg.Backoff == 0 {
		cfg.Backoff = time.Millisecond
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	n, err := newNotifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close(context.Background()) })
	return n
}

func notifyTestClose(t *testing.T, n *notifier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNotifyPayloadShape(t *testing.T) {
	srv, sink := notifyTestServer(t, nil)
	n := notifyTestNew(t, notifyConfig{URL: srv.URL + "/hook"})
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	n.Notify(notifyEvent{Kind: notifyJobFailed, Repository: "acme/app", Job: "build <x>", JobID: 42,
		Reason: "runner exited 1", Time: at})
	notifyTestClose(t, n)

	if len(sink.bodies) != 1 {
		t.Fatalf("got %d requests", len(sink.bodies))
	}
	b := sink.bodies[0]
	text, _ := b["text"].(string)
	if !strings.Contains(text, "acme/app") || !strings.Contains(text, "runner exited 1") || !strings.Contains(text, "&lt;x&gt;") {
		t.Fatalf("text = %q", text)
	}
	want := map[string]any{"source": "runquill", "kind": "job_failed", "repository": "acme/app",
		"job": "build <x>", "job_id": float64(42), "reason": "runner exited 1", "timestamp": "2026-11-19T12:00:00Z"}
	for k, v := range want {
		if !reflect.DeepEqual(b[k], v) {
			t.Errorf("%s = %#v, want %#v", k, b[k], v)
		}
	}
	if sink.agents[0] != "runquill" {
		t.Errorf("User-Agent = %q", sink.agents[0])
	}

	srv2, sink2 := notifyTestServer(t, nil)
	n2 := notifyTestNew(t, notifyConfig{URL: srv2.URL, Format: notifyFormatText})
	n2.Notify(notifyEvent{Kind: notifyHostOffline, Host: "w1"})
	notifyTestClose(t, n2)
	if len(sink2.bodies) != 1 || len(sink2.bodies[0]) != 1 || sink2.bodies[0]["text"] == nil {
		t.Fatalf("text format body = %#v", sink2.bodies)
	}
}

func TestNotifyCooldownAndRecovery(t *testing.T) {
	srv, sink := notifyTestServer(t, nil)
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	n := notifyTestNew(t, notifyConfig{URL: srv.URL, now: clock})

	offline := notifyEvent{Kind: notifyHostOffline, Host: "w1"}
	n.Notify(offline)
	n.Notify(offline)                                          // suppressed
	n.Notify(notifyEvent{Kind: notifyHostOffline, Host: "w2"}) // different key
	advance(10 * time.Minute)
	n.Notify(offline) // still within cooldown
	advance(21 * time.Minute)
	n.Notify(offline)                                            // cooldown elapsed: re-notify
	n.Notify(notifyEvent{Kind: notifyHostRecovered, Host: "w1"}) // sent
	n.Notify(notifyEvent{Kind: notifyHostRecovered, Host: "w1"}) // nothing to recover
	n.Notify(offline)                                            // recovery reset the cooldown
	n.Notify(notifyEvent{Kind: notifyHostRecovered, Host: "never-offline"})

	// Queue stalled, cleared, stalled again inside the cooldown.
	n.Notify(notifyEvent{Kind: notifyQueueStalled, Waiting: time.Hour, Threshold: time.Minute})
	n.Notify(notifyEvent{Kind: notifyQueueStalled, Waiting: time.Hour, Threshold: time.Minute})
	n.Notify(notifyEvent{Kind: notifyQueueRecovered})
	n.Notify(notifyEvent{Kind: notifyQueueStalled, Waiting: time.Hour, Threshold: time.Minute})
	notifyTestClose(t, n)

	got := sink.kinds()
	want := []string{"host_offline", "host_offline", "host_offline", "host_recovered", "host_offline",
		"queue_stalled", "queue_recovered", "queue_stalled"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}

func TestNotifyRetryThenSuccess(t *testing.T) {
	var calls atomic.Int64
	srv, sink := notifyTestServer(t, func(_ *notifyTestSink, w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	n := notifyTestNew(t, notifyConfig{URL: srv.URL, Attempts: 3})
	n.Notify(notifyEvent{Kind: notifyHostOffline, Host: "w1"})
	notifyTestClose(t, n)
	if sink.requests.Load() != 3 || n.Sent() != 1 || n.Failed() != 0 {
		t.Fatalf("requests=%d sent=%d failed=%d", sink.requests.Load(), n.Sent(), n.Failed())
	}

	// A 4xx is not retried, and a failed delivery clears the cooldown.
	srv2, sink2 := notifyTestServer(t, func(_ *notifyTestSink, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	n2 := notifyTestNew(t, notifyConfig{URL: srv2.URL, Attempts: 3})
	n2.Notify(notifyEvent{Kind: notifyHostOffline, Host: "w1"})
	deadline := time.Now().Add(5 * time.Second)
	for n2.Failed() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	n2.Notify(notifyEvent{Kind: notifyHostOffline, Host: "w1"})
	notifyTestClose(t, n2)
	if sink2.requests.Load() != 2 || n2.Failed() != 2 {
		t.Fatalf("requests=%d failed=%d", sink2.requests.Load(), n2.Failed())
	}
}

func TestNotifyDropOldestWhenFull(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	srv, sink := notifyTestServer(t, func(_ *notifyTestSink, _ http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
	})
	n := notifyTestNew(t, notifyConfig{URL: srv.URL, QueueSize: 1})
	n.Notify(notifyEvent{Kind: notifyHostOffline, Host: "a"})
	<-started // worker is busy with "a"
	for _, h := range []string{"b", "c", "d"} {
		n.Notify(notifyEvent{Kind: notifyHostOffline, Host: h})
	}
	if n.Dropped() != 2 {
		t.Fatalf("dropped = %d", n.Dropped())
	}
	// A dropped event's cooldown is cleared, so it can be queued again.
	n.Notify(notifyEvent{Kind: notifyHostOffline, Host: "b"})
	close(release)
	notifyTestClose(t, n)

	var hosts []string
	for _, b := range sink.bodies {
		hosts = append(hosts, b["host"].(string))
	}
	if !reflect.DeepEqual(hosts, []string{"a", "b"}) {
		t.Fatalf("delivered %v", hosts)
	}
	n.Notify(notifyEvent{Kind: notifyHostOffline, Host: "late"})
	if n.Dropped() != 4 {
		t.Fatalf("dropped after close = %d", n.Dropped())
	}
}

func TestNotifyCloseFlushes(t *testing.T) {
	srv, sink := notifyTestServer(t, func(_ *notifyTestSink, _ http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
	})
	n := notifyTestNew(t, notifyConfig{URL: srv.URL})
	for i := range 10 {
		n.Notify(notifyEvent{Kind: notifyJobFailed, Repository: "acme/app", JobID: int64(i)})
	}
	notifyTestClose(t, n)
	if sink.requests.Load() != 10 || n.Sent() != 10 {
		t.Fatalf("requests=%d sent=%d", sink.requests.Load(), n.Sent())
	}
	if err := n.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Close with an expiring context cancels in-flight work.
	block := make(chan struct{})
	defer close(block)
	slow, _ := notifyTestServer(t, func(_ *notifyTestSink, _ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	n2 := notifyTestNew(t, notifyConfig{URL: slow.URL, Timeout: time.Minute})
	n2.Notify(notifyEvent{Kind: notifyHostOffline, Host: "w1"})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := n2.Close(ctx); err != context.DeadlineExceeded {
		t.Fatalf("Close = %v", err)
	}
}

func TestNotifyNilSafe(t *testing.T) {
	var n *notifier
	n.Notify(notifyEvent{Kind: notifyHostOffline})
	if n.Close(context.Background()) != nil || n.Dropped() != 0 || n.Sent() != 0 || n.Failed() != 0 || n.StallAfter() != 0 {
		t.Fatal("nil notifier misbehaved")
	}
}

func TestNotifyURLNeverLeaks(t *testing.T) {
	const secret = "T000/B000/secret-token-xyz"
	var logs bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&notifyLockedWriter{w: &logs, mu: &logMu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	check := func(what, s string) {
		t.Helper()
		if strings.Contains(s, "secret-token-xyz") {
			t.Fatalf("%s leaked the URL: %s", what, s)
		}
	}

	for _, bad := range []string{"http://example.com/" + secret, "ftp://example.com/" + secret, "://" + secret, "https:///" + secret} {
		_, err := newNotifier(notifyConfig{URL: bad})
		if err == nil {
			t.Fatalf("accepted %q", bad)
		}
		check("constructor error", err.Error())
	}
	_, err := notifierFromEnv(func(k string) string {
		return map[string]string{"RUNQUILL_NOTIFY_URL": "http://hooks.example.com/" + secret}[k]
	})
	if err == nil {
		t.Fatal("env accepted plain http")
	}
	check("env error", err.Error())

	// Persistent 500.
	failing, _ := notifyTestServer(t, func(_ *notifyTestSink, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	// Connection refused.
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedURL := refused.URL
	refused.Close()
	// Redirect to another host (same IP, different port).
	other, otherSink := notifyTestServer(t, nil)
	redirect, _ := notifyTestServer(t, func(_ *notifyTestSink, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/"+secret, http.StatusTemporaryRedirect)
	})
	for _, base := range []string{failing.URL, refusedURL, redirect.URL} {
		n := notifyTestNew(t, notifyConfig{URL: base + "/" + secret + "?token=secret-token-xyz", Logger: logger, Attempts: 2})
		n.Notify(notifyEvent{Kind: notifyHostOffline, Host: "w1"})
		notifyTestClose(t, n)
		if n.Failed() != 1 {
			t.Fatalf("%s: failed = %d", base, n.Failed())
		}
		if _, err := n.post([]byte(`{}`)); err == nil {
			t.Fatal("post after close succeeded")
		} else {
			check("post error", err.Error())
		}
	}
	if otherSink.requests.Load() != 0 {
		t.Fatal("followed a cross-host redirect")
	}
	logMu.Lock()
	out := logs.String()
	logMu.Unlock()
	check("logs", out)
	if !strings.Contains(out, "HTTP 500") || !strings.Contains(out, "redirect") || !strings.Contains(out, "refused") {
		t.Fatalf("logs missing expected failures:\n%s", out)
	}
}

type notifyLockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *notifyLockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestNotifyRedirectSameHostFollowed(t *testing.T) {
	var srv *httptest.Server
	sink := &notifyTestSink{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, srv.URL+"/new", http.StatusPermanentRedirect)
			return
		}
		sink.record(r)
	}))
	defer srv.Close()
	n := notifyTestNew(t, notifyConfig{URL: srv.URL + "/old"})
	n.Notify(notifyEvent{Kind: notifyHostOffline, Host: "w1"})
	notifyTestClose(t, n)
	if n.Sent() != 1 || sink.requests.Load() != 1 {
		t.Fatalf("sent=%d requests=%d", n.Sent(), sink.requests.Load())
	}
}

func TestNotifierFromEnv(t *testing.T) {
	envOf := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	n, err := notifierFromEnv(envOf(nil))
	if n != nil || err != nil {
		t.Fatalf("unset: %v %v", n, err)
	}
	n, err = notifierFromEnv(envOf(map[string]string{"RUNQUILL_NOTIFY_URL": "https://hooks.example.com/x",
		"RUNQUILL_NOTIFY_COOLDOWN": "5m", "RUNQUILL_NOTIFY_STALL_AFTER": "20m", "RUNQUILL_NOTIFY_FORMAT": "text"}))
	if err != nil || n == nil {
		t.Fatalf("set: %v", err)
	}
	defer n.Close(context.Background())
	if n.cfg.Cooldown != 5*time.Minute || n.StallAfter() != 20*time.Minute || n.cfg.Format != notifyFormatText {
		t.Fatalf("cfg = %+v", n.cfg)
	}
	for _, bad := range []map[string]string{
		{"RUNQUILL_NOTIFY_URL": "https://h.example.com/x", "RUNQUILL_NOTIFY_COOLDOWN": "soon"},
		{"RUNQUILL_NOTIFY_URL": "https://h.example.com/x", "RUNQUILL_NOTIFY_FORMAT": "xml"},
	} {
		if _, err := notifierFromEnv(envOf(bad)); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestDeriveNotifyEvents(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	kinds := func(evs []notifyEvent) []string {
		var out []string
		for _, e := range evs {
			out = append(out, string(e.Kind)+":"+e.Host)
		}
		return out
	}
	online := map[string]bool{"b": false, "a": true, "c": false}
	evs, st := deriveNotifyEvents(notifyFleetState{}, notifyFleetInput{Now: now,
		OldestQueuedAge: 15 * time.Minute, StallAfter: 10 * time.Minute, HostsOnline: online})
	if got, want := kinds(evs), []string{"queue_stalled:", "host_offline:b", "host_offline:c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tick 1 = %v", got)
	}
	if evs[0].Waiting != 15*time.Minute || evs[0].Threshold != 10*time.Minute || !evs[0].Time.Equal(now) {
		t.Fatalf("stall event = %+v", evs[0])
	}
	online["a"] = false // caller mutation must not affect returned state
	if st.Offline["a"] || !st.Stalled {
		t.Fatalf("state = %+v", st)
	}

	// Persistent conditions repeat; the notifier's cooldown dedupes them.
	evs, st2 := deriveNotifyEvents(st, notifyFleetInput{Now: now, OldestQueuedAge: 20 * time.Minute,
		StallAfter: 10 * time.Minute, HostsOnline: map[string]bool{"b": false, "c": false}})
	if got := kinds(evs); !reflect.DeepEqual(got, []string{"queue_stalled:", "host_offline:b", "host_offline:c"}) {
		t.Fatalf("tick 2 = %v", got)
	}

	// Recovery is emitted once on the transition; a removed host is forgotten.
	evs, st3 := deriveNotifyEvents(st2, notifyFleetInput{Now: now, StallAfter: 10 * time.Minute,
		HostsOnline: map[string]bool{"b": true}})
	if got := kinds(evs); !reflect.DeepEqual(got, []string{"queue_recovered:", "host_recovered:b"}) {
		t.Fatalf("tick 3 = %v", got)
	}
	if !st2.Offline["b"] {
		t.Fatal("prev state mutated")
	}
	evs, _ = deriveNotifyEvents(st3, notifyFleetInput{Now: now, StallAfter: 10 * time.Minute,
		HostsOnline: map[string]bool{"b": true, "c": true}})
	if len(evs) != 0 {
		t.Fatalf("tick 4 = %v", kinds(evs))
	}
}
