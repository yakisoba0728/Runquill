// Runquill panel shell: sign-in, navigation, hash router, theme, live data.
//
// VIEW CONTRACT
// Each screen is panelweb/views/<id>.js with a default export:
//
//   export default {
//     id: "queue",                 // same as its route: #/queue
//     title: "Queue",              // English i18n key, shown as the page heading
//     mount(root, ctx) {},         // build the screen inside root (a <section>)
//     update(snapshot) {},         // optional: each new fleet snapshot while mounted
//     unmount() {},                // optional: stop timers and requests
//     params(search) {},           // optional: deep links (see below)
//     commands(ctx) { return []; },// optional: extra command palette entries
//   };
//
// Register it in `routes` below (id, title, group, minimum role, whether the
// sample panel shows it, whether it has fixtures). The nav entry comes from
// that list. ctx provides:
//   ctx.demo        true in the sample panel (no server; never opens EventSource)
//   ctx.user        { login, role } when signed in, null in the sample panel
//   ctx.can(role)   true when the user has at least that role (always false in
//                   the sample panel, so write controls stay hidden there)
//   ctx.snapshot()  the latest fleet snapshot, or null before the first one
//   ctx.api         get/post/put/del(path, data) and download(path, type, onProgress);
//                   use relative paths such as "./api/usage". Errors are ApiError
//                   with .status and a translated .message. In the sample panel,
//                   GETs are answered by the view's fixtures, panelweb/demo/<id>.js,
//                   whose default export maps "GET /api/..." to (query, body) => data;
//                   everything else is refused.
//   ctx.refresh()   fetch a fresh snapshot now (after a change)
//   ctx.toast(text, state), ctx.confirm(text, confirmLabel), ctx.navigate(id, params)
//   ctx.params()    URLSearchParams of the hash query, e.g. #/hosts?host=local
//
// Deep links: a route may carry a query (#/queue?job=4828). The command
// palette opens snapshot items that way: runners as runners?runner=NAME,
// waiting jobs as queue?job=ID and hosts as hosts?host=NAME. A view that
// defines params(search) receives the query after mount and whenever only the
// query changes; it should focus or filter to that item. A view without
// params() is opened as is and the query is removed from the address.
//
// Commands: commands(ctx) returns [{ label, run, values }] while the view is
// mounted. label is an English i18n key (translated by the palette, matched
// in both languages) whose {placeholders} come from values; run() is called
// after the palette closes. Use it for actions a
// keyboard user would otherwise have to tab to, and respect ctx.can().
// Files under views/ and demo/ are served without Go changes; a new top-level
// module needs its own route in panel.go and api/openapi.yaml, so keep
// view-specific helpers in views/.
// Visible strings go through tr() from i18n.js; add each English sentence as a
// key with its Korean translation there. Build DOM with ui.js (textContent
// only; never innerHTML with server data).
import { createApi, onUnauthorized, request } from "./api.js";
import { pendingJob, summary } from "./fleet.js";
import { language, localizeDocument, serverText, tr } from "./i18n.js";
import { bindConfirmDialog, confirmAction, fill, format, h, roleLabel, toast } from "./ui.js";

const roles = { viewer: 1, operator: 2, admin: 3 };

const routes = [
  { id: "overview", title: "Overview", group: "Operations" },
  { id: "queue", title: "Queue", group: "Operations" },
  { id: "jobs", title: "Jobs", group: "Operations", fixtures: true },
  { id: "runners", title: "Runners", group: "Operations" },
  { id: "hosts", title: "Hosts & pools", group: "Operations", fixtures: true },
  { id: "usage", title: "Usage", group: "Operations", fixtures: true },
  { id: "logs", title: "Logs", group: "Operations", fixtures: true },
  { id: "migrate", title: "Migrate", group: "Administration", role: "admin", fixtures: true },
  { id: "backups", title: "Backups", group: "Administration", role: "admin", fixtures: true },
  { id: "audit", title: "Audit", group: "Administration", role: "admin", demo: false },
  { id: "team", title: "Team & tokens", group: "Administration", demo: false },
  { id: "settings", title: "Settings", group: "Administration", role: "admin", demo: false },
];

const demo = document.body.dataset.mode === "demo";
const $ = (id) => document.getElementById(id);
let user = null;
let current = null;
let active = null; // { route, view }
let navigation = 0;

localizeDocument();
bindConfirmDialog();

// Theme: system, light or dark, saved per browser.
function setTheme(value) {
  document.documentElement.dataset.theme = value;
  for (const other of document.querySelectorAll("[data-theme-control]")) other.value = value;
  try { localStorage.setItem("runquill-theme", value); } catch {}
}

for (const control of document.querySelectorAll("[data-theme-control]")) {
  control.value = document.documentElement.dataset.theme || "system";
  control.addEventListener("change", () => setTheme(control.value));
}

function languageLinks() {
  const target = new URL(location.href);
  target.searchParams.set("lang", language === "ko" ? "en" : "ko");
  for (const link of document.querySelectorAll(".language-toggle")) link.href = target;
}

$("skip").addEventListener("click", (event) => {
  event.preventDefault();
  $("main").focus();
});

// Navigation.

function allowed(route) {
  if (demo) return route.demo !== false;
  return !!user && roles[user.role] >= roles[route.role || "viewer"];
}

function buildNav() {
  const nav = $("nav");
  nav.replaceChildren();
  let group = null;
  for (const route of routes.filter(allowed)) {
    if (route.group !== group) {
      group = route.group;
      nav.append(h("h2", {}, tr(group)));
    }
    nav.append(h("a", { href: "#/" + route.id, dataset: { route: route.id } },
      h("span", {}, tr(route.title)), h("span", { class: "count", dataset: { count: route.id } })));
  }
  nav.append(h("a", { class: "nav-source", href: "https://github.com/yakisoba0728/Runquill", target: "_blank", rel: "noopener noreferrer" },
    tr("Source on GitHub")));
}

$("menu-toggle").addEventListener("click", () => {
  const open = $("menu-toggle").getAttribute("aria-expanded") !== "true";
  $("menu-toggle").setAttribute("aria-expanded", String(open));
  $("nav").dataset.open = String(open);
  if (open) $("nav").querySelector("a[aria-current]")?.focus();
});
// Esc closes the narrow-screen menu and returns focus to its button.
$("nav").addEventListener("keydown", (event) => {
  if (event.key !== "Escape" || $("nav").dataset.open !== "true") return;
  $("menu-toggle").setAttribute("aria-expanded", "false");
  $("nav").dataset.open = "false";
  $("menu-toggle").focus();
});

function hashQuery() {
  const at = location.hash.indexOf("?");
  return new URLSearchParams(at < 0 ? "" : location.hash.slice(at + 1));
}

// go opens a route, optionally with a deep-link query; the same address
// again still re-runs the router so the item is focused again.
function go(id, params) {
  const query = params ? new URLSearchParams(params).toString() : "";
  const hash = "#/" + id + (query ? "?" + query : "");
  if (location.hash === hash) void route(true);
  else location.hash = hash;
}

function context(route, fixtures) {
  return {
    demo,
    user,
    can: (role) => !demo && !!user && roles[user.role] >= roles[role],
    snapshot: () => current,
    api: createApi({ demo, fixtures }),
    refresh: () => source.refresh(),
    toast,
    confirm: confirmAction,
    navigate: (id, params) => go(id, params),
    params: hashQuery,
    route: route.id,
  };
}

async function route(focus) {
  languageLinks();
  if (!demo && !user) return; // signed out: the sign-in screen is showing
  const match = /^#\/([a-z-]+)/.exec(location.hash);
  if (location.hash && !match) return; // an in-page anchor, not a route
  const visible = routes.filter(allowed);
  const next = visible.find((candidate) => candidate.id === match?.[1]) || visible[0];
  if (!match || match[1] !== next.id) history.replaceState(null, "", "#/" + next.id);
  const params = hashQuery();
  for (const link of $("nav").querySelectorAll("a")) {
    if (link.dataset.route === next.id) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  $("menu-toggle").setAttribute("aria-expanded", "false");
  $("nav").dataset.open = "false";
  if (active?.route === next) return deepLink(params);
  const ticket = ++navigation;
  try { active?.view.unmount?.(); } catch (error) { console.error(error); }
  active = null;
  document.title = tr(next.title) + " · Runquill";
  let view, fixtures;
  try {
    view = (await import("./views/" + next.id + ".js")).default;
    if (demo && next.fixtures) fixtures = (await import("./demo/" + next.id + ".js")).default;
  } catch (error) {
    console.error(error);
    if (ticket === navigation) $("view").replaceChildren(h("p", { class: "placeholder" }, tr("This screen could not be loaded. Reload the page.")));
    return;
  }
  if (ticket !== navigation) return;
  const root = h("section", { class: "view-root", dataset: { view: next.id } });
  $("view").replaceChildren(root);
  const ctx = context(next, fixtures);
  active = { route: next, view, ctx };
  view.mount(root, ctx);
  view.update?.(current);
  if (focus) root.querySelector("[data-view-heading]")?.focus({ preventScroll: true });
  window.scrollTo(0, 0);
  deepLink(params);
}

// deepLink hands a hash query to the mounted view, or drops it from the
// address when the view does not take deep links.
function deepLink(params) {
  if (!active || ![...params.keys()].length) return;
  if (typeof active.view.params === "function") {
    try { active.view.params(params); } catch (error) { console.error(error); }
  } else {
    history.replaceState(null, "", "#/" + active.route.id);
  }
}

window.addEventListener("hashchange", () => void route(true));

// Live data. GET /api/events (SSE) is the primary source. If the stream
// cannot open (no EventSource, 401, 429 for too many tabs) or stays down,
// /snapshot is polled with exponential backoff and the stream is tried again
// later. A hidden tab holds no connection.

let connection = { state: "", label: "Connecting" };

function setConnection(state, label) {
  connection = { state, label };
  const target = $("connection-state");
  if (target.textContent !== tr(label)) target.textContent = tr(label);
  target.dataset.state = state;
  renderChrome();
}

const source = (() => {
  const interval = 3000;
  const maxInterval = 30000;
  let stream = null;
  let timer = null;
  let watchdog = 0;
  let failures = 0;
  let retryStreamAt = 0;
  let stopped = true;
  let retryAt = 0;

  function receive(snapshot) {
    current = snapshot;
    failures = 0;
    retryAt = 0;
    render();
  }

  function stop() {
    stopped = true;
    stream?.close();
    stream = null;
    clearTimeout(timer);
    clearTimeout(watchdog);
    watchdog = 0;
  }

  function start() {
    stop();
    stopped = false;
    if (document.hidden) return setConnection("offline", "Paused while hidden");
    if (typeof EventSource === "function" && Date.now() >= retryStreamAt) open();
    else void poll();
  }

  function open() {
    setConnection("", "Connecting");
    const events = new EventSource("./api/events");
    stream = events;
    events.addEventListener("open", () => {
      clearTimeout(watchdog);
      watchdog = 0;
      setConnection("ok", "Live");
    });
    events.addEventListener("snapshot", (event) => {
      try { receive(JSON.parse(event.data)); } catch (error) { console.error(error); }
      if (connection.label !== "Live") setConnection("ok", "Live");
    });
    events.addEventListener("error", () => {
      if (stream !== events) return;
      if (events.readyState === EventSource.CLOSED) return fallback();
      // The server ends each stream after ten minutes; EventSource reconnects
      // by itself. Only a reconnect that keeps failing counts as lost. Each
      // failed retry fires error again, so the deadline runs from the first.
      setConnection("waiting", "Reconnecting");
      if (watchdog) return;
      watchdog = setTimeout(() => {
        watchdog = 0;
        if (stream === events && events.readyState !== EventSource.OPEN) fallback();
      }, 10000);
    });
  }

  function fallback() {
    stream?.close();
    stream = null;
    clearTimeout(watchdog);
    watchdog = 0;
    retryStreamAt = Date.now() + 60000;
    void poll();
  }

  async function fetchSnapshot() {
    const response = await fetch("./snapshot", { cache: "no-store", signal: AbortSignal.timeout(5000) });
    if (response.status === 401) {
      stop();
      signedOut("Your session ended. Sign in again to view the fleet.");
      return null;
    }
    if (!response.ok) throw new Error("snapshot unavailable");
    return response.json();
  }

  async function poll() {
    clearTimeout(timer);
    if (stopped) return;
    try {
      const snapshot = await fetchSnapshot();
      if (!snapshot) return;
      receive(snapshot);
    } catch {
      failures++;
    }
    if (stopped || document.hidden) return;
    if (!failures && typeof EventSource === "function" && Date.now() >= retryStreamAt) return open();
    const delay = failures ? Math.min(maxInterval, interval * 2 ** failures) : interval;
    if (failures) {
      retryAt = Date.now() + delay;
      setConnection("failed", "Connection lost");
    } else {
      setConnection("ok", "Polling");
    }
    timer = setTimeout(poll, delay);
  }

  async function refresh() {
    if (demo || stopped) return;
    try {
      const snapshot = await fetchSnapshot();
      if (snapshot) receive(snapshot);
    } catch {}
  }

  document.addEventListener("visibilitychange", () => {
    if (demo || !user) return;
    if (document.hidden) {
      stop();
      setConnection("offline", "Paused while hidden");
    } else {
      start();
    }
  });

  return { start, stop, refresh, retrySeconds: () => retryAt ? Math.max(0, Math.ceil((retryAt - Date.now()) / 1000)) : 0 };
})();

// Chrome: connection age, notices and nav counts. Views render themselves.

function renderChrome() {
  const age = $("connection-age");
  const text = current?.sampled_at ? tr("Updated {time}", { time: language === "ko" ? format.relative(current.sampled_at) : format.relative(current.sampled_at).toLowerCase() }) : "";
  if (age.textContent !== text) age.textContent = text;

  // The lost-connection notice is not a live region: the top bar's status
  // already announces it, and its countdown would be read every second.
  const notices = [];
  const lost = connection.label === "Connection lost";
  if (current?.fleet?.docker_error) notices.push(["waiting", serverText(current.fleet.docker_error)]);
  if (current?.poll_failed) notices.push(["waiting", tr("The latest GitHub poll failed. Check the manager logs.")]);
  const box = $("notices");
  const key = JSON.stringify([lost, !!current, notices]);
  if (box.dataset.key !== key) {
    box.dataset.key = key;
    const countdown = lost ? h("p", { class: "notice", dataset: { state: "failed" } }, ...fill(current
      ? "Connection lost. Retrying in {n}s. Showing the last available snapshot." : "Connection lost. Retrying in {n}s.",
    { n: h("span", { dataset: { countdown: "" } }) })) : null;
    box.replaceChildren(countdown || "",
      ...notices.map(([tone, text]) => h("p", { class: "notice", dataset: { state: tone }, role: "status" }, text)));
  }
  const seconds = box.querySelector("[data-countdown]");
  if (seconds && seconds.textContent !== String(source.retrySeconds())) seconds.textContent = String(source.retrySeconds());
}

function setCount(id, value, label, tone = "") {
  const target = document.querySelector('[data-count="' + id + '"]');
  if (!target) return;
  target.replaceChildren(value ? String(value) : "", value ? h("span", { class: "visually-hidden" }, " " + label) : "");
  target.dataset.state = tone;
}

function render() {
  const counts = summary(current);
  setCount("queue", counts?.queued, tr("queued"), counts?.queued ? "waiting" : "");
  setCount("runners", counts?.running, tr("running"));
  const down = counts ? counts.hostsOffline + counts.hostsUnavailable : 0;
  setCount("hosts", down, tr("offline"), down ? "failed" : "");
  renderChrome();
  try { active?.view.update?.(current); } catch (error) { console.error(error); }
}

setInterval(renderChrome, 1000);

// Command palette: Cmd/Ctrl+K anywhere, or "/" outside text fields. A modal
// combobox over every screen, the shell's own actions, the mounted view's
// commands(ctx) and items from the latest snapshot (opened as deep links).
(() => {
  const dialog = $("palette");
  const input = $("palette-input");
  const list = $("palette-list");
  const status = $("palette-status");
  const limit = 50;
  let items = [];
  let shown = [];
  let index = 0;
  let opener = null;

  // Each entry is matched on its translated text and on the English key, so
  // "hosts" finds 호스트 및 풀 in the Korean panel as well.
  const english = (label, values = {}) => label.replace(/\{([a-z]+)\}/g, (token, key) => values[key] ?? token);
  const command = (label, kind, run, values = {}) => ({ text: tr(label, values), keys: [tr(label, values), english(label, values)], kind, run });
  const jump = (text, kind, id, params) => ({ text, keys: [text], kind, run: () => go(id, params), jump: true });

  function collect() {
    const result = [];
    for (const route of routes.filter(allowed)) {
      result.push({ ...command("Go to {screen}", "Screen", () => go(route.id), { screen: tr(route.title) }),
        keys: [tr("Go to {screen}", { screen: tr(route.title) }), english("Go to {screen}", { screen: route.title })] });
    }
    if (active && typeof active.view.commands === "function") {
      try {
        for (const entry of active.view.commands(active.ctx) || []) {
          if (entry?.label && typeof entry.run === "function") result.push(command(entry.label, active.route.title, entry.run, entry.values));
        }
      } catch (error) { console.error(error); }
    }
    result.push(command(language === "ko" ? "Switch to English" : "Switch to Korean", "Language", () => {
      const target = new URL(location.href);
      target.searchParams.set("lang", language === "ko" ? "en" : "ko");
      location.assign(target);
    }));
    for (const [value, label] of [["system", "Use the system theme"], ["light", "Use the light theme"], ["dark", "Use the dark theme"]]) {
      result.push(command(label, "Appearance", () => setTheme(value)));
    }
    if (!demo) {
      result.push(command("Refresh now", "Panel", () => source.refresh()));
      result.push(command("Sign out", "Panel", () => $("signout").click()));
    }
    const visible = new Set(routes.filter(allowed).map((route) => route.id));
    if (visible.has("runners")) {
      for (const runner of current?.runner_inventory || []) {
        if (runner.runtime_state === "running") result.push(jump(runner.name, "Runner", "runners", { runner: runner.name }));
      }
    }
    if (visible.has("queue")) {
      for (const job of (current?.fleet?.jobs || []).filter(pendingJob)) {
        result.push(jump(job.repository + " #" + job.id, "Waiting job", "queue", { job: String(job.id) }));
      }
    }
    if (visible.has("hosts")) {
      for (const host of current?.fleet?.hosts || []) result.push(jump(host.name, "Host", "hosts", { host: host.name }));
    }
    return result;
  }

  // score ranks a match: 0 prefix, 1 word start, 2 substring, 3 letters in
  // order (subsequence), -1 none.
  function score(query, text) {
    const value = String(text).toLowerCase();
    const at = value.indexOf(query);
    if (at === 0) return 0;
    if (at > 0) return /[\s/#._-]/.test(value[at - 1]) ? 1 : 2;
    let position = 0;
    for (const letter of query.replace(/\s+/g, "")) {
      position = value.indexOf(letter, position);
      if (position < 0) return -1;
      position++;
    }
    return 3;
  }

  function filter() {
    const query = input.value.trim().toLowerCase();
    let matches;
    if (!query) {
      matches = items.filter((item) => !item.jump);
    } else {
      matches = items.map((item, order) => {
        const ranks = item.keys.map((key) => score(query, key)).filter((rank) => rank >= 0);
        return { item, order, rank: ranks.length ? Math.min(...ranks) : -1 };
      }).filter((entry) => entry.rank >= 0)
        .sort((a, b) => a.rank - b.rank || a.order - b.order).map((entry) => entry.item);
    }
    shown = matches.slice(0, limit);
    list.replaceChildren(...shown.map((item, i) => h("li", {
      id: "palette-option-" + i, role: "option", "aria-selected": "false", dataset: { index: String(i) },
    }, h("span", { class: "palette-label" }, item.text), h("span", { class: "palette-kind" }, tr(item.kind)))));
    status.textContent = !matches.length ? tr("No matching commands.")
      : matches.length > limit ? tr("{shown} of {n} results. Keep typing to narrow them.", { shown: limit, n: matches.length })
        : tr(matches.length === 1 ? "1 result" : "{n} results", { n: matches.length });
    highlight(0);
  }

  function highlight(next) {
    if (!shown.length) {
      input.removeAttribute("aria-activedescendant");
      return;
    }
    index = (next + shown.length) % shown.length;
    for (const option of list.children) {
      if (option.dataset.index) option.setAttribute("aria-selected", String(Number(option.dataset.index) === index));
    }
    const option = $("palette-option-" + index);
    input.setAttribute("aria-activedescendant", option.id);
    option.scrollIntoView({ block: "nearest" });
  }

  function close() {
    if (!dialog.open) return;
    dialog.close();
    input.removeAttribute("aria-activedescendant");
    const target = opener;
    opener = null;
    if (target?.isConnected && target !== document.body) target.focus({ preventScroll: true });
  }

  function choose(i) {
    const item = shown[i];
    if (!item) return;
    close();
    try {
      const result = item.run();
      if (result?.catch) result.catch((error) => console.error(error));
    } catch (error) {
      console.error(error);
    }
  }

  function open() {
    if (dialog.open || $("shell").hidden || $("confirm-dialog").open) return;
    opener = document.activeElement;
    items = collect();
    input.value = "";
    dialog.showModal();
    input.focus();
    filter();
  }

  input.addEventListener("input", filter);
  input.addEventListener("keydown", (event) => {
    if (event.isComposing) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      highlight(index + (event.key === "ArrowDown" ? 1 : -1));
    } else if (event.key === "PageDown" || event.key === "PageUp") {
      event.preventDefault();
      highlight(Math.max(0, Math.min(shown.length - 1, index + (event.key === "PageDown" ? 8 : -8))));
    } else if (event.key === "Enter") {
      event.preventDefault();
      choose(index);
    } else if (event.key === "Tab") {
      event.preventDefault(); // the field is the dialog's only stop
    }
  });
  list.addEventListener("mousedown", (event) => event.preventDefault()); // keep focus in the field
  list.addEventListener("click", (event) => {
    const option = event.target.closest("[role=option]");
    if (option) choose(Number(option.dataset.index));
  });
  list.addEventListener("mousemove", (event) => {
    const option = event.target.closest("[role=option]");
    if (option && Number(option.dataset.index) !== index) highlight(Number(option.dataset.index));
  });
  dialog.addEventListener("cancel", (event) => {
    event.preventDefault();
    close();
  });
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) close(); // the backdrop
  });

  const mac = /mac|iphone|ipad/i.test(navigator.userAgentData?.platform || navigator.platform || "");
  $("palette-key").textContent = mac ? "⌘K" : "Ctrl K";
  $("palette-open").setAttribute("aria-keyshortcuts", mac ? "Meta+K /" : "Control+K /");
  $("palette-open").addEventListener("click", open);
  document.addEventListener("keydown", (event) => {
    if (event.defaultPrevented || event.isComposing) return;
    if ((event.metaKey || event.ctrlKey) && !event.altKey && !event.shiftKey && event.key.toLowerCase() === "k") {
      event.preventDefault();
      if (dialog.open) close();
      else open();
      return;
    }
    if (event.key !== "/" || event.metaKey || event.ctrlKey || event.altKey || dialog.open) return;
    const target = event.target;
    if (target instanceof Element && target.closest("input, textarea, select, [contenteditable], dialog")) return;
    event.preventDefault();
    open();
  });
})();

// Sign-in and session.

function signedOut(error) {
  source.stop();
  const wasSignedIn = !!user;
  user = null;
  active?.view.unmount?.();
  active = null;
  $("view").replaceChildren();
  $("shell").hidden = true;
  $("sign-in").hidden = false;
  $("signin-host").textContent = location.host;
  document.title = tr("Sign in to Runquill");
  const note = $("signin-message");
  note.textContent = error ? tr(error) : "";
  note.hidden = !error;
  if (wasSignedIn) $("signin-title").focus();
}

onUnauthorized(() => signedOut("Your session ended. Sign in again to view the fleet."));

$("signout").addEventListener("click", async () => {
  $("signout").disabled = true;
  try {
    await request("./api/logout", { method: "POST" });
    window.location.assign("./");
  } catch (error) {
    $("signout").disabled = false;
    if (error.status !== 401) toast(error.message, "failed");
  }
});

async function startDemo() {
  const { sample } = await import("./demo.js");
  $("demo-bar").hidden = false;
  setConnection("", "Sample data");
  const scenario = (name) => {
    const next = sample(name);
    $("scenario-note").textContent = tr(next.note);
    current = next.snapshot;
    render();
    return next;
  };
  // Switching scenario opens the job it is about, as a deep link into the
  // queue, so the change is visible without hunting for it.
  $("scenario").addEventListener("change", () => {
    const next = scenario($("scenario").value);
    if (next.focus) go(next.focus.route, next.focus.params);
  });
  scenario($("scenario").value);
}

async function startLive() {
  try {
    user = await request("./api/me");
  } catch (error) {
    if (error.status !== 401) signedOut("The panel could not check your session. Try again later.");
    else signedOut("");
    return false;
  }
  $("who").replaceChildren(h("strong", {}, "@" + user.login), " ", roleLabel(user.role));
  $("who").hidden = false;
  $("signout").hidden = false;
  source.start();
  return true;
}

if (demo || await startLive()) {
  $("shell").hidden = false;
  if (demo) await startDemo();
  buildNav();
  render();
  await route(false);
}
