// Phase 5 (design §7.7): row menus on the waiting tab and the hosts table, and the runner log tools folded into one Menu
// at phone width. Same actions, role gates and confirmations as the pane footer and the palette.
import { expect, test, type Page } from "@playwright/test";
import { sample } from "../src/demo";
import { startLive, watchPage } from "./live";

let check: () => void;
test.beforeEach(async ({ page }) => {
  check = await watchPage(page);
});
test.afterEach(() => check());

const toast = (page: Page, message: string) => page.getByRole("region", { name: "Notifications" }).getByText(message);
const waiting = (page: Page) => page.getByRole("grid", { name: "Waiting" });

test("waiting rows: Tab reaches only the current row's menu, which cancels without opening the pane", async ({ page }) => {
  const live = await startLive(page);
  await page.route("**/api/jobs/4828/cancel", (route) => {
    live.snapshot.fleet!.jobs = live.snapshot.fleet!.jobs!.filter((job) => job.id !== 4828);
    return route.fulfill({ status: 204 });
  });
  await page.goto("./#/jobs");
  const rows = waiting(page).getByRole("row");
  await rows.nth(1).focus(); // #4827, then #4828
  await page.keyboard.press("Tab");
  await expect(page.getByRole("button", { name: "Actions for job 4827" })).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(rows.nth(1)).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(rows.nth(2)).toBeFocused();
  await page.keyboard.press("Tab");
  const trigger = page.getByRole("button", { name: "Actions for job 4828" });
  await expect(trigger).toBeFocused();
  await expect(page.getByRole("button", { name: "Actions for job 4827" })).toHaveAttribute("tabindex", "-1");

  await page.keyboard.press("Enter");
  const menu = page.getByRole("menu");
  await expect(menu.getByRole("menuitem")).toHaveText(["Retry now", "Cancel job"]); // #4828 has a retry delay
  await expect(menu.getByRole("menuitem", { name: "Retry now" })).toBeFocused();
  await page.keyboard.press("End");
  await expect(menu.getByRole("menuitem", { name: "Cancel job" })).toBeFocused();
  await page.keyboard.press("Enter");
  const confirm = page.getByRole("alertdialog");
  await expect(confirm).toContainText("Cancel job #4828 in Runquill?");
  await confirm.getByRole("button", { name: "Cancel job" }).click();
  await expect(toast(page, "Job cancelled.")).toBeVisible();
  await expect(waiting(page).getByText("#4828")).toHaveCount(0);
  await expect(page).toHaveURL(/#\/jobs$/);
  // The row left with its menu: focus stays in the table, on its current row, and Tab still reaches only its menu.
  await expect(rows.nth(1)).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("button", { name: "Actions for job 4827" })).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(rows.nth(1)).toBeFocused();
  expect(live.requests.filter((request) => request.method() === "POST").map((request) => new URL(request.url()).pathname))
    .toEqual(["/api/jobs/4828/cancel"]);
});

test("waiting rows with a pane open: Escape closes only the menu, and a click inside it is not a row click", async ({ page }) => {
  await startLive(page);
  await page.goto("./#/jobs/4828");
  await expect(page.getByRole("heading", { name: "Job #4828" })).toBeVisible();
  const trigger = page.getByRole("button", { name: "Actions for job 4827" });
  await trigger.click();
  const menu = page.getByRole("menu");
  await expect(menu).toBeVisible();
  await menu.click({ position: { x: 3, y: 3 } }); // the menu's padding, inside the row
  await expect(page).toHaveURL(/#\/jobs\/4828$/);
  await menu.getByRole("menuitem", { name: "Cancel job" }).focus();
  await page.keyboard.press("j"); // typeahead, not the pane's next item
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(page).toHaveURL(/#\/jobs\/4828$/);
  await expect(page.getByRole("heading", { name: "Job #4828" })).toBeVisible();
  await expect(trigger).toBeFocused();
});

test("only operators get row menus, and only on the waiting tab and the hosts table", async ({ page }) => {
  await startLive(page, "operator");
  await page.goto("./#/jobs?tab=running");
  await expect(page.getByRole("grid", { name: "Running" }).getByRole("row").nth(1)).toBeVisible();
  await expect(page.getByRole("button", { name: /^Actions/ })).toHaveCount(0);
  await page.goto("./#/jobs?tab=cleaned");
  await expect(page.getByRole("grid", { name: "Cleaned up" }).getByRole("row").nth(1)).toBeVisible();
  await expect(page.getByRole("button", { name: /^Actions/ })).toHaveCount(0);
});

test("the sample panel has no row menus", async ({ page }) => {
  await page.goto("./#/jobs");
  await expect(waiting(page).getByRole("row")).toHaveCount(3);
  await expect(page.getByRole("button", { name: /^Actions/ })).toHaveCount(0);
  await page.goto("./#/hosts");
  await expect(page.getByRole("grid", { name: "Hosts" }).getByRole("row").nth(1)).toBeVisible();
  await expect(page.getByRole("button", { name: /^Actions/ })).toHaveCount(0);
});

test("viewers have no row menus", async ({ page }) => {
  await startLive(page, "viewer");
  await page.goto("./#/jobs");
  await expect(waiting(page).getByRole("row")).toHaveCount(3);
  await expect(page.getByRole("button", { name: /^Actions/ })).toHaveCount(0);
  await page.goto("./#/hosts");
  await expect(page.getByRole("grid", { name: "Hosts" }).getByRole("row").nth(1)).toBeVisible();
  await expect(page.getByRole("button", { name: /^Actions/ })).toHaveCount(0);
});

test("host rows: pause from the menu keeps focus on its trigger; admins enroll a worker from it", async ({ page }) => {
  const snapshot = sample("first").snapshot;
  snapshot.fleet!.hosts!.find((host) => host.name === "agent://edge-1")!.enrollment_available = true;
  const live = await startLive(page, "admin", snapshot);
  let body: unknown;
  await page.route("**/api/hosts/cordon", (route) => {
    body = route.request().postDataJSON();
    live.snapshot.fleet!.hosts!.find((host) => host.name === "local")!.cordoned = true;
    return route.fulfill({ status: 204 });
  });
  await page.route("**/api/hosts/edge-1/enrollment", (route) => route.fulfill({
    json: { command: "runquill worker enroll --code abc", start_command: "runquill worker start", expires_at: new Date(Date.now() + 600_000).toISOString() },
  }));
  await page.goto("./#/hosts");
  const local = page.getByRole("button", { name: "Actions on local" });
  await local.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("menu").getByRole("menuitem")).toHaveText(["Pause assignments"]);
  await expect(page.getByRole("menuitem", { name: "Pause assignments" })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(toast(page, "New assignments paused. Existing jobs can finish.")).toBeVisible();
  expect(body).toEqual({ host: "local", cordoned: true });
  await expect(local).toBeFocused();
  await expect(page).toHaveURL(/#\/hosts$/);
  await page.keyboard.press("Enter");
  await expect(page.getByRole("menuitem", { name: "Resume assignments" })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(local).toBeFocused();

  const edge = page.getByRole("button", { name: "Actions on agent://edge-1" });
  await edge.focus();
  await page.keyboard.press("Enter");
  await page.getByRole("menuitem", { name: "Enroll worker" }).click();
  const enroll = page.getByRole("dialog", { name: "Enroll worker" });
  await expect(enroll.getByRole("textbox", { name: "Enrollment command" })).toBeFocused();
  await enroll.getByRole("button", { name: "Hide commands" }).click();
  await expect(enroll).toHaveCount(0);
  await expect(edge).toBeFocused();
});

test("phone width: the log tools fold into one menu inside the sheet", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await startLive(page);
  await page.route("**/api/runners/runquill-demo-4826/logs?*", (route) => route.fulfill({
    contentType: "text/plain; charset=utf-8", body: "2026-11-28T10:00:00.000000000Z Listening for Jobs\n",
  }));
  await page.goto("./#/runners/runquill-demo-4826/logs");
  await expect(page.getByRole("region", { name: "Runner log output" })).toContainText("Listening for Jobs");
  await expect(page.getByRole("button", { name: "Download" })).toBeHidden();
  const actions = page.getByRole("button", { name: "Actions" });
  await actions.focus();
  await page.keyboard.press("Enter");
  const menu = page.getByRole("menu");
  await expect(menu.getByRole("menuitem")).toHaveText(["Pause", "Reconnect", "Copy", "Download", "Clear"]);
  await expect(menu.getByRole("menuitem", { name: "Pause" })).toBeFocused();
  await page.keyboard.press("Escape"); // closes the menu, not the sheet
  await expect(menu).toBeHidden();
  await expect(actions).toBeFocused();
  await expect(page.getByRole("heading", { name: "runquill-demo-4826" })).toBeVisible();
  await page.keyboard.press("Enter");
  await menu.getByRole("menuitem", { name: "Clear" }).click();
  await expect(page.getByText("0 lines", { exact: true })).toBeVisible();
  await expect(actions).toBeFocused();
  const download = page.waitForEvent("download");
  await actions.click();
  await menu.getByRole("menuitem", { name: "Download" }).click();
  expect((await download).suggestedFilename()).toBe("runquill-demo-4826.log");
});
