// Job insight: the stage timing bar and the CPU and memory meters shown in
// the job details of Queue and Jobs. Timing comes from a job's timing object;
// resources only from GET /api/jobs/{id} (job_resources.go).
import { tr } from "../i18n.js";
import { format, h, state } from "../ui.js";

const svgNS = "http://www.w3.org/2000/svg";

const millisText = (value) => !Number.isFinite(value) ? "—" : value < 1000 ? tr("{n}ms", { n: value }) : format.duration(value / 1000);
const seconds = (value) => Number.isFinite(value) ? value : null;
const millis = (value) => Number.isFinite(value) ? value / 1000 : null;

// stages lists a job's stages in order as { key, label, seconds, text };
// seconds is null for a stage that was not observed. Jobs that recorded JIT
// timestamps split the queue into scheduler, registration and launch.
export function stages(timing = {}) {
  const list = [{ key: "discovery", label: "Discovery time", seconds: seconds(timing.creation_to_discovery_seconds) }];
  if (timing.jit_requested_at) {
    list.push(
      { key: "queue", label: "Scheduler wait", seconds: millis(timing.scheduler_wait_millis), text: millisText(timing.scheduler_wait_millis) },
      { key: "jit", label: "JIT registration", seconds: millis(timing.jit_registration_millis), text: millisText(timing.jit_registration_millis) },
      { key: "runtime", label: "Runtime launch", seconds: millis(timing.runtime_launch_millis), text: millisText(timing.runtime_launch_millis) },
      { key: "startup", label: "GitHub assignment", seconds: seconds(timing.launch_to_github_start_seconds) },
    );
  } else {
    list.push({ key: "queue", label: "Queue time", seconds: seconds(timing.runquill_queue_seconds) },
      { key: "startup", label: "Startup time", seconds: seconds(timing.provision_to_start_seconds) });
  }
  list.push({ key: "execution", label: "Execution", seconds: seconds(timing.execution_seconds) });
  for (const stage of list) stage.text ??= format.duration(stage.seconds);
  return list;
}

// timingView is a stacked bar of the observed stages over a legend that
// carries every number, so the bar itself stays decorative. Execution can
// dwarf the millisecond stages, so each segment keeps a minimum width.
export function timingView(timing) {
  const list = stages(timing);
  const shown = list.filter((stage) => stage.seconds > 0);
  const total = shown.reduce((sum, stage) => sum + stage.seconds, 0);
  const legend = h("dl", { class: "stage-legend", "aria-label": tr("Stage timing") }, ...list.map((stage) => h("div", {
    class: stage.seconds == null ? "unobserved" : null,
  }, h("dt", {}, h("span", { class: "stage-swatch", dataset: { stage: stage.key } }), tr(stage.label)), h("dd", {}, stage.text))));
  if (!shown.length) return h("div", { class: "stage-timing" }, legend);
  const shares = shown.map((stage) => Math.max(0.025, stage.seconds / total));
  const sum = shares.reduce((a, b) => a + b, 0);
  const bar = document.createElementNS(svgNS, "svg");
  for (const [name, value] of Object.entries({ class: "stage-bar", viewBox: "0 0 100 10", preserveAspectRatio: "none", "aria-hidden": "true", focusable: "false" })) {
    bar.setAttribute(name, value);
  }
  let x = 0;
  shown.forEach((stage, index) => {
    const width = (shares[index] / sum) * 100;
    const rect = document.createElementNS(svgNS, "rect");
    for (const [name, value] of Object.entries({ x, y: 0, width: Math.max(0.2, width - 0.4), height: 10, "data-stage": stage.key })) {
      rect.setAttribute(name, String(value));
    }
    bar.append(rect);
    x += width;
  });
  return h("div", { class: "stage-timing" }, bar,
    h("p", { class: "stage-total muted" }, tr("{time} across the observed stages", { time: format.duration(total) })), legend);
}

const cpus = (value) => tr("{n} CPUs", { n: format.number(Math.round(value * 100) / 100) });
const mib = (value) => format.bytes(value * 1048576);

// meter is one resource: its peak against the guaranteed limit, and the
// highest raised limit when elastic limits lent more. A 0 limit is unlimited.
function meter(label, peak, limit, raised, text, notes) {
  const top = Math.max(limit || 0, raised || 0);
  const value = !Number.isFinite(peak) ? tr("Not sampled")
    : limit > 0 ? tr("{used} of {limit} limit", { used: text(peak), limit: text(limit) })
      : limit === 0 ? tr("{used}, unlimited", { used: text(peak) }) : text(peak);
  const tight = limit > 0 && peak / limit > 0.8;
  return h("div", {},
    h("dt", {}, tr(label)),
    h("dd", {}, h("span", { class: "resource-value" }, value),
      top > 0 && Number.isFinite(peak) ? h("progress", { class: "resource-meter" + (tight ? " tight" : ""), max: top, value: Math.min(top, peak), "aria-hidden": "true" }) : null,
      ...notes.filter(Boolean).map((note) => h("span", { class: "sub" }, note))));
}

// resourceView renders GET /api/jobs/{id}'s resources; pending while that
// request runs, null when nothing was recorded for the job.
export function resourceView(resources, { pending = false, failed = "" } = {}) {
  if (pending) return h("p", { class: "muted" }, tr("Loading resource samples…"));
  if (failed) return h("p", { class: "muted" }, failed);
  if (!resources) return h("p", { class: "muted" }, tr("No resource samples for this job. Runquill samples runners on the local Docker host only."));
  const r = resources;
  const raised = (max, limit, text) => max > 0 && limit > 0 && max > limit ? tr("Raised to {limit} by elastic limits", { limit: text(max) }) : null;
  return h("div", { class: "resources" },
    r.oom_killed ? h("p", {}, state("FAILED", "Killed for memory"), " ",
      h("span", { class: "muted" }, tr("The container engine reported an out-of-memory kill. Check the worker pool memory limit."))) : null,
    h("dl", { class: "resource-list", "aria-label": tr("Resources") },
      meter("CPU peak", r.cpu_peak, r.cpu_limit, r.cpu_limit_max, cpus, [
        Number.isFinite(r.cpu_average) ? tr("Average {value}", { value: cpus(r.cpu_average) }) : null,
        raised(r.cpu_limit_max, r.cpu_limit, cpus)]),
      meter("Memory peak", r.memory_peak_mib, r.memory_limit_mib, r.memory_limit_max_mib, mib, [
        raised(r.memory_limit_max_mib, r.memory_limit_mib, mib)])),
    h("p", { class: "muted resource-note" }, tr(r.samples === 1 ? "From 1 sample." : "From {n} samples.", { n: format.number(r.samples) })));
}
