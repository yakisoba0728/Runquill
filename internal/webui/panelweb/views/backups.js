import { serverText, tr } from "../i18n.js";
import {
  block, button, errorText, fill, format, h, intro, kv, loadStyle, message, pageHead, saveFile, state, statusLine, time,
} from "../ui.js";

let parts = null;

// Environment variables that turn scheduled backups on, by name only.
const variables = [
  ["RUNQUILL_BACKUP_INTERVAL", "How often to back up, such as 24h. At least 1m."],
  ["RUNQUILL_BACKUP_DIR", "Directory on the manager that keeps the copies."],
  ["RUNQUILL_BACKUP_KEEP", "Optional. Copies to keep in the directory (default 7)."],
  ["RUNQUILL_BACKUP_PASSPHRASE", "Optional. Seals each copy with a passphrase (or use RUNQUILL_BACKUP_PASSPHRASE_FILE)."],
  ["RUNQUILL_BACKUP_S3_ENDPOINT", "Optional. Also uploads to S3-compatible storage, with RUNQUILL_BACKUP_S3_BUCKET, RUNQUILL_BACKUP_S3_ACCESS_KEY_ID and RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY."],
];

function onOff(on, onLabel = "On", offLabel = "Off") {
  return state(on ? "UP" : "", on ? onLabel : offLabel, on ? "ok" : "");
}

function scheduleRows(status) {
  const remote = status.remote || {};
  return [
    ["Scheduled backups", onOff(status.scheduled, "Enabled", "Disabled")],
    status.scheduled && ["Interval", format.duration(status.interval_seconds)],
    status.scheduled && ["Copies kept", format.number(status.keep)],
    status.scheduled && ["Directory", h("span", { class: "mono" }, status.directory || "—")],
    ["Sealing", onOff(status.sealing), status.sealing ? tr("Copies are encrypted with the backup passphrase.") : null],
    status.scheduled && ["Remote copy", onOff(remote.configured),
      remote.configured ? tr("{bucket} at {host}", { bucket: remote.bucket, host: remote.endpoint_host || "—" }) : null],
    remote.configured && ["Remote region", h("span", { class: "mono" }, remote.region || "—")],
    remote.configured && remote.prefix && ["Remote prefix", h("span", { class: "mono" }, remote.prefix)],
    remote.configured && ["Remote copies kept", remote.keep ? format.number(remote.keep) : tr("All (never pruned)")],
  ];
}

// A successful run clears the error, so any error text means the latest run failed.
function resultState(last) {
  if (last.error) return state("FAILED");
  return last.last_attempt ? state("DONE", "Succeeded") : state("", "No run yet", "");
}

function remoteState(last) {
  if (last.remote === "uploaded") return state("DONE", "Uploaded");
  if (last.remote === "failed") return state("FAILED", "Upload failed");
  return "—";
}

function lastRows(status) {
  const last = status.last || {};
  return [
    ["Result", resultState(last)],
    ["Last attempt", time(last.last_attempt, "date")],
    ["Last success", time(last.last_success, "date")],
    last.name && ["File", h("span", { class: "mono" }, last.name)],
    last.size && ["Size", format.bytes(last.size)],
    last.name && ["Sealed", tr(last.sealed ? "Yes" : "No")],
    status.remote?.configured && ["Remote upload", remoteState(last), last.remote_at ? tr("At {time}", { time: format.date(last.remote_at) }) : null],
    last.remote_pending && ["Waiting to upload", format.number(last.remote_pending)],
    last.remote_error && ["Upload error", h("span", { class: "error-text" }, serverText(last.remote_error))],
    last.error && ["Error", h("span", { class: "error-text" }, serverText(last.error))],
  ];
}

function enableNote() {
  return block("How to enable", { note: "Set these on the manager and restart it. Values stay on the server; the panel shows names only." },
    h("dl", { class: "kv wide", "aria-label": tr("Backup environment variables") },
      ...variables.map(([name, text]) => h("div", {}, h("dt", {}, h("code", {}, name)), h("dd", {}, tr(text))))));
}

function render(status) {
  const p = parts;
  p.schedule.replaceChildren(kv(scheduleRows(status), { wide: true, label: "Backup schedule" }));
  p.last.replaceChildren(status.scheduled || status.last?.last_attempt || status.last?.error
    ? kv(lastRows(status), { wide: true, label: "Last scheduled run" })
    : h("p", { class: "muted" }, tr("No scheduled run yet.")));
  p.enable.replaceChildren(...(status.scheduled ? [] : [enableNote()]));
  p.verificationBlock.hidden = !status.scheduled;
  renderVerification(p, status.verification);
}

function renderVerification(current, verification) {
  current.verification.replaceChildren(verification ? kv([
    ["Result", verification.ok ? state("DONE", "Verified") : state("FAILED", "Verification failed")],
    ["File", h("span", { class: "mono" }, verification.name)],
    ["Checked at", time(verification.checked_at, "date")],
    verification.error && ["Error", h("span", { class: "error-text" }, serverText(verification.error))],
  ], { wide: true, label: "Last backup verification" }) : h("p", { class: "muted" }, tr("No backup verification has run yet.")));
}

async function verifyBackup() {
  const current = parts;
  if (current.verify.disabled) return;
  current.refresh.disabled = true;
  current.verify.disabled = true;
  current.verify.setAttribute("aria-busy", "true");
  message(current.verificationOutput, tr("Verifying the latest local backup…"));
  try {
    const result = await current.ctx.api.post("./api/backups/verify", undefined, { timeout: 30 * 60 * 1000 + 10000 });
    if (parts !== current) return;
    renderVerification(current, result);
    message(current.verificationOutput, tr(result?.ok ? "The latest backup was verified." : "Verification failed."), result?.ok ? "ok" : "failed");
  } catch (error) {
    if (parts === current && error.status !== 401) message(current.verificationOutput, errorText(error), "failed");
  } finally {
    current.refresh.disabled = false;
    current.verify.disabled = false;
    current.verify.removeAttribute("aria-busy");
  }
}

async function load() {
  const current = parts;
  if (current.refresh.disabled) return;
  current.refresh.disabled = true;
  if (current.verify) current.verify.disabled = true;
  current.refresh.setAttribute("aria-busy", "true");
  message(current.output, "");
  try {
    const status = await current.ctx.api.get("./api/backups/status");
    if (parts !== current) return;
    current.loaded = true;
    render(status);
  } catch (error) {
    if (parts === current && error.status !== 401) {
      message(current.output, errorText(error), "failed");
      if (!current.loaded) current.schedule.replaceChildren(h("p", { class: "muted" }, tr("Backup status is unavailable.")));
    }
  } finally {
    if (!current.loaded) current.last.replaceChildren();
    current.refresh.disabled = false;
    if (current.verify) current.verify.disabled = false;
    current.refresh.removeAttribute("aria-busy");
  }
}

function downloadBlock(ctx) {
  const output = statusLine();
  if (!ctx.can("admin")) {
    return { element: block("Queue backup", { note: "A consistent copy of the job queue database." },
      h("p", { class: "muted" }, tr(ctx.demo ? "Downloads are not available in the sample panel." : "Only admins can download queue backups."))) };
  }
  const progress = h("progress", { max: 100, hidden: true, "aria-label": tr("Queue backup progress") });
  const download = button("Download queue backup", async () => {
    download.disabled = true;
    download.setAttribute("aria-busy", "true");
    message(output, tr("Preparing queue backup…"));
    progress.hidden = false;
    progress.removeAttribute("value");
    let shown = "";
    try {
      // Large queues take a while to copy and send, so there is no deadline.
      const blob = await ctx.api.download("./api/backup", "application/vnd.sqlite3", (received, total) => {
        if (total) progress.value = Math.floor(received * 100 / total);
        const text = total ? Math.floor(received * 100 / total) + "%" : format.bytes(received);
        if (text !== shown) message(output, tr("Downloading queue backup… {progress}", { progress: (shown = text) }));
      });
      saveFile(blob, "runquill-queue-backup.sqlite3");
      message(output, tr("Queue backup downloaded."), "ok");
    } catch (error) {
      if (error.status !== 401) message(output, errorText(error), "failed");
    } finally {
      progress.hidden = true;
      download.disabled = false;
      download.removeAttribute("aria-busy");
    }
  });
  return { download, element: block("Queue backup", { note: "A consistent, unsealed copy of the job queue database, downloaded now. It is recorded in the audit log." },
    h("div", { class: "toolbar" }, download), progress, output) };
}

export default {
  id: "backups",
  title: "Backups",

  mount(root, ctx) {
    loadStyle("backups");
    const refresh = button("Refresh", () => void load());
    const schedule = h("div", {}, h("p", { class: "muted" }, tr("Loading…")));
    const last = h("div");
    const enable = h("div");
    const output = statusLine();
    const queueBackup = downloadBlock(ctx);
    const verification = h("div");
    const verificationOutput = statusLine();
    const verify = ctx.can("admin") && !ctx.demo ? button("Verify latest backup", () => void verifyBackup()) : null;
    const verificationBlock = block("Backup verification", { note: "Checks the latest local scheduled copy and its GitHub App identity." },
      verify ? h("div", { class: "toolbar" }, verify) : null, verification, verificationOutput);
    verificationBlock.hidden = true;
    root.append(
      pageHead("Backups", refresh),
      intro(...fill("Restore a backup on the manager with {command}; the panel never restores.", { command: h("code", {}, "runquill restore") })),
      block("Schedule", {}, schedule, output),
      block("Last scheduled run", {}, last),
      verificationBlock,
      enable,
      queueBackup.element,
    );
    parts = { ctx, refresh, schedule, last, enable, output, download: queueBackup.download, verify, verificationBlock, verification, verificationOutput };
    void load();
  },

  commands() {
    if (!parts) return [];
    const { download, verify, verificationBlock } = parts;
    return [
      { label: "Refresh backup status", run: () => { if (parts) void load(); } },
      verify && !verificationBlock.hidden && !verify.disabled && { label: "Verify latest backup", run: () => verify.isConnected && verify.click() },
      download && !download.disabled && { label: "Download queue backup", run: () => download.isConnected && download.click() },
    ].filter(Boolean);
  },

  unmount() {
    parts = null;
  },
};
