import { createServer, request as forward } from "node:http";
import type { AddressInfo } from "node:net";
import { expect, test, type Locator, type Page } from "@playwright/test";
import { startLive, text, watchPage } from "./live";

let check: () => void;
test.beforeEach(async ({ page }) => {
  check = await watchPage(page);
});
test.afterEach(() => check());

const toast = (page: Page, message: string | RegExp) => page.getByRole("region", { name: "Notifications" }).getByText(message);
const dialog = (page: Page) => page.getByRole("alertdialog");
// Keyboard activation: WebKit does not focus a clicked button, so tests that check focus press Enter on it.
const press = async (page: Page, button: Locator) => {
  await button.focus();
  await page.keyboard.press("Enter");
};

test.describe("sample panel", () => {
  test("runner logs show the fixed lines; no write controls; nothing leaves the browser", async ({ page }) => {
    const sent: string[] = [];
    page.on("request", (request) => new URL(request.url()).pathname.startsWith("/api/") && sent.push(request.url()));
    await page.goto("./#/runners/runquill-demo-4826/logs");
    const output = page.getByRole("region", { name: "Runner log output" });
    await expect(output).toContainText("Successfully renew job request 4826");
    await expect(output).toContainText("√ Connected to GitHub"); // escape sequences removed
    expect(await output.textContent()).not.toContain("\u001b");
    await expect(page.getByRole("status").filter({ hasText: "Showing the last 200 lines." })).toBeVisible();
    await expect(page.getByRole("switch", { name: "Follow" })).toBeDisabled();
    await expect(page.getByText("The sample panel shows fixed sample lines and cannot follow output.")).toBeVisible();

    await page.goto("./#/jobs/4828");
    await expect(page.getByRole("heading", { name: "Job #4828" })).toBeVisible();
    await expect(page.getByRole("button", { name: /Retry now|Cancel job|Check repository/ })).toHaveCount(0);
    await page.goto("./#/hosts/local");
    await expect(page.getByRole("heading", { name: "local", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: /assignments/ })).toHaveCount(0);
    expect(sent).toEqual([]);
  });
});

test.describe("live panel", () => {
  test("retry: confirm, POST, toast, fresh snapshot, pane closed", async ({ page }) => {
    const live = await startLive(page);
    await page.route("**/api/jobs/4828/retry", (route) => {
      const job = live.snapshot.fleet!.jobs!.find((candidate) => candidate.id === 4828)!;
      delete job.retry_at;
      job.status = "WAITING";
      return route.fulfill({ status: 204 });
    });
    await page.goto("./#/jobs/4828");
    const retry = page.getByRole("button", { name: "Retry now for job 4828" });
    await retry.click();
    await expect(dialog(page)).toContainText("Retry job #4828 now?");
    await dialog(page).getByRole("button", { name: "Retry now" }).click();
    await expect(toast(page, "The job will be retried on the next scheduler pass.")).toBeVisible();
    await expect(page).toHaveURL(/#\/jobs$/);
    const posted = live.requests.findIndex((request) => request.method() === "POST");
    expect(live.requests[posted].url()).toMatch(/\/api\/jobs\/4828\/retry$/);
    expect(live.requests.slice(posted + 1).some((request) => request.url().endsWith("/snapshot"))).toBe(true);
  });

  test("cancel: a danger confirm, then the job is gone from the queue", async ({ page }) => {
    const live = await startLive(page);
    await page.route("**/api/jobs/4827/cancel", (route) => {
      live.snapshot.fleet!.jobs = live.snapshot.fleet!.jobs!.filter((job) => job.id !== 4827);
      return route.fulfill({ status: 204 });
    });
    await page.goto("./#/jobs/4827");
    await expect(page.getByRole("button", { name: "Retry now for job 4827" })).toHaveCount(0); // no retry delay
    await page.getByRole("button", { name: "Cancel job for job 4827" }).click();
    await dialog(page).getByRole("button", { name: "Cancel job" }).click();
    await expect(toast(page, "Job cancelled.")).toBeVisible();
    await expect(page.getByRole("grid", { name: "Waiting" }).getByText("#4827")).toHaveCount(0);
  });

  test("a refused action shows the server's reason in a toast and keeps focus on its button", async ({ page }) => {
    await startLive(page);
    await page.route("**/api/jobs/4828/retry", (route) => text(route, 403, "Your panel permissions changed. Reload and try again."));
    await page.goto("./#/jobs/4828");
    await press(page, page.getByRole("button", { name: "Retry now for job 4828" }));
    await dialog(page).getByRole("button", { name: "Retry now" }).click();
    await expect(toast(page, "Your panel permissions changed. Reload and try again.")).toBeVisible();
    await expect(page.getByRole("button", { name: "Retry now for job 4828" })).toBeFocused();
    await expect(page).toHaveURL(/#\/jobs\/4828$/);
  });

  test("palette: the open job's actions", async ({ page }) => {
    await startLive(page);
    await page.goto("./#/jobs/4828");
    await expect(page.getByRole("heading", { name: "Job #4828" })).toBeVisible();
    await page.keyboard.press("ControlOrMeta+k");
    await page.getByRole("combobox", { name: "Commands" }).fill("cancel job");
    await page.getByRole("option", { name: "Cancel job #4828" }).click();
    await expect(dialog(page)).toContainText("Cancel job #4828 in Runquill?");
    await page.keyboard.press("Escape");
    await expect(dialog(page)).toHaveCount(0);
    // The palette that opened it is gone, so focus goes to the pane's own button.
    await expect(page.getByRole("button", { name: "Cancel job for job 4828" })).toBeFocused();
  });

  test("cordon round trip: pause, then the pane offers resume", async ({ page }) => {
    const live = await startLive(page);
    let body: unknown;
    await page.route("**/api/hosts/cordon", (route) => {
      body = route.request().postDataJSON();
      live.snapshot.fleet!.hosts!.find((host) => host.name === "local")!.cordoned = true;
      return route.fulfill({ status: 204 });
    });
    await page.goto("./#/hosts/local");
    await press(page, page.getByRole("button", { name: "Pause assignments on local" }));
    await expect(toast(page, "New assignments paused. Existing jobs can finish.")).toBeVisible();
    expect(body).toEqual({ host: "local", cordoned: true });
    await expect(page.getByRole("button", { name: "Resume assignments on local" })).toBeFocused();
  });

  test("admin: worker enrollment and repository check", async ({ page }) => {
    const snapshot = (await import("../src/demo")).sample("first").snapshot;
    snapshot.fleet!.hosts!.find((host) => host.name === "agent://edge-1")!.enrollment_available = true;
    await startLive(page, "admin", snapshot);
    await page.route("**/api/hosts/edge-1/enrollment", (route) => route.fulfill({
      json: { command: "runquill worker enroll --code abc", start_command: "runquill worker start", expires_at: new Date(Date.now() + 600_000).toISOString() },
    }));
    await page.route("**/api/diagnose", (route) => route.fulfill({ json: { ok: false, report: "GitHub App: installed\nLabel: missing" } }));
    await page.goto("./#/hosts/agent%3A%2F%2Fedge-1");
    await press(page, page.getByRole("button", { name: "Enroll worker on agent://edge-1" }));
    const enroll = page.getByRole("dialog", { name: "Enroll worker" });
    await expect(enroll.getByRole("textbox", { name: "Enrollment command" })).toHaveValue("runquill worker enroll --code abc");
    await expect(enroll.getByRole("textbox", { name: "Enrollment command" })).toBeFocused();
    await enroll.getByRole("button", { name: "Hide commands" }).click();
    await expect(enroll).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Enroll worker on agent://edge-1" })).toBeFocused();

    await page.goto("./#/jobs");
    await page.getByRole("button", { name: "Check repository" }).first().click();
    const check = page.getByRole("dialog", { name: "Check repository" });
    await check.getByRole("textbox", { name: "GitHub repository" }).fill("garden-lab/forecast");
    await check.getByRole("button", { name: "Check repository" }).click();
    await expect(check.getByText("Repository check needs attention.")).toBeVisible();
    await expect(check.getByLabel("Report")).toContainText("Label: missing");
    await page.keyboard.press("Escape");

    // From the palette: closing the dialog returns focus to the header button, not to the gone palette.
    await page.keyboard.press("ControlOrMeta+k");
    await page.getByRole("combobox", { name: "Commands" }).fill("check repository");
    await page.keyboard.press("Enter");
    await expect(check).toBeVisible();
    await expect(check.getByRole("textbox", { name: "GitHub repository" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(check).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Check repository" }).first()).toBeFocused();
  });

  test("viewer: no write controls and no Logs tab", async ({ page }) => {
    await startLive(page, "viewer");
    await page.goto("./#/runners/runquill-demo-4826/logs");
    await expect(page.getByRole("heading", { name: "runquill-demo-4826" })).toBeVisible();
    await expect(page.getByRole("tab", { name: "Logs" })).toHaveCount(0);
    await expect(page.getByRole("link", { name: /View logs/ })).toHaveCount(0);
    await page.goto("./#/jobs/4828");
    await expect(page.getByRole("heading", { name: "Job #4828" })).toBeVisible();
    await expect(page.getByRole("button", { name: /Retry now|Cancel job|Check repository/ })).toHaveCount(0);
  });

  test("runner logs stream from the manager", async ({ page }) => {
    const live = await startLive(page);
    await page.route("**/api/runners/runquill-demo-4826/logs?*", (route) => text(route, 200,
      "2026-11-28T10:00:00.000000000Z Listening for Jobs\n2026-11-28T10:00:02.000000000Z \u001b[32mRunning job: build\u001b[0m"));
    await page.goto("./#/runners/runquill-demo-4826/logs");
    const output = page.getByRole("region", { name: "Runner log output" });
    await expect(output).toContainText("Running job: build");
    await expect(page.getByRole("status").filter({ hasText: "The log stream ended. The last lines may explain why." })).toBeVisible();
    expect(live.requests.some((request) => /logs\?tail=200&follow=1$/.test(request.url()))).toBe(true);
    await page.getByRole("button", { name: "Reconnect" }).click();
    await expect(page.getByText("2 lines")).toBeVisible();
  });

  test("a follow stream: lines over time, pause, jump to latest, stop, and no stream after leaving the tab", async ({ page }) => {
    // A same-origin front for the Go server (Chromium refuses a cross-port detour to loopback) that writes the runner
    // log itself, a line at a time, as `docker logs --follow` does.
    const closed: number[] = [];
    let opened = 0;
    const server = createServer((request, response) => {
      if (!request.url!.startsWith("/api/runners/")) {
        const upstream = forward({ host: "127.0.0.1", port: 18181, path: request.url, method: request.method, headers: request.headers }, (answer) => {
          response.writeHead(answer.statusCode!, answer.headers);
          answer.pipe(response);
        });
        request.pipe(upstream);
        return;
      }
      const id = ++opened;
      response.writeHead(200, { "Content-Type": "text/plain; charset=utf-8" });
      let n = 0;
      const line = () => response.write(`2026-11-28T10:00:00.000000000Z line ${++n}\n`);
      for (let i = 0; i < 60; i++) line();
      const timer = setInterval(line, 50);
      request.on("close", () => {
        clearInterval(timer);
        closed.push(id);
      });
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const port = (server.address() as AddressInfo).port;
    try {
      await startLive(page);
      await page.route("**/api/runners/runquill-demo-4826/logs?*", (route) => route.continue());
      await page.goto(`http://127.0.0.1:${port}/#/runners/runquill-demo-4826/logs`);
      const output = page.getByRole("region", { name: "Runner log output" });
      await expect(page.getByRole("status").filter({ hasText: "Following new output." })).toBeVisible();
      await expect(output).toContainText("line 70");

      await page.getByRole("button", { name: "Pause" }).click();
      await expect(page.getByText(/new lines waiting/)).toBeVisible();
      const paused = await output.locator(".log-line").count();
      await page.waitForTimeout(300);
      expect(await output.locator(".log-line").count()).toBe(paused);
      await page.getByRole("button", { name: "Resume" }).click();
      await expect(page.getByText(/new lines waiting/)).toHaveCount(0);

      // Scrolled up, new lines do not pull the view down; Jump to latest does.
      await output.evaluate((element) => (element.scrollTop = 0));
      await expect(page.getByRole("button", { name: "Jump to latest" })).toBeVisible();
      await page.getByRole("button", { name: "Jump to latest" }).click();
      await expect(page.getByRole("button", { name: "Jump to latest" })).toHaveCount(0);

      await page.getByRole("button", { name: "Stop" }).click();
      await expect(page.getByRole("status").filter({ hasText: "Stopped. The lines shown are kept." })).toBeVisible();
      await expect.poll(() => closed).toEqual([1]);

      await page.getByRole("button", { name: "Reconnect" }).click();
      await expect(page.getByRole("status").filter({ hasText: "Following new output." })).toBeVisible();
      await page.getByRole("tab", { name: "Details" }).click();
      await expect.poll(() => closed).toEqual([1, 2]);
      await page.getByRole("tab", { name: "Logs" }).click();
      await expect.poll(() => opened).toBe(3);
      await page.keyboard.press("Escape"); // closing the pane ends the stream too
      await expect.poll(() => closed).toEqual([1, 2, 3]);
    } finally {
      server.closeAllConnections();
      server.close();
    }
  });

  test("REST failures: a notice, then 401 returns to sign-in", async ({ page }) => {
    await startLive(page);
    let status = 500;
    await page.route("**/api/jobs?*", (route) => text(route, status, status === 401 ? "Sign in to continue." : "Could not read the job history."));
    await page.goto("./#/jobs?tab=history");
    await expect(page.getByRole("alert").filter({ hasText: "Job history is unavailable." })).toBeVisible();
    status = 401;
    await page.getByRole("button", { name: "Try again" }).click();
    await expect(page.getByText("Your session ended. Sign in again to view the fleet.")).toBeVisible();
  });

  test("screens render without a snapshot and with one lacking fleet data", async ({ page }) => {
    await startLive(page, "admin", {});
    for (const screen of ["hosts", "jobs", "runners"]) {
      await page.goto("./#/" + screen);
      await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
      await page.waitForTimeout(300);
      expect(await page.evaluate(() => 1 + 1)).toBe(2); // still responsive
    }
    await page.route("**/api/events", (route) => text(route, 503, "Unavailable."));
    await page.route("**/snapshot", (route) => text(route, 503, "Unavailable."));
    await page.goto("./#/hosts");
    await expect(page.getByRole("status").filter({ hasText: "Connection lost" })).toBeVisible({ timeout: 15_000 });
    await expect(page.getByRole("heading", { level: 1, name: "Hosts" })).toBeVisible();
  });

  test("history: a loading state, then older pages", async ({ page }) => {
    const live = await startLive(page);
    const row = (id: number) => ({ id, repository: "garden-lab/forecast", outcome: "finished", queued_at: new Date().toISOString() });
    let release = () => {};
    const first = new Promise<void>((resolve) => (release = resolve));
    await page.route("**/api/jobs?*", async (route) => {
      const before = new URL(route.request().url()).searchParams.get("before");
      if (!before) {
        await first;
        return route.fulfill({ json: { jobs: Array.from({ length: 25 }, (_, i) => row(5000 - i)), next: 4976 } });
      }
      return route.fulfill({ json: { jobs: Array.from({ length: 3 }, (_, i) => row(Number(before) - i)) } });
    });
    await page.goto("./#/jobs?tab=history");
    await expect(page.getByRole("grid", { name: "Job history" })).toHaveAttribute("aria-busy", "true");
    release();
    await expect(page.getByText("25 shown")).toBeVisible();
    await page.getByRole("button", { name: "Load older jobs" }).click();
    await expect(page.getByText("28 shown")).toBeVisible();
    await expect(page.getByRole("button", { name: "Load older jobs" })).toHaveCount(0);
    expect(live.requests.some((request) => /before=4976/.test(request.url()))).toBe(true);
  });
});
