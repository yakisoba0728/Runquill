// Queue: waiting, assigned and recently cleaned jobs from the live snapshot,
// with a detail panel per job, retry/cancel for operators and the repository
// check for admins. #/queue?job=ID opens that job in whichever list holds it.
import { jobActionButtons, pendingJob, poolName } from "../fleet.js";
import { serverText, tr } from "../i18n.js";
import {
  block, button, createKv, createTable, errorText, externalLink, field, fill, format, h, isGitHubJobURL, linkTarget, message, pageHead,
  state, statusLine, submitForm,
} from "../ui.js";
import { resourceView, timingView } from "./jobinsight.js";

let parts = null;
const lists = [["queue", "Queue"], ["active", "Assigned"], ["recent", "Cleaned up"]];
const emptyText = { queue: "The queue is empty.", active: "No assigned jobs.", recent: "No confirmed runner cleanup yet." };

function groups(snapshot) {
  const active = snapshot?.fleet?.jobs || [];
  return {
    queue: active.filter(pendingJob).sort((a, b) => (a.queue_position || Infinity) - (b.queue_position || Infinity) ||
      (a.retry_at || "").localeCompare(b.retry_at || "")),
    active: active.filter((job) => !pendingJob(job)),
    recent: snapshot?.recent || [],
  };
}

function reason(job) {
  const text = job.diagnostic?.message || job.retry_reason;
  return text ? serverText(text) : "";
}

function render() {
  if (!parts) return;
  const { ctx, table, tabs, search, pool, detail, target } = parts;
  const snapshot = ctx.snapshot();
  const all = groups(snapshot);
  // A deep link shows its job once: in the list that holds it, unfiltered.
  const wanted = target.key();
  const linked = wanted && lists.map(([id]) => [id, all[id].find((job) => String(job.id) === wanted)]).find(([, job]) => job);
  if (linked && parts.linked !== wanted) {
    parts.linked = wanted;
    parts.list = linked[0];
    parts.selected = linked[1].id;
    search.value = "";
    pool.value = "";
  }
  for (const [id, label] of lists) {
    tabs[id].setAttribute("aria-pressed", String(parts.list === id));
    tabs[id].replaceChildren(tr(label), h("span", { class: "count" }, snapshot?.fleet ? String(all[id].length) : ""));
  }
  const pools = [...new Set([...all.queue, ...all.active, ...all.recent, ...(snapshot?.fleet?.hosts || [])]
    .map((item) => item.pool || "default"))].sort();
  if (pool.dataset.options !== pools.join(",")) {
    const chosen = pool.value;
    pool.replaceChildren(pool.options[0], ...pools.map((name) => h("option", { value: name }, poolName(name))));
    pool.value = pools.includes(chosen) ? chosen : "";
    pool.dataset.options = pools.join(",");
  }
  const query = search.value.trim().toLowerCase().replace(/^#/, "");
  const jobs = all[parts.list].filter((job) => (!pool.value || (job.pool || "default") === pool.value) &&
    (!query || job.repository.toLowerCase().includes(query) || String(job.id).includes(query)));
  table.update(jobs, !snapshot?.fleet ? "Waiting for a fleet snapshot…" : query || pool.value ? "No jobs match these filters." : emptyText[parts.list]);
  // The selected job stays open when a filter hides it or it moves to another list, as a waiting job does once assigned.
  renderDetail(jobs.find((job) => job.id === parts.selected)
    ?? lists.flatMap(([id]) => all[id]).find((job) => job.id === parts.selected), detail);
  target.settle({
    ready: !!snapshot?.fleet, table, found: !!linked,
    gone: (id) => ctx.toast(tr("Job #{id} is no longer in the queue.", { id }), ""),
  });
}

// leave drops the deep link once the person picks something else.
function leave() {
  parts.target.forget();
  parts.linked = null;
}

// loadInsight fetches GET /api/jobs/{id} for the selected job's resources and
// execution time, again whenever its state changes; render() runs when it ends.
function loadInsight(job) {
  const key = job.id + ":" + (job.status || "CLEANED");
  if (parts.insight?.key === key) return parts.insight;
  const insight = { key, pending: true, data: null, failed: "" };
  parts.insight = insight;
  parts.ctx.api.get("./api/jobs/" + encodeURIComponent(job.id)).then((data) => { insight.data = data; }, (error) => {
    // A job not yet in the history has nothing recorded.
    if (error.status !== 404) insight.failed = errorText(error);
  }).finally(() => {
    insight.pending = false;
    if (parts?.insight === insight) render();
  });
  return insight;
}

function renderDetail(job, target) {
  if (!job) {
    target.hidden = true;
    target.replaceChildren();
    return;
  }
  const insight = loadInsight(job);
  // The queue time of a waiting job grows every snapshot; only the timing part follows it.
  const timing = { ...job.timing, execution_seconds: job.timing?.execution_seconds ?? insight.data?.timing?.execution_seconds };
  const timingKey = JSON.stringify(timing);
  const signature = JSON.stringify({ ...job, timing: { ...job.timing, runquill_queue_seconds: null } }) + parts.ctx.can("operator") +
    JSON.stringify([insight.key, insight.pending, insight.failed]);
  if (!target.hidden && target.dataset.signature === signature) {
    if (parts.timingKey !== timingKey) {
      parts.timingKey = timingKey;
      target.querySelector("[data-timing]")?.replaceChildren(timingView(timing));
    }
    return;
  }
  const focused = target.contains(document.activeElement) ? document.activeElement.textContent : null;
  target.dataset.signature = signature;
  parts.timingKey = timingKey;
  target.hidden = false;
  const actions = pendingJob(job) && !job.runner ? jobActionButtons(parts.ctx, job, async () => {
    if (!parts) return;
    leave(); // the job leaves the queue; that is the point, not news
    parts.selected = null;
    await parts?.ctx.refresh();
    render();
  }) : [];
  const link = isGitHubJobURL(job.url) ? externalLink(tr("Open in GitHub"), job.url) : null;
  const source = job.timing?.discovery_source;
  const facts = createKv([["status", "State"], ["source", "Discovered by"], ["pool", "Pool"], ["host", "Host"], ["runner", "Runner"],
    ["label", "Runner label"], ["os", "Runner OS"], ["image", "Runner image"], ["retry", "Retry time"]], { wide: true, label: "Job details" });
  facts.set("status", state(job.status || "CLEANED"));
  facts.set("source", source ? tr(source === "poll" ? "GitHub API polling" : "Webhook") : "—");
  facts.set("pool", poolName(job.pool));
  facts.set("host", job.runner_host || "—");
  facts.set("runner", job.runner ? h("span", { class: "mono" }, job.runner) : "—");
  facts.set("label", job.runner_label ? h("code", {}, job.runner_label) : "—");
  facts.set("os", job.runner_os || "—");
  facts.set("image", job.runner_image ? h("span", { class: "mono" }, job.runner_image) : "—");
  facts.show("retry", !!job.retry_at);
  if (job.retry_at) facts.set("retry", format.time(job.retry_at));
  const events = [
    ["created", "Created on GitHub", timing.github_created_at],
    ["discovered", "Discovered", timing.discovered_at],
    ["jit", "JIT requested", timing.jit_requested_at],
    ["provisioned", "Runner provisioned", timing.jit_registered_at || timing.provisioned_at],
    ["runtime", "Runtime accepted launch", timing.runner_launch_at],
    ["started", "Job started on GitHub", timing.github_started_at],
    ["finished", "Completion observed", job.finished_at],
    ["cleaned", "Runner cleaned up", job.cleaned_at],
  ].filter(([, , at]) => at);
  const timeline = createKv(events.map(([key, label]) => [key, label]), { label: "Observed job events" });
  for (const [key, , at] of events) timeline.set(key, format.time(at));
  const why = reason(job) || serverText(job.pool_error || "");
  const retry = job.retry_reason && serverText(job.retry_reason) !== why ? serverText(job.retry_reason) : "";
  target.replaceChildren(
    h("div", { class: "detail-head" }, h("h2", { id: "job-detail-title", tabindex: "-1" }, tr("Job #{id}", { id: job.id })),
      h("span", { class: "muted" }, job.repository), link,
      actions.length ? h("div", { class: "actions" }, ...actions) : null),
    h("div", { class: "detail-body" },
      why || retry ? h("p", { class: "reason" }, [why, retry].filter(Boolean).join(" ")) : null,
      h("div", {}, h("h3", {}, tr("Stage timing")), h("div", { "data-timing": "" }, timingView(timing)),
        h("h3", { class: "spaced" }, tr("Resources")),
        resourceView(insight.data?.resources, { pending: insight.pending, failed: insight.failed })),
      h("div", {}, h("h3", {}, tr("Details")), facts.element,
        events.length ? h("h3", { class: "spaced" }, tr("Timeline")) : null, events.length ? timeline.element : null)),
  );
  if (focused) [...target.querySelectorAll("button, a")].find((node) => node.textContent === focused)?.focus({ preventScroll: true });
}

function showList(id) {
  if (!parts) return;
  leave();
  parts.list = id;
  parts.selected = null;
  render();
  parts.tabs[id].focus();
}

function diagnoseBlock(ctx) {
  const command = h("code", {}, "runquill diagnose OWNER/REPO");
  const hint = h("p", { class: "muted" },
    ...fill("Missing a job? Run {command} to check GitHub App access, its job label and delivery.", { command }));
  if (!ctx.can("admin")) return block("Missing a job?", {}, hint);
  const output = statusLine();
  const report = h("pre", { class: "report", "aria-live": "polite", hidden: true });
  const form = h("form", { class: "toolbar" },
    field("GitHub repository", h("input", { name: "repository", required: true, maxLength: 140, placeholder: "OWNER/REPO", autocomplete: "off" })),
    button("Check repository", null, { type: "submit" }));
  submitForm(form, output, async (data) => {
    report.hidden = true;
    const result = await ctx.api.post("./api/diagnose", { repository: data.get("repository") }, { timeout: 10000 });
    report.textContent = result.report;
    report.hidden = false;
    message(output, tr(result.ok ? "Repository check complete." : "Repository check needs attention."), result.ok ? "ok" : "failed");
  });
  return block("Missing a job?", {}, hint, form, output, report);
}

export default {
  id: "queue",
  title: "Queue",

  mount(root, ctx) {
    const tabs = {};
    for (const [id] of lists) {
      tabs[id] = h("button", { type: "button", "aria-pressed": "false", on: { click: () => showList(id) } });
    }
    const filtered = () => {
      leave();
      render();
    };
    const search = h("input", { type: "search", placeholder: tr("Repository or job ID"), on: { input: filtered } });
    const pool = h("select", { on: { change: filtered } }, h("option", { value: "" }, tr("All pools")));
    const detail = h("section", { class: "detail", "aria-labelledby": "job-detail-title", hidden: true });
    const table = createTable({
      label: "Job results",
      key: (job) => job.id,
      rowClass: (job) => job.id === parts?.selected ? "selected" : "",
      highlighted: () => parts?.target.key(),
      columns: [
        { label: "Position", num: true, cell: (job) => job.queue_position ?? "—" },
        { label: "Repository", rowHeader: true, className: "long", cell: (job) => h("button", {
          type: "button", class: "cell-button", "aria-expanded": String(job.id === parts.selected),
          "aria-label": tr("{repo} job {id}", { repo: job.repository, id: job.id }),
          on: { click: () => {
            leave();
            parts.selected = parts.selected === job.id ? null : job.id;
            render();
            if (parts.selected) detail.scrollIntoView({ block: "nearest" });
          } },
        }, job.repository) },
        { label: "Job", className: "id", cell: (job) => "#" + job.id },
        { label: "Pool", cell: (job) => [poolName(job.pool),
          job.runner_os ? h("span", { class: "sub" }, tr("Runner OS {name}", { name: job.runner_os })) : null] },
        { label: "State", cell: (job) => [state(job.status || "CLEANED"),
          job.retry_at ? h("span", { class: "sub" }, tr("Retry time {time}", { time: format.time(job.retry_at) })) : null] },
        { label: "Reason", className: "wrap", cell: (job) => reason(job) || "—" },
        { label: "Queue time", num: true, cell: (job) => format.duration(job.timing?.runquill_queue_seconds) },
        { label: "Host", className: "long", cell: (job) => job.runner_host || "—" },
      ],
    });
    root.append(
      pageHead("Queue"),
      h("div", { class: "block" },
        h("div", { class: "tabs", role: "group", "aria-label": tr("Choose a job list") }, ...Object.values(tabs)),
        h("div", { class: "toolbar" },
          field("Filter jobs", search, { grow: true }),
          field("Pool", pool)),
        table.element,
        detail),
      ctx.demo ? "" : diagnoseBlock(ctx),
    );
    parts = { ctx, table, tabs, search, pool, detail, list: "queue", selected: null, target: linkTarget("queue", "job"), linked: null, insight: null };
    render();
  },

  params(search) {
    if (!parts) return;
    parts.linked = null;
    parts.target.open(search);
    render();
  },

  update() {
    if (parts) render();
  },

  commands() {
    if (!parts) return [];
    return [
      { label: "Show waiting jobs", run: () => showList("queue") },
      { label: "Show assigned jobs", run: () => showList("active") },
      { label: "Show cleaned-up jobs", run: () => showList("recent") },
      (parts.search.value || parts.pool.value) && { label: "Clear job filters", run: () => {
        parts.search.value = "";
        parts.pool.value = "";
        render();
      } },
    ].filter(Boolean);
  },

  unmount() {
    parts = null;
  },
};

