// Logs: the container output of a leased runner from
// GET /api/runners/{name}/logs?tail=N&follow=1 (operators and admins with a
// signed-in session). The response is plain text: a buffered tail, or with
// follow a chunked stream that ends cleanly at the server's limits. Log lines
// are untrusted: they are written with textContent only, and terminal escape
// sequences are removed before display.
import { poolName, runnerRows, runnerState } from "../fleet.js";
import { tr } from "../i18n.js";
import {
  button, createTable, errorText, externalLink, format, h, intro, isGitHubJobURL, loadStyle, message, pageHead, saveFile, select,
  statusLine,
} from "../ui.js";

const maxLines = 5000; // lines kept on screen and in the pause buffer
const maxLineLength = 4000; // characters shown per line; the rest is cut
const followLimit = 15 * 60 * 1000; // the server ends a follow after 15 minutes
const limitMarker = "[Runquill: log output limit reached]";
const timestamp = /^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)) (.*)$/s;

let parts = null;

// clean removes ANSI/VT escape sequences and makes other control characters
// visible, so log text can never restyle or rewrite the page.
function clean(line) {
  let text = line.endsWith("\r") ? line.slice(0, -1) : line;
  text = text
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "") // CSI: colours, cursor moves
    .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?/g, "") // OSC: titles, hyperlinks
    .replace(/\x1b[@-_]/g, "") // other two-byte escapes
    .replace(/[\x00-\x08\x0b-\x1f\x7f\x9b]/g, "\ufffd");
  return text.length > maxLineLength ? text.slice(0, maxLineLength) + " " + tr("[{n} characters cut]", { n: text.length - maxLineLength }) : text;
}

// unsupported says why the server cannot read logs for a runner host.
function unsupported(host) {
  if (host === "kubernetes") return "Not available for Kubernetes runners.";
  if (host?.startsWith("agent://")) return "Not available for agent:// workers.";
  return "";
}

function liveRunners(snapshot) {
  return runnerRows(snapshot).filter((runner) => runner.job_id);
}

function lineNode(text) {
  const match = timestamp.exec(text);
  if (!match) return h("div", { class: "log-line" }, text);
  return h("div", { class: "log-line" },
    h("span", { class: "log-time", title: match[1] }, format.time(match[1])), " ", match[2]);
}

// Pane state and rendering. Incoming lines collect in `pending` and are
// appended once per animation frame as one DocumentFragment.

function setStatus(text, tone = "") {
  message(parts.status, text, tone);
}

function updateCount() {
  const { pane } = parts;
  parts.count.textContent = tr("{n} lines", { n: format.number(pane.shown.length) });
  parts.dropped.hidden = !pane.dropped;
  parts.dropped.textContent = tr("{n} older lines were dropped to keep the view responsive. Download keeps only the lines shown.", { n: format.number(pane.dropped) });
  parts.pauseNote.hidden = !pane.paused || !pane.pending.length;
  parts.pauseNote.textContent = tr("{n} new lines waiting", { n: format.number(pane.pending.length) });
}

// A frame is the normal trigger; the timer covers browsers that stop frames
// for a page that is not painted (a background window or an embedded pane).
function schedule() {
  if (!parts || parts.pane.frame || parts.pane.paused) return;
  parts.pane.frame = requestAnimationFrame(flush);
  parts.pane.timer = setTimeout(flush, 250);
}

function unschedule(pane) {
  cancelAnimationFrame(pane.frame);
  clearTimeout(pane.timer);
  pane.frame = 0;
  pane.timer = 0;
}

function flush() {
  const current = parts;
  if (!current) return;
  const { pane, output } = current;
  unschedule(pane);
  if (pane.paused || !pane.pending.length) return updateCount();
  let lines = pane.pending;
  pane.pending = [];
  if (lines.length > maxLines) {
    pane.dropped += lines.length - maxLines;
    lines = lines.slice(-maxLines);
  }
  // Follow the tail only when the reader is already at the bottom.
  const atBottom = output.scrollHeight - output.scrollTop - output.clientHeight < 24;
  const fragment = document.createDocumentFragment();
  for (const line of lines) fragment.append(lineNode(line));
  output.append(fragment);
  pane.shown.push(...lines);
  const excess = pane.shown.length - maxLines;
  if (excess > 0) {
    pane.shown.splice(0, excess);
    pane.dropped += excess;
    const range = document.createRange();
    range.setStartBefore(output.firstChild);
    range.setEndAfter(output.children[excess - 1]);
    range.deleteContents();
  }
  if (atBottom) output.scrollTop = output.scrollHeight;
  else current.jump.hidden = false;
  updateCount();
}

function receive(lines) {
  const { pane } = parts;
  pane.pending.push(...lines);
  if (pane.pending.length > maxLines) {
    const extra = pane.pending.length - maxLines;
    pane.pending.splice(0, extra);
    pane.dropped += extra;
  }
  if (pane.paused) updateCount();
  else schedule();
}

function clearPane() {
  const { pane } = parts;
  unschedule(pane);
  Object.assign(pane, { pending: [], shown: [], dropped: 0 });
  parts.output.replaceChildren();
  parts.jump.hidden = true;
  updateCount();
}

// Streaming.

function stopStream() {
  if (!parts) return;
  parts.stream?.abort();
  parts.stream = null;
  if (parts.stopButton) parts.stopButton.hidden = true;
}

async function open() {
  const current = parts;
  const runner = current?.selected;
  if (!runner) return;
  stopStream();
  clearPane();
  current.reconnect.hidden = true;
  const tail = current.tail.value;
  const follow = current.follow.checked && !current.ctx.demo;
  const path = "./api/runners/" + encodeURIComponent(runner.name) + "/logs?tail=" + tail + (follow ? "&follow=1" : "");
  setStatus(tr("Connecting…"));

  if (current.ctx.demo) {
    try {
      const text = await current.ctx.api.get(path);
      if (parts !== current || current.selected !== runner) return;
      receive(String(text).split("\n").filter((line, i, all) => line || i < all.length - 1).map(clean));
      setStatus(tr("Showing the last {n} lines.", { n: tail }));
    } catch (error) {
      if (parts === current) setStatus(errorText(error), "failed");
    }
    return;
  }

  const controller = new AbortController();
  current.stream = controller;
  current.stopButton.hidden = !follow;
  const started = Date.now();
  let limited = false;
  let response;
  try {
    // The shared client signs out on 401 and decodes {error} bodies.
    response = await current.ctx.api.stream(path, { signal: controller.signal });
  } catch (error) {
    if (current.stream !== controller || error?.name === "AbortError") return;
    current.stream = null;
    current.stopButton.hidden = true;
    if (error.status === 401) return;
    setStatus(errorText(error), "failed");
    // A lost connection, 429 (too many streams) and server errors may pass;
    // 501 and 404 do not.
    current.reconnect.hidden = ![0, 429, 502, 503].includes(error.status);
    return;
  }
  try {
    setStatus(follow ? tr("Following new output.") : tr("Showing the last {n} lines.", { n: tail }));
    const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
    let rest = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      if (current.stream !== controller) return;
      const pieces = (rest + value).split("\n");
      rest = pieces.pop();
      if (pieces.includes(limitMarker)) limited = true;
      if (pieces.length) receive(pieces.map(clean));
    }
    if (rest) receive([clean(rest)]);
    if (current.stream !== controller) return;
    current.stream = null;
    current.stopButton.hidden = true;
    if (!follow) {
      setStatus(tr("Showing the last {n} lines.", { n: tail }));
      current.reconnect.hidden = false;
      return;
    }
    const gone = !liveRunners(current.ctx.snapshot()).some((candidate) => candidate.name === runner.name);
    if (gone) setStatus(tr("The runner finished, so its log stream ended."));
    else if (limited) setStatus(tr("The stream stopped at the server's 8 MiB limit. Reconnect to continue from the latest lines."), "waiting");
    else if (Date.now() - started >= followLimit - 5000) setStatus(tr("Follow streams end after 15 minutes. Reconnect to continue."), "waiting");
    else setStatus(tr("The log stream ended. The last lines may explain why."), "waiting");
    current.reconnect.hidden = gone;
  } catch (error) {
    if (error?.name === "AbortError" || current.stream !== controller) return;
    current.stream = null;
    current.stopButton.hidden = true;
    setStatus(tr("The log stream was interrupted. Check the connection and reconnect."), "failed");
    current.reconnect.hidden = false;
  }
}

function choose(runner) {
  if (!parts || !runner) return;
  parts.selected = runner;
  parts.finished = null;
  parts.paneBlock.hidden = false;
  parts.paneTitle.textContent = runner.name;
  const job = runner.job;
  parts.paneJob.replaceChildren(...(job ? [
    job.repository + " #" + job.id + " · " + (runner.host || "—") + " · ",
    isGitHubJobURL(job.url)
      ? externalLink(tr("Step logs on GitHub"), job.url, tr("Step logs for {repo} job {id} on GitHub", { repo: job.repository, id: job.id }))
      : tr("Step logs are on GitHub."),
  ] : [runner.host || ""]));
  setPaused(false);
  parts.table.update(parts.rows);
  void open();
}

function setPaused(paused) {
  const { pane } = parts;
  pane.paused = paused;
  parts.pause.textContent = tr(paused ? "Resume" : "Pause");
  parts.pause.setAttribute("aria-pressed", String(paused));
  if (!paused) schedule();
  updateCount();
}

function exported() {
  return parts.pane.shown.join("\n") + (parts.pane.shown.length ? "\n" : "");
}

export default {
  id: "logs",
  title: "Logs",

  mount(root, ctx) {
    loadStyle("logs");
    const allowed = ctx.demo || ctx.can("operator");
    const lead = intro("This is the runner container's own output: registration, job pickup and shutdown. Job step logs stay on GitHub.");
    const columns = [
      { label: "Runner", className: "id", cell: (runner) => runner.name },
      { label: "Host", className: "long", cell: (runner) => runner.host || "—" },
      { label: "Pool", cell: (runner) => poolName(runner.pool || runner.job?.pool) },
      { label: "Runtime state", cell: runnerState },
      { label: "GitHub job", className: "long", cell: (runner) => {
        const job = runner.job;
        if (!job) return "—";
        const label = job.repository + " #" + job.id;
        return isGitHubJobURL(job.url) ? externalLink(label, job.url) : label;
      } },
    ];
    if (allowed) {
      columns.push({ label: "", cell: (runner) => {
        const reason = unsupported(runner.host);
        if (reason) return h("span", { class: "muted" }, tr(reason));
        const selected = parts?.selected?.name === runner.name;
        return h("div", { class: "row-actions" }, button(selected ? "Showing" : "View logs", () => choose(runner), {
          small: true, "aria-pressed": String(selected), "aria-label": tr("View logs for {runner}", { runner: runner.name }),
        }));
      } });
    }
    const table = createTable({
      label: "Live runners", key: (runner) => runner.name, columns,
      rowClass: (runner) => parts?.selected?.name === runner.name ? "selected" : "",
    });

    root.append(pageHead("Logs"), lead);
    root.append(h("div", { class: "block" }, table.element));
    if (!allowed) {
      lead.after(h("div", { class: "block" }, h("p", { class: "inline-message" },
        tr("Operators and admins can view runner logs. Ask an admin if you need access."))));
      parts = { ctx, table, rows: [], allowed };
      return;
    }

    const tail = select("tail", [["100", "100 lines"], ["200", "200 lines"], ["500", "500 lines"], ["1000", "1,000 lines"], ["2000", "2,000 lines"]]);
    tail.value = "200";
    const follow = h("input", { type: "checkbox", checked: !ctx.demo, disabled: ctx.demo });
    const wrap = h("input", { type: "checkbox", checked: true });
    const output = h("div", { class: "log-output wrap", tabindex: "0", "aria-label": tr("Runner log output") });
    const jump = button("Jump to latest", () => {
      output.scrollTop = output.scrollHeight;
      jump.hidden = true;
    }, { small: true, class: "button small log-jump", hidden: true });
    const pause = button("Pause", () => setPaused(!parts.pane.paused), { small: true, "aria-pressed": "false" });
    const copy = button("Copy", async () => {
      try {
        await navigator.clipboard.writeText(exported());
        ctx.toast(tr("Copied {n} lines.", { n: format.number(parts.pane.shown.length) }));
      } catch {
        ctx.toast(tr("The browser did not allow copying. Use Download instead."), "failed");
      }
    }, { small: true });
    const download = button("Download", () => saveFile(new Blob([exported()], { type: "text/plain;charset=utf-8" }), parts.selected.name + ".log"),
      { small: true });
    const clear = button("Clear", () => clearPane(), { small: true });
    const reconnect = button("Reconnect", () => void open(), { small: true, hidden: true });
    const stopButton = button("Stop", () => {
      stopStream();
      setStatus(tr("Stopped. The lines shown are kept."));
      parts.reconnect.hidden = false;
    }, { small: true, hidden: true });
    const status = statusLine();
    const count = h("span", { class: "muted" });
    const pauseNote = h("span", { class: "muted", hidden: true });
    const dropped = h("p", { class: "muted logs-dropped", hidden: true });
    const paneTitle = h("h2", { class: "mono" });
    const paneJob = h("p", { class: "muted" });

    const labelled = (label, control) => h("label", { class: "check" }, control, h("span", {}, tr(label)));
    const paneBlock = h("section", { class: "block", hidden: true, "aria-label": tr("Runner log") },
      h("div", { class: "block-head" }, h("div", {}, paneTitle, paneJob)),
      h("div", { class: "toolbar logs-toolbar" },
        h("label", { class: "check" }, h("span", {}, tr("Tail")), tail),
        labelled("Follow", follow), labelled("Wrap lines", wrap),
        pause, stopButton, reconnect, copy, download, clear,
        h("span", { class: "logs-meta" }, count, pauseNote)),
      ctx.demo ? h("p", { class: "muted logs-note" }, tr("The sample panel shows fixed sample lines and cannot follow output.")) : null,
      h("div", { class: "log-frame" }, output, jump),
      dropped, status);
    root.append(paneBlock);

    tail.addEventListener("change", () => void open());
    follow.addEventListener("change", () => void open());
    wrap.addEventListener("change", () => output.classList.toggle("wrap", wrap.checked));
    output.addEventListener("scroll", () => {
      if (output.scrollHeight - output.scrollTop - output.clientHeight < 24) jump.hidden = true;
    }, { passive: true });

    parts = {
      ctx, table, rows: [], allowed, selected: null, stream: null,
      tail, follow, output, jump, pause, reconnect, download, stopButton, status, count, pauseNote, dropped, paneBlock, paneTitle, paneJob,
      pane: { frame: 0, timer: 0, pending: [], shown: [], dropped: 0, paused: false },
    };
  },

  update(snapshot) {
    if (!parts) return;
    parts.rows = liveRunners(snapshot);
    const empty = !snapshot?.fleet ? "Waiting for a fleet snapshot…"
      : snapshot.fleet.docker_error ? "Runner observations are unavailable." : "No runner is running a job right now.";
    parts.table.update(parts.rows, empty);
    const selected = parts.selected;
    if (!selected || !parts.allowed) return;
    const still = parts.rows.find((runner) => runner.name === selected.name);
    if (still) {
      parts.selected = still;
    } else if (!parts.finished) {
      parts.finished = selected.name;
      // docker logs --follow ends by itself when the container stops; abort in
      // case the stream is still open, and keep what was shown.
      if (parts.stream) {
        stopStream();
      }
      setStatus(tr("This runner finished and was removed. Its last output is kept below."), "");
      parts.reconnect.hidden = true;
    }
    if (still && parts.finished === selected.name) parts.finished = null;
  },

  commands(ctx) {
    if (!parts?.allowed) return [];
    const list = parts.rows.filter((runner) => !unsupported(runner.host) && runner.name !== parts.selected?.name)
      .map((runner) => ({ label: "View logs for {runner}", values: { runner: runner.name }, run: () => choose(runner) }));
    if (!parts.selected || !parts.pane) return list;
    return [
      { label: parts.pane.paused ? "Resume log output" : "Pause log output", run: () => parts && setPaused(!parts.pane.paused) },
      !parts.reconnect.hidden && { label: "Reconnect to the runner log", run: () => void open() },
      !parts.stopButton.hidden && !ctx.demo && { label: "Stop following the runner log", run: () => parts.stopButton.click() },
      { label: "Download the runner log", run: () => parts?.download.click() },
      ...list,
    ].filter(Boolean);
  },

  unmount() {
    if (parts?.pane) unschedule(parts.pane);
    stopStream();
    parts = null;
  },
};
