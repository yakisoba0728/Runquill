// Hosts & pools: capacity per worker host (or Kubernetes target) with
// maintenance controls, and the same capacity summed per worker pool.
// #/hosts?host=NAME highlights one host.
import { availableSlots, poolName, workerState } from "../fleet.js";
import { serverText, tr } from "../i18n.js";
import {
  block, button, createKv, createTable, errorText, fill, format, h, linkTarget, message, pageHead, state, statusLine, time,
} from "../ui.js";

let parts = null;

// image_status is set only for local and ssh:// hosts while RUNQUILL_IMAGE_PULL
// is missing or always; the image name and pull error arrive in host.error.
const imageStates = { ready: "Image ready", pulling: "Pulling image", missing: "Image missing", failed: "Image pull failed" };

function pools(hosts) {
  const result = new Map();
  for (const host of hosts) {
    const key = host.pool || "default";
    if (!result.has(key)) result.set(key, { name: key, hosts: 0, ready: 0, reserved: 0, free: 0, occupied: 0, limit: 0 });
    const pool = result.get(key);
    pool.hosts++;
    pool.ready += Number(workerState(host) === "UP");
    pool.reserved += host.reserved_slots;
    pool.occupied += host.occupied_slots;
    pool.limit += host.slot_limit;
    pool.free += availableSlots(host);
  }
  return [...result.values()].sort((a, b) => a.name.localeCompare(b.name));
}

export default {
  id: "hosts",
  title: "Hosts & pools",

  mount(root, ctx) {
    const target = linkTarget("hosts", "host");
    const hostColumns = [
      { label: "Host", className: "id long", cell: (host) => [host.name, host.error ? h("span", { class: "sub" }, serverText(host.error)) : null] },
      { label: "Pool", cell: (host) => poolName(host.pool) },
      { label: "State", cell: (host) => [state(workerState(host)),
        imageStates[host.image_status] ? h("span", { class: "sub" }, tr(imageStates[host.image_status])) : null] },
      { label: "Slot limit", num: true, cell: (host) => format.number(host.slot_limit) },
      { label: "Occupied", num: true, cell: (host) => format.number(host.occupied_slots) },
      { label: "Reserved", num: true, cell: (host) => format.number(host.reserved_slots) },
      { label: "Available", num: true, cell: (host) => format.number(availableSlots(host)) },
    ];
    if (ctx.can("operator")) {
      hostColumns.push({ label: "", cell: (host) => {
        const label = host.cordoned ? "Resume assignments" : "Pause assignments";
        const control = button(label, async () => {
          control.disabled = true;
          try {
            await ctx.api.post("./api/hosts/cordon", { host: host.name, cordoned: !host.cordoned });
          } catch (error) {
            if (error.status !== 401) ctx.toast(errorText(error), "failed");
            control.disabled = false;
            return;
          }
          ctx.toast(tr(host.cordoned ? "New assignments resumed." : "New assignments paused. Existing jobs can finish."));
          await ctx.refresh();
          control.disabled = false;
        }, { small: true, "aria-label": tr("{action} on {host}", { action: tr(label), host: host.name }) });
        return h("div", { class: "row-actions" }, control);
      } });
    }
    const hosts = createTable({ label: "Worker inventory", key: (host) => host.name, columns: hostColumns, highlighted: target.key });
    const poolTable = createTable({
      label: "Worker pools",
      key: (pool) => pool.name,
      columns: [
        { label: "Pool", cell: (pool) => poolName(pool.name) },
        { label: "Ready hosts", num: true, cell: (pool) => tr("{ready} of {total}", { ready: pool.ready, total: pool.hosts }) },
        { label: "Occupied", num: true, cell: (pool) => format.number(pool.occupied) },
        { label: "Reserved", num: true, cell: (pool) => format.number(pool.reserved) },
        { label: "Available", num: true, cell: (pool) => format.number(pool.free) },
        { label: "Slot limit", num: true, cell: (pool) => format.number(pool.limit) },
        { label: "Usage", cell: (pool) => h("progress", {
          class: "meter", max: Math.max(1, pool.limit), value: Math.min(Math.max(1, pool.limit), pool.occupied + pool.reserved),
          "aria-label": tr("{host} capacity", { host: poolName(pool.name) }),
        }) },
      ],
    });
    const hostBlock = block("Hosts", {}, hosts.element);
    const elastic = elasticSection(ctx);
    root.append(pageHead("Hosts & pools"), hostBlock,
      block("Worker pools", { note: "Host capacity before the fleet slot limit applies." }, poolTable.element),
      elastic.element);
    parts = { ctx, target, hosts, poolTable, hostHeading: hostBlock.querySelector("h2"), elastic };
  },

  params(search) {
    if (!parts) return;
    parts.target.open(search);
    this.update(parts.ctx.snapshot());
  },

  update(snapshot) {
    if (!parts) return;
    const fleet = snapshot?.fleet;
    const list = fleet?.hosts || [];
    parts.hostHeading.textContent = tr(fleet?.backend === "kubernetes" ? "Runtime targets" : "Hosts");
    const empty = !fleet ? "Waiting for a fleet snapshot…" : "No host observations yet.";
    parts.hosts.update(list, empty);
    parts.poolTable.update(pools(list), empty);
    parts.target.settle({
      ready: !!fleet, table: parts.hosts,
      found: list.some((host) => host.name === parts.target.key()),
      gone: (name) => parts.ctx.toast(tr("The host {name} is no longer reported.", { name }), ""),
    });
  },

  commands(ctx) {
    if (!parts || !ctx.can("operator")) return [];
    const hosts = ctx.snapshot()?.fleet?.hosts || [];
    return hosts.map((host) => ({
      label: host.cordoned ? "Resume assignments on {host}" : "Pause assignments on {host}",
      values: { host: host.name },
      run: () => {
        const row = [...parts.hosts.element.querySelectorAll("tbody tr")].find((candidate) => candidate.dataset.key === host.name);
        row?.querySelector("button")?.click();
      },
    }));
  },

  unmount() {
    parts?.elastic.stop();
    parts = null;
  },
};

// Elastic limits: GET /api/elastic (elastic.go report()). Read-only; the
// manager decides and, in apply mode, changes runner limits itself.

const elasticModes = { off: "Off", observe: "Observe only", apply: "Apply" };

// Decision reasons come from elastic.go with numbers filled in; serverText
// matches them against the templates in i18n.js.
function reasonText(reason) {
  return reason ? serverText(reason) : "—";
}

const cpus = (value) => tr("{n} CPUs", { n: format.number(Math.round(value * 100) / 100) });
const mib = (value) => format.bytes(value * 1048576);
const amount = (resource, value) => resource === "memory" ? mib(value) : cpus(value);

function elasticSection(ctx) {
  const output = statusLine();
  const summary = createKv([["mode", "Mode"], ["interval", "Check interval"]], { wide: true, label: "Elastic limits" });
  const off = h("p", { class: "muted", hidden: true },
    ...fill("Elastic limits are off. Set {variable} on the manager to see what Runquill would lend, then {apply} to change runner limits.", {
      variable: h("code", {}, "RUNQUILL_ELASTIC=observe"), apply: h("code", {}, "apply"),
    }));
  const hostTable = createTable({
    label: "Elastic surplus per host",
    key: (host) => host.host,
    columns: [
      { label: "Host", className: "id", cell: (host) => [host.host,
        host.memory_overcommitted ? h("span", { class: "sub" }, state("FAILED", "Memory overcommitted")) : null] },
      { label: "Host CPUs", num: true, cell: (host) => format.number(host.cpus) },
      { label: "Surplus CPUs", num: true, cell: (host) => format.number(Math.round(host.surplus_cpus * 100) / 100) },
      { label: "Lent CPUs", num: true, cell: (host) => format.number(Math.round(host.lent_cpus * 100) / 100) },
      { label: "Host memory", num: true, cell: (host) => mib(host.memory_mib) },
      { label: "Surplus memory", num: true, cell: (host) => mib(host.surplus_memory_mib) },
      { label: "Extra memory", num: true, cell: (host) => mib(host.extra_memory_mib) },
      { label: "Held slots", num: true, cell: (host) => format.number(host.held_slots) },
    ],
  });
  const decisionTable = createTable({
    label: "Recent elastic decisions",
    key: (decision) => decision.at + decision.host + decision.runner + decision.resource,
    columns: [
      { label: "Time", num: true, cell: (decision) => time(decision.at, "time") },
      { label: "Runner", className: "id", cell: (decision) => [decision.runner, h("span", { class: "sub" }, decision.host)] },
      { label: "Resource", cell: (decision) => tr(decision.resource === "memory" ? "Memory" : "CPU") },
      { label: "Change", num: true, cell: (decision) => tr("{from} → {to}", {
        from: amount(decision.resource, decision.from), to: amount(decision.resource, decision.to) }) },
      { label: "Reason", className: "wrap", cell: (decision) => [reasonText(decision.reason),
        decision.forced ? h("span", { class: "sub" }, tr("Reclaimed at once to keep host guarantees.")) : null] },
      { label: "Applied", cell: (decision) => decision.error
        ? [state("FAILED"), h("span", { class: "sub" }, decision.error)]
        : decision.applied ? state("DONE", "Applied") : state("", "Observed only", "") },
    ],
  });
  const details = h("div", { hidden: true },
    h("h3", { class: "subhead" }, tr("Surplus per host")), hostTable.element,
    h("h3", { class: "subhead" }, tr("Recent decisions")), decisionTable.element);
  const element = block("Elastic limits", { note: "Runners may borrow CPU and memory that no other runner was promised." },
    summary.element, off, details, output);

  let timer = 0;
  let stopped = false;
  async function load() {
    clearTimeout(timer);
    let next = 15000;
    try {
      const report = await ctx.api.get("./api/elastic");
      if (stopped) return;
      message(output, "");
      const mode = elasticModes[report.mode] ? report.mode : "off";
      summary.set("mode", state(mode === "apply" ? "UP" : "", elasticModes[mode], mode === "apply" ? "ok" : ""),
        mode === "observe" ? tr("Decisions are recorded but runner limits are not changed.") : null);
      summary.show("interval", mode !== "off");
      if (report.interval_seconds) summary.set("interval", format.duration(report.interval_seconds));
      off.hidden = mode !== "off";
      details.hidden = mode === "off";
      hostTable.update(report.hosts || [], "No hosts sampled yet.");
      decisionTable.update(report.decisions || [], "No decisions yet.");
      if (report.interval_seconds) next = Math.max(5000, report.interval_seconds * 1000);
    } catch (error) {
      if (stopped || error.status === 401) return;
      message(output, errorText(error), "failed");
      next = 60000;
    }
    if (!stopped && !ctx.demo) timer = setTimeout(() => void load(), next);
  }
  void load();
  return { element, stop() { stopped = true; clearTimeout(timer); } };
}
