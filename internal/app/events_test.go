package app

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type eventSource struct {
	mu   sync.Mutex
	data string
}

func (s *eventSource) set(data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
}

func (s *eventSource) read() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []byte(s.data), nil
}

func receiveEvent(t *testing.T, s *panelSubscriber) panelEvent {
	t.Helper()
	select {
	case event := <-s.events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived")
		return panelEvent{}
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *panelEventHub) state() (subs, builds int, running bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs), h.builds, h.stop != nil
}

func TestPanelEventsFanOutOneBuildPerInterval(t *testing.T) {
	source := &eventSource{data: `{"n":1}`}
	hub := newPanelEventHub(source.read, 10*time.Millisecond)
	if _, _, running := hub.state(); running {
		t.Fatal("the broadcaster started before any subscriber")
	}
	subs := make([]*panelSubscriber, 3)
	for i := range subs {
		var err error
		if subs[i], err = hub.subscribe(int64(i + 1)); err != nil {
			t.Fatal(err)
		}
	}
	first := receiveEvent(t, subs[0])
	for _, s := range subs[1:] {
		if event := receiveEvent(t, s); event.id != first.id || string(event.data) != `{"n":1}` {
			t.Fatalf("subscribers saw different snapshots: %+v vs %+v", event, first)
		}
	}

	// Unchanged content is not re-sent.
	_, builds, _ := hub.state()
	waitFor(t, "more builds", func() bool { _, b, _ := hub.state(); return b >= builds+3 })
	for _, s := range subs {
		select {
		case event := <-s.events:
			t.Fatalf("unchanged snapshot re-sent: %+v", event)
		default:
		}
	}
	source.set(`{"n":2}`)
	for _, s := range subs {
		if event := receiveEvent(t, s); event.id <= first.id || string(event.data) != `{"n":2}` {
			t.Fatalf("changed snapshot: %+v", event)
		}
	}

	// A late subscriber gets the current snapshot without waiting a tick.
	late, err := hub.subscribe(9)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-late.events:
		if string(event.data) != `{"n":2}` {
			t.Fatalf("late subscriber saw %s", event.data)
		}
	default:
		t.Fatal("late subscriber was not primed")
	}
	for _, s := range append(subs, late) {
		hub.unsubscribe(s)
	}
	if n, _, running := hub.state(); n != 0 || running {
		t.Fatalf("after last unsubscribe: %d subscribers, running %v", n, running)
	}
	_, builds, _ = hub.state()
	time.Sleep(50 * time.Millisecond)
	if _, after, _ := hub.state(); after > builds+1 {
		t.Fatalf("the broadcaster kept building after the last subscriber left: %d -> %d", builds, after)
	}
}

func TestPanelEventsSlowSubscriberKeepsOnlyLatest(t *testing.T) {
	source := &eventSource{data: "0"}
	hub := newPanelEventHub(source.read, 5*time.Millisecond)
	slow, err := hub.subscribe(1)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := hub.subscribe(2)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.unsubscribe(slow)
	defer hub.unsubscribe(fast)
	for i := 1; i <= 5; i++ {
		want := strconv.Itoa(i)
		source.set(want)
		waitFor(t, "fast subscriber "+want, func() bool {
			select {
			case event := <-fast.events:
				return string(event.data) == want
			default:
				return false
			}
		})
	}
	if len(slow.events) != 1 {
		t.Fatalf("slow subscriber buffered %d events", len(slow.events))
	}
	if event := <-slow.events; string(event.data) != "5" {
		t.Fatalf("slow subscriber kept %s, want the latest", event.data)
	}
}

func TestPanelEventsSubscriberCaps(t *testing.T) {
	hub := newPanelEventHub((&eventSource{data: "x"}).read, time.Hour)
	hub.maxTotal, hub.maxPerUser = 3, 2
	a1, _ := hub.subscribe(1)
	if _, err := hub.subscribe(1); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.subscribe(1); err != errPanelEventsFull {
		t.Fatalf("third stream for one user: %v", err)
	}
	if _, err := hub.subscribe(2); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.subscribe(3); err != errPanelEventsFull {
		t.Fatalf("stream over the global cap: %v", err)
	}
	hub.unsubscribe(a1)
	hub.unsubscribe(a1) // repeated cleanup is harmless
	if _, err := hub.subscribe(1); err != nil {
		t.Fatalf("a freed slot was not reusable: %v", err)
	}
}

type eventStream struct {
	t      *testing.T
	lines  *bufio.Reader
	cancel context.CancelFunc
	resp   *http.Response
}

func (s *eventStream) next() string {
	s.t.Helper()
	var block []string
	for {
		line, err := s.lines.ReadString('\n')
		if err != nil {
			s.t.Fatalf("stream ended: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if len(block) > 0 && block[0] != "retry: 3000" {
				return strings.Join(block, "\n")
			}
			block = nil
			continue
		}
		block = append(block, line)
	}
}

type eventServer struct {
	q      *queue
	access *panelAccess
	hub    *panelEventHub
	server *httptest.Server
	source *eventSource
	cookie *http.Cookie
}

func newEventServer(t *testing.T, writeTimeout time.Duration) *eventServer {
	t.Helper()
	q, err := openQueue(filepath.Join(t.TempDir(), "queue.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.close)
	access := panelAccessForTest(t, q)
	key, err := access.startSession(t.Context(), panelIdentity{GitHubID: 101, Login: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	source := &eventSource{data: `{"n":1}`}
	hub := newPanelEventHub(source.read, 10*time.Millisecond)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/events", access.require("fleet:read", hub.serve))
	server := httptest.NewUnstartedServer(mux)
	server.Config.WriteTimeout = writeTimeout
	server.Start()
	t.Cleanup(server.Close)
	return &eventServer{q: q, access: access, hub: hub, server: server, source: source,
		cookie: &http.Cookie{Name: access.cookieName("session"), Value: key}}
}

func (e *eventServer) open(t *testing.T, header http.Header) *eventStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.server.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range header {
		req.Header[key] = values
	}
	req.AddCookie(e.cookie)
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: HTTP %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return &eventStream{t: t, lines: bufio.NewReader(resp.Body), cancel: cancel, resp: resp}
}

func TestPanelEventsStreamOverHTTP(t *testing.T) {
	e := newEventServer(t, 100*time.Millisecond)
	e.hub.heartbeat = 60 * time.Millisecond
	stream := e.open(t, nil)
	first := stream.next()
	if !strings.Contains(first, "event: snapshot\ndata: {\"n\":1}") || !strings.HasPrefix(first, "id: ") {
		t.Fatalf("initial event: %q", first)
	}
	id := strings.TrimPrefix(strings.SplitN(first, "\n", 2)[0], "id: ")
	// Heartbeats spaced past the server's write timeout prove the deadline is extended.
	for range 3 {
		if got := stream.next(); got != ": ping" {
			t.Fatalf("heartbeat: %q", got)
		}
	}
	e.source.set(`{"n":2}`)
	for {
		got := stream.next()
		if got == ": ping" {
			continue
		}
		if !strings.Contains(got, `data: {"n":2}`) || strings.Contains(got, "id: "+id+"\n") {
			t.Fatalf("change event: %q", got)
		}
		break
	}

	// Last-Event-ID for the current snapshot skips the repeat.
	current := strconv.FormatUint(func() uint64 { e.hub.mu.Lock(); defer e.hub.mu.Unlock(); return e.hub.last.id }(), 10)
	resumed := e.open(t, http.Header{"Last-Event-Id": {current}})
	if got := resumed.next(); got != ": ping" {
		t.Fatalf("resumed stream repeated the snapshot: %q", got)
	}

	stream.cancel()
	resumed.cancel()
	waitFor(t, "disconnect cleanup", func() bool { n, _, running := e.hub.state(); return n == 0 && !running })
}

func TestPanelEventsEndOnServerShutdown(t *testing.T) {
	e := newEventServer(t, 0)
	stream := e.open(t, nil)
	stream.next()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := e.server.Config.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown waited on the event stream: %v", err)
	}
	waitFor(t, "stream cleanup", func() bool { n, _, _ := e.hub.state(); return n == 0 })
}

func TestPanelEventsRouteAccess(t *testing.T) {
	f := newPanelFixture(t)
	mux := http.NewServeMux()
	registerEventRoutes(mux, f.access, newPanel(config{}))
	f.handler = mux
	f.expect(f.call(http.MethodGet, "/api/events", "", "", false), http.StatusUnauthorized)

	// Read tokens reach /snapshot, so they reach the stream too.
	key := panelSecret()
	digest := panelDigest(key)
	if _, err := f.q.Exec(`INSERT INTO panel_tokens(digest,user_id,name,scope,expires_at,created_at) VALUES (?,102,'read','fleet:read',?,0)`,
		digest[:], time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, f.access.cfg.origin+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer rqk_"+key)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); mux.ServeHTTP(rec, req) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: snapshot\ndata: {") {
		t.Fatalf("token stream: HTTP %d %q", rec.Code, rec.Body)
	}
}
