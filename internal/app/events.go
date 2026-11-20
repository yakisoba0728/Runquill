package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	panelEventInterval  = 2 * time.Second
	panelEventHeartbeat = 15 * time.Second
	// Streams end periodically so EventSource reconnects and re-authenticates;
	// a revoked session or demoted user stops receiving fleet data.
	panelEventLifetime = 10 * time.Minute
	// Each write extends the server's write deadline past the next heartbeat.
	panelEventWriteWindow    = panelEventHeartbeat + 15*time.Second
	panelEventMaxSubscribers = 64
	panelEventMaxPerUser     = 4
)

var errPanelEventsFull = errors.New("Too many live connections. Close another panel tab and try again.")

type panelEvent struct {
	id   uint64
	data []byte
}

// panelSubscriber holds at most one pending event; a newer one replaces it so
// a slow client only ever misses intermediate snapshots.
type panelSubscriber struct {
	events chan panelEvent
}

// offer is called only with the hub lock held, so after draining the slot no
// other sender can refill it and the final send never blocks.
func (s *panelSubscriber) offer(event panelEvent) {
	select {
	case s.events <- event:
		return
	default:
	}
	select {
	case <-s.events:
	default:
	}
	s.events <- event
}

// panelEventHub builds the snapshot once per interval for every subscriber.
// The snapshot does not vary by identity: /snapshot serves one copy to all
// fleet:read callers, so no per-visibility grouping is needed.
type panelEventHub struct {
	source     func() ([]byte, error)
	interval   time.Duration
	heartbeat  time.Duration
	lifetime   time.Duration
	maxTotal   int
	maxPerUser int

	mu      sync.Mutex
	subs    map[*panelSubscriber]int64
	perUser map[int64]int
	stop    chan struct{} // nil while no broadcaster runs
	last    panelEvent
	nextID  uint64
	builds  int
}

func newPanelEventHub(source func() ([]byte, error), interval time.Duration) *panelEventHub {
	return &panelEventHub{source: source, interval: interval, heartbeat: panelEventHeartbeat,
		lifetime: panelEventLifetime, maxTotal: panelEventMaxSubscribers, maxPerUser: panelEventMaxPerUser,
		subs: make(map[*panelSubscriber]int64), perUser: make(map[int64]int),
		// Seeding from the clock keeps IDs increasing across manager restarts.
		nextID: uint64(time.Now().UnixMilli())}
}

func panelSnapshotSource(panel *panelState) func() ([]byte, error) {
	return func() ([]byte, error) {
		panel.mu.RLock()
		snapshot := panel.snapshot
		panel.mu.RUnlock()
		return json.Marshal(snapshot)
	}
}

// registerEventRoutes serves GET /api/events with the same permission as /snapshot.
func registerEventRoutes(mux *http.ServeMux, a *panelAccess, panel *panelState) {
	hub := newPanelEventHub(panelSnapshotSource(panel), panelEventInterval)
	mux.HandleFunc("GET /api/events", a.require("fleet:read", hub.serve))
}

func (h *panelEventHub) subscribe(user int64) (*panelSubscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= h.maxTotal || h.perUser[user] >= h.maxPerUser {
		return nil, errPanelEventsFull
	}
	s := &panelSubscriber{events: make(chan panelEvent, 1)}
	h.subs[s] = user
	h.perUser[user]++
	if h.last.data != nil {
		s.events <- h.last
	}
	if h.stop == nil {
		h.stop = make(chan struct{})
		go h.run(h.stop)
	}
	return s, nil
}

func (h *panelEventHub) unsubscribe(s *panelSubscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	user, ok := h.subs[s]
	if !ok {
		return
	}
	delete(h.subs, s)
	if h.perUser[user]--; h.perUser[user] <= 0 {
		delete(h.perUser, user)
	}
	if len(h.subs) == 0 && h.stop != nil {
		close(h.stop)
		h.stop = nil
	}
}

func (h *panelEventHub) run(stop chan struct{}) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		h.publish(stop)
		select {
		case <-ticker.C:
		case <-stop:
			return
		}
	}
}

func (h *panelEventHub) publish(stop chan struct{}) {
	select {
	case <-stop:
		return
	default:
	}
	data, err := h.source()
	if err != nil {
		slog.Warn("encode panel snapshot", "error", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.builds++
	if h.stop != stop || bytes.Equal(data, h.last.data) {
		return
	}
	h.nextID++
	h.last = panelEvent{id: h.nextID, data: data}
	for s := range h.subs {
		s.offer(h.last)
	}
}

func (h *panelEventHub) serve(w http.ResponseWriter, r *http.Request, user panelIdentity) {
	sub, err := h.subscribe(user.GitHubID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer h.unsubscribe(sub)
	// A client that already holds the current snapshot skips its repeat.
	seen, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	control := http.NewResponseController(w)
	write := func(chunks ...[]byte) bool {
		err := control.SetWriteDeadline(time.Now().Add(panelEventWriteWindow))
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		for _, chunk := range chunks {
			if _, err := w.Write(chunk); err != nil {
				return false
			}
		}
		return control.Flush() == nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if !write([]byte("retry: 3000\n\n")) {
		return
	}
	heartbeat := time.NewTicker(h.heartbeat)
	defer heartbeat.Stop()
	lifetime := time.NewTimer(h.lifetime)
	defer lifetime.Stop()
	closing := panelServerClosing(r)
	for {
		select {
		case event := <-sub.events:
			if event.id == seen {
				continue
			}
			// json.Marshal never emits raw newlines, so one data line suffices.
			if !write([]byte("id: "+strconv.FormatUint(event.id, 10)+"\nevent: snapshot\ndata: "), event.data, []byte("\n\n")) {
				return
			}
		case <-heartbeat.C:
			if !write([]byte(": ping\n\n")) {
				return
			}
		case <-lifetime.C:
			return
		case <-closing:
			return
		case <-r.Context().Done():
			return
		}
	}
}

var panelClosing sync.Map // *http.Server -> chan struct{}

// panelServerClosing reports when the serving http.Server begins Shutdown,
// which otherwise waits for long-lived streams to finish on their own.
func panelServerClosing(r *http.Request) <-chan struct{} {
	server, _ := r.Context().Value(http.ServerContextKey).(*http.Server)
	if server == nil {
		return nil
	}
	closing := make(chan struct{})
	existing, loaded := panelClosing.LoadOrStore(server, closing)
	if !loaded {
		server.RegisterOnShutdown(func() { close(closing) })
	}
	return existing.(chan struct{})
}
