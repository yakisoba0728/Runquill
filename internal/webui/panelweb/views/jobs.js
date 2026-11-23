// Jobs: finished and waiting jobs from GET /api/jobs (kept seven days), with
// filters, paging and retry/cancel for waiting jobs. Selecting a job ID opens
// its stage timing and resources from GET /api/jobs/{id}; #/jobs?job=ID too.
import { jobActionButtons, poolName } from "../fleet.js";
import { serverText, tr } from "../i18n.js";
import {
  button, createKv, createTable, errorText, externalLink, field, format, h, intro, isGitHubJobURL, message, pageHead, select, state,
  statusLine, time,
} from "../ui.js";
import { resourceView, timingView } from "./jobinsight.js";

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

// openDetail loads one job and shows it under the table. focus moves to its
// heading once it arrives, for a person who chose it from the list.
async function openDetail(id, focus = true) {
  const current = parts;
  if (!current) return;
  current.selected = id;
  current.table.update(current.rows);
  const ticket = ++current.detailTicket;
  const { detail } = current;
  detail.hidden = false;
  detail.replaceChildren(h("p", { class: "muted detail-loading" }, tr("Loading job #{id}…", { id })));
  let job;
  try {
    job = await current.ctx.api.get("./api/jobs/" + encodeURIComponent(id));
  } catch (error) {
    if (parts !== current || ticket !== current.detailTicket || error.status === 401) return;
    detail.replaceChildren(detailHead(id, null), h("p", { class: "detail-body error-text" }, errorText(error)));
    if (focus) detail.querySelector("h2").focus();
    return;
  }
  if (parts !== current || ticket !== current.detailTicket) return;
  const facts = createKv([["outcome", "Outcome"], ["pool", "Pool"], ["host", "Host"], ["label", "Runner label"], ["os", "Runner OS"],
    ["queued", "Queued"], ["started", "Started"], ["finished", "Finished"]], { wide: true, label: "Job details" });
  facts.set("outcome", state(outcomeState(job)));
  facts.set("pool", job.pool ? poolName(job.pool) : "—");
  facts.set("host", job.host || "—");
  facts.set("label", job.runner_label ? h("code", {}, job.runner_label) : "—");
  facts.set("os", job.runner_os || "—");
  facts.set("queued", format.date(job.queued_at));
  facts.set("started", format.date(job.started_at));
  facts.set("finished", format.date(job.finished_at));
  const why = job.failure || job.diagnostic?.message || (job.outcome === "waiting" ? job.retry_reason : "");
  detail.replaceChildren(detailHead(id, job),
    h("div", { class: "detail-body" },
      why ? h("p", { class: "reason" }, serverText(why)) : null,
      h("div", {}, h("h3", {}, tr("Stage timing")), timingView(job.timing || {}),
        h("h3", { class: "spaced" }, tr("Resources")), resourceView(job.resources)),
      h("div", {}, h("h3", {}, tr("Details")), facts.element)));
  if (focus) detail.querySelector("h2").focus();
}

function detailHead(id, job) {
  return h("div", { class: "detail-head" },
    h("h2", { id: "job-history-title", tabindex: "-1" }, tr("Job #{id}", { id })),
    job ? h("span", { class: "muted" }, job.repository) : null,
    job && isGitHubJobURL(job.url) ? externalLink(tr("Open in GitHub"), job.url) : null,
    h("div", { class: "actions" }, button("Close", closeDetail, { small: true, "aria-label": tr("Close job #{id}", { id }) })));
}

function closeDetail() {
  if (!parts) return;
  const id = parts.selected;
  parts.selected = null;
  parts.detailTicket++;
  parts.detail.hidden = true;
  parts.detail.replaceChildren();
  parts.table.update(parts.rows);
  if (location.hash.startsWith("#/jobs?")) history.replaceState(null, "", "#/jobs");
  [...parts.table.element.querySelectorAll("tbody tr")].find((row) => row.dataset.key === String(id))?.querySelector(".cell-button")?.focus();
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
      rowClass: (job) => job.id === parts?.selected ? "selected" : "",
      columns: [
        { label: "Repository", className: "long", cell: (job) => isGitHubJobURL(job.url)
          ? externalLink(job.repository, job.url, tr("{repo} job {id} on GitHub", { repo: job.repository, id: job.id }))
          : job.repository },
        { label: "Job", className: "id", cell: (job) => [h("button", {
          type: "button", class: "cell-button", "aria-expanded": String(job.id === parts?.selected), "aria-controls": "job-history-detail",
          "aria-label": tr("Details of job {id}", { id: job.id }),
          on: { click: () => job.id === parts.selected ? closeDetail() : void openDetail(job.id) },
        }, "#" + job.id), job.runner_label ? h("span", { class: "sub" }, job.runner_label) : null] },
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
    const detail = h("section", { class: "detail", id: "job-history-detail", "aria-labelledby": "job-history-title", hidden: true });
    root.append(
      pageHead("Jobs"),
      intro("Runquill keeps finished jobs for seven days. Job logs stay on GitHub. Select a job ID for its stage timing and resources."),
      h("div", { class: "block" },
        form, table.element,
        h("div", { class: "table-foot" }, count, more),
        output, detail),
    );
    parts = { ctx, form, table, more, count, output, detail, rows: [], cursor: 0, loaded: false, failed: false, ticket: 0,
      selected: null, detailTicket: 0 };
    render();
    void load();
  },

  params(search) {
    const id = Number(search.get("job"));
    if (parts && Number.isSafeInteger(id) && id > 0) void openDetail(id);
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
