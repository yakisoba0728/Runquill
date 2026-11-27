// Browser tests (design §7.2) against the real Go server: `runquill panel --demo` serves the committed panelv2/ build
// with the real CSP. Live-mode tests intercept /, /api/* and /snapshot (e2e/live.ts). Run `npm run build` first.
// `npm run e2e` runs Chromium (not in CI yet, design §7.5); `npx playwright test` runs Firefox and WebKit too.
import { defineConfig, devices } from "@playwright/test";

const port = 18181;

export default defineConfig({
  testDir: "e2e",
  testMatch: /\.e2e\.ts$/,
  fullyParallel: true,
  reporter: "list",
  use: { baseURL: `http://127.0.0.1:${port}/`, trace: "off" },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
    { name: "firefox", use: { ...devices["Desktop Firefox"] } },
    { name: "webkit", use: { ...devices["Desktop Safari"] } },
  ],
  webServer: {
    command: `go run . panel --demo --port ${port}`,
    cwd: "../../..",
    env: { RUNQUILL_PANEL_V2: "1" },
    url: `http://127.0.0.1:${port}/`,
    timeout: 180_000,
    reuseExistingServer: !process.env.CI,
  },
});
