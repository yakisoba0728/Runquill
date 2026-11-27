import { afterEach, describe, expect, it, vi } from "vitest";
import { answerSamples, ApiError, download, onUnauthorized, request } from "./api";
import { setCatalogue } from "./i18n";

afterEach(() => {
  vi.unstubAllGlobals();
  setCatalogue("en", {});
});

const answer = (status: number, body: string) => vi.stubGlobal("fetch", vi.fn(async () => new Response(body, { status })));

describe("api", () => {
  it("translates a plain-text error and keeps the status", async () => {
    setCatalogue("ko", { "Job not found.": "작업을 찾을 수 없습니다." });
    answer(404, "Job not found.\n");
    await expect(request("./api/jobs/1")).rejects.toMatchObject({ status: 404, message: "작업을 찾을 수 없습니다." });
  });

  it("reads a JSON error with code and pull request", async () => {
    answer(409, JSON.stringify({ error: "A branch exists.", code: "branch_exists", pull_request: "https://github.com/o/r/pull/1" }));
    const error = (await request("./api/migrate/open").catch((e) => e)) as ApiError;
    expect([error.message, error.code, error.pullRequest]).toEqual(["A branch exists.", "branch_exists", "https://github.com/o/r/pull/1"]);
  });

  it("ends the session on 401", async () => {
    const ended = vi.fn();
    onUnauthorized(ended);
    answer(401, "Sign in.");
    await expect(request("./api/me")).rejects.toMatchObject({ status: 401 });
    expect(ended).toHaveBeenCalledOnce();
  });

  it("explains a network failure and a timeout", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("Failed to fetch"); }));
    await expect(request("./api/me")).rejects.toMatchObject({ status: 0, message: "Could not reach the manager. Check the connection." });
    vi.stubGlobal("fetch", vi.fn(async () => { throw new DOMException("late", "TimeoutError"); }));
    await expect(request("./api/me")).rejects.toMatchObject({ status: 0, message: "The manager did not answer in time. Try again." });
  });

  it("decodes JSON and 204", async () => {
    answer(200, '{"login":"octo","role":"admin"}');
    await expect(request("./api/me")).resolves.toEqual({ login: "octo", role: "admin" });
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 204 })));
    await expect(request("./api/logout", { method: "POST" })).resolves.toBeNull();
  });

  it("answers sample GETs from fixtures and refuses everything else", async () => {
    const network = vi.fn();
    vi.stubGlobal("fetch", network);
    answerSamples((path, query) => (path === "/api/jobs" ? { path, limit: query.get("limit") } : undefined));
    try {
      await expect(request("./api/jobs?limit=25")).resolves.toEqual({ path: "/api/jobs", limit: "25" });
      await expect(request("./api/jobs/1/retry", { method: "POST" })).rejects.toMatchObject({ status: 0, message: "Not available in the sample panel." });
      await expect(request("./api/team")).rejects.toBeInstanceOf(ApiError);
      expect(network).not.toHaveBeenCalled();
    } finally {
      answerSamples(null);
    }
  });

  it("downloads a body in chunks with progress, and refuses in the sample panel", async () => {
    const body = new ReadableStream({ start(c) { c.enqueue(new Uint8Array([1, 2])); c.enqueue(new Uint8Array([3])); c.close(); } });
    const fetch = vi.fn(async () => new Response(body, { headers: { "Content-Length": "3" } }));
    vi.stubGlobal("fetch", fetch);
    const progress = vi.fn();
    const blob = await download("./api/backup", "application/vnd.sqlite3", progress);
    expect([blob.size, blob.type]).toEqual([3, "application/vnd.sqlite3"]);
    expect(progress.mock.calls).toEqual([[2, 3], [3, 3]]);
    expect(fetch).toHaveBeenCalledWith("./api/backup", { method: "POST", cache: "no-store" });
    answerSamples(() => undefined);
    try {
      await expect(download("./api/backup", "x")).rejects.toMatchObject({ message: "Not available in the sample panel." });
    } finally {
      answerSamples(null);
    }
  });
});
