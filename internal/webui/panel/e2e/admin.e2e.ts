// Phase 4: sign-in reasons and the administration screens (settings, team, API tokens, audit, backups, migration) in
// live mode against the Go demo server's page and CSP, with /api/* answered here (e2e/live.ts).
import { expect, test, type Page } from "@playwright/test";
import { startLive, text, watchPage } from "./live";

let check: () => void;
test.beforeEach(async ({ page }) => {
  check = await watchPage(page);
});
test.afterEach(() => check());

const toast = (page: Page, message: string | RegExp) => page.getByRole("region", { name: "Notifications" }).getByText(message);
const confirmDialog = (page: Page) => page.getByRole("alertdialog");
const saveBar = (page: Page) => page.getByRole("region", { name: "Unsaved changes" });

const settings = (revision = 3) => ({
  backend: "docker",
  webhook_enabled: true,
  runner_images: [{ label: "ubuntu22.04" }, { label: "debian13" }],
  policy: {
    revision,
    slot_limit: 4,
    poll_interval: "30s",
    runner_image: "ghcr.io/yakisoba0728/runquill-runner:v0.3.3",
    docker_hosts: ["local", "agent://edge-1"],
    host_limits: { local: 2 },
    worker_pools: {
      build: { hosts: ["local"], repositories: null, cpus: 2, memory_mib: 4096, image: "", cache: true, cache_writes: "default_branch", max_per_repository: 3 },
    },
  },
});

test.describe("sign-in", () => {
  for (const [code, message] of [
    ["expired", "Your sign-in expired. Continue with GitHub to try again."],
    ["cancelled", "Sign-in was cancelled or already used. Continue with GitHub to try again."],
    ["github", "Could not verify the GitHub login. Try again."],
    ["not_enrolled", "Your GitHub account is not enrolled in this Runquill panel."],
    ["unavailable", "Could not start a panel session."],
  ]) {
    test(`the ${code} reason shows once`, async ({ page }) => {
      await startLive(page);
      await page.route("**/api/me", (route) => text(route, 401, "Sign in to continue."));
      await page.goto("./?signin=" + code);
      await expect(page.getByRole("alert")).toHaveText(message);
      expect(new URL(page.url()).search).toBe("");
      await page.reload();
      await expect(page.getByRole("heading", { name: "Sign in to Runquill" })).toBeVisible();
      await expect(page.getByRole("alert")).toHaveCount(0);
    });
  }

  test("an unknown reason is ignored", async ({ page }) => {
    await startLive(page);
    await page.route("**/api/me", (route) => text(route, 401, "Sign in to continue."));
    await page.goto("./?signin=nope");
    await expect(page.getByRole("heading", { name: "Sign in to Runquill" })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
  });
});

test.describe("settings", () => {
  test("unsaved changes: leaving shakes the bar, staying keeps the edit, discard and leave goes on", async ({ page }) => {
    await startLive(page, "admin");
    await page.route("**/api/settings", (route) => route.fulfill({ json: settings() }));
    await page.goto("./#/settings");
    const limit = page.getByRole("spinbutton", { name: "Fleet slot limit" });
    await limit.fill("6");
    await expect(saveBar(page)).toContainText("Unsaved changes: 1");

    // A nav link: the move is undone and the bar says why.
    await page.getByRole("link", { name: "Jobs" }).click();
    await expect(saveBar(page)).toContainText("Unsaved changes (1) kept you here.");
    await expect(page).toHaveURL(/#\/settings$/);
    await expect(page.getByRole("heading", { level: 1, name: "Settings" })).toBeVisible();

    // The palette and Back are held the same way; the edit stays.
    await page.keyboard.press("ControlOrMeta+k");
    await page.getByRole("combobox", { name: "Commands" }).fill("go to usage");
    await page.getByRole("option", { name: "Go to Usage" }).click();
    await expect(page).toHaveURL(/#\/settings$/);
    await page.goBack();
    await expect(page).toHaveURL(/#\/settings$/);
    await expect(limit).toHaveValue("6");

    // Staying and editing again clears the warning; leaving again offers discard.
    await limit.fill("7");
    await expect(saveBar(page)).toContainText("Unsaved changes: 1");
    await expect(saveBar(page)).not.toContainText("kept you here");
    await page.getByRole("link", { name: "Hosts" }).click();
    await saveBar(page).getByRole("button", { name: "Discard and leave" }).click();
    await expect(page).toHaveURL(/#\/hosts$/);
    await expect(page.getByRole("heading", { level: 1, name: "Hosts" })).toBeVisible();
  });

  test("save round trip keeps fields the editor does not show; a server refusal keeps the edits", async ({ page }) => {
    const live = await startLive(page, "admin");
    const bodies: Record<string, unknown>[] = [];
    let refuse = true;
    await page.route("**/api/settings", (route) => {
      if (route.request().method() === "GET") return route.fulfill({ json: settings() });
      bodies.push(route.request().postDataJSON());
      if (refuse) return text(route, 400, "worker pool build: warm runners cannot be combined with a shared build cache");
      const saved = settings(4);
      saved.policy.slot_limit = 6;
      return route.fulfill({ json: saved });
    });
    await page.goto("./#/settings");
    await page.getByRole("spinbutton", { name: "Fleet slot limit" }).fill("6");
    await page.getByRole("spinbutton", { name: "Warm runners per host" }).fill("2");
    await expect(saveBar(page)).toContainText("Unsaved changes: 2");
    await saveBar(page).getByRole("button", { name: "Save" }).click();
    await expect(saveBar(page)).toContainText("Not saved: worker pool build: warm runners cannot be combined with a shared build cache");
    await expect(page.getByRole("spinbutton", { name: "Warm runners per host" })).toHaveValue("2");
    expect(bodies[0]).toMatchObject({
      revision: 3, slot_limit: 6, docker_hosts: ["local", "agent://edge-1"], host_limits: { local: 2 },
      worker_pools: { build: { warm: 2, cache: true, cache_writes: "default_branch", max_per_repository: 3, repositories: null } },
    });
    expect((bodies[0].worker_pools as { build: object }).build).not.toHaveProperty("privileged");

    refuse = false;
    await page.getByRole("spinbutton", { name: "Warm runners per host" }).fill("");
    await page.keyboard.press("ControlOrMeta+s");
    await expect(saveBar(page)).toContainText("Settings saved.");
    await expect(saveBar(page)).toBeHidden();
    await expect(page.getByRole("spinbutton", { name: "Fleet slot limit" })).toHaveValue("6");
    expect(bodies[1]).toMatchObject({ revision: 3, slot_limit: 6 });
    // Saved: nothing holds the next move.
    await page.getByRole("link", { name: "Jobs" }).click();
    await expect(page).toHaveURL(/#\/jobs$/);
    expect(live.requests.filter((request) => request.method() === "PUT")).toHaveLength(2);
  });

  test("a native check stops the save before any request", async ({ page }) => {
    const live = await startLive(page, "admin");
    await page.route("**/api/settings", (route) => route.fulfill({ json: settings() }));
    await page.goto("./#/settings");
    await page.getByRole("button", { name: "Add pool" }).click();
    await expect(page.getByRole("textbox", { name: "Pool name" }).last()).toBeFocused();
    await page.getByRole("textbox", { name: "Pool name" }).last().fill("Bad Name");
    await saveBar(page).getByRole("button", { name: "Save" }).click();
    await expect(page.getByRole("textbox", { name: "Pool name" }).last()).toBeFocused();
    expect(live.requests.filter((request) => request.method() === "PUT")).toHaveLength(0);
  });
});

test.describe("settings keyboard", () => {
  test("an invalid field in a closed pool opens it and takes focus", async ({ page }) => {
    const live = await startLive(page, "admin");
    await page.route("**/api/settings", (route) => route.fulfill({ json: settings() }));
    await page.goto("./#/settings");
    const name = page.getByRole("textbox", { name: "Pool name" });
    await name.fill("");
    await page.getByRole("button", { name: "New pool" }).click(); // the pool's header follows its (now empty) name
    await expect(name).toBeHidden();
    await saveBar(page).getByRole("button", { name: "Save" }).click();
    await expect(name).toBeVisible();
    await expect(name).toBeFocused();
    expect(live.requests.filter((request) => request.method() === "PUT")).toHaveLength(0);
  });

  test("removing a worker: cancel returns focus, confirm moves it to the next worker", async ({ page }) => {
    await startLive(page, "admin");
    await page.route("**/api/settings", (route) => route.fulfill({ json: settings() }));
    await page.goto("./#/settings");
    const remove = page.getByRole("button", { name: "Remove worker local" });
    await remove.focus();
    await page.keyboard.press("Enter");
    await expect(confirmDialog(page)).toContainText("Remove the worker local from these settings?");
    await page.keyboard.press("Escape");
    await expect(remove).toBeFocused();
    await page.keyboard.press("Enter");
    await confirmDialog(page).getByRole("button", { name: "Remove" }).click();
    await expect(page.getByRole("textbox", { name: "Worker address" }).first()).toHaveValue("agent://edge-1");
    // The next worker is an outbound agent: its address is read-only, so its slot limit takes focus.
    await expect(page.getByRole("spinbutton", { name: "Worker slot limit" }).first()).toBeFocused();
    await expect(saveBar(page)).toContainText("Unsaved changes: 1");
  });
});

test.describe("tokens and team", () => {
  test("a new token shows once, then is gone", async ({ page }) => {
    const live = await startLive(page, "viewer");
    let tokens: object[] = [];
    await page.route("**/api/tokens", (route) => {
      if (route.request().method() === "GET") return route.fulfill({ json: tokens });
      tokens = [{ id: 9, user_id: 1, name: "Metrics", scope: "fleet:read", expires_at: Math.floor(Date.now() / 1000) + 30 * 86400 }];
      return route.fulfill({ json: { ...tokens[0], secret: "rq_secret_value" } });
    });
    await page.goto("./#/tokens");
    await expect(page.getByText("No active API tokens. Create one when a script needs access.")).toBeVisible();
    // A viewer can pick fleet:read only.
    await page.getByRole("combobox", { name: "Scope" }).click();
    await expect(page.getByRole("option", { name: "Cancel and retry jobs" })).toHaveAttribute("aria-disabled", "true");
    await page.keyboard.press("Escape");
    await page.getByRole("textbox", { name: "Token name" }).fill("Metrics");
    await page.getByRole("button", { name: "Create token" }).click();
    const secret = page.getByRole("textbox", { name: "New API token" });
    await expect(secret).toHaveValue("rq_secret_value");
    await expect(secret).toBeFocused();
    expect(live.requests.find((request) => request.method() === "POST")!.postDataJSON()).toEqual({ name: "Metrics", scope: "fleet:read", days: 30 });
    await expect(page.getByRole("table", { name: "API tokens" })).toContainText("Metrics");

    await page.getByRole("button", { name: "Hide token" }).click();
    await expect(secret).toHaveCount(0);
    await expect(page.getByRole("textbox", { name: "Token name" })).toBeFocused();
    expect(await page.content()).not.toContain("rq_secret_value");
  });

  test("leaving the screen drops the secret", async ({ page }) => {
    await startLive(page, "operator");
    await page.route("**/api/tokens", (route) => route.request().method() === "GET"
      ? route.fulfill({ json: [] }) : route.fulfill({ json: { id: 1, name: "x", scope: "jobs:write", expires_at: 0, secret: "rq_once" } }));
    await page.goto("./#/tokens");
    await page.getByRole("textbox", { name: "Token name" }).fill("x");
    await page.getByRole("button", { name: "Create token" }).click();
    await expect(page.getByRole("textbox", { name: "New API token" })).toHaveValue("rq_once");
    await page.getByRole("link", { name: "Overview" }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
    await page.goBack();
    await expect(page.getByRole("heading", { level: 1, name: "API tokens" })).toBeVisible();
    expect(await page.content()).not.toContain("rq_once");
  });

  test("team: a role change asks first and saves", async ({ page }) => {
    const live = await startLive(page, "admin");
    let members = [{ github_id: 1, login: "octocat", role: "admin", disabled: false }, { github_id: 583231, login: "hubot", role: "viewer", disabled: false }];
    await page.route("**/api/team?*", (route) => route.fulfill({ json: members }));
    await page.route("**/api/team/583231", (route) => {
      members = members.map((m) => (m.github_id === 583231 ? { ...m, ...route.request().postDataJSON() } : m));
      return route.fulfill({ status: 204 });
    });
    await page.goto("./#/team");
    const role = page.getByRole("combobox", { name: "Role for @hubot" });
    await role.click();
    await page.getByRole("option", { name: "Operator" }).click();
    await page.getByRole("button", { name: "Change role for @hubot" }).click();
    await expect(confirmDialog(page)).toContainText("Change @hubot to Operator?");
    await confirmDialog(page).getByRole("button", { name: "Change role" }).click();
    await expect(toast(page, "Role changed.")).toBeVisible();
    expect(live.requests.find((request) => request.method() === "PUT")!.postDataJSON()).toEqual({ role: "operator", disabled: false });
    await expect(role).toHaveText("Operator");
    await expect(page.getByRole("button", { name: "Change role for @hubot" })).toHaveCount(0);
  });
});

test.describe("audit, backups and migration", () => {
  test("audit filters live in the address and load older pages", async ({ page }) => {
    const live = await startLive(page, "admin");
    const entry = (id: number) => ({ id, actor: 1, actor_login: "octocat", action: "job.cancel", target: String(4800 + id), occurred_at: 1_790_000_000 });
    await page.route("**/api/audit?*", (route) => {
      const before = new URL(route.request().url()).searchParams.get("before");
      return route.fulfill({ json: before ? [entry(1)] : Array.from({ length: 50 }, (_, i) => entry(100 - i)) });
    });
    await page.goto("./#/audit?action=job.");
    await expect(page.getByRole("combobox", { name: "Action" })).toHaveText("Job actions");
    await expect(page.getByRole("table", { name: "Audit history" })).toContainText("job.cancel");
    await page.getByRole("searchbox", { name: "Actor" }).fill("octocat");
    await expect(page).toHaveURL(/#\/audit\?action=job\.&actor=octocat$/);
    await page.getByRole("button", { name: "Load older entries" }).click();
    await expect(page.getByRole("button", { name: "Load older entries" })).toHaveCount(0);
    expect(live.requests.some((request) => /action=job\.&actor=octocat&before=51$/.test(request.url()))).toBe(true);
  });

  test("backups: the queue backup downloads with progress", async ({ page }) => {
    await startLive(page, "admin");
    await page.route("**/api/backups/status", (route) => route.fulfill({ json: { scheduled: false, sealing: false, remote: { configured: false }, last: {} } }));
    await page.route("**/api/backup", (route) => route.fulfill({ body: Buffer.from("SQLite format 3\u0000"), headers: { "Content-Length": "16" } }));
    await page.goto("./#/backups");
    await expect(page.getByText("RUNQUILL_BACKUP_INTERVAL")).toBeVisible();
    const download = page.waitForEvent("download");
    await page.getByRole("button", { name: "Download queue backup" }).click();
    expect((await download).suggestedFilename()).toBe("runquill-queue-backup.sqlite3");
    await expect(page.getByRole("status").filter({ hasText: "Queue backup downloaded." })).toBeVisible();
  });

  test("backups: a verification keeps running after leaving and shows its result on return", async ({ page }) => {
    await startLive(page, "admin");
    const status = { scheduled: true, interval_seconds: 86400, keep: 7, directory: "/b", sealing: false, remote: { configured: false }, last: {} } as Record<string, unknown>;
    let release = () => {};
    const done = new Promise<void>((resolve) => (release = resolve));
    await page.route("**/api/backups/status", (route) => route.fulfill({ json: status }));
    await page.route("**/api/backups/verify", async (route) => {
      await done;
      status.verification = { name: "runquill-queue-1.sqlite3", checked_at: new Date().toISOString(), ok: true };
      return route.fulfill({ json: status.verification });
    });
    await page.goto("./#/backups");
    await page.getByRole("button", { name: "Verify latest backup" }).click();
    await page.getByRole("link", { name: "Overview" }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
    await page.getByRole("link", { name: "Backups" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Verifying the latest local backup…" })).toBeVisible();
    release();
    await expect(page.getByRole("status").filter({ hasText: "The latest backup was verified." })).toBeVisible();
    await expect(page.getByText("runquill-queue-1.sqlite3")).toBeVisible();
  });

  test("migration: the diff scrolls sideways from the keyboard on a phone", async ({ page, browserName }) => {
    // WebKit (as macOS Safari) does not scroll a focused overflow box with the arrow keys; it still takes focus.
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("./#/migrate");
    const diff = page.getByRole("region", { name: "Changes in .github/workflows/ci.yml" });
    await diff.focus();
    await expect(diff).toBeFocused();
    test.skip(browserName === "webkit", "WebKit does not scroll a focused overflow box with the arrow keys");
    for (let i = 0; i < 6; i++) await page.keyboard.press("ArrowRight");
    await expect.poll(() => diff.evaluate((element) => element.scrollLeft)).toBeGreaterThan(0);
  });

  test("migration: a preview shows each file's diff", async ({ page }) => {
    const live = await startLive(page, "admin");
    const { answer } = await import("../src/demo");
    await page.route("**/api/migrate/status", (route) => route.fulfill({ json: { missing: ["workflows"] } }));
    await page.route("**/api/settings", (route) => route.fulfill({ json: settings() }));
    await page.route("**/api/migrate/preview", (route) => route.fulfill({ json: answer("/api/migrate/preview", new URLSearchParams()) }));
    await page.goto("./#/migrate");
    await expect(page.getByRole("table", { name: "GitHub App permissions" })).toContainText("Missing");
    await page.getByRole("button", { name: "Preview changes" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Enter the repository as OWNER/REPO." })).toBeVisible();
    await expect(page.getByRole("textbox", { name: "Repository" })).toBeFocused();
    await page.getByRole("textbox", { name: "Repository" }).fill("garden-lab/papertrail");
    await page.getByRole("button", { name: "Add mapping" }).click();
    await page.getByRole("textbox", { name: "Runs-on labels, mapping 2" }).fill("ubuntu-24.04");
    await page.getByRole("combobox", { name: "Pool, mapping 2" }).click();
    await page.getByRole("option", { name: "build" }).click();
    await page.getByRole("button", { name: "Preview changes" }).click();
    const diff = page.getByRole("table", { name: "Changes in .github/workflows/ci.yml" });
    await expect(diff).toContainText("runs-on: ubuntu-latest");
    await expect(diff.getByRole("row").filter({ hasText: "Removed" }).first()).toContainText("7");
    expect(live.requests.find((request) => request.method() === "POST")!.postDataJSON()).toEqual({
      repository: "garden-lab/papertrail",
      mapping: [{ from: ["ubuntu-latest"], pool: "default" }, { from: ["ubuntu-24.04"], pool: "build" }],
    });
    // Missing write permission: the open step says so and its button is disabled.
    await expect(page.getByText("Grant the three write permissions in step 1 first.")).toBeVisible();
    await expect(page.getByRole("button", { name: "Open pull request" })).toBeDisabled();
    // An edit drops the preview.
    await page.getByRole("textbox", { name: "Repository" }).fill("garden-lab/forecast");
    await expect(diff).toHaveCount(0);
  });
});

test.describe("sample panel administration", () => {
  test("backups and migration show fixtures without writes", async ({ page }) => {
    const sent: string[] = [];
    page.on("request", (request) => new URL(request.url()).pathname.startsWith("/api/") && sent.push(request.url()));
    await page.goto("./#/backups");
    await expect(page.getByText("runquill-queue-20261119T030000Z.sqlite3.sealed").first()).toBeVisible();
    await expect(page.getByText("Downloads are not available in the sample panel.")).toBeVisible();
    await expect(page.getByRole("button", { name: /Verify latest backup|Download queue backup/ })).toHaveCount(0);
    await page.goto("./#/migrate");
    await expect(page.getByRole("table", { name: "Changes in .github/workflows/release.yml" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Open pull request" })).toHaveCount(0);
    for (const screen of ["settings", "team", "audit", "tokens"]) {
      await page.goto("./#/" + screen);
      await expect(page).toHaveURL(/#\/overview$/);
    }
    expect(sent).toEqual([]);
  });
});
