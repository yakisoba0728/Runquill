import { describe, expect, it } from "vitest";
import { historyQuery, jobId, pickTab } from "./jobs/params";
import { rightsizingQuery, usageQuery, usageRange } from "./usage/params";

const p = (query: string) => new URLSearchParams(query);
const lists = { waiting: [{ id: 1, repository: "a" }], running: [{ id: 2, repository: "a" }], cleaned: [{ id: 3, repository: "a" }] };

describe("jobs address", () => {
  it("reads only numeric job ids", () => {
    expect(jobId("4828")).toBe(4828);
    expect(jobId("48x")).toBeNull();
    expect(jobId(undefined)).toBeNull();
  });

  it("shows the address's tab, else the list holding the job, else the history", () => {
    expect(pickTab("cleaned", 1, lists)).toBe("cleaned");
    expect(pickTab("bogus", null, lists)).toBe("waiting");
    expect(pickTab(null, 2, lists)).toBe("running");
    expect(pickTab(null, 3, lists)).toBe("cleaned");
    expect(pickTab(null, 9, lists)).toBe("history");
    expect(pickTab(null, 9, null)).toBe("waiting");
    // A chosen tab wins over following the open job, so Waiting stays reachable while job 2 (running) is open.
    expect(pickTab("waiting", 2, lists)).toBe("waiting");
  });

  it("builds the history query with since from now", () => {
    const now = Date.parse("2026-11-28T12:00:00Z");
    expect(historyQuery(p("repo=+garden-lab/x+&outcome=failed&range=3600"), 0, now).toString())
      .toBe("limit=25&repository=garden-lab%2Fx&outcome=failed&since=" + (now / 1000 - 3600));
    expect(historyQuery(p("outcome=nope&range=5"), 4810, now).toString()).toBe("limit=25&before=4810");
  });
});

describe("usage address", () => {
  const now = Date.parse("2026-11-28T12:00:00Z");

  it("defaults to the last 30 UTC days and reads presets", () => {
    expect(usageRange(p(""), now)).toEqual({ from: "2026-10-30", to: "2026-11-28", preset: "30", problem: "" });
    expect(usageRange(p("range=2"), now)).toMatchObject({ from: "2026-11-27", to: "2026-11-28", preset: "2" });
  });

  it("takes custom dates and refuses a reversed or too long range", () => {
    expect(usageRange(p("from=2026-11-01&to=2026-11-28"), now)).toMatchObject({ preset: "custom", problem: "" });
    expect(usageRange(p("from=2026-11-28&to=2026-11-01"), now).problem).not.toBe("");
    expect(usageRange(p("from=2025-01-01&to=2026-11-28"), now).problem).not.toBe("");
    expect(usageRange(p("from=2026-11-01"), now).problem).not.toBe("");
  });

  it("builds the usage and right-sizing queries", () => {
    expect(usageQuery(p("range=7&group=pool&repo=garden-lab/x&metric=p95"), now).toString())
      .toBe("from=2026-11-22&to=2026-11-28&group=pool&repository=garden-lab%2Fx");
    expect(usageQuery(p("group=nope"), now).get("group")).toBe("day");
    expect(rightsizingQuery(p("group=job&days=3")).toString()).toBe("group=job&days=3");
    expect(rightsizingQuery(p("days=30")).toString()).toBe("group=pool&days=7");
  });
});
