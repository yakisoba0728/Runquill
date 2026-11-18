import { language, localizeDocument, tr } from "./i18n.js";

localizeDocument();
const el = (id) => document.getElementById(id);
const text = (id, value) => { el(id).textContent = tr(value); };
const demoMode = document.body.dataset.mode === "demo";
const viewButtons = document.querySelectorAll("[data-view]");
const demoButtons = document.querySelectorAll("[data-scenario]");
let view = "active";
let selected = null;
let current = null;
let refreshError = false;
let management = null;
let user = null;

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

function githubLink(id, url) {
  const link = el(id);
  link.hidden = !(url && /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/actions\/runs\/\d+\/job\/\d+$/.test(url));
  if (link.hidden) link.removeAttribute("href");
  else link.href = url;
}

function tone(status) {
  if (["RUNNING", "UP", "CLEANED"].includes(status)) return "ok";
  if (["OFFLINE", "UNAVAILABLE"].includes(status)) return "bad";
  if (["RETRY", "WAITING", "CORDONED", "CLEANUP"].includes(status)) return "warn";
  return "";
}

function badge(status, label = status) {
  const result = node("span", label, "pill");
  result.dataset.tone = tone(status);
  return result;
}

function renderHosts(hosts) {
  const target = el("host-rows");
  target.replaceChildren();
  if (!hosts?.length) {
    target.append(node("p", "No host observations yet.", "empty"));
    return;
  }
  for (const host of hosts) {
    const row = node("div", null, "host-row");
    const name = node("div", null, "host-name");
    const state = host.cordoned && host.status === "UP" ? "CORDONED" : host.status;
    name.append(node("span", host.name), badge(state));
    const used = host.occupied_slots + host.reserved_slots;
    const capacity = node("div", null, "host-meta");
    capacity.append(node("span", tr("{used} of {limit} slots", { used, limit: host.slot_limit })),
      node("span", host.reserved_slots ? tr("{n} reserved", { n: host.reserved_slots }) : " "));
    const bar = node("progress");
    bar.max = Math.max(1, host.slot_limit);
    bar.value = Math.min(bar.max, used);
    bar.dataset.reserved = String(host.reserved_slots > 0);
    bar.setAttribute("aria-label", tr("{host} capacity", { host: host.name }));
    row.append(name, capacity, bar);
    if (user && user.role !== "viewer") {
      const control = node("button", host.cordoned ? "Resume assignments" : "Pause assignments", "button host-control");
      control.type = "button";
      control.setAttribute("aria-label", tr("{action} on {host}", { action: control.textContent, host: host.name }));
      control.addEventListener("click", async () => {
        control.disabled = true;
        await management.updateHost(host);
        control.disabled = false;
      });
      row.append(control);
    }
    target.append(row);
  }
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
    job.retry_at ? tr("Retry time {time}", { time: stamp(job.retry_at) }) : "",
  ].filter(Boolean);
  text("detail-reason", detailsText.join(" · ") || "Waiting for job events.");
  const retry = job.retry_reason && job.retry_reason !== reason ? job.retry_reason : "";
  el("detail-retry").hidden = !retry;
  text("detail-retry", retry);
  el("detail-route").hidden = !job.runner_label;
  text("detail-label", job.runner_label || "");
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

function renderJobs() {
  if (!current) return;
  const rows = el("job-rows");
  const previouslyFocused = document.activeElement?.dataset.job;
  rows.replaceChildren();
  const active = current.fleet?.jobs || [];
  const recent = current.recent || [];
  const jobs = view === "active" ? active : recent;
  el("job-diagnose").hidden = demoMode || view !== "active" || !current.fleet;
  text("jobs-summary", tr("{active} active · {recent} recently cleaned", { active: active.length, recent: recent.length }));
  const empty = el("job-empty");
  empty.hidden = jobs.length !== 0;
  empty.textContent = tr(current.fleet ? (view === "active" ? "No active or queued jobs." : "No confirmed runner cleanup yet.") : "Waiting for a fleet snapshot…");
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
    const meta = ["#" + job.id, job.queue_position ? tr("queue {position}", { position: job.queue_position }) : null].filter(Boolean);
    identity.append(button, node("span", meta.join(" · "), "job-meta"));
    const stateColumn = node("td");
    const state = job.status || "CLEANED";
    stateColumn.append(badge(state));
    row.append(identity, stateColumn, node("td", job.runner_host || "—"));
    rows.append(row);
  }
  renderDetails(detailJob);
  if (previouslyFocused) rows.querySelector('[data-job="' + CSS.escape(previouslyFocused) + '"]')?.focus({ preventScroll: true });
}

function render() {
  if (!current) return;
  const fleet = current.fleet;
  const counts = fleet?.runners;
  const hosts = fleet?.hosts || [];
  const waiting = fleet?.jobs?.filter((job) => job.status === "WAITING").length;
  const used = counts && counts.occupied_slots + counts.reserved_slots;
  text("running", counts?.known_running ?? "—");
  text("waiting", waiting ?? "—");
  text("slots", counts ? used + " / " + fleet.slot_limit : "—");
  text("slot-note", counts?.reserved_slots ? tr("{n} reserved", { n: counts.reserved_slots }) : "Used and reserved capacity");
  text("healthy", hosts.length ? hosts.filter((host) => host.status === "UP").length + " / " + hosts.length : "—");
  text("host-description", fleet?.backend === "kubernetes" ? "Kubernetes runner pool capacity" : "Capacity by Docker engine");
  text("host-note", counts?.offline_hosts ? tr("{n} offline", { n: counts.offline_hosts }) : "Reachable and ready");

  let notice = refreshError ? "Panel refresh failed. Showing the last available snapshot." : fleet?.docker_error;
  if (!notice && current.poll_failed) notice = "The latest GitHub poll failed. Check the manager logs.";
  el("notice").hidden = !notice;
  text("notice", notice || "");
  text("updated", demoMode ? "Sample data" : current.sampled_at ? tr("Observed {time}", { time: relative(current.sampled_at).toLowerCase() }) : "Awaiting first snapshot");
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

async function refresh() {
  try {
    const response = await fetch("./snapshot", { cache: "no-store", signal: AbortSignal.timeout(5000) });
    if (response.status === 401) {
      management.showLogin("Your session ended. Sign in again to view the fleet.");
      return;
    }
    if (!response.ok) throw new Error("snapshot unavailable");
    current = await response.json();
    refreshError = false;
    render();
  } catch {
    refreshError = true;
    if (current) render();
    else {
      el("notice").hidden = false;
      text("notice", "No snapshot is available yet. Runquill will try again.");
    }
  }
  setTimeout(refresh, 3000);
}

if (demoMode) {
  const { sample } = await import("./demo.js");
  el("demo-controls").hidden = false;
  function scenario(name) {
    const next = sample(name);
    current = next.snapshot;
    text("scenario-note", next.note);
    setView("active");
    if (name !== "first") selected = current.fleet.jobs[0].id;
    for (const button of demoButtons) button.setAttribute("aria-pressed", String(button.dataset.scenario === name));
    render();
  }
  for (const button of demoButtons) button.addEventListener("click", () => scenario(button.dataset.scenario));
  scenario("first");
} else {
  text("intro", "Track job discovery, runner capacity and cleanup.");
  management = await import("./manage.js");
  user = await management.initializeManagement();
  if (user) refresh();
}
