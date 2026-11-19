// Fleet snapshot helpers shared by several views. The snapshot shape is the
// JSON of GET /snapshot (and each `snapshot` event of GET /api/events).
import { tr } from "./i18n.js";
import { button, errorText, state } from "./ui.js";

// Docker and Kubernetes runtime states, as runner_inventory reports them.
const runtimeStates = { running: "RUNNING", paused: "PAUSED", created: "STARTING", pending: "STARTING", dead: "CLEANUP", exited: "CLEANUP" };

// runnerState renders a runner's runtime state, or that its lease has no
// observation (an offline host, or a runner that is gone).
export function runnerState(runner) {
  return runner.runtime_state
    ? state(runtimeStates[runner.runtime_state] || runner.runtime_state.toUpperCase())
    : state("UNKNOWN", "Lease not observed");
}

// runnerRows joins runner_inventory with the jobs the runners hold.
export function runnerRows(snapshot) {
  const jobs = new Map((snapshot?.fleet?.jobs || []).map((job) => [job.id, job]));
  return (snapshot?.runner_inventory || []).map((runner) => ({ ...runner, job: jobs.get(runner.job_id) }));
}

// jobActionButtons returns Retry now / Cancel job controls for a job that is
// waiting without a runner, for operators and admins. Each asks first; done
// runs after the server applied the change.
export function jobActionButtons(ctx, job, done) {
  if (!ctx.can("operator")) return [];
  const kinds = job.retry_at ? ["retry", "cancel"] : ["cancel"];
  return kinds.map((kind) => {
    const label = kind === "retry" ? "Retry now" : "Cancel job";
    const control = button(label, async () => {
      const question = kind === "cancel"
        ? "Cancel job #{id} in Runquill? It is marked failed and no runner starts for it. The run stays queued on GitHub until you cancel it there."
        : "Retry job #{id} now? Runquill tries to start a runner on its next pass.";
      if (!await ctx.confirm(tr(question, { id: job.id }), label)) return;
      control.disabled = true;
      try {
        await ctx.api.post("./api/jobs/" + encodeURIComponent(job.id) + "/" + kind);
      } catch (error) {
        if (error.status !== 401) ctx.toast(errorText(error), "failed");
        control.disabled = false;
        return;
      }
      ctx.toast(tr(kind === "cancel" ? "Job cancelled." : "The job will be retried on the next scheduler pass."));
      control.disabled = false;
      // done refreshes the screen; the person may have left it meanwhile.
      try { await done(); } catch (error) { console.error(error); }
    }, { small: true, "aria-label": tr("{action} for job {id}", { action: tr(label), id: job.id }) });
    return control;
  });
}

export function pendingJob(job) {
  return job.status === "WAITING" || job.status === "RETRY";
}

export function workerState(host) {
  return host.cordoned && host.status === "UP" ? "CORDONED" : host.status;
}

export function availableSlots(host) {
  return workerState(host) === "UP" ? Math.max(0, host.slot_limit - host.occupied_slots - host.reserved_slots) : 0;
}

export function poolName(pool) {
  return !pool || pool === "default" ? tr("Default pool") : pool;
}

// summary condenses a snapshot into the counts the overview, nav and other
// views show. Every field is null until the first snapshot with fleet data.
export function summary(snapshot) {
  const fleet = snapshot?.fleet;
  if (!fleet) return null;
  const hosts = fleet.hosts || [];
  const jobs = fleet.jobs || [];
  const queued = jobs.filter(pendingJob);
  const counts = fleet.runners || {};
  const used = (counts.occupied_slots || 0) + (counts.reserved_slots || 0);
  const free = Math.max(0, Math.min(fleet.slot_limit - used, hosts.reduce((sum, host) => sum + availableSlots(host), 0)));
  const byState = (name) => hosts.filter((host) => workerState(host) === name).length;
  return {
    running: counts.known_running ?? 0,
    queued: queued.length,
    ready: queued.filter((job) => job.status === "WAITING").length,
    retrying: queued.filter((job) => job.status === "RETRY").length,
    assigned: jobs.length - queued.length,
    oldestWait: queued.reduce((max, job) => Math.max(max, job.timing?.runquill_queue_seconds ?? -1), -1),
    used, free, limit: fleet.slot_limit,
    reserved: counts.reserved_slots || 0,
    unassigned: counts.unassigned_reserved_slots || 0,
    hostsKnown: hosts.length > 0 || !fleet.docker_error,
    hosts: hosts.length,
    hostsUp: byState("UP"), hostsOffline: byState("OFFLINE"),
    hostsUnavailable: byState("UNAVAILABLE"), hostsCordoned: byState("CORDONED"),
    kubernetes: fleet.backend === "kubernetes",
  };
}
