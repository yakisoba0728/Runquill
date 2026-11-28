import { describe, expect, it } from "vitest";
import { attention } from "./attention";
import { summary } from "./fleet";
import type { Snapshot } from "./types";

const snapshot: Snapshot = {
  last_poll: "2026-11-28T00:00:00Z",
  poll_failed: true,
  fleet: {
    slot_limit: 4,
    docker_error: "Cannot connect to the Docker daemon",
    runners: { known_running: 1, occupied_slots: 1, reserved_slots: 1 },
    hosts: [
      { name: "local", status: "UP", slot_limit: 2, occupied_slots: 1, reserved_slots: 0 },
      { name: "agent://edge-1", status: "OFFLINE", slot_limit: 2, occupied_slots: 0, reserved_slots: 1, last_seen: "2026-11-27T23:00:00Z" },
      { name: "east", status: "UP", cordoned: true, slot_limit: 2, occupied_slots: 0, reserved_slots: 0, image_status: "failed" },
    ],
    jobs: [
      { id: 1, repository: "o/a", status: "RETRY", retry_reason: "no image", timing: { runquill_queue_seconds: 40 } },
      { id: 2, repository: "o/b", status: "WAITING", timing: { runquill_queue_seconds: 9 } },
      { id: 3, repository: "o/c", status: "OFFLINE" },
      { id: 4, repository: "o/d", status: "CLEANUP", diagnostic: { code: "runner_memory_limit" } },
    ],
  },
};

describe("attention", () => {
  it("lists problems in priority order with links", () => {
    expect(attention(snapshot).map((item) => [item.key, item.href ?? item.section])).toEqual([
      ["docker", "#/hosts"],
      ["poll", "github-connection"],
      ["host:agent://edge-1", "#/hosts/agent%3A%2F%2Fedge-1"],
      ["image:east", "#/hosts/east"],
      ["offline:3", "#/jobs/3"],
      ["retry:1", "#/jobs/1"],
      ["memory:4", "#/jobs/4"],
    ]);
  });

  it("asks for App evidence until a webhook or poll arrives, and is empty without a snapshot", () => {
    expect(attention({ fleet: { slot_limit: 1 } }).map((item) => item.key)).toEqual(["evidence"]);
    expect(attention(null)).toEqual([]);
  });
});

describe("summary", () => {
  it("counts capacity and host states", () => {
    expect(summary(snapshot)).toMatchObject({
      running: 1, queued: 2, ready: 1, retrying: 1, oldestWait: 40, used: 2, free: 1, limit: 4,
      hosts: 3, hostsUp: 1, hostsOffline: 1, hostsCordoned: 1, hostsKnown: true,
    });
    expect(summary({})).toBeNull();
  });
});
