// Snapshot helpers shared by the shell and screens, ported from panelweb/fleet.js and the state labels in panelweb/ui.js.
import { t } from "./i18n";
import type { Host, Job, Snapshot } from "./types";

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
