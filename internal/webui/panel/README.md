# Runquill panel v2 (source)

The rebuilt operations panel: React 19, TypeScript and [Stratum](https://www.npmjs.com/package/@yakihyuk0728/stratum-ui) `0.1.1`, built with Vite. The design and its phases are in [docs/design/panel-v2.md](../../../docs/design/panel-v2.md). Phase 1 covers the shell, sign-in, live updates, routing, translations, the command palette and the Overview screen. Phase 2 adds the Jobs, Runners, Hosts and Usage screens. Phase 3 adds the operator and admin actions on them: job retry and cancel, the repository check, pausing a host, worker enrollment and the runner Logs tab. Phase 4 adds the administration screens (Settings, Team, Audit log, Backups, Workflow migration) and API tokens. Phase 5 makes v2 the default panel.

## Build

Use the Node version in `.node-version` (26.13.1).

```bash
cd internal/webui/panel
npm ci
npm run dev          # sample panel at http://127.0.0.1:5173
RUNQUILL_PANEL_DEV_TARGET=http://127.0.0.1:8081 npm run dev   # proxies /api, /snapshot and /auth to a local manager
npm run typecheck && npm test
npm run build        # writes ../panelv2/, commit the result
npm run size         # gzip budget, design §6.8
npx playwright install  # on Ubuntu, add --with-deps
npx playwright test     # Chromium, Firefox and WebKit against the Go demo server
npm run e2e             # Chromium only
```

The e2e tests use the committed `panelv2/` build that the Go server embeds, so run `npm run build` first. CI runs all three engines with the pinned Playwright release and caches their binaries. Stop any existing server on port 18181 before testing. Live-mode tests keep the server's page and CSP but answer `/api/*` and `/snapshot` themselves (`e2e/live.ts`): a real manager session needs a GitHub sign-in.

The build is byte-identical across runs and across macOS arm64, Linux arm64 and Linux amd64 with the same lockfile and Node version. `go build` never needs Node, because the output is committed. From the repository root, run `go run . panel --demo` to try the embedded sample panel. `.gitattributes` marks the directory as generated.

## Output

```
internal/webui/panelv2/
  index.html                  <body data-mode="demo">; one <script src> per file, no inline script or style
  theme.js                    public/theme.js as is: applies the saved theme before the first paint
  assets/panel-<hash>.js      entry: shell, sign-in, live store, router and everything they import statically; no
                              static imports of its own (loads the screens with import(`./…`))
  assets/panel-<hash>.css     stratum.css + src/panel.css
  assets/chunk-<hash>.js      lazy: each screen, the Korean catalogue, the server message templates, and code only
                              screens share
  assets/demo-<hash>.js       lazy, sample panel only: everything under src/demo/
```

The `$initial` group in `vite.config.ts` keeps the entry whole (design §7.7). Every file in `assets/` matches `^(panel|chunk|demo)-[A-Za-z0-9_-]+\.(js|css)$`, and no name starts with `_` or `.` (a directory `go:embed` drops those). The sample fixtures are grouped into `demo-*` by module path (`src/demo/`). Code in `src/demo/` imports only types from the rest of `src`, so no shared code ends up in that chunk.

## What the server must serve

The manager and demo serve these v2 routes by default. The deprecated `RUNQUILL_PANEL_V2=0` (or `false`) opt-out selects v1 for one release.

| Route | Answer |
| --- | --- |
| `GET /` | `index.html`. A live panel replaces `data-mode="demo"` with `data-mode="live"`, as for v1. |
| `GET /theme.js` | `theme.js` |
| `GET /assets/{file}` | 400 unless the name matches the pattern above. A live panel answers 404 for `demo-*`; `panel --demo` serves it. |

Keep the same headers as v1. That includes the CSP `default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'` and `Cache-Control: no-store`. The build was checked in Chromium under exactly that policy, with no violations. The app calls the existing `/api/me`, `/api/logout`, `/api/events`, `/snapshot` and `./auth/github`, all relative to the page, so it also works under a sub-path such as GitHub Pages.

## Layout

- `src/main.tsx`: language, then the session or the sample panel, then render.
- `src/live.ts`: the SSE/polling state machine from v1, as a store for `useSyncExternalStore`.
- `src/router.ts`, `src/routes.ts`: hash routes, v1 address rewrites and the screen table.
- `src/i18n/`: `ko.json` and `server-patterns.json`, converted once from v1 `panelweb/i18n.js`. The keys are the English text. Add a translation for every new English string. A test checks the literals in `src`. While v1 lives, server messages need a translation in both places.
- `src/request.ts`: `useRequest` for screen REST loads, with optional polling. `src/commands.ts`: palette commands a screen (and its open pane) registers.
- `src/session.ts`: `can(role)`, always false in the sample panel. `src/actions.ts`: `send()`/`post()` for writes (failures, 403 included, as a toast), `useBusy()` for a button's pending state and `saveFile()`. `src/dialogs.tsx`: `confirm()`.
- `src/router.ts` also holds the leave guard (`useLeaveGuard`): the settings screen blocks leaving with unsaved changes and its SaveBar offers "Discard and leave".
- `src/components/`: local stand-ins for parts Stratum does not have yet (design §8). `index.tsx` is in the entry chunk; parts only screens use (`StageBar`, `ResourceMeters`, `BarChart`, `CopyField`, `RowMenu`) have their own files. `RowMenu` is the row action menu of the waiting tab and the hosts table (Stratum has no row action column yet). The log viewer is `src/screens/runners/RunnerLogs.tsx`.
- `src/screens/<id>/`: one lazy chunk per screen. Rules worth testing on their own live beside the screen (`settings/draft.ts`, `migrate/mapping.ts`).
- `src/demo/`: sample scenarios and the GET fixtures (`fixtures.ts`, runner logs included), built into `demo-*` only. `main.tsx` registers `answer` with `answerSamples` in `api.ts`, so the sample panel never reaches a server.
