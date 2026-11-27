// Usage: finished-job totals from GET /api/usage over a range of UTC days,
// grouped by day, repository, pool or host, with a per-day column chart, and
// CPU and memory right-sizing from GET /api/usage/rightsizing.
import { poolName } from "../fleet.js";
import { language, tr } from "../i18n.js";
import {
  block, button, createKv, createTable, errorText, field, format, h, loadStyle, message, pageHead, select, settled, state, statusLine,
} from "../ui.js";

const maxDays = 400;
const svgNS = "http://www.w3.org/2000/svg";
let parts = null;

// Metrics the bar column and the chart can show. value() reads a totals row;
// text() formats it.
const metrics = {
  jobs: { label: "Finished jobs", value: (row) => row.jobs, text: (row) => format.number(row.jobs) },
  failed: { label: "Failed", value: (row) => row.failed, text: (row) => format.number(row.failed) },
  runner: { label: "Runner time", value: (row) => row.runner_seconds, text: (row) => format.duration(row.runner_seconds) },
  p50: { label: "Median wait", value: (row) => row.wait_count ? row.wait_p50_seconds : 0, text: (row) => waitText(row, row.wait_p50_seconds) },
  p95: { label: "95th percentile wait", value: (row) => row.wait_count ? row.wait_p95_seconds : 0, text: (row) => waitText(row, row.wait_p95_seconds) },
};

const groupLabels = { day: "Day", repository: "Repository", pool: "Pool", host: "Host" };
const groupCommands = { day: "Group usage by day", repository: "Group usage by repository", pool: "Group usage by pool", host: "Group usage by host" };
const tableLabels = { day: "Usage by day", repository: "Usage by repository", pool: "Usage by pool", host: "Usage by host" };

// Wait fields are 0 when no finished job in the row had a known wait.
function waitText(row, seconds) {
  return row.wait_count ? format.duration(seconds) : "—";
}

function svg(tag, attributes = {}, ...children) {
  const element = document.createElementNS(svgNS, tag);
  for (const [name, value] of Object.entries(attributes)) element.setAttribute(name, String(value));
  element.append(...children);
  return element;
}

// UTC calendar days as YYYY-MM-DD, the server's day keys.
function utcDay(offset = 0) {
  const date = new Date();
  date.setUTCDate(date.getUTCDate() + offset);
  return date.toISOString().slice(0, 10);
}

function dayLabel(key, long = false) {
  const date = new Date(key + "T00:00:00Z");
  if (Number.isNaN(date.getTime())) return key;
  return date.toLocaleDateString(language, long
    ? { timeZone: "UTC", year: "numeric", month: "short", day: "numeric" }
    : { timeZone: "UTC", month: "short", day: "numeric", weekday: "short" });
}

function keyLabel(group, key) {
  if (group === "day") return dayLabel(key);
  if (group === "pool") return poolName(key);
  if (!key) return tr(group === "host" ? "No host" : "Unknown");
  return key;
}

function daysBetween(from, to) {
  return Math.round((Date.parse(to + "T00:00:00Z") - Date.parse(from + "T00:00:00Z")) / 86400000);
}

function applyPreset(form) {
  const days = Number(form.elements.range.value);
  if (!days) return;
  form.elements.to.value = utcDay();
  form.elements.from.value = utcDay(1 - days);
}

function chart(series, metric) {
  const values = series.map((row) => metric.value(row));
  const top = Math.max(1, ...values);
  const width = Math.max(1, series.length) * 10;
  const height = 100;
  const plot = svg("svg", {
    class: "usage-chart", viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: "none", role: "img",
    "aria-label": tr("{metric} per day, highest {value}", { metric: tr(metric.label), value: metric.text(series[values.indexOf(top)] || {}) }),
  });
  plot.append(svg("line", { class: "axis", x1: 0, y1: height - 0.5, x2: width, y2: height - 0.5 }));
  series.forEach((row, index) => {
    const value = values[index];
    const bar = Math.max(value ? 1.5 : 0, (value / top) * (height - 4));
    const rect = svg("rect", { x: index * 10 + 1.5, y: height - bar, width: 7, height: bar, class: row.failed && metric === metrics.failed ? "bar failed" : "bar" });
    rect.append(svg("title", {}, dayLabel(row.key, true) + ": " + metric.text(row)));
    plot.append(rect);
  });
  const peak = series[values.indexOf(top)];
  return h("figure", { class: "usage-figure" },
    h("div", { class: "usage-scale muted" }, h("span", {}, peak && values.some(Boolean) ? metric.text(peak) : "0"), h("span", {}, "0")),
    plot,
    h("figcaption", { class: "usage-axis muted" },
      h("span", {}, series.length ? dayLabel(series[0].key, true) : ""),
      h("span", {}, tr("{metric} per day (UTC)", { metric: tr(metric.label) })),
      h("span", {}, series.length ? dayLabel(series[series.length - 1].key, true) : "")));
}

function table(group, metric, rows) {
  const top = Math.max(1, ...rows.map((row) => metric.value(row)));
  const view = createTable({
    label: tableLabels[group],
    key: (row) => row.key || "-",
    columns: [
      { label: groupLabels[group], className: group === "day" ? null : "id long", cell: (row) => keyLabel(group, row.key) },
      { label: "Finished jobs", num: true, cell: (row) => format.number(row.jobs) },
      { label: "Failed", num: true, cell: (row) => format.number(row.failed) },
      { label: "Runner time", num: true, cell: (row) => format.duration(row.runner_seconds) },
      { label: "Median wait", num: true, cell: (row) => waitText(row, row.wait_p50_seconds) },
      { label: "95th percentile wait", num: true, cell: (row) => waitText(row, row.wait_p95_seconds) },
      { label: "Relative to largest", className: "usage-bar", cell: (row) => h("progress", {
        class: "meter", max: top, value: metric.value(row),
        "aria-label": tr("{metric} for {key}: {value}", { metric: tr(metric.label), key: keyLabel(group, row.key), value: metric.text(row) }),
      }) },
    ],
  });
  view.update(rows, "No finished jobs in this range.");
  return view.element;
}

function render() {
  if (!parts?.data) return;
  const { data, form, totals, results, notice, range } = parts;
  const metric = metrics[form.elements.metric.value] || metrics.jobs;
  const t = data.totals;
  range.textContent = tr("{from} to {to}, UTC days", { from: dayLabel(data.from, true), to: dayLabel(data.to, true) });
  totals.set("jobs", format.number(t.jobs));
  totals.set("failed", format.number(t.failed), t.jobs ? tr("{percent} of jobs", { percent: (t.failed / t.jobs).toLocaleString(language, { style: "percent", maximumFractionDigits: 1 }) }) : null);
  totals.set("runner", format.duration(t.runner_seconds), t.jobs ? tr("{time} per job", { time: format.duration(t.runner_seconds / t.jobs) }) : null);
  totals.set("waits", format.number(t.wait_count));
  totals.set("average", waitText(t, t.wait_avg_seconds));
  totals.set("p50", waitText(t, t.wait_p50_seconds));
  totals.set("p95", waitText(t, t.wait_p95_seconds));
  message(notice, data.truncated ? tr("Showing the {n} busiest groups. Totals include every group.", { n: format.number(data.series.length) }) : "", "waiting");
  const rows = data.group === "day" ? [...data.series].reverse() : data.series;
  results.replaceChildren(...[
    data.group === "day" && t.jobs ? chart(data.series, metric) : null,
    table(data.group, metric, t.jobs ? rows : []),
  ].filter(Boolean));
}

async function load() {
  const current = parts;
  const { form, output } = current;
  const from = form.elements.from.value;
  const to = form.elements.to.value;
  message(output, "");
  if (!from || !to || to < from || daysBetween(from, to) >= maxDays) {
    message(output, tr("Choose a start date on or before the end date, at most 400 days apart."), "failed");
    return;
  }
  const params = new URLSearchParams({ from, to, group: form.elements.group.value });
  const repository = form.elements.repository.value.trim();
  if (repository) params.set("repository", repository);
  const ticket = ++current.ticket;
  current.status.textContent = tr("Loading usage…");
  try {
    const data = await current.ctx.api.get("./api/usage?" + params);
    if (parts !== current || ticket !== current.ticket) return;
    current.data = data;
    current.status.textContent = "";
    render();
  } catch (error) {
    if (parts !== current || ticket !== current.ticket) return;
    current.status.textContent = "";
    if (error.status !== 401) message(output, errorText(error), "failed");
  }
}

// Right-sizing verdicts (job_resources.go rightsizingResource.finish), as a
// state tone and label: a tight limit is a problem, an oversized one a waste.
const verdicts = {
  tight: ["failed", "Tight"],
  oversized: ["waiting", "Oversized"],
  ok: ["ok", "Fits"],
  too_few_jobs: ["", "Too few jobs"],
  unlimited: ["", "No limit"],
};

const coreText = (value) => tr("{n} CPUs", { n: format.number(Math.round(value * 100) / 100) });
const mibText = (value) => format.bytes(value * 1048576);

function resourceCell(resource, text) {
  if (!resource) return h("span", { class: "muted" }, tr("Not sampled"));
  const [tone, label] = verdicts[resource.verdict] || ["", resource.verdict];
  const peak = resource.limit > 0 ? tr("Peak {peak} of {limit}", { peak: text(resource.peak_max), limit: text(resource.limit) })
    : tr("Peak {peak}", { peak: text(resource.peak_max) });
  const ratio = Number.isFinite(resource.peak_ratio)
    ? tr("Highest peak {percent} of its limit", { percent: resource.peak_ratio.toLocaleString(language, { style: "percent", maximumFractionDigits: 0 }) }) : null;
  return [state("", label, tone), h("span", { class: "sub" }, peak), ratio ? h("span", { class: "sub" }, ratio) : null];
}

// The job group key is "owner/repo runner-label" (one label per workflow job).
function rightsizingKey(group, key) {
  if (group === "pool") return poolName(key);
  const [repository, label] = key.split(" ");
  return [repository, label ? h("span", { class: "sub" }, label) : null];
}

function rightsizingSection(ctx) {
  const form = h("form", { class: "toolbar", "aria-label": tr("Right-sizing filters") },
    field("Group by", select("group", [["pool", "Pool"], ["job", "Workflow job"]])),
    field("Finished in", select("days", [1, 2, 3, 5, 7].map((days) => [String(days), days === 1 ? "Last day" : tr("Last {n} days", { n: days })]))),
    button("Apply filters", null, { type: "submit" }));
  form.elements.days.value = "7";
  const output = statusLine();
  const status = h("span", { class: "muted", role: "status" });
  let group = "pool";
  const table = createTable({
    label: "CPU and memory right-sizing",
    key: (row) => row.key || "-",
    columns: [
      { label: "Group", rowHeader: true, className: "id long", cell: (row) => rightsizingKey(group, row.key) },
      { label: "Sampled jobs", num: true, cell: (row) => tr("{sampled} of {total}", { sampled: format.number(row.sampled_jobs), total: format.number(row.jobs) }) },
      { label: "CPU", className: "wrap", cell: (row) => resourceCell(row.cpu, coreText) },
      { label: "Memory", className: "wrap", cell: (row) => resourceCell(row.memory, mibText) },
      { label: "Memory kills", num: true, cell: (row) => row.oom_kills ? state("FAILED", format.number(row.oom_kills), "failed") : "0" },
    ],
  });
  let ticket = 0;
  async function load() {
    const current = ++ticket;
    const params = new URLSearchParams({ group: form.elements.group.value, days: form.elements.days.value });
    message(output, "");
    status.textContent = tr("Loading right-sizing…");
    try {
      const data = await ctx.api.get("./api/usage/rightsizing?" + params);
      if (current !== ticket || !table.element.isConnected) return;
      group = data.group;
      table.update(data.groups, "No finished jobs with resource samples in this range.");
      status.textContent = "";
      message(output, data.truncated ? tr("Showing the {n} groups with the most jobs.", { n: format.number(data.groups.length) }) : "", "waiting");
    } catch (error) {
      if (current !== ticket) return;
      status.textContent = "";
      if (error.status !== 401) message(output, errorText(error), "failed");
    }
  }
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    void load();
  });
  for (const name of ["group", "days"]) form.elements[name].addEventListener("change", settled(() => void load()));
  table.update([], "Loading…");
  void load();
  return block("Right-sizing", {
    actions: [status],
    note: "Compares recent jobs' CPU and memory peaks with their limits. Runquill samples runners on the local Docker host only.",
  }, form, table.element, output,
  h("p", { class: "muted usage-note" }, tr("Tight: a peak above 80% of the limit, or a memory kill. Oversized: every peak below 25% of the limit, over at least 3 sampled jobs.")));
}

export default {
  id: "usage",
  title: "Usage",

  mount(root, ctx) {
    loadStyle("usage");
    const form = h("form", { class: "toolbar", "aria-label": tr("Usage filters") },
      field("Range", select("range", [["2", "Yesterday and today"], ["7", "Last 7 days"], ["30", "Last 30 days"],
        ["90", "Last 90 days"], ["", "Custom dates"]])),
      field("From (UTC)", h("input", { type: "date", name: "from", required: true })),
      field("To (UTC)", h("input", { type: "date", name: "to", required: true })),
      field("Group by", select("group", Object.entries(groupLabels))),
      field("Repository", h("input", { type: "search", name: "repository", placeholder: "OWNER/REPO", maxLength: 140 }), { grow: true }),
      field("Bar shows", select("metric", Object.entries(metrics).map(([id, metric]) => [id, metric.label]))),
      button("Apply filters", null, { type: "submit" }));
    form.elements.range.value = "30";
    applyPreset(form);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void load();
    });
    // Custom dates leaves the dates for the person to edit and apply; focus stays on the select.
    form.elements.range.addEventListener("change", settled(() => {
      if (!form.elements.range.value) return;
      applyPreset(form);
      void load();
    }));
    for (const name of ["from", "to"]) form.elements[name].addEventListener("change", () => { form.elements.range.value = ""; });
    form.elements.group.addEventListener("change", settled(() => void load()));
    form.elements.metric.addEventListener("change", () => render());

    const totals = createKv([
      ["jobs", "Finished jobs"], ["failed", "Failed in Runquill"], ["runner", "Runner time"],
      ["waits", "Jobs with a known wait"], ["average", "Average wait"], ["p50", "Median wait"], ["p95", "95th percentile wait"],
    ], { label: "Usage totals" });
    const range = h("p", { class: "muted" });
    const status = h("span", { class: "muted", role: "status" });
    const output = statusLine();
    const notice = statusLine();
    const results = h("div", { class: "usage-results" });

    root.append(
      pageHead("Usage"),
      h("div", { class: "block" }, form, output),
      block("Totals", { actions: [range] }, h("div", { class: "usage-totals" }, totals.element)),
      block("Breakdown", { actions: [status] }, notice, results),
      rightsizingSection(ctx),
      h("p", { class: "block muted usage-note" }, tr("Counts finished jobs by the UTC day they ended. Failed means Runquill gave up on the job or it was cancelled; GitHub's own job conclusion is not counted. Runner time runs from provisioning to cleanup. Waits run from queueing to provisioning and are estimated from histogram buckets.")),
    );
    parts = { ctx, form, totals, range, status, output, notice, results, data: null, ticket: 0 };
    void load();
  },

  commands() {
    if (!parts) return [];
    const range = (days, label) => ({ label, run: () => {
      if (!parts) return;
      parts.form.elements.range.value = String(days);
      applyPreset(parts.form);
      void load();
    } });
    return [
      range(7, "Show usage for the last 7 days"),
      range(30, "Show usage for the last 30 days"),
      range(90, "Show usage for the last 90 days"),
      ...Object.entries(groupLabels).filter(([id]) => id !== parts.form.elements.group.value).map(([id]) => ({
        label: groupCommands[id], run: () => {
          if (!parts) return;
          parts.form.elements.group.value = id;
          void load();
        },
      })),
    ];
  },

  unmount() {
    parts = null;
  },
};
