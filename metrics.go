package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type fleetMetrics struct {
	sample       atomic.Pointer[fleetMetricSample]
	starts       atomic.Uint64
	launchErrors atomic.Uint64
	releases     atomic.Uint64
	tickErrors   atomic.Uint64
	notify       *notifier
}

type fleetMetricSample struct {
	at, ready, retry, oldestAge        int64
	limit, occupied, reserved, running int
	offline                            int
}

func (m *fleetMetrics) capture(ctx context.Context, q *queue, hosts []hostCapacity, unassigned, limit int, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	sample := &fleetMetricSample{at: now.Unix(), limit: limit, reserved: unassigned}
	var oldest int64
	err := q.QueryRowContext(ctx, `
        SELECT count(*) FILTER (WHERE retry_at <= ?),
               count(*) FILTER (WHERE retry_at > ?),
               coalesce(min(coalesce(discovered_at, changed_at)), ?)
        FROM jobs WHERE phase = ? AND runner_name IS NULL
    `, now.Unix(), now.Unix(), now.Unix(), queued).Scan(&sample.ready, &sample.retry, &oldest)
	if err != nil {
		return err
	}
	sample.oldestAge = max(0, now.Unix()-oldest)
	for _, host := range hosts {
		if host.Containers == nil {
			sample.offline++
			sample.reserved += host.Occupied
			continue
		}
		sample.occupied += host.Occupied
		for _, state := range host.Containers {
			if state == "running" {
				sample.running++
			}
		}
	}
	m.sample.Store(sample)
	return nil
}

func (m *fleetMetrics) writePrometheus(w io.Writer) {
	counter := func(name, help string, value uint64) {
		fmt.Fprintf(w, "# HELP runquill_%s %s\n# TYPE runquill_%s counter\nrunquill_%s %d\n", name, help, name, name, value)
	}
	counter("runner_starts_total", "Runner runtime starts that succeeded.", m.starts.Load())
	counter("runner_launch_errors_total", "Errors while attempting to launch a runner.", m.launchErrors.Load())
	counter("runner_lease_releases_total", "Runner leases released after reconciliation.", m.releases.Load())
	counter("scheduler_tick_errors_total", "Scheduler ticks that returned an error.", m.tickErrors.Load())
	if m.notify != nil {
		counter("notifications_sent_total", "Notifications delivered to the webhook.", m.notify.Sent())
		counter("notifications_failed_total", "Notifications not delivered after all attempts.", m.notify.Failed())
		counter("notifications_dropped_total", "Notifications discarded because the queue was full or closed.", m.notify.Dropped())
	}

	s := m.sample.Load()
	if s == nil {
		return
	}
	gauge := func(name, help string, value int64) {
		fmt.Fprintf(w, "# HELP runquill_%s %s\n# TYPE runquill_%s gauge\nrunquill_%s %d\n", name, help, name, name, value)
	}
	gauge("fleet_snapshot_timestamp_seconds", "Unix time of the last successful fleet sample.", s.at)
	gauge("queue_ready_jobs", "Queued jobs without a runner that are outside their retry window.", s.ready)
	gauge("queue_retry_jobs", "Queued jobs without a runner waiting for their retry window.", s.retry)
	gauge("queue_oldest_job_age_seconds", "Age of the oldest queued job without a runner at the last sample.", s.oldestAge)
	gauge("runner_slot_limit", "Configured fleet-wide slot limit.", int64(s.limit))
	gauge("runner_occupied_slots", "Runner slots observed occupied on reachable hosts.", int64(s.occupied))
	gauge("runner_reserved_slots", "Slots reserved for offline hosts and unassigned leases.", int64(s.reserved))
	gauge("runners_running", "Runner runtimes observed running.", int64(s.running))
	gauge("hosts_offline", "Hosts whose runner inventory could not be observed.", int64(s.offline))
}

type metricsEndpoint struct {
	fleetMetrics
	server   *http.Server
	listener net.Listener
}

func openMetrics(addr, tokenPath string) (*metricsEndpoint, error) {
	if addr == "" {
		return nil, nil
	}
	host, port, splitErr := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	_, portErr := strconv.ParseUint(port, 10, 16)
	if splitErr != nil || ip == nil || portErr != nil {
		return nil, errors.New("RUNQUILL_METRICS_ADDR must use a literal IP and numeric port")
	}
	var tokenHash [sha256.Size]byte
	if tokenPath != "" {
		data, err := os.ReadFile(tokenPath)
		if err != nil {
			return nil, fmt.Errorf("read RUNQUILL_METRICS_TOKEN_FILE: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if len(token) < 32 || len(token) > 256 || strings.IndexFunc(token, func(r rune) bool { return r < '!' || r > '~' }) >= 0 {
			return nil, errors.New("RUNQUILL_METRICS_TOKEN_FILE must contain a token of 32 to 256 printable ASCII characters without spaces")
		}
		tokenHash = sha256.Sum256([]byte(token))
	} else if !ip.IsLoopback() {
		return nil, errors.New("RUNQUILL_METRICS_TOKEN_FILE is required for non-loopback metrics listeners")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for metrics: %w", err)
	}
	endpoint := &metricsEndpoint{listener: listener}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if tokenPath != "" {
			scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			hash := sha256.Sum256([]byte(raw))
			if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(hash[:], tokenHash[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "Metrics credentials required.", http.StatusUnauthorized)
				return
			}
		} else {
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				host = strings.Trim(r.Host, "[]")
			}
			if !isPanelLoopback(host) {
				http.Error(w, "Use the loopback metrics address.", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		endpoint.writePrometheus(w)
	})
	endpoint.server = &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 8 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	return endpoint, nil
}
