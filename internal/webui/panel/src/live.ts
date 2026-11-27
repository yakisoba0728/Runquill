// The live fleet snapshot, ported from the source state machine in panelweb/app.js (docs/panel.md#live-updates).
// GET /api/events (SSE) is the primary source. If the stream cannot open or stays down, /snapshot is polled with
// exponential backoff and the stream is tried again later. A hidden tab holds no connection. Screens read it with
// useLive(select); select must return a stored reference (derive with useMemo), or React re-renders forever.
import { useSyncExternalStore } from "react";
import { signalUnauthorized } from "./api";
import type { Snapshot } from "./types";

export type Connection = "connecting" | "live" | "polling" | "reconnecting" | "lost" | "hidden" | "sample";

export interface LiveState {
  snapshot: Snapshot | null;
  connection: Connection;
  /** When the next poll runs after a failure (ms since epoch), else 0. */
  retryAt: number;
}

const interval = 3000;
const maxInterval = 30000;

export function createLive() {
  let state: LiveState = { snapshot: null, connection: "connecting", retryAt: 0 };
  const listeners = new Set<() => void>();
  let stream: EventSource | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let watchdog: ReturnType<typeof setTimeout> | undefined;
  let quiet: ReturnType<typeof setTimeout> | undefined;
  let failures = 0;
  let retryStreamAt = 0;
  let stopped = true; // no connection held right now
  let enabled = false; // signed in and started: a visible tab reconnects

  function set(patch: Partial<LiveState>) {
    const next = { ...state, ...patch };
    if ((Object.keys(patch) as (keyof LiveState)[]).every((key) => Object.is(next[key], state[key]))) return;
    state = next;
    for (const listener of listeners) listener();
  }

  function receive(snapshot: Snapshot) {
    failures = 0;
    set({ snapshot, retryAt: 0 });
  }

  function close() {
    stopped = true;
    stream?.close();
    stream = null;
    clearTimeout(timer);
    clearTimeout(watchdog);
    clearTimeout(quiet);
    watchdog = undefined;
  }

  function connect() {
    close();
    stopped = false;
    if (document.hidden) return set({ connection: "hidden" });
    if (typeof EventSource === "function" && Date.now() >= retryStreamAt) open();
    else void poll();
  }

  function open() {
    set({ connection: "connecting" });
    const events = new EventSource("./api/events");
    stream = events;
    events.addEventListener("open", () => {
      clearTimeout(watchdog);
      clearTimeout(quiet);
      watchdog = undefined;
      set({ connection: "live" });
    });
    events.addEventListener("snapshot", (event) => {
      try {
        receive(JSON.parse((event as MessageEvent).data));
      } catch (error) {
        console.error(error);
      }
      set({ connection: "live" });
    });
    events.addEventListener("error", () => {
      if (stream !== events) return;
      if (events.readyState === EventSource.CLOSED) return fallback();
      // The server ends each stream after ten minutes and EventSource reconnects by itself. Only a reconnect that
      // keeps failing counts as lost; the deadline runs from the first error. The status is a live region, so a
      // routine reconnect that opens within four seconds is not announced at all.
      if (watchdog) return;
      quiet = setTimeout(() => {
        if (stream === events && events.readyState !== EventSource.OPEN) set({ connection: "reconnecting" });
      }, 4000);
      watchdog = setTimeout(() => {
        watchdog = undefined;
        if (stream === events && events.readyState !== EventSource.OPEN) fallback();
      }, 10000);
    });
  }

  function fallback() {
    stream?.close();
    stream = null;
    clearTimeout(quiet);
    clearTimeout(watchdog);
    watchdog = undefined;
    retryStreamAt = Date.now() + 60000;
    void poll();
  }

  async function fetchSnapshot(): Promise<Snapshot | null> {
    const response = await fetch("./snapshot", { cache: "no-store", signal: AbortSignal.timeout(5000) });
    if (response.status === 401) {
      stop();
      signalUnauthorized();
      return null;
    }
    if (!response.ok) throw new Error("snapshot unavailable");
    return response.json();
  }

  async function poll() {
    clearTimeout(timer);
    if (stopped) return;
    try {
      const snapshot = await fetchSnapshot();
      if (!snapshot) return;
      receive(snapshot);
    } catch {
      failures++;
    }
    if (stopped || document.hidden) return;
    if (!failures && typeof EventSource === "function" && Date.now() >= retryStreamAt) return open();
    const delay = failures ? Math.min(maxInterval, interval * 2 ** failures) : interval;
    if (failures) set({ connection: "lost", retryAt: Date.now() + delay });
    else set({ connection: "polling" });
    timer = setTimeout(poll, delay);
  }

  function visibility() {
    if (!enabled) return;
    if (document.hidden) {
      close();
      set({ connection: "hidden" });
    } else {
      connect();
    }
  }

  function stop() {
    enabled = false;
    close();
    document.removeEventListener("visibilitychange", visibility);
  }

  return {
    subscribe(listener: () => void) {
      listeners.add(listener);
      return () => void listeners.delete(listener);
    },
    get: () => state,
    /** Starts the connection for a signed-in user. */
    start() {
      if (!enabled) document.addEventListener("visibilitychange", visibility);
      enabled = true;
      connect();
    },
    /** Drops the connection for good (sign-out, session end). */
    stop,
    /** Reads /snapshot once now, e.g. after a change. */
    async refresh() {
      if (stopped) return;
      try {
        const snapshot = await fetchSnapshot();
        if (snapshot) receive(snapshot);
      } catch {}
    },
    /** The sample panel: one fixed snapshot, no connection. */
    sample(snapshot: Snapshot) {
      set({ snapshot, connection: "sample", retryAt: 0 });
    },
  };
}

export type Live = ReturnType<typeof createLive>;
export const live = createLive();

export function useLive<T>(select: (state: LiveState) => T): T {
  return useSyncExternalStore(live.subscribe, () => select(live.get()));
}
