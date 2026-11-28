// Large queues and long table text: the overview groups job problems, the jobs table shows 200 rows at a time and
// searches without writing the address per key, and cells that cut their text show it whole on hover.
import { expect, test, type Page } from "@playwright/test";
import { sample } from "../src/demo";
import type { Snapshot } from "../src/types";
import { startLive, watchPage } from "./live";

let check: () => void;
test.beforeEach(async ({ page }) => {
  check = await watchPage(page);
});
test.afterEach(() => check());

/** The first sample with 1500 waiting jobs, every third one waiting to retry, in queue order 1…1500. */
function busy(): Snapshot {
  const snapshot = sample("first").snapshot;
  snapshot.fleet!.jobs = Array.from({ length: 1500 }, (_, i) => ({
    id: i + 1, repository: "garden-lab/r" + (i % 40), status: i % 3 ? "WAITING" : "RETRY", queue_position: i + 1,
    retry_reason: i % 3 ? undefined : "select runner image: runner image \"ubuntu2004\" is not in the runner image catalogue",
  }));
  return snapshot;
}

const bodyRows = (page: Page) => page.getByRole("grid", { name: "Waiting" }).locator(".data-table-body > [role=row]");

test("a large queue: one attention item per kind, linking to the filtered queue", async ({ page }) => {
  await startLive(page, "viewer", busy());
  await page.goto("./#/overview");
  const list = page.locator(".attention > li");
  const group = page.getByRole("link", { name: "500 jobs are waiting to retry" });
  await expect(group).toHaveAttribute("href", "#/jobs?state=RETRY");
  await expect(page.getByRole("link", { name: /builder-east is unavailable/ })).toBeVisible(); // not buried
  const n = await list.count();
  expect(n).toBeLessThanOrEqual(5);
  await expect(page.getByRole("heading", { name: `Needs attention (${n})` })).toBeVisible();
  await expect(page.getByRole("link", { name: new RegExp(`^Overview ${n} need attention`) })).toBeVisible();

  await group.click();
  await expect(page).toHaveURL(/#\/jobs\?state=RETRY$/);
  await expect(page.getByRole("combobox", { name: "State" })).toContainText("Retry scheduled");
  await expect(bodyRows(page)).toHaveCount(200);
  await expect(page.getByText("200 of 500 shown")).toBeVisible();
});

test("the jobs table shows 200 rows, more on request, and always the open job", async ({ page }) => {
  await startLive(page, "viewer", busy());
  await page.goto("./#/jobs");
  await expect(bodyRows(page)).toHaveCount(200);
  await expect(page.getByText("200 of 1,500 shown")).toBeVisible();
  const more = page.getByRole("button", { name: "Show more" });
  await more.focus();
  await page.keyboard.press("Enter");
  await expect(bodyRows(page)).toHaveCount(400);
  await expect(bodyRows(page).nth(200)).toBeFocused(); // the first row it revealed
  await expect(bodyRows(page).nth(200)).toContainText("#201");

  // j past the last shown row opens the next job and shows its row.
  await page.goto("./#/jobs/200");
  await page.reload(); // a fresh screen: 200 rows again
  await expect(bodyRows(page)).toHaveCount(200);
  await page.getByRole("heading", { name: "Job #200" }).click();
  await page.keyboard.press("j");
  await expect(page).toHaveURL(/#\/jobs\/201$/);
  await expect(page.getByRole("heading", { name: "Job #201" })).toBeVisible();
  await expect(bodyRows(page)).toHaveCount(201);
  await expect(bodyRows(page).nth(200)).toContainText("#201");
});

test("job search filters as typed and writes the address once", async ({ page }) => {
  await startLive(page, "viewer", busy());
  await page.goto("./#/jobs");
  await expect(bodyRows(page)).toHaveCount(200);
  const search = page.getByRole("searchbox", { name: "Filter jobs" });
  await search.pressSequentially("1499", { delay: 20 });
  await expect(page).toHaveURL(/#\/jobs\?q=1499$/);
  await expect(bodyRows(page)).toHaveCount(1);
  await expect(search).toHaveValue("1499");
  // Clear filters empties the box too.
  await search.fill("no such repository");
  await page.getByRole("button", { name: "Clear filters" }).click();
  await expect(page).toHaveURL(/#\/jobs$/);
  await expect(search).toHaveValue("");
  await expect(bodyRows(page)).toHaveCount(200);
});

test("grouped attention items are translated", async ({ page }) => {
  await startLive(page, "viewer", busy());
  await page.goto("./?lang=ko#/overview");
  await expect(page.getByRole("link", { name: "작업 500개가 재시도를 기다립니다" })).toBeVisible();
});

test.describe("sample panel at 1366px", () => {
  test.use({ viewport: { width: 1366, height: 768 } });

  test("cut cells show their whole text on hover", async ({ page }) => {
    await page.goto("./#/jobs");
    await expect(page.getByRole("grid", { name: "Waiting" }).getByTitle(
      'select runner image: runner image "ubuntu2004" is not in the runner image catalogue; add it to RUNQUILL_RUNNER_IMAGES',
    )).toBeVisible();
    await expect(page.getByRole("grid", { name: "Waiting" }).getByTitle("Runner OS debian13")).toBeVisible();
    await page.goto("./#/hosts");
    await expect(page.getByTitle("pulling runner image ghcr.io/garden-lab/runner-build:2026.11")).toBeVisible();
  });

  test("the runners table fits, with the image under the runner name", async ({ page }) => {
    await page.goto("./#/runners");
    const table = page.locator(".data-table").filter({ has: page.getByRole("grid", { name: "Runner inventory" }) });
    await expect(table.getByTitle("ghcr.io/yakisoba0728/runquill-runner:v0.3.3", { exact: true })).toBeVisible();
    await expect(table.getByRole("link", { name: "View logs for runquill-demo-4826" })).toBeVisible();
    expect(await table.evaluate((element) => element.scrollWidth - element.clientWidth)).toBeLessThanOrEqual(0);
  });
});
