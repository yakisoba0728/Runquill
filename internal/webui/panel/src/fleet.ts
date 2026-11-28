// Snapshot helpers shared by the shell and screens, ported from panelweb/fleet.js and the state labels in panelweb/ui.js.
import { duration, millis } from "./format";
import { serverText, t } from "./i18n";
import type { Host, Job, JobTiming, Runner, Snapshot } from "./types";

export type Tone = "neutral" | "success" | "warning" | "danger";

const tones: Record<string, Tone> = {
  RUNNING: "success", UP: "success", CLEANED: "success", FINISHED: "success", DONE: "success",
  WAITING: "warning", RETRY: "warning", CORDONED: "warning", CLEANUP: "warning", STARTING: "warning", PAUSED: "warning",
  FAILED: "danger", UNAVAILABLE: "danger",
};

// English labels for the status codes in snapshots and job history.
const labels: Record<string, string> = {
  RUNNING: "Running", UP: "Ready", CLEANED: "Cleaned up", FINISHED: "Finished", DONE: "Done",
  WAITING: "Waiting", RETRY: "Retry scheduled", CORDONED: "On maintenance", CLEANUP: "Cleaning up",
  STARTING: "Starting", PAUSED: "Paused", FAILED: "Failed", UNAVAILABLE: "Unavailable", OFFLINE: "Offline",
  UNKNOWN: "Unknown",
};

export const statusTone = (status: string): Tone => tones[status] ?? "neutral";
export const statusLabel = (status: string) => t(labels[status] ?? status);

export const pendingJob = (job: Job) => job.status === "WAITING" || job.status === "RETRY";

/** Jobs in recent carry no status: their runner is cleaned up. */
export const jobStatus = (job: Job) => job.status || "CLEANED";

/** Why a job waits or failed, already translated (v1 order: diagnostic, retry reason, pool error). */
export const jobReason = (job: Job) => serverText(job.diagnostic?.message || job.retry_reason || job.pool_error || "");

/** The live job lists of the jobs screen (design §4.4). */
export function jobLists(snapshot: Snapshot | null) {
  const jobs = snapshot?.fleet?.jobs ?? [];
  return {
    waiting: jobs.filter(pendingJob).sort((a, b) => (a.queue_position || Infinity) - (b.queue_position || Infinity) ||
      (a.retry_at || "").localeCompare(b.retry_at || "")),
    running: jobs.filter((job) => !pendingJob(job)),
    cleaned: snapshot?.recent ?? [],
  };
}

export type LiveTab = keyof ReturnType<typeof jobLists>;

/** The search (repository or job number, a leading # ignored), pool and state filters of the live tabs. */
export function filterJobs(jobs: Job[], query: string, pool: string, state = "") {
  const q = query.trim().toLowerCase().replace(/^#/, "");
  return jobs.filter((job) => (!pool || (job.pool || "default") === pool) && (!state || jobStatus(job) === state) &&
    (!q || job.repository.toLowerCase().includes(q) || String(job.id).includes(q)));
}

// Docker and Kubernetes runtime states, as runner_inventory reports them.
const runtimeStates: Record<string, string> = {
  running: "RUNNING", paused: "PAUSED", created: "STARTING", pending: "STARTING", dead: "CLEANUP", exited: "CLEANUP",
};

/** A runner's runtime state as a status code; UNKNOWN when its lease has no observation. */
export const runnerStatus = (runner: Runner) =>
  runner.runtime_state ? runtimeStates[runner.runtime_state] ?? runner.runtime_state.toUpperCase() : "UNKNOWN";

export type RunnerRow = Runner & { job?: Job };

/** runner_inventory joined with the jobs the runners hold. */
export function runnerRows(snapshot: Snapshot | null): RunnerRow[] {
  const jobs = new Map((snapshot?.fleet?.jobs ?? []).map((job) => [job.id, job]));
  return (snapshot?.runner_inventory ?? []).map((runner) => ({ ...runner, job: runner.job_id ? jobs.get(runner.job_id) : undefined }));
}

export interface PoolRow {
  name: string; hosts: number; ready: number; occupied: number; reserved: number; free: number; limit: number; warm: number; warmTarget: number;
}

/** Host capacity summed per worker pool, before the fleet slot limit applies. */
export function pools(hosts: Host[]): PoolRow[] {
  const result = new Map<string, PoolRow>();
  for (const host of hosts) {
    const name = host.pool || "default";
    const pool = result.get(name) ?? { name, hosts: 0, ready: 0, occupied: 0, reserved: 0, free: 0, limit: 0, warm: 0, warmTarget: 0 };
    result.set(name, pool);
    pool.hosts++;
    pool.ready += Number(workerState(host) === "UP");
    pool.occupied += host.occupied_slots;
    pool.reserved += host.reserved_slots;
    pool.limit += host.slot_limit;
    pool.free += availableSlots(host);
    pool.warm += host.warm ?? 0;
    pool.warmTarget = Math.max(pool.warmTarget, host.warm_target ?? 0);
  }
  return [...result.values()].sort((a, b) => a.name.localeCompare(b.name));
}

export interface Stage {
  key: string;
  /** English i18n key. */
  label: string;
  /** null when the stage was not observed. */
  seconds: number | null;
  text: string;
}

const num = (value: number | null | undefined) => (typeof value === "number" && Number.isFinite(value) ? value : null);

/**
 * A job's stages in order (v1 jobinsight.js). Jobs that recorded JIT timestamps split the queue into scheduler wait,
 * registration and launch.
 */
export function stages(timing: JobTiming = {}): Stage[] {
  const ms = (value: number | null | undefined) => ({ seconds: num(value) === null ? null : value! / 1000, text: millis(value) });
  const list: Stage[] = [];
  const add = (key: string, label: string, seconds: number | null, text = duration(seconds)) => list.push({ key, label, seconds, text });
  add("discovery", "Discovery time", num(timing.creation_to_discovery_seconds));
  if (timing.jit_requested_at) {
    const scheduler = ms(timing.scheduler_wait_millis);
    const jit = ms(timing.jit_registration_millis);
    const runtime = ms(timing.runtime_launch_millis);
    add("queue", "Scheduler wait", scheduler.seconds, scheduler.text);
    add("jit", "JIT registration", jit.seconds, jit.text);
    add("runtime", "Runtime launch", runtime.seconds, runtime.text);
    add("startup", "GitHub assignment", num(timing.launch_to_github_start_seconds));
  } else {
    add("queue", "Queue time", num(timing.runquill_queue_seconds));
    add("startup", "Startup time", num(timing.provision_to_start_seconds));
  }
  add("execution", "Execution", num(timing.execution_seconds));
  return list;
}

export const workerState = (host: Host) => (host.cordoned && host.status === "UP" ? "CORDONED" : host.status);

export const availableSlots = (host: Host) =>
  workerState(host) === "UP" ? Math.max(0, host.slot_limit - host.occupied_slots - host.reserved_slots) : 0;

export const poolName = (pool?: string) => (!pool || pool === "default" ? t("Default pool") : pool);

export function isGitHubJobURL(url: unknown): url is string {
  return typeof url === "string" && /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/actions\/runs\/\d+\/job\/\d+$/.test(url);
}

export type Summary = NonNullable<ReturnType<typeof summary>>;

/** The counts the overview and nav show. null until the first snapshot with fleet data. */
export function summary(snapshot: Snapshot | null) {
  const fleet = snapshot?.fleet;
  if (!fleet) return null;
  const hosts = fleet.hosts ?? [];
  const jobs = fleet.jobs ?? [];
  const queued = jobs.filter(pendingJob);
  const counts = fleet.runners ?? {};
  const used = (counts.occupied_slots ?? 0) + (counts.reserved_slots ?? 0);
  const free = Math.max(0, Math.min(fleet.slot_limit - used, hosts.reduce((sum, host) => sum + availableSlots(host), 0)));
  const byState = (name: string) => hosts.filter((host) => workerState(host) === name).length;
  return {
    running: counts.known_running ?? 0,
    queued: queued.length,
    ready: queued.filter((job) => job.status === "WAITING").length,
    retrying: queued.filter((job) => job.status === "RETRY").length,
    oldestWait: queued.reduce((max, job) => Math.max(max, job.timing?.runquill_queue_seconds ?? -1), -1),
    used,
    free,
    limit: fleet.slot_limit,
    reserved: counts.reserved_slots ?? 0,
    unassigned: counts.unassigned_reserved_slots ?? 0,
    hostsKnown: hosts.length > 0 || !fleet.docker_error,
    hosts: hosts.length,
    hostsUp: byState("UP"),
    hostsOffline: byState("OFFLINE"),
    hostsUnavailable: byState("UNAVAILABLE"),
    hostsCordoned: byState("CORDONED"),
    kubernetes: fleet.backend === "kubernetes",
  };
}
