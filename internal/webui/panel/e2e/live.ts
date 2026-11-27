// Live-mode harness (design §7.2): the Go demo server's own page and headers (CSP included), switched to
// data-mode="live", with /api/* and /snapshot answered here. A real live manager needs a GitHub OAuth sign-in for a
// session, so the API is intercepted instead. One mutable snapshot feeds /api/events and /snapshot, and the write
// handlers change it, as the manager would.
import { expect, type Page, type Request, type Route } from "@playwright/test";
import { sample } from "../src/demo";
import type { Role, Snapshot } from "../src/types";

export interface Live {
  snapshot: Snapshot;
  /** Every /api/* and /snapshot request, in order. */
  requests: Request[];
}

const text = (route: Route, status: number, body: string) =>
  route.fulfill({ status, contentType: "text/plain; charset=utf-8", body: body + "\n" });

/** Fails the test on a CSP violation or a console error; call before page.goto. */
export async function watchPage(page: Page) {
  const problems: string[] = [];
  await page.addInitScript(() => {
    document.addEventListener("securitypolicyviolation", (event) => {
      console.error("CSP violation: " + event.violatedDirective + " " + event.blockedURI);
    });
  });
  page.on("console", (message) => {
    // A failed request already shows as the panel's own notice or toast; browsers also log it.
    if (message.type() === "error" && !/Failed to load resource|status of [45]\d\d/.test(message.text())) problems.push(message.text());
  });
  page.on("pageerror", (error) => problems.push(error.message));
  return () => expect(problems).toEqual([]);
}

export async function startLive(page: Page, role: Role = "operator", snapshot: Snapshot = sample("first").snapshot): Promise<Live> {
  const state: Live = { snapshot, requests: [] };
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (path.startsWith("/api/") || path === "/snapshot") state.requests.push(request);
    expect(path, "a live panel never loads sample data").not.toMatch(/\/demo-/);
  });
  await page.route((url) => url.pathname === "/", async (route) => {
    const response = await route.fetch();
    const body = (await response.text()).replace('data-mode="demo"', 'data-mode="live"');
    await route.fulfill({ response, body });
  });
  // Anything a test does not answer is not found (registered first, so later routes win).
  await page.route("**/api/**", (route) => text(route, 404, "Not found."));
  await page.route("**/api/me", (route) => route.fulfill({ json: { login: "octocat", role } }));
  // One snapshot per connection, then a long retry: later changes arrive through /snapshot (live.refresh, polling).
  await page.route("**/api/events", (route) => route.fulfill({
    contentType: "text/event-stream",
    body: "retry: 3600000\nevent: snapshot\ndata: " + JSON.stringify(state.snapshot) + "\n\n",
  }));
  await page.route("**/snapshot", (route) => route.fulfill({ json: state.snapshot }));
  return state;
}

export { text };
