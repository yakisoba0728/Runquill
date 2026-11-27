# Runquill panel v2 (source)

The rebuilt operations panel: React 19, TypeScript and [Stratum](https://www.npmjs.com/package/@yakihyuk0728/stratum-ui) `0.1.1`, built with Vite. The design and its phases are in [docs/design/panel-v2.md](../../../docs/design/panel-v2.md). Phase 1 covers the shell, sign-in, live updates, routing, translations, the command palette and the Overview screen. The other screens show a placeholder.

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
```

The build is byte-identical across runs and across macOS arm64, Linux arm64 and Linux amd64 with the same lockfile and Node version. `go build` never needs Node, because the output is committed. Embedding and serving it behind `RUNQUILL_PANEL_V2=1` is the next (Go) step. `.gitattributes` marks the directory as generated.

## Output

```
internal/webui/panelv2/
  index.html                  <body data-mode="demo">; one <script src> per file, no inline script or style
  theme.js                    public/theme.js as is: applies the saved theme before the first paint
  assets/panel-<hash>.js      entry: shell, sign-in, live store, router (loads every other chunk with import(`./…`))
  assets/panel-<hash>.css     stratum.css + src/panel.css
  assets/chunk-<hash>.js      lazy: each screen, the Korean catalogue, the server message templates (screens import the entry back)
  assets/demo-<hash>.js       lazy, sample panel only: everything under src/demo/
```

Every file in `assets/` matches `^(panel|chunk|demo)-[A-Za-z0-9_-]+\.(js|css)$`, and no name starts with `_` or `.` (a directory `go:embed` drops those). The sample fixtures are grouped into `demo-*` by module path (`src/demo/`). Code in `src/demo/` imports only types from the rest of `src`, so no shared code ends up in that chunk.

## What the server must serve

These routes apply when `RUNQUILL_PANEL_V2=1`. Otherwise the server keeps the v1 routes unchanged.

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
- `src/components/`: local stand-ins for parts Stratum does not have yet (design §8).
- `src/screens/<id>/`: one lazy chunk per screen.
- `src/demo/`: sample scenarios, built into `demo-*` only.
