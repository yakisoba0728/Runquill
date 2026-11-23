// Overview: fleet capacity, host health, GitHub delivery, first-job progress
// and the jobs waiting longest.
import { pendingJob, poolName, summary } from "../fleet.js";
import { serverText, tr } from "../i18n.js";
import { block, createKv, createTable, externalLink, fill, format, h, isGitHubJobURL, pageHead, state } from "../ui.js";

let parts = null;

function count(value, tone) {
  return value && tone ? h("span", { class: "state", dataset: { state: tone } }, format.number(value)) : format.number(value);
}

export default {
  id: "overview",
  title: "Overview",

  mount(root, ctx) {
    const capacity = createKv([["running", "Running runners"], ["queued", "Queued jobs"], ["wait", "Longest observed wait"],
      ["slots", "Slots in use"]], { label: "Fleet capacity" });
    const hosts = createKv([["total", "Worker hosts"], ["up", "Ready"], ["offline", "Offline"], ["unavailable", "Unavailable"],
      ["cordoned", "On maintenance"]], { label: "Host health" });
    const github = createKv([["scope", "Configured access"], ["public", "Public repositories"], ["app", "App connection"], ["webhook", "Last matched webhook"],
      ["poll", "Last successful poll"]], { label: "GitHub connection" });
    const steps = [
      ["app", "Connect the GitHub App"],
      ["seen", "Run the first workflow"],
      ["clean", "Confirm cleanup"],
    ].map(([key, title]) => {
      const marker = h("span", { class: "state" });
      const note = h("p");
      const item = h("li", {}, marker, h("h3", {}, tr(title)), note);
      return { key, marker, note, item };
    });
    const firstLink = externalLink(tr("View first job on GitHub"), "#");
    firstLink.hidden = true;
    const waiting = createTable({
      label: "Jobs waiting for a runner",
      key: (job) => job.id,
      empty: "The queue is empty.",
      columns: [
        { label: "Position", num: true, cell: (job) => job.queue_position ?? "—" },
        { label: "Repository", rowHeader: true, className: "long", cell: (job) => h("a", {
          href: "#/queue?job=" + encodeURIComponent(job.id), "aria-label": tr("{repo} job {id}", { repo: job.repository, id: job.id }),
        }, job.repository) },
        { label: "Job", className: "id", cell: (job) => "#" + job.id },
        { label: "Pool", cell: (job) => poolName(job.pool) },
        { label: "State", cell: (job) => state(job.status) },
        { label: "Reason", className: "wrap", cell: (job) => serverText(job.diagnostic?.message || job.retry_reason || "") || "—" },
        { label: "Queue time", num: true, cell: (job) => format.duration(job.timing?.runquill_queue_seconds) },
      ],
    });
    const queueLink = h("a", { href: "#/queue" }, tr("Open queue"));
    const more = h("p", { class: "table-foot", hidden: true });
    root.append(
      pageHead("Overview"),
      h("div", { class: "split" },
        block("Capacity", {}, capacity.element),
        block("Hosts", {}, hosts.element)),
      h("div", { class: "split" },
        block("GitHub connection", {}, github.element),
        block("First job setup", {
          actions: [externalLink(tr("Setup guide"), "https://github.com/yakisoba0728/Runquill#verify-your-first-job")],
        }, h("ol", { class: "checklist" }, ...steps.map((step) => step.item)), h("p", { class: "table-foot" }, firstLink))),
      block("Waiting jobs", { actions: [queueLink] }, waiting.element, more),
    );
    parts = { capacity, hosts, github, steps, firstLink, waiting, more, demo: ctx.demo };
  },

  update(snapshot) {
    if (!parts) return;
    const counts = summary(snapshot);
    const { capacity, hosts, github } = parts;
    capacity.set("running", counts ? format.number(counts.running) : "—");
    capacity.set("queued", counts ? count(counts.queued, "waiting") : "—",
      counts && tr("{ready} ready, {retrying} retrying", { ready: counts.ready, retrying: counts.retrying }));
    capacity.set("wait", counts && counts.oldestWait >= 0 ? format.duration(counts.oldestWait) : "—");
    capacity.set("slots", counts ? tr("{used} of {limit}", { used: format.number(counts.used), limit: format.number(counts.limit) }) : "—",
      counts && [tr("{n} free", { n: counts.free }), tr("{n} reserved", { n: counts.reserved }),
        counts.unassigned ? tr("{n} unassigned", { n: counts.unassigned }) : ""].filter(Boolean).join(", "));
    hosts.label("total", counts?.kubernetes ? "Runtime targets" : "Worker hosts");
    const known = counts?.hostsKnown;
    hosts.set("total", known ? format.number(counts.hosts) : "—");
    hosts.set("up", known ? count(counts.hostsUp, "ok") : "—");
    hosts.set("offline", known ? count(counts.hostsOffline, "offline") : "—");
    hosts.set("unavailable", known ? count(counts.hostsUnavailable, "failed") : "—");
    hosts.set("cordoned", known ? count(counts.hostsCordoned, "waiting") : "—");

    const scope = snapshot?.fleet?.repository_scope;
    github.show("scope", scope === "selected" || scope === "all");
    github.set("scope", tr(scope === "selected" ? "Only selected repositories" : "All repositories"));
    // RUNQUILL_ALLOW_PUBLIC_REPOSITORIES is a deployment setting, so the panel only reports it.
    github.show("public", !!snapshot?.fleet?.public_repositories);
    github.set("public", tr("Allowed, fork runs refused"));
    const connected = !!(snapshot?.last_webhook || snapshot?.last_poll);
    github.set("app", connected ? state("UP", "Activity verified") : state("UNKNOWN", "Awaiting evidence"));
    github.set("webhook", snapshot?.webhook ? format.relative(snapshot.last_webhook) : tr("Disabled"));
    github.set("poll", !snapshot?.polling ? tr("Disabled")
      : snapshot.poll_failed ? state("FAILED", tr("Failed, last success {time}", { time: format.relative(snapshot.last_poll) }))
        : format.relative(snapshot.last_poll));

    const done = { app: connected, seen: !!snapshot?.first_job_seen, clean: !!snapshot?.first_job_clean };
    const notes = {
      app: () => tr(connected ? "Runquill observed a GitHub delivery or a successful poll." : "Run setup and install the App on your personal account."),
      seen: () => done.seen ? tr("Runquill discovered the first-job workflow.")
        : fill("In a private repository, run {command}, commit the workflow, then select Run workflow on GitHub.",
          { command: h("code", {}, "runquill workflow init") }),
      clean: () => done.clean ? tr("Runner cleanup confirmed for job #{id}.", { id: snapshot.first_job_clean.id })
        : tr("When the job finishes, Runquill removes its runner. Check the job result on GitHub."),
    };
    for (const step of parts.steps) {
      step.marker.dataset.state = done[step.key] ? "ok" : "";
      step.marker.textContent = tr(done[step.key] ? "Done" : "Not yet");
      const note = notes[step.key]();
      const text = Array.isArray(note) ? note.map((part) => part.textContent ?? part).join("") : note;
      if (step.note.textContent !== text) step.note.replaceChildren(...[note].flat());
    }
    const url = snapshot?.first_job_clean?.url;
    parts.firstLink.hidden = !isGitHubJobURL(url);
    if (isGitHubJobURL(url)) parts.firstLink.href = url;

    const pending = (snapshot?.fleet?.jobs || []).filter(pendingJob)
      .sort((a, b) => (a.queue_position || Infinity) - (b.queue_position || Infinity));
    const queued = pending.slice(0, 8);
    parts.waiting.update(queued, snapshot?.fleet ? "The queue is empty." : "Waiting for a fleet snapshot…");
    const moreText = pending.length > queued.length
      ? tr("Showing {shown} of {total} waiting jobs. Open the queue for the rest.", { shown: queued.length, total: pending.length }) : "";
    parts.more.hidden = !moreText;
    if (parts.more.textContent !== moreText) parts.more.textContent = moreText;
  },

  unmount() {
    parts = null;
  },
};
