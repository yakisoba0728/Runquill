// Runners: every runner container or Pod Runquill observes, and leases it
// holds without an observation. #/runners?runner=NAME highlights one.
import { poolName, runnerRows, runnerState } from "../fleet.js";
import { tr } from "../i18n.js";
import { createTable, externalLink, h, isGitHubJobURL, linkTarget, pageHead, time } from "../ui.js";

// Logs can be read only for runners on local and ssh:// Docker hosts (runner_logs.go).
const logsReadable = (runner) => runner.job_id && runner.host && runner.host !== "kubernetes" && !runner.host.startsWith("agent://");

let parts = null;

export default {
  id: "runners",
  title: "Runners",

  mount(root, ctx) {
    const target = linkTarget("runners", "runner");
    const summary = h("p", { class: "muted" });
    const columns = [
      { label: "Runner", className: "id", cell: (runner) => runner.name },
      { label: "Host", className: "long", cell: (runner) => runner.host || "—" },
      { label: "Pool", cell: (runner) => poolName(runner.pool || runner.job?.pool) },
      { label: "Runtime state", cell: runnerState },
      // A start time, not a running duration, so rows stay unchanged between snapshots.
      { label: "Provisioned", num: true, cell: (runner) => time(runner.job?.timing?.provisioned_at, "date") },
      { label: "Runner image", className: "long", cell: (runner) => {
        const image = runner.runner_image || runner.job?.runner_image;
        const os = runner.runner_os || runner.job?.runner_os;
        return [image ? h("span", { class: "mono" }, image) : "—",
          os ? h("span", { class: "sub" }, tr("Runner OS {name}", { name: os })) : null];
      } },
      { label: "GitHub job", className: "long", cell: (runner) => {
        const job = runner.job;
        if (!job) return h("span", { class: "muted" }, tr("Untracked runner"));
        const label = job.repository + " #" + job.id;
        return isGitHubJobURL(job.url) ? externalLink(label, job.url) : label;
      } },
    ];
    // The Logs screen is for operators and admins; the sample panel shows it to everyone.
    if (ctx.demo || ctx.can("operator")) {
      columns.push({ label: "", cell: (runner) => logsReadable(runner) ? h("a", {
        href: "#/logs?runner=" + encodeURIComponent(runner.name), "aria-label": tr("View logs for {runner}", { runner: runner.name }),
      }, tr("View logs")) : null });
    }
    const table = createTable({ label: "Runner inventory", key: (runner) => runner.name, highlighted: target.key, columns });

    root.append(pageHead("Runners"), h("div", { class: "block" }, h("div", { class: "block-head" }, summary), table.element));
    parts = { ctx, summary, table, target };
  },

  params(search) {
    if (!parts) return;
    parts.target.open(search);
    this.update(parts.ctx.snapshot());
  },

  update(snapshot) {
    if (!parts) return;
    const inventory = runnerRows(snapshot);
    const observed = inventory.filter((runner) => runner.runtime_state).length;
    parts.summary.textContent = snapshot?.fleet
      ? tr("{observed} observed, {leases} unobserved leases", { observed, leases: inventory.length - observed }) : "";
    parts.table.update(inventory, !snapshot?.fleet ? "Waiting for a fleet snapshot…"
      : snapshot.fleet.docker_error ? "Runner observations are unavailable." : "No runners or retained leases.");
    parts.target.settle({
      ready: !!snapshot?.fleet, table: parts.table,
      found: inventory.some((runner) => runner.name === parts.target.key()),
      gone: (name) => parts.ctx.toast(tr("The runner {name} is no longer listed.", { name }), ""),
    });
  },

  unmount() {
    parts = null;
  },
};
