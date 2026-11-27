import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { onUnauthorized } from "./api";
import { createLive } from "./live";

class FakeEventSource extends EventTarget {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  static last: FakeEventSource;
  readyState = 0;
  constructor(public url: string) {
    super();
    FakeEventSource.last = this;
  }
  close() {
    this.readyState = 2;
  }
  opened() {
    this.readyState = 1;
    this.dispatchEvent(new Event("open"));
  }
  failed() {
    this.readyState = 0;
    this.dispatchEvent(new Event("error"));
  }
  send(data: unknown) {
    this.dispatchEvent(new MessageEvent("snapshot", { data: JSON.stringify(data) }));
  }
}

let hidden = false;
let fetchStatus = 200;

beforeEach(() => {
  vi.useFakeTimers();
  hidden = false;
  fetchStatus = 200;
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal("fetch", vi.fn(async () => (fetchStatus === 0
    ? Promise.reject(new TypeError("offline"))
    : new Response(JSON.stringify({ sampled_at: "now" }), { status: fetchStatus }))));
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("live", () => {
  it("goes live on the stream and keeps the snapshot", () => {
    const live = createLive();
    live.start();
    expect(live.get().connection).toBe("connecting");
    FakeEventSource.last.opened();
    FakeEventSource.last.send({ sampled_at: "a" });
    expect(live.get()).toMatchObject({ connection: "live", snapshot: { sampled_at: "a" } });
    live.stop();
  });

  it("stays quiet for a reconnect within 4s, says reconnecting after 4s and polls after 10s", async () => {
    const live = createLive();
    live.start();
    const stream = FakeEventSource.last;
    stream.opened();
    stream.failed();
    await vi.advanceTimersByTimeAsync(3900);
    expect(live.get().connection).toBe("live");
    stream.failed(); // a second failed retry does not restart the deadline
    await vi.advanceTimersByTimeAsync(200);
    expect(live.get().connection).toBe("reconnecting");
    await vi.advanceTimersByTimeAsync(6000);
    expect(live.get().connection).toBe("polling");
    expect(stream.readyState).toBe(2);
    live.stop();
  });

  it("backs off while polling fails, then retries the stream after 60s", async () => {
    const live = createLive();
    live.start();
    const first = FakeEventSource.last;
    fetchStatus = 0;
    first.readyState = 2;
    first.dispatchEvent(new Event("error")); // closed: fall back at once
    await vi.advanceTimersByTimeAsync(0);
    expect(live.get().connection).toBe("lost");
    expect(live.get().retryAt - Date.now()).toBe(6000); // 3s × 2¹
    await vi.advanceTimersByTimeAsync(6000);
    expect(live.get().retryAt - Date.now()).toBe(12000);
    fetchStatus = 200;
    await vi.advanceTimersByTimeAsync(12000);
    expect(live.get()).toMatchObject({ connection: "polling", retryAt: 0 });
    await vi.advanceTimersByTimeAsync(60000);
    expect(FakeEventSource.last).not.toBe(first); // stream tried again after a good poll
    expect(live.get().connection).toBe("connecting");
    live.stop();
  });

  it("drops the connection while hidden and reconnects when shown", () => {
    const live = createLive();
    live.start();
    const stream = FakeEventSource.last;
    hidden = true;
    document.dispatchEvent(new Event("visibilitychange"));
    expect(live.get().connection).toBe("hidden");
    expect(stream.readyState).toBe(2);
    hidden = false;
    document.dispatchEvent(new Event("visibilitychange"));
    expect(FakeEventSource.last).not.toBe(stream);
    live.stop();
    hidden = true;
    document.dispatchEvent(new Event("visibilitychange"));
    expect(live.get().connection).toBe("connecting"); // stopped: no longer reacts
  });

  it("ends the session when /snapshot answers 401", async () => {
    const ended = vi.fn();
    onUnauthorized(ended);
    vi.stubGlobal("EventSource", undefined);
    fetchStatus = 401;
    const live = createLive();
    live.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(ended).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(60000);
    expect(fetch).toHaveBeenCalledOnce();
  });

  it("shows a sample snapshot without connecting", () => {
    const live = createLive();
    live.sample({ sampled_at: "s" });
    expect(live.get()).toMatchObject({ connection: "sample", snapshot: { sampled_at: "s" } });
    expect(fetch).not.toHaveBeenCalled();
  });
});
