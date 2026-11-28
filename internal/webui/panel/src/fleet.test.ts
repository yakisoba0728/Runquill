import { describe, expect, it } from "vitest";
import { filterJobs, jobLists, jobStatus, pools, runnerStatus, stages } from "./fleet";
import type { Host, Job, Snapshot } from "./types";

const job = (id: number, status: string, extra: Partial<Job> = {}): Job => ({ id, repository: "garden-lab/r" + id, status, ...extra });

describe("fleet", () => {
  it("splits the live jobs into waiting (by position), running and cleaned", () => {
    const snapshot: Snapshot = {
      fleet: { slot_limit: 4, jobs: [job(1, "RUNNING"), job(2, "RETRY", { retry_at: "2026-11-28T00:00:00Z" }), job(3, "WAITING", { queue_position: 1 }), job(4, "OFFLINE")] },
      recent: [{ id: 5, repository: "garden-lab/r5" }],
    };
    const lists = jobLists(snapshot);
    expect(lists.waiting.map((j) => j.id)).toEqual([3, 2]);
    expect(lists.running.map((j) => j.id)).toEqual([1, 4]);
    expect(jobStatus(lists.cleaned[0])).toBe("CLEANED");
  });

  it("filters by repository or job number, pool and state", () => {
    const jobs = [job(4827, "WAITING", { repository: "garden-lab/forecast" }), job(4828, "RETRY", { repository: "garden-lab/pattern-kit", pool: "build" })];
    expect(filterJobs(jobs, " #4828", "").map((j) => j.id)).toEqual([4828]);
    expect(filterJobs(jobs, "FORE", "").map((j) => j.id)).toEqual([4827]);
    expect(filterJobs(jobs, "", "default").map((j) => j.id)).toEqual([4827]);
    expect(filterJobs(jobs, "", "build").map((j) => j.id)).toEqual([4828]);
    expect(filterJobs(jobs, "", "", "RETRY").map((j) => j.id)).toEqual([4828]);
    expect(filterJobs(jobs, "4827", "", "RETRY")).toEqual([]);
    expect(filterJobs([{ id: 5, repository: "o/r" }], "", "", "CLEANED").map((j) => j.id)).toEqual([5]); // recent jobs have no status
  });

  it("lists stages, splitting the queue when JIT times were recorded", () => {
    expect(stages({ creation_to_discovery_seconds: 1, runquill_queue_seconds: 5 }).map((s) => [s.key, s.seconds, s.text])).toEqual([
      ["discovery", 1, "1s"], ["queue", 5, "5s"], ["startup", null, "—"], ["execution", null, "—"],
    ]);
    const jit = stages({ jit_requested_at: "x", scheduler_wait_millis: 250, jit_registration_millis: 1200, runtime_launch_millis: 0 });
    expect(jit.map((s) => s.key)).toEqual(["discovery", "queue", "jit", "runtime", "startup", "execution"]);
    expect(jit.slice(1, 4).map((s) => [s.seconds, s.text])).toEqual([[0.25, "250ms"], [1.2, "1s"], [0, "0ms"]]);
  });

  it("sums host capacity per pool", () => {
    const host = (name: string, pool: string, status: string, used: number): Host =>
      ({ name, pool, status, slot_limit: 2, occupied_slots: used, reserved_slots: 0, warm: 1, warm_target: 2 });
    expect(pools([host("a", "build", "UP", 1), host("b", "build", "OFFLINE", 0), host("c", "", "UP", 0)])).toEqual([
      { name: "build", hosts: 2, ready: 1, occupied: 1, reserved: 0, free: 1, limit: 4, warm: 2, warmTarget: 2 },
      { name: "default", hosts: 1, ready: 1, occupied: 0, reserved: 0, free: 2, limit: 2, warm: 1, warmTarget: 2 },
    ]);
  });

  it("names runtime states", () => {
    expect(runnerStatus({ name: "r", runtime_state: "exited" })).toBe("CLEANUP");
    expect(runnerStatus({ name: "r", runtime_state: "restarting" })).toBe("RESTARTING");
    expect(runnerStatus({ name: "r" })).toBe("UNKNOWN");
  });
});
