import { defineConfig, type Plugin } from "vitest/config";
import react from "@vitejs/plugin-react";

// Dev server only: a local manager panel to proxy to, e.g. http://127.0.0.1:8081.
const target = process.env.RUNQUILL_PANEL_DEV_TARGET;

// Serves index.html in live mode when proxying to a manager, as the manager itself does.
const liveMode = (): Plugin => ({
  name: "runquill-live-mode",
  apply: "serve",
  transformIndexHtml: (html) => (target ? html.replace('data-mode="demo"', 'data-mode="live"') : html),
});

export default defineConfig({
  base: "./", // GitHub Pages sub-path and the manager root alike
  plugins: [react(), liveMode()],
  build: {
    // Phase 1-5 output; panelweb/ is the v1 panel and must survive emptyOutDir.
    outDir: "../panelv2",
    emptyOutDir: true,
    target: "es2023",
    modulePreload: { polyfill: false }, // no inline script under the panel CSP
    sourcemap: false,
    rolldownOptions: {
      output: {
        // Fixed prefixes: go:embed drops names starting with _ or ., and the live server refuses demo-*.
        entryFileNames: "assets/panel-[hash].js",
        // Everything under src/demo/ (fixtures, scenarios) goes to demo-* only, by module path. Everything the entry
        // imports statically ($initial) stays in the entry: without that group Rolldown 1.3 moves the modules the entry
        // shares with screen chunks (React, the shell, Stratum internals) into common chunks the entry imports, which cost
        // 3 KB gzip on the first screen (design §7.7). The screens import the entry, as Rollup builds do.
        codeSplitting: { groups: [{ name: "demo", test: /[\\/]src[\\/]demo[\\/]/ }, { name: "initial", tags: ["$initial"], priority: -1 }] },
        chunkFileNames: (chunk) => (chunk.name === "demo" ? "assets/demo-[hash].js" : "assets/chunk-[hash].js"),
        assetFileNames: "assets/panel-[hash][extname]",
      },
    },
  },
  server: target
    ? {
        proxy: Object.fromEntries(["/api", "/snapshot", "/auth"].map((path) => [path, {
          target, changeOrigin: true, headers: { origin: target }, // pass the manager's Host and Origin checks
        }])),
      }
    : undefined,
  test: { environment: "jsdom" },
});
