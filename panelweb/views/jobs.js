// Jobs: finished and waiting jobs from GET /api/jobs (kept seven days), with
// filters, paging and retry/cancel for waiting jobs.
import { jobActionButtons, poolName } from "../fleet.js";
import { serverText, tr } from "../i18n.js";
import {
  button, createTable, errorText, externalLink, field, format, h, intro, isGitHubJobURL, message, pageHead, select, state, statusLine,
  time,
} from "../ui.js";

const pageSize = 25;
let parts = null;

// runnerOS is the os__NAME__ selector of a job's label as a sub-line, or null.
function runnerOS(job) {
  return job.runner_os ? h("span", { class: "sub" }, tr("Runner OS {name}", { name: job.runner_os })) : null;
}

function outcomeState(job) {
  if (job.outcome === "waiting") return job.retry_at ? "RETRY" : "WAITING";
  return job.outcome.toUpperCase();
}

function query(form, more) {
  const data = new FormData(form);
  const params = new URLSearchParams({ limit: String(pageSize) });
  const repository = String(data.get("repository") || "").trim();
  if (repository) params.set("repository", repository);
  if (data.get("outcome")) params.set("outcome", String(data.get("outcome")));
  const range = Number(data.get("range"));
  if (range) params.set("since", String(Math.floor(Date.now() / 1000) - range));
  if (more && parts.cursor) params.set("before", String(parts.cursor));
  return params;
}

async function load(more = false) {
  const current = parts;
  if (!current) return;
  const ticket = ++current.ticket;
  message(current.output, "");
  current.more.disabled = true;
  if (!more) {
    current.loaded = false;
    current.rows = [];
    render();
  }
  try {
    const page = await current.ctx.api.get("./api/jobs?" + query(current.form, more));
    if (parts !== current || ticket !== current.ticket) return;
    current.rows = more ? current.rows.concat(page.jobs) : page.jobs;
    current.cursor = page.next || 0;
    current.loaded = true;
    render();
  } catch (error) {
    if (parts !== current || ticket !== current.ticket || error.status === 401) return;
    current.failed = true;
    render();
    message(current.output, errorText(error), "failed");
  } finally {
    current.more.disabled = false;
  }
}

function render() {
  const { table, rows, loaded, failed, more, count, cursor } = parts;
  table.update(rows, loaded ? "No jobs match these filters." : failed ? "Job history is unavailable." : "Loading job history…");
  more.hidden = !cursor;
  count.textContent = loaded ? tr("{n} shown", { n: format.number(rows.length) }) : "";
  parts.failed = false;
}

function clearFilters() {
  if (!parts) return;
  parts.form.reset();
  void load();
}

export default {
  id: "jobs",
  title: "Jobs",

  mount(root, ctx) {
    const form = h("form", { class: "toolbar", "aria-label": tr("Job filters") },
      field("Repository", h("input", { type: "search", name: "repository", placeholder: "OWNER/REPO", maxLength: 140 }), { grow: true }),
      field("Outcome", select("outcome", [["", "All outcomes"], ["waiting", "Waiting"], ["running", "Running"],
        ["finished", "Finished"], ["failed", "Failed"]])),
      field("Time range", select("range", [["", "Any time"], ["3600", "Last hour"], ["86400", "Last 24 hours"],
        ["604800", "Last 7 days"]])),
      button("Apply filters", null, { type: "submit" }));
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void load();
    });
    for (const name of ["outcome", "range"]) form.elements[name].addEventListener("change", () => void load());
    const table = createTable({
      label: "Job history",
      key: (job) => job.id,
      columns: [
        { label: "Repository", className: "long", cell: (job) => isGitHubJobURL(job.url)
          ? externalLink(job.repository, job.url, tr("{repo} job {id} on GitHub", { repo: job.repository, id: job.id }))
          : job.repository },
        { label: "Job", className: "id", cell: (job) => ["#" + job.id, job.runner_label ? h("span", { class: "sub" }, job.runner_label) : null] },
        { label: "Outcome", className: "wrap", cell: (job) => {
          const reason = job.failure || job.diagnostic?.message || (job.outcome === "waiting" ? job.retry_reason : "");
          return [state(outcomeState(job)),
            reason ? h("span", { class: "sub" }, tr(job.failure || job.diagnostic ? "Reason: {reason}" : "Last retry: {reason}", { reason: serverText(reason) })) : null,
            job.retry_at ? h("span", { class: "sub" }, tr("Retry time {time}", { time: format.time(job.retry_at) })) : null];
        } },
        { label: "Pool", cell: (job) => [job.pool ? poolName(job.pool) : "—", runnerOS(job)] },
        { label: "Host", className: "long", cell: (job) => job.host || "—" },
        { label: "Queued", num: true, cell: (job) => time(job.queued_at) },
        { label: "Started", num: true, cell: (job) => time(job.started_at) },
        { label: "Finished", num: true, cell: (job) => time(job.finished_at) },
        { label: "Queue time", num: true, cell: (job) => format.duration(job.wait_seconds) },
        ctx.can("operator") && { label: "", cell: (job) => {
          const actions = job.outcome === "waiting" ? jobActionButtons(ctx, job, async () => {
            await load();
            void ctx.refresh();
          }) : [];
          return actions.length ? h("div", { class: "row-actions" }, ...actions) : "";
        } },
      ].filter(Boolean),
    });
    const more = button("Load older jobs", () => void load(true));
    const count = h("span");
    const output = statusLine();
    root.append(
      pageHead("Jobs"),
      intro("Runquill keeps finished jobs for seven days. Job logs stay on GitHub."),
      h("div", { class: "block" },
        form, table.element,
        h("div", { class: "table-foot" }, count, more),
        output),
    );
    parts = { ctx, form, table, more, count, output, rows: [], cursor: 0, loaded: false, failed: false, ticket: 0 };
    render();
    void load();
  },

  commands() {
    if (!parts) return [];
    return [
      { label: "Clear job filters", run: clearFilters },
      parts.cursor && !parts.more.disabled && { label: "Load older jobs", run: () => void load(true) },
    ].filter(Boolean);
  },

  unmount() {
    parts = null;
  },
};
