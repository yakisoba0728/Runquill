import { describe, expect, it } from "vitest";
import { draftOf, listChanges, settingsBody, type Settings } from "./draft";

const settings = (): Settings => ({
  backend: "docker",
  webhook_enabled: true,
  policy: {
    revision: 7,
    slot_limit: 4,
    poll_interval: "30s",
    runner_image: "ghcr.io/x/runner:v1",
    docker_hosts: ["local", "agent://edge-1"],
    host_limits: { local: 2 },
    worker_pools: {
      build: { hosts: ["local"], repositories: null, cpus: 2, memory_mib: 4096, image: "", cache: true, cache_writes: "default_branch",
        max_per_repository: 3, warm: 2, warm_max_idle_minutes: 30, images: ["ubuntu22.04"] },
      lint: { hosts: [], repositories: ["o/r"], cpus: 1, memory_mib: 512, image: "img", privileged: false },
    },
  },
});

describe("settings draft", () => {
  it("has no changes right after a load", () => {
    const draft = draftOf(settings());
    expect(listChanges(draft.workers, draft.workers) + listChanges(draft.pools, draft.pools)).toBe(0);
    // Two answers of the same settings make equal drafts (the baseline after a save).
    expect(JSON.stringify(draftOf(settings()))).toBe(JSON.stringify(draft));
  });

  it("counts added, removed and changed entries once each", () => {
    const { workers } = draftOf(settings());
    const edited = [{ ...workers[0], limit: "3" }, { key: "new", address: "ssh://b", limit: "" }];
    expect(listChanges(workers, edited)).toBe(3); // local changed, edge-1 removed, ssh://b added
    expect(listChanges(workers, [...workers])).toBe(0);
  });

  it("saves back what it loaded, pool fields it does not edit included", () => {
    const draft = draftOf(settings());
    const body = settingsBody(draft);
    expect(body).toMatchObject({ revision: 7, slot_limit: 4, poll_interval: "30s", docker_hosts: ["local", "agent://edge-1"], host_limits: { local: 2 } });
    expect(JSON.parse(JSON.stringify(body.worker_pools))).toEqual({
      build: { hosts: ["local"], repositories: null, cpus: 2, memory_mib: 4096, image: "", cache: true, cache_writes: "default_branch",
        max_per_repository: 3, warm: 2, warm_max_idle_minutes: 30, images: ["ubuntu22.04"] }, // privileged stays absent
      lint: { hosts: [], repositories: ["o/r"], cpus: 1, memory_mib: 512, image: "img", privileged: false, cache: false },
    });
  });

  it("keeps the privileged default absent and writes a change", () => {
    const draft = draftOf(settings());
    draft.pools[0].privileged = false;
    draft.pools[1].privileged = true;
    const pools = settingsBody(draft).worker_pools;
    expect([pools.build.privileged, pools.lint.privileged]).toEqual([false, true]);
  });

  it("drops empty warm, OS list and unrestricted repositories, and trims", () => {
    const draft = draftOf(settings());
    Object.assign(draft.pools[0], { warm: "", warmIdle: "0", images: " ", restrict: false, repositories: "o/x", name: " build " });
    draft.workers[0].limit = "";
    const body = JSON.parse(JSON.stringify(settingsBody(draft)));
    expect(body.host_limits).toEqual({});
    expect(body.worker_pools.build).not.toHaveProperty("warm");
    expect(body.worker_pools.build).not.toHaveProperty("warm_max_idle_minutes");
    expect(body.worker_pools.build).not.toHaveProperty("images");
    expect(body.worker_pools.build.repositories).toBeNull();
  });

  it("reads lists one per line or comma-separated", () => {
    const draft = draftOf(settings());
    expect(draft.pools[0].images).toBe("ubuntu22.04");
    Object.assign(draft.pools[1], { hosts: "local\n ssh://a , ssh://b\n\n", repositories: "o/r\no/s" });
    const pool = settingsBody(draft).worker_pools.lint;
    expect([pool.hosts, pool.repositories]).toEqual([["local", "ssh://a", "ssh://b"], ["o/r", "o/s"]]);
  });

  it("refuses duplicate pool names and bad runner OS names", () => {
    const draft = draftOf(settings());
    draft.pools[1].name = "build";
    expect(() => settingsBody(draft)).toThrow("Use a different name for each worker pool.");
    draft.pools[1].name = "lint";
    draft.pools[1].images = "debian13, Ubuntu";
    expect(() => settingsBody(draft)).toThrow("Pool lint: Ubuntu is not a runner OS name.");
    draft.pools[1].images = Array.from({ length: 65 }, (_, i) => "os" + i).join(",");
    expect(() => settingsBody(draft)).toThrow("Pool lint may allow at most 64 runner OS names.");
  });
});
