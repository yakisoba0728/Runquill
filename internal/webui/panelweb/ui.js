// Shared DOM helpers for views. Everything writes text with textContent;
// nothing here parses HTML. Labels are translated by the caller with tr().
import { ApiError } from "./api.js";
import { language, tr } from "./i18n.js";

// h builds an element. props: class, text, dataset, on (event handlers) and
// any attribute or DOM property. Children are nodes or plain strings.
export function h(tag, props = {}, ...children) {
  const element = document.createElement(tag);
  for (const [name, value] of Object.entries(props || {})) {
    if (value == null || value === false) continue;
    if (name === "class") element.className = value;
    else if (name === "text") element.textContent = value;
    else if (name === "dataset") Object.assign(element.dataset, value);
    else if (name === "on") for (const [event, handler] of Object.entries(value)) element.addEventListener(event, handler);
    else if (name in element && !name.includes("-")) element[name] = value;
    else element.setAttribute(name, value === true ? "" : value);
  }
  for (const child of children.flat()) {
    if (child == null || child === false) continue;
    element.append(child instanceof Node ? child : String(child));
  }
  return element;
}

// Formatting. Dates accept ISO strings or Unix seconds.

function toDate(value) {
  if (value == null || value === "") return null;
  const date = typeof value === "number" ? new Date(value < 1e12 ? value * 1000 : value) : new Date(value);
  return Number.isNaN(date.getTime()) ? null : date;
}

export const format = {
  number: (value) => Number.isFinite(value) ? value.toLocaleString(language) : "—",
  duration(seconds) {
    if (!Number.isFinite(seconds)) return "—";
    seconds = Math.max(0, Math.round(seconds));
    const minutes = Math.floor(seconds / 60);
    const hours = Math.floor(minutes / 60);
    if (hours) return minutes % 60 ? tr("{hours}h {minutes}m", { hours, minutes: minutes % 60 }) : tr("{hours}h", { hours });
    if (minutes) return tr("{minutes}m {seconds}s", { minutes, seconds: seconds % 60 });
    return tr("{seconds}s", { seconds });
  },
  bytes(bytes) {
    if (!Number.isFinite(bytes)) return "—";
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    let unit = 0;
    while (bytes >= 1024 && unit < units.length - 1) { bytes /= 1024; unit++; }
    return bytes.toLocaleString(language, { maximumFractionDigits: unit ? 1 : 0 }) + " " + units[unit];
  },
  time(value) {
    const date = toDate(value);
    return date ? date.toLocaleTimeString(language, { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "—";
  },
  date(value) {
    const date = toDate(value);
    return date ? date.toLocaleString(language, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "—";
  },
  day(value) {
    const date = toDate(value);
    return date ? date.toLocaleDateString(language, { year: "numeric", month: "short", day: "numeric" }) : "—";
  },
  relative(value) {
    const date = toDate(value);
    if (!date) return tr("Not observed");
    const seconds = Math.max(0, Math.floor((Date.now() - date.getTime()) / 1000));
    if (seconds < 5) return tr("Just now");
    if (seconds < 60) return tr("{n}s ago", { n: seconds });
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return tr("{n}m ago", { n: minutes });
    const hours = Math.floor(minutes / 60);
    return hours < 24 ? tr("{n}h ago", { n: hours }) : tr("{n}d ago", { n: Math.floor(hours / 24) });
  },
};

// time renders a <time> element with a machine-readable value.
export function time(value, style = "date") {
  const date = toDate(value);
  if (!date) return "—";
  return h("time", { dateTime: date.toISOString(), title: date.toLocaleString(language) }, format[style](value));
}

// State labels. Colour signals state only: ok, waiting, failed, offline;
// anything else is neutral.
const states = {
  ok: ["RUNNING", "UP", "CLEANED", "FINISHED", "DONE"],
  waiting: ["WAITING", "RETRY", "CORDONED", "CLEANUP", "STARTING", "PAUSED"],
  failed: ["FAILED", "UNAVAILABLE"],
  offline: ["OFFLINE"],
};

export function stateOf(status) {
  for (const [state, list] of Object.entries(states)) if (list.includes(status)) return state;
  return "";
}

// English labels for the status codes in snapshots and job history.
const statusLabels = {
  RUNNING: "Running", UP: "Ready", CLEANED: "Cleaned up", FINISHED: "Finished", DONE: "Done",
  WAITING: "Waiting", RETRY: "Retry scheduled", CORDONED: "On maintenance", CLEANUP: "Cleaning up",
  STARTING: "Starting", PAUSED: "Paused", FAILED: "Failed", UNAVAILABLE: "Unavailable", OFFLINE: "Offline",
  UNKNOWN: "Unknown",
};

// state renders a marker and a translated label. label defaults to the
// status's English name.
export function state(status, label = statusLabels[status] || status, tone = stateOf(status)) {
  return h("span", { class: "state", dataset: { state: tone } }, tr(label));
}

export function roleLabel(role) {
  return tr({ viewer: "Viewer", operator: "Operator", admin: "Admin" }[role] || role);
}

export function isGitHubJobURL(url) {
  return typeof url === "string" && /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/actions\/runs\/\d+\/job\/\d+$/.test(url);
}

export function externalLink(text, url, label) {
  return h("a", { href: url, target: "_blank", rel: "noopener noreferrer", "aria-label": label }, text);
}

// Layout pieces.

export function pageHead(title, ...actions) {
  return h("header", { class: "page-head" },
    h("h1", { tabindex: "-1", "data-view-heading": true }, tr(title)),
    actions.length ? h("div", { class: "actions" }, ...actions) : null);
}

// block returns a titled section. Its heading id labels the section.
let blockCount = 0;
export function block(title, { actions = [], note } = {}, ...content) {
  const id = "block-" + ++blockCount;
  const head = h("div", { class: "block-head" },
    h("div", {}, h("h2", { id }, tr(title)), note ? h("p", {}, tr(note)) : null),
    actions.length ? h("div", { class: "actions" }, ...actions) : null);
  return h("section", { class: "block", "aria-labelledby": id }, head, ...content);
}

// kv renders a definition list from [label, value, note?] rows; value may be
// a node. wide puts values on the left for prose.
export function kv(rows, { wide = false, label } = {}) {
  return h("dl", { class: wide ? "kv wide" : "kv", "aria-label": label ? tr(label) : null },
    ...rows.filter(Boolean).map(([term, value, note]) => h("div", {},
      h("dt", {}, tr(term)),
      h("dd", {}, value instanceof Node ? value : String(value ?? "—"), note ? h("span", { class: "sub" }, note) : null))));
}

// createKv builds a definition list once and updates single values in place,
// so periodic snapshots never rebuild focusable content.
// rows: [[key, label]]. set(key, value, note) accepts text or a node;
// label(key, text) renames a row; show(key, visible) hides one.
export function createKv(rows, { wide = false, label } = {}) {
  const parts = new Map();
  const element = h("dl", { class: wide ? "kv wide" : "kv", "aria-label": label ? tr(label) : null });
  for (const [key, term] of rows) {
    const dt = h("dt", {}, tr(term));
    const dd = h("dd", {}, "—");
    const row = h("div", {}, dt, dd);
    parts.set(key, { row, dt, dd, key: "" });
    element.append(row);
  }
  return {
    element,
    set(key, value, note) {
      const part = parts.get(key);
      const nodes = [value instanceof Node ? value : String(value ?? "—"), note ? h("span", { class: "sub" }, note) : null];
      const signature = nodes.map((node) => node instanceof Node ? node.outerHTML ?? node.textContent : node ?? "").join("\u0000");
      if (part.key === signature) return;
      part.key = signature;
      part.dd.replaceChildren(...nodes.filter((node) => node != null));
    },
    label(key, text) { parts.get(key).dt.textContent = tr(text); },
    show(key, visible) { parts.get(key).row.hidden = !visible; },
  };
}

// fill translates a template and replaces {name} placeholders with nodes,
// for sentences that contain code or links.
export function fill(template, nodes) {
  return tr(template).split(/(\{[a-z]+\})/).map((part) => {
    const name = /^\{([a-z]+)\}$/.exec(part)?.[1];
    return name && nodes[name] ? nodes[name] : part;
  }).filter((part) => part !== "");
}

export function field(label, control, { grow = false, className = "" } = {}) {
  return h("label", { class: ["field", grow ? "grow" : "", className].filter(Boolean).join(" ") }, h("span", {}, tr(label)), control);
}

export function button(label, onClick, { primary = false, small = false, type = "button", ...props } = {}) {
  return h("button", {
    class: ["button", primary ? "primary" : "", small ? "small" : ""].filter(Boolean).join(" "),
    type, on: onClick ? { click: onClick } : null, ...props,
  }, tr(label));
}

export function select(name, options, props = {}) {
  return h("select", { name, ...props }, ...options.map(([value, label]) => h("option", { value }, tr(label))));
}

export function placeholder(title, text = "This screen is not available yet.") {
  return [pageHead(title), h("p", { class: "placeholder" }, tr(text))];
}

// createTable builds a real table with a header row and returns update(rows),
// which re-renders only rows whose content changed, so text selection, hover,
// in-flight button states and keyboard focus survive each snapshot.
// columns: [{ label, num, className, cell(row) }]; key(row) identifies a row.
// One column's cells are row headers (the first, or the column marked
// rowHeader), so screen readers announce each value with its row. highlighted() names the key of a row a deep link
// points at; it is marked and focusable, and focus(key) moves there.
export function createTable({ label, columns, key, empty = "No entries.", rowClass, highlighted }) {
  const body = h("tbody");
  const caption = h("caption", { class: "visually-hidden" }, tr(label));
  const head = h("thead", {}, h("tr", {}, ...columns.map((column) =>
    h("th", { scope: "col", class: column.num ? "num" : null }, column.label ? tr(column.label) : h("span", { class: "visually-hidden" }, tr("Actions"))))));
  const element = h("div", { class: "table-wrap", role: "region", tabindex: "0", "aria-label": tr(label) },
    h("table", {}, caption, head, body));
  const controls = "button, a, input, select";
  const headerColumn = Math.max(0, columns.findIndex((column) => column.rowHeader));
  // The markup a row was built with, by key; a row built the same way again keeps its live node.
  let built = new Map();
  function update(rows, emptyText = empty) {
    const active = document.activeElement;
    let restore = null;
    if (active && body.contains(active)) {
      const row = active.closest("tr");
      restore = { key: row?.dataset.key, index: active === row ? -1 : [...row.querySelectorAll(controls)].indexOf(active) };
    }
    const target = highlighted?.();
    const previous = built;
    built = new Map();
    const nodes = rows.map((row) => {
      const id = String(key(row));
      const mark = target != null && String(target) === id;
      const tr_ = h("tr", {
        dataset: { key: id }, class: [rowClass?.(row) || "", mark ? "highlight" : ""].filter(Boolean).join(" ") || null,
        tabindex: mark ? "-1" : null, "aria-current": mark ? "true" : null,
      });
      columns.forEach((column, index) => {
        const value = column.cell(row);
        const header = index === headerColumn;
        tr_.append(h(header ? "th" : "td", {
          scope: header ? "row" : null,
          class: [column.num ? "num" : "", column.className || ""].filter(Boolean).join(" ") || null,
        }, ...(Array.isArray(value) ? value : [value ?? "—"])));
      });
      // Control values set as properties (a select's value) are not in the markup.
      const markup = tr_.outerHTML + [...tr_.querySelectorAll("input, select")].map((control) => "\u0000" + control.value + control.checked).join("");
      const kept = previous.get(id);
      const node = kept?.markup === markup && !built.has(id) ? kept.node : tr_;
      built.set(id, { markup, node });
      return node;
    });
    if (!rows.length) {
      const text = tr(emptyText);
      const kept = previous.get("\u0000empty");
      const node = kept?.markup === text ? kept.node : h("tr", { class: "empty-row" }, h("td", { colSpan: columns.length }, text));
      built.set("\u0000empty", { markup: text, node });
      nodes.push(node);
    }
    // Only rows that changed are replaced; unchanged rows stay where they are.
    for (const node of [...body.children]) if (!nodes.includes(node)) node.remove();
    nodes.forEach((node, index) => { if (body.children[index] !== node) body.insertBefore(node, body.children[index] || null); });
    if (restore?.key && !body.contains(document.activeElement)) {
      const row = [...body.rows].find((candidate) => candidate.dataset.key === restore.key);
      const next = restore.index < 0 ? row : row?.querySelectorAll(controls)[restore.index];
      if (next?.matches("[tabindex], " + controls)) next.focus({ preventScroll: true });
    }
  }
  // focus moves keyboard focus to a highlighted row and scrolls it into view.
  function focus(id) {
    const row = [...body.rows].find((candidate) => candidate.dataset.key === String(id));
    if (!row) return false;
    if (!row.hasAttribute("tabindex")) row.tabIndex = -1;
    row.focus({ preventScroll: true });
    row.scrollIntoView({ block: "center", behavior: reducedMotion() ? "auto" : "smooth" });
    return true;
  }
  return { element, update, focus };
}

// linkTarget follows the item a deep link names, such as #/runners?runner=X.
// open(search) reads it; after each render, settle() focuses its row once
// the data has loaded, or, when the item is not there, drops the query from
// the address and calls gone(key) so the view can say so.
export function linkTarget(route, param) {
  let key = null;
  let pending = false;
  const forget = () => {
    key = null;
    pending = false;
    if (location.hash.startsWith("#/" + route + "?")) history.replaceState(null, "", "#/" + route);
  };
  return {
    key: () => key,
    open(search) {
      key = search.get(param) || null;
      pending = !!key;
    },
    settle({ ready, found, table, gone }) {
      if (!key || !ready) return;
      if (!found) {
        const missing = key;
        forget();
        gone?.(missing);
        return;
      }
      if (!pending) return;
      pending = false;
      const target = key;
      // After the router has focused the page heading.
      setTimeout(() => table.focus(target), 0);
    },
    forget,
  };
}

export function reducedMotion() {
  return window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;
}

// loadStyle adds views/<id>.css to the page once, for a screen's own rules.
export function loadStyle(id) {
  const href = "./views/" + id + ".css";
  if (document.querySelector('link[rel="stylesheet"][href="' + href + '"]')) return;
  document.head.append(h("link", { rel: "stylesheet", href }));
}

// intro is the short explanation under a page heading: an English key, or
// nodes already translated (such as the result of fill()).
export function intro(...content) {
  const single = content.length === 1 && typeof content[0] === "string";
  return h("p", { class: "page-intro muted" }, ...(single ? [tr(content[0])] : content));
}

// statusLine is an inline result line for a form or a block, announced
// politely when it changes. Fill it with message().
export function statusLine(props = {}) {
  return h("p", { class: "inline-message", role: "status", "aria-live": "polite", hidden: true, ...props });
}

// saveFile offers a Blob to the browser as a download named filename.
export function saveFile(blob, filename) {
  const url = URL.createObjectURL(blob);
  const link = h("a", { href: url, download: filename, hidden: true });
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

// Feedback.

// errorText gives a translated message for any thrown value.
export function errorText(error) {
  return error instanceof ApiError ? error.message : tr("Request failed.");
}

let dialogResolve = null;

// confirmAction asks a yes/no question in a modal dialog and resolves to true
// when the person confirms.
export function confirmAction(message, confirmLabel = "Confirm") {
  const dialog = document.getElementById("confirm-dialog");
  if (!dialog?.showModal) return Promise.resolve(window.confirm(message));
  dialogResolve?.(false);
  document.getElementById("confirm-text").textContent = message;
  document.getElementById("confirm-ok").textContent = tr(confirmLabel);
  const opener = document.activeElement;
  dialog.showModal();
  document.getElementById("confirm-cancel").focus();
  return new Promise((resolve) => {
    dialogResolve = (answer) => {
      dialogResolve = null;
      if (dialog.open) dialog.close();
      opener?.focus?.({ preventScroll: true });
      resolve(answer);
    };
  });
}

export function bindConfirmDialog() {
  const dialog = document.getElementById("confirm-dialog");
  document.getElementById("confirm-ok").addEventListener("click", () => dialogResolve?.(true));
  document.getElementById("confirm-cancel").addEventListener("click", () => dialogResolve?.(false));
  dialog.addEventListener("cancel", (event) => { event.preventDefault(); dialogResolve?.(false); });
}

// toast shows a short message. state is "ok", "failed" or "" (neutral).
// A failure stays until dismissed, since it is often the only report of a
// failed action; other toasts wait while the pointer or focus is on them.
export function toast(message, tone = "ok") {
  const region = document.getElementById(tone === "failed" ? "alerts" : "toasts") || document.getElementById("toasts");
  const item = h("div", { class: "toast", dataset: { state: tone } }, h("p", {}, message));
  const close = () => item.remove();
  item.append(h("button", { type: "button", "aria-label": tr("Dismiss"), on: { click: close } }, "×"));
  region.append(item);
  if (tone === "failed") return;
  let timer = 0;
  const wait = () => { clearTimeout(timer); timer = setTimeout(close, 6000); };
  const hold = () => clearTimeout(timer);
  for (const event of ["pointerenter", "focusin"]) item.addEventListener(event, hold);
  item.addEventListener("pointerleave", wait);
  item.addEventListener("focusout", (event) => { if (!item.contains(event.relatedTarget)) wait(); });
  wait();
}

// whenVisible runs fn now, or once the tab is shown again, so views that poll
// on their own timers pause in hidden tabs as the live snapshot does.
export function whenVisible(fn) {
  if (!document.hidden) return fn();
  document.addEventListener("visibilitychange", function resume() {
    if (document.hidden) return;
    document.removeEventListener("visibilitychange", resume);
    fn();
  });
}

// message shows (or clears) an inline status line next to a form.
export function message(target, text, tone = "") {
  target.textContent = text || "";
  target.dataset.state = tone;
  target.hidden = !text;
}

// submitForm wires a form: prevents navigation (CSP blocks form actions),
// disables its submit button while action runs and reports failures inline.
export function submitForm(form, output, action) {
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const submit = form.querySelector("button[type=submit]");
    if (submit) submit.disabled = true;
    message(output, "");
    try {
      await action(new FormData(form));
    } catch (error) {
      if (error.status !== 401) message(output, errorText(error), "failed");
    } finally {
      if (submit) submit.disabled = false;
    }
  });
}
