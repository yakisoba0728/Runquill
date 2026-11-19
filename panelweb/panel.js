import { language, localizeDocument, serverText, tr } from "./i18n.js";

localizeDocument();
const el = (id) => document.getElementById(id);
const text = (id, value) => { el(id).textContent = tr(value); };
const demoMode = document.body.dataset.mode === "demo";
const viewButtons = document.querySelectorAll("[data-view]");
const demoButtons = document.querySelectorAll("[data-scenario]");
const themeControls = document.querySelectorAll("[data-theme-control]");
const navLinks = [...document.querySelectorAll(".sidebar nav a")];
let view = "queue";
let selected = null;
let current = null;
let retrySeconds = 0;
let management = null;
let user = null;

function applyTheme(choice) {
  const theme = choice === "light" || choice === "dark" ? choice : "system";
  document.body.dataset.panelTheme = theme;
  for (const control of themeControls) control.value = theme;
}

let savedTheme;
try { savedTheme = localStorage.getItem("runquill-login-theme"); } catch {}
applyTheme(savedTheme);
for (const control of themeControls) {
  control.addEventListener("change", () => {
    applyTheme(control.value);
    try { localStorage.setItem("runquill-login-theme", control.value); } catch {}
  });
}

function highlightNavigation() {
  const active = navLinks.find((link) => !link.hidden && link.hash === location.hash) || navLinks[0];
  for (const link of navLinks) link.removeAttribute("aria-current");
  active.setAttribute("aria-current", "location");
}

highlightNavigation();
window.addEventListener("hashchange", highlightNavigation);

function node(tag, value, css) {
  const result = document.createElement(tag);
  if (value != null) result.textContent = tr(value);
  if (css) result.className = css;
  return result;
}

function relative(value) {
  if (!value) return tr("Not observed");
  const seconds = Math.max(0, Math.floor((Date.now() - Date.parse(value)) / 1000));
  if (seconds < 5) return tr("Just now");
  if (seconds < 60) return tr("{n}s ago", { n: seconds });
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return tr("{n}m ago", { n: minutes });
  const hours = Math.floor(minutes / 60);
  return hours < 24 ? tr("{n}h ago", { n: hours }) : tr("{n}d ago", { n: Math.floor(hours / 24) });
}

function stamp(value) {
  return new Date(value).toLocaleTimeString(language, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function isGitHubJobURL(url) {
  return typeof url === "string" && /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/actions\/runs\/\d+\/job\/\d+$/.test(url);
}

function githubLink(id, url) {
  const link = el(id);
  link.hidden = !isGitHubJobURL(url);
  if (link.hidden) link.removeAttribute("href");
  else link.href = url;
}

function tone(status) {
  if (["RUNNING", "UP", "CLEANED", "FINISHED"].includes(status)) return "ok";
  if (["OFFLINE", "UNAVAILABLE", "FAILED"].includes(status)) return "bad";
  if (["RETRY", "WAITING", "CORDONED", "CLEANUP"].includes(status)) return "warn";
  return "";
}

function badge(status, label = status) {
  const result = node("span", label, "pill");
  result.dataset.tone = tone(status);
  return result;
}

function duration(seconds) {
  if (!Number.isFinite(seconds)) return "—";
  const minutes = Math.floor(seconds / 60);
  const hours = Math.floor(minutes / 60);
  if (hours) return tr("{hours}h {minutes}m", { hours, minutes: minutes % 60 });
  if (minutes) return tr("{minutes}m {seconds}s", { minutes, seconds: seconds % 60 });
  return tr("{seconds}s", { seconds });
}

function workerState(host) {
  return host.cordoned && host.status === "UP" ? "CORDONED" : host.status;
}

function availableSlots(host) {
  return workerState(host) === "UP" ? Math.max(0, host.slot_limit - host.occupied_slots - host.reserved_slots) : 0;
}

function poolName(pool) {
  return !pool || pool === "default" ? tr("Default pool") : pool;
}

function renderHosts(hosts) {
  const target = el("host-rows");
  const previouslyFocused = document.activeElement?.dataset.worker;
  target.replaceChildren();
  el("host-empty").hidden = hosts.length > 0;
  text("host-empty", "No host observations yet.");
  for (const host of hosts) {
    const row = node("tr");
    const identity = node("td", host.name, "host-id");
    if (host.error) identity.append(node("small", host.error, "row-note"));
    const state = node("td");
    state.append(badge(workerState(host)));
    const action = node("td");
    if (user && user.role !== "viewer") {
      const control = node("button", host.cordoned ? "Resume assignments" : "Pause assignments", "button host-control");
      control.type = "button";
      control.dataset.worker = host.name;
      control.setAttribute("aria-label", tr("{action} on {host}", { action: control.textContent, host: host.name }));
      control.addEventListener("click", async () => {
        control.disabled = true;
        await management.updateHost(host);
        control.disabled = false;
      });
      action.append(control);
    } else {
      action.textContent = "—";
    }
    row.append(identity, node("td", poolName(host.pool)), state, node("td", host.slot_limit), node("td", host.occupied_slots),
      node("td", host.reserved_slots), node("td", availableSlots(host)), action);
    target.append(row);
    if (previouslyFocused === host.name) action.firstElementChild?.focus({ preventScroll: true });
  }
}

function renderPools(hosts) {
  const target = el("pool-rows");
  target.replaceChildren();
  const pools = new Map();
  for (const host of hosts) {
    const key = host.pool || "default";
    if (!pools.has(key)) pools.set(key, { hosts: 0, ready: 0, reserved: 0, free: 0, occupied: 0, limit: 0 });
    const pool = pools.get(key);
    pool.hosts++;
    pool.ready += Number(workerState(host) === "UP");
    pool.reserved += host.reserved_slots;
    pool.occupied += host.occupied_slots;
    pool.limit += host.slot_limit;
    pool.free += availableSlots(host);
  }
  if (!pools.size) {
    target.append(node("p", "No host observations yet.", "empty"));
    return;
  }
  for (const [name, pool] of [...pools].sort(([a], [b]) => a.localeCompare(b))) {
    const row = node("div", null, "pool-row");
    const head = node("div", null, "pool-name");
    head.append(node("span", poolName(name)), node("span", tr("{ready} of {total} ready", { ready: pool.ready, total: pool.hosts }), "row-note"));
    const capacity = node("div", null, "pool-meta");
    capacity.append(node("span", tr("{used} of {limit} slots", { used: pool.occupied + pool.reserved, limit: pool.limit })),
      node("span", tr("{n} free", { n: pool.free })));
    const bar = node("progress");
    bar.max = Math.max(1, pool.limit);
    bar.value = Math.min(bar.max, pool.occupied + pool.reserved);
    bar.dataset.reserved = String(pool.reserved > 0);
    bar.setAttribute("aria-label", tr("{host} capacity", { host: poolName(name) }));
    row.append(head, capacity, bar);
    if (pool.reserved) row.append(node("small", tr("{n} reserved", { n: pool.reserved }), "row-note"));
    target.append(row);
  }
}

function renderInventory(inventory, jobs) {
  const target = el("runner-rows");
  target.replaceChildren();
  el("runner-empty").hidden = inventory.length > 0;
  text("runner-empty", current.fleet?.docker_error ? "Runner observations are unavailable." : "No runners or retained leases.");
  const observed = inventory.filter((runner) => runner.runtime_state).length;
  text("runner-summary", tr("{observed} observed · {leases} unobserved leases", { observed, leases: inventory.length - observed }));
  const jobsByID = new Map(jobs.map((job) => [job.id, job]));
  for (const runner of inventory) {
    const row = node("tr");
    const job = jobsByID.get(runner.job_id);
    const runtime = runner.runtime_state;
    const state = !runtime ? "UNKNOWN" : { running: "RUNNING", paused: "PAUSED", created: "STARTING", pending: "STARTING",
      dead: "CLEANUP", exited: "CLEANUP" }[runtime] || runtime;
    const stateColumn = node("td");
    stateColumn.append(badge(state, runtime ? state : "Lease not observed"));
    const jobCell = node("td");
    if (job) {
      const label = job.repository + " #" + job.id;
      if (isGitHubJobURL(job.url)) {
        const link = node("a", label);
        link.href = job.url;
        link.target = "_blank";
        link.rel = "noopener noreferrer";
        jobCell.append(link);
      } else {
        jobCell.textContent = label;
      }
    } else {
      jobCell.append(node("span", "Untracked runner"));
    }
    row.append(node("td", runner.name, "host-id"), node("td", runner.host || "—"),
      node("td", poolName(runner.pool || job?.pool)), stateColumn, jobCell);
    target.append(row);
  }
}

function dateStamp(value) {
  return value ? new Date(value).toLocaleString(language, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "—";
}

function canChangeJobs() {
  return !demoMode && !!user && user.role !== "viewer";
}

// Buttons for a job that waits without a runner. Each asks for confirmation in
// manage.js; done runs after the server applied the change.
function jobActionButtons(job, messageID, done) {
  if (!canChangeJobs()) return [];
  const actions = [];
  if (job.retry_at) actions.push(["retry", "Retry now"]);
  actions.push(["cancel", "Cancel job"]);
  return actions.map(([kind, label]) => {
    const control = node("button", label, "button");
    control.type = "button";
    control.dataset.jobAction = kind + ":" + job.id;
    control.setAttribute("aria-label", tr("{action} for job {id}", { action: tr(label), id: job.id }));
    control.addEventListener("click", async () => {
      control.disabled = true;
      if (await management.jobAction(job, kind, messageID)) await done();
      control.disabled = false;
    });
    return control;
  });
}

function renderDetails(job) {
  const details = el("job-details");
  details.hidden = !job;
  if (!job) return;
  text("detail-title", tr("Job #{id}", { id: job.id }));
  githubLink("detail-link", job.url);
  const source = job.timing?.discovery_source;
  const reason = tr(job.diagnostic?.message || "") || job.pool_error || "";
  const detailsText = [
    reason,
    source ? tr(source === "poll" ? "Discovered by GitHub API polling." : "Discovered by webhook.") : "",
    job.runner_host ? tr("Host {name}", { name: job.runner_host }) : "",
    job.runner ? tr("Runner {name}", { name: job.runner }) : "",
    job.pool ? tr("Pool {name}", { name: poolName(job.pool) }) : "",
    job.retry_at ? tr("Retry time {time}", { time: stamp(job.retry_at) }) : "",
  ].filter(Boolean);
  text("detail-reason", detailsText.join(" · ") || "Waiting for job events.");
  const retry = job.retry_reason && job.retry_reason !== reason ? job.retry_reason : "";
  el("detail-retry").hidden = !retry;
  text("detail-retry", retry);
  el("detail-route").hidden = !job.runner_label;
  text("detail-label", job.runner_label || "");
  const detailActions = el("detail-actions");
  const focusedAction = document.activeElement?.dataset.jobAction;
  detailActions.replaceChildren(...(pendingJob(job) && !job.runner ? jobActionButtons(job, "job-action-message", async () => {
    selected = null;
    await refresh();
  }) : []));
  detailActions.hidden = !detailActions.childElementCount;
  if (focusedAction) detailActions.querySelector('[data-job-action="' + CSS.escape(focusedAction) + '"]')?.focus({ preventScroll: true });
  const timings = el("detail-timings");
  timings.replaceChildren();
  for (const [label, seconds] of [
    ["Discovery time", job.timing?.creation_to_discovery_seconds],
    ["Queue time", job.timing?.runquill_queue_seconds],
    ["Startup time", job.timing?.provision_to_start_seconds],
  ]) {
    if (!Number.isFinite(seconds)) continue;
    const item = node("div");
    item.append(node("dt", label), node("dd", duration(seconds)));
    timings.append(item);
  }
  timings.hidden = !timings.childElementCount;
  const timeline = el("detail-timeline");
  timeline.replaceChildren();
  for (const [label, time] of [
    ["Created on GitHub", job.timing?.github_created_at],
    ["Discovered", job.timing?.discovered_at],
    ["Runner provisioned", job.timing?.provisioned_at],
    ["Job started on GitHub", job.timing?.github_started_at],
    ["Completion observed", job.finished_at],
    ["Runner cleaned up", job.cleaned_at],
  ]) {
    if (!time) continue;
    const row = node("li");
    const value = node("time", stamp(time));
    value.dateTime = time;
    row.append(node("span", label), value);
    timeline.append(row);
  }
}

function pendingJob(job) {
  return job.status === "WAITING" || job.status === "RETRY";
}

function updatePoolFilter(active, recent, hosts) {
  const control = el("job-pool");
  const pools = [...new Set([...active, ...recent, ...hosts].map((item) => item.pool || "default"))].sort();
  if (pools.join(",") === control.dataset.options) return;
  const selectedPool = control.value;
  const options = pools.map((pool) => {
    const option = node("option", poolName(pool));
    option.value = pool;
    return option;
  });
  control.replaceChildren(control.options[0], ...options);
  control.value = pools.includes(selectedPool) ? selectedPool : "";
  control.dataset.options = pools.join(",");
}

function renderJobs() {
  if (!current) return;
  const rows = el("job-rows");
  const previouslyFocused = document.activeElement?.dataset.job;
  rows.replaceChildren();
  const active = current.fleet?.jobs || [];
  const recent = current.recent || [];
  const queued = active.filter(pendingJob);
  updatePoolFilter(active, recent, current.fleet?.hosts || []);
  const group = view === "queue" ? queued.sort((a, b) => (a.queue_position || Number.MAX_SAFE_INTEGER) - (b.queue_position || Number.MAX_SAFE_INTEGER) ||
    (a.retry_at || "").localeCompare(b.retry_at || "")) : view === "active" ? active.filter((job) => !pendingJob(job)) : recent;
  const query = el("job-search").value.trim().toLowerCase().replace(/^#/, "");
  const pool = el("job-pool").value;
  const jobs = group.filter((job) => (!pool || (job.pool || "default") === pool) &&
    (!query || job.repository.toLowerCase().includes(query) || String(job.id).includes(query)));
  el("job-diagnose").hidden = demoMode || view !== "queue" || !current.fleet;
  text("jobs-summary", tr("{queued} queued · {assigned} assigned · {recent} cleaned recently",
    { queued: queued.length, assigned: active.length - queued.length, recent: recent.length }));
  const empty = el("job-empty");
  empty.hidden = jobs.length !== 0;
  const emptyLabel = { queue: "The queue is empty.", active: "No assigned jobs.", recent: "No confirmed runner cleanup yet." };
  empty.textContent = tr(!current.fleet ? "Waiting for a fleet snapshot…" : (query || pool) ? "No jobs match these filters." : emptyLabel[view]);
  const detailJob = jobs.find((job) => job.id === selected);
  for (const job of jobs) {
    const row = node("tr");
    const identity = node("td");
    const button = node("button", job.repository, "job-button");
    button.type = "button";
    button.dataset.job = job.id;
    button.setAttribute("aria-expanded", String(job.id === selected));
    button.setAttribute("aria-controls", "job-details");
    button.setAttribute("aria-label", tr("{repo} job {id}", { repo: job.repository, id: job.id }));
    button.addEventListener("click", () => {
      selected = selected === job.id ? null : job.id;
      renderJobs();
    });
    identity.append(button, node("span", "#" + job.id + (job.pool ? " · " + poolName(job.pool) : ""), "job-meta"));
    const reason = job.status === "RETRY" ? job.retry_reason || job.diagnostic?.message : job.diagnostic?.message || job.retry_reason;
    if (view === "queue" && reason) identity.append(node("small", reason, "row-note"));
    const stateColumn = node("td");
    const state = job.status || "CLEANED";
    stateColumn.append(badge(state));
    if (job.retry_at) stateColumn.append(node("small", tr("Retry time {time}", { time: stamp(job.retry_at) }), "row-note"));
    row.append(node("td", job.queue_position ?? "—", "numeric"), identity, stateColumn,
      node("td", duration(job.timing?.runquill_queue_seconds), "numeric"), node("td", job.runner_host || "—"));
    rows.append(row);
  }
  renderDetails(detailJob);
  if (previouslyFocused) rows.querySelector('[data-job="' + CSS.escape(previouslyFocused) + '"]')?.focus({ preventScroll: true });
}

const historyPage = 25;
let historyRows = [];
let historyCursor = 0;
let historyLoaded = false;

function historyQuery(more) {
  const form = new FormData(el("history-form"));
  const query = new URLSearchParams({ limit: String(historyPage) });
  const repository = String(form.get("repository") || "").trim();
  if (repository) query.set("repository", repository);
  if (form.get("outcome")) query.set("outcome", String(form.get("outcome")));
  const range = Number(form.get("range"));
  if (range) query.set("since", String(Math.floor(Date.now() / 1000) - range));
  if (more && historyCursor) query.set("before", String(historyCursor));
  return query;
}

async function loadHistory(more = false, keepMessage = false) {
  if (!demoMode && !user) return;
  const query = historyQuery(more);
  if (!keepMessage) el("history-message").hidden = true;
  let page;
  try {
    page = demoMode ? (await import("./demo.js")).history(query) : await management.panelRequest("./api/jobs?" + query);
  } catch (error) {
    if (error.status === 401) {
      signedOut = true;
      management.showLogin("Your session ended. Sign in again to view the fleet.");
      return;
    }
    el("history-message").hidden = false;
    text("history-message", serverText(error.message) || "Request failed.");
    return;
  }
  historyRows = more ? historyRows.concat(page.jobs) : page.jobs;
  historyCursor = page.next || 0;
  historyLoaded = true;
  renderHistory();
}

function historyState(job) {
  if (job.outcome === "waiting") return job.retry_at ? "RETRY" : "WAITING";
  return job.outcome.toUpperCase();
}

function renderHistory() {
  const rows = el("history-rows");
  rows.replaceChildren();
  el("history-empty").hidden = historyRows.length > 0;
  text("history-empty", historyLoaded ? "No jobs match these filters." : "Loading job history…");
  el("history-more").hidden = !historyCursor;
  for (const job of historyRows) {
    const row = node("tr");
    const identity = node("td");
    if (isGitHubJobURL(job.url)) {
      const link = node("a", job.repository);
      link.href = job.url;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      link.setAttribute("aria-label", tr("{repo} job {id} on GitHub", { repo: job.repository, id: job.id }));
      identity.append(link);
    } else {
      identity.append(node("span", job.repository));
    }
    identity.append(node("span", "#" + job.id + (job.runner_label ? " · " + job.runner_label : ""), "job-meta"));
    const state = historyState(job);
    const outcome = node("td");
    outcome.append(badge(state));
    const reason = job.failure || (job.outcome === "waiting" ? job.retry_reason : "");
    if (reason) outcome.append(node("small", tr(job.failure ? "Reason: {reason}" : "Last retry: {reason}", { reason: tr(reason) }), "row-note"));
    if (job.retry_at) outcome.append(node("small", tr("Retry time {time}", { time: stamp(job.retry_at) }), "row-note"));
    const actions = node("td");
    const buttons = job.outcome === "waiting" ? jobActionButtons(job, "history-message", async () => {
      await loadHistory(false, true);
      void refresh();
    }) : [];
    if (buttons.length) {
      const group = node("div", null, "job-actions");
      group.append(...buttons);
      actions.append(group);
    } else {
      actions.textContent = "—";
    }
    row.append(identity, outcome, node("td", job.pool ? poolName(job.pool) : "—"), node("td", job.host || "—"),
      node("td", dateStamp(job.queued_at), "history-cell"), node("td", dateStamp(job.started_at), "history-cell"),
      node("td", dateStamp(job.finished_at), "history-cell"), node("td", duration(job.wait_seconds), "numeric"), actions);
    rows.append(row);
  }
}

el("history-form").addEventListener("submit", (event) => {
  event.preventDefault();
  void loadHistory();
});
el("history-more").addEventListener("click", () => void loadHistory(true));

function render() {
  if (!current) return;
  const fleet = current.fleet;
  const counts = fleet?.runners;
  const hosts = fleet?.hosts || [];
  const jobs = fleet?.jobs || [];
  const queued = jobs.filter(pendingJob);
  const waiting = queued.filter((job) => job.status === "WAITING").length;
  const oldest = queued.reduce((max, job) => Math.max(max, job.timing?.runquill_queue_seconds ?? -1), -1);
  const used = counts && counts.occupied_slots + counts.reserved_slots;
  const free = counts ? Math.max(0, Math.min(fleet.slot_limit - used, hosts.reduce((sum, host) => sum + availableSlots(host), 0))) : 0;
  text("running", counts?.known_running ?? "—");
  text("waiting", fleet ? queued.length : "—");
  const queueNote = [tr("{n} ready", { n: waiting }), tr("{n} retrying", { n: queued.length - waiting })];
  if (oldest >= 0) queueNote.push(tr("Longest observed wait {time}", { time: duration(oldest) }));
  text("queue-note", fleet ? queueNote.join(" · ") : "");
  text("slots", counts ? used + " / " + fleet.slot_limit : "—");
  const slotNote = counts ? [tr("{n} free", { n: free }), tr("{n} reserved", { n: counts.reserved_slots })] : [];
  if (counts?.unassigned_reserved_slots) slotNote.push(tr("{n} unassigned", { n: counts.unassigned_reserved_slots }));
  text("slot-note", slotNote.join(" · "));
  const hostsKnown = fleet && (hosts.length > 0 || !fleet.docker_error);
  text("host-count", hostsKnown ? hosts.length : "—");
  text("host-count-label", fleet?.backend === "kubernetes" ? "Runtime targets" : "Worker hosts");
  text("host-description", fleet?.backend === "kubernetes" ? "Kubernetes runner pool capacity" : "Capacity by Docker engine");
  const hostSummary = ["UP", "OFFLINE", "UNAVAILABLE", "CORDONED"].map((status) => hosts.filter((host) => workerState(host) === status).length);
  const hostLabels = ["{n} ready", "{n} offline", "{n} unavailable", "{n} on maintenance"];
  text("host-note", hostsKnown ? hostSummary.map((n, i) => tr(hostLabels[i], { n })).join(" · ") : "");

  let notice = retrySeconds ? tr("Connection lost. Retrying in {n}s. Showing the last available snapshot.", { n: retrySeconds })
    : serverText(fleet?.docker_error);
  if (!notice && current.poll_failed) notice = "The latest GitHub poll failed. Check the manager logs.";
  el("notice").hidden = !notice;
  text("notice", notice || "");
  text("updated", current.sampled_at ? tr("Observed {time}", { time: relative(current.sampled_at).toLowerCase() }) : "Awaiting first snapshot");
  el("update-dot").dataset.tone = notice ? "warn" : current.sampled_at ? "ok" : "";

  const scope = fleet?.repository_scope;
  el("access-scope").hidden = scope !== "selected" && scope !== "all";
  text("access-scope-value", scope === "selected" ? "Only selected repositories" : "All repositories");
  const connected = !!(current.last_webhook || current.last_poll);
  text("app-signal", connected ? "Activity verified" : "Awaiting evidence");
  el("app-signal").dataset.tone = connected ? "ok" : "";
  text("webhook-signal", current.webhook ? relative(current.last_webhook) : "Disabled");
  text("poll-signal", current.polling ? relative(current.last_poll) : "Disabled");
  el("poll-signal").dataset.tone = current.poll_failed ? "warn" : "";

  for (const [key, done] of [
    ["app", connected], ["seen", current.first_job_seen], ["clean", !!current.first_job_clean],
  ]) {
    const check = document.querySelector('[data-check="' + key + '"]');
    check.dataset.done = String(!!done);
    check.textContent = done ? "✓" : "";
  }
  text("check-app", connected ? "Runquill observed a GitHub delivery or a successful poll." : "Run setup and install the App on your personal account.");
  el("check-seen").hidden = !current.first_job_seen;
  el("check-seen-instruction").hidden = current.first_job_seen;
  text("check-clean", current.first_job_clean
    ? tr("Runner cleanup confirmed for job #{id}.", { id: current.first_job_clean.id })
    : "When the job finishes, Runquill removes its runner. Check the job result on GitHub.");
  githubLink("check-link", current.first_job_clean?.url);

  renderHosts(hosts);
  renderPools(hosts);
  renderInventory(current.runner_inventory || [], jobs);
  renderJobs();
}

function setView(name) {
  view = name;
  selected = null;
  for (const button of viewButtons) button.setAttribute("aria-pressed", String(button.dataset.view === view));
}

for (const button of viewButtons) {
  button.addEventListener("click", () => {
    setView(button.dataset.view);
    renderJobs();
  });
}
el("job-search").addEventListener("input", renderJobs);
el("job-pool").addEventListener("change", renderJobs);

const refreshInterval = 3000;
const maxRetryInterval = 30000;
let refreshTimer = null;
let refreshing = false;
let failures = 0;
let signedOut = false;

// Polls every 3s while the tab is visible. Failures back off exponentially up
// to 30s; a hidden tab stops polling and refreshes as soon as it is shown.
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = null;
  if (signedOut || document.hidden) return;
  const delay = failures ? Math.min(maxRetryInterval, refreshInterval * 2 ** failures) : refreshInterval;
  refreshTimer = setTimeout(refresh, delay);
  return delay;
}

async function refresh() {
  if (refreshing || signedOut) return;
  clearTimeout(refreshTimer);
  refreshTimer = null;
  refreshing = true;
  try {
    const response = await fetch("./snapshot", { cache: "no-store", signal: AbortSignal.timeout(5000) });
    if (response.status === 401) {
      signedOut = true;
      management.showLogin("Your session ended. Sign in again to view the fleet.");
      return;
    }
    if (!response.ok) throw new Error("snapshot unavailable");
    current = await response.json();
    failures = 0;
    retrySeconds = 0;
    render();
  } catch {
    failures++;
  } finally {
    refreshing = false;
  }
  if (signedOut) return;
  const delay = scheduleRefresh() ?? Math.min(maxRetryInterval, refreshInterval * 2 ** failures);
  if (!failures) return;
  retrySeconds = Math.round(delay / 1000);
  if (current) render();
  else {
    el("notice").hidden = false;
    text("notice", tr("Connection lost. Retrying in {n}s.", { n: retrySeconds }));
    el("update-dot").dataset.tone = "warn";
  }
}

function watchVisibility() {
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) scheduleRefresh();
    else void refresh();
  });
}

if (demoMode) {
  const { sample } = await import("./demo.js");
  el("demo-controls").hidden = false;
  function scenario(name) {
    const next = sample(name);
    current = next.snapshot;
    text("scenario-note", next.note);
    setView(name === "first" ? "queue" : "active");
    if (name !== "first") selected = current.fleet.jobs[0].id;
    for (const button of demoButtons) button.setAttribute("aria-pressed", String(button.dataset.scenario === name));
    render();
  }
  for (const button of demoButtons) button.addEventListener("click", () => scenario(button.dataset.scenario));
  scenario("first");
  void loadHistory();
} else {
  management = await import("./manage.js");
  user = await management.initializeManagement();
  if (user) {
    highlightNavigation();
    watchVisibility();
    void refresh();
    void loadHistory();
  }
}
