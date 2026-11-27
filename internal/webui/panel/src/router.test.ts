import { describe, expect, it } from "vitest";
import { parse, resolve } from "./router";

const all = ["overview", "jobs", "runners", "hosts", "usage", "tokens", "settings", "team", "audit", "backups", "migrate"];
const viewer = ["overview", "jobs", "runners", "hosts", "usage", "tokens"];

describe("router", () => {
  it("parses screen, id, sub and query", () => {
    const route = parse("#/jobs/4828?tab=history")!;
    expect([route.screen, route.id, route.sub, route.params.get("tab")]).toEqual(["jobs", "4828", undefined, "history"]);
    expect(parse("#/runners/runquill-demo-1/logs")!.sub).toBe("logs");
  });

  it("decodes an encoded host name as one id", () => {
    expect(parse("#/hosts/agent%3A%2F%2Fedge-1")!.id).toBe("agent://edge-1");
  });

  it("ignores an in-page anchor", () => {
    expect(parse("#main")).toBeNull();
    expect(resolve("#main", all, true)).toBeNull();
  });

  it.each([
    ["#/queue", "#/jobs"],
    ["#/queue?job=4828", "#/jobs/4828"],
    ["#/jobs?job=4828", "#/jobs/4828"],
    ["#/runners?runner=runquill-demo-4826", "#/runners/runquill-demo-4826"],
    ["#/logs", "#/runners"],
    ["#/logs?runner=r1", "#/runners/r1/logs"],
    ["#/hosts?host=agent://edge-1", "#/hosts/agent%3A%2F%2Fedge-1"],
  ])("rewrites the v1 address %s", (from, to) => {
    expect(resolve(from, all, true)?.redirect).toBe(to);
  });

  it("keeps a new address as it is", () => {
    expect(resolve("#/jobs?tab=history", all, true)).toEqual({ route: parse("#/jobs?tab=history") });
    expect(resolve("#/jobs/4828", all, true)?.redirect).toBeUndefined();
  });

  it("sends the team screen to tokens for non-admins", () => {
    expect(resolve("#/team", viewer, false)?.redirect).toBe("#/tokens");
    expect(resolve("#/team", all, true)?.redirect).toBeUndefined();
  });

  it("opens the first visible screen for an unknown, forbidden or empty address", () => {
    expect(resolve("#/nope", viewer, false)?.redirect).toBe("#/overview");
    expect(resolve("#/settings", viewer, false)?.redirect).toBe("#/overview");
    expect(resolve("", viewer, false)?.redirect).toBe("#/overview");
  });
});
