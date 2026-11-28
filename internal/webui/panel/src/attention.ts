// The overview's "Needs attention" list (design §3.2), from snapshot fields only, in priority order. The nav badge
// counts the same list, so each kind of job problem counts once when it is grouped.
import { number } from "./format";
import { serverText } from "./i18n";
import type { Job, Snapshot } from "./types";

export interface AttentionItem {
  key: string;
  tone: "warning" | "danger";
  /** English i18n key and its values. */
  title: string;
  values?: Record<string, string | number>;
  /** Already translated detail, if any. */
  detail?: string;
  /** Shown as "last seen …" when there is no other detail. */
  seen?: string;
  /** A route, or the id of a block on the overview. */
  href?: string;
  section?: string;
}

const enc = encodeURIComponent;

/** Up to this many jobs of one kind are listed one by one; more become one item, so a busy queue cannot bury a host. */
export const jobsPerKind = 3;

function perJob(jobs: Job[], item: (job: Job) => AttentionItem, group: AttentionItem): AttentionItem[] {
  return jobs.length > jobsPerKind ? [{ ...group, values: { n: number(jobs.length) } }] : jobs.map(item);
}

export function attention(snapshot: Snapshot | null): AttentionItem[] {
  if (!snapshot) return [];
  const items: AttentionItem[] = [];
  const fleet = snapshot.fleet;
  if (fleet?.docker_error) {
    items.push({ key: "docker", tone: "danger", title: "Runquill could not find its runner hosts", detail: serverText(fleet.docker_error), href: "#/hosts" });
  }
  if (snapshot.poll_failed) {
    items.push({ key: "poll", tone: "warning", title: "The latest GitHub poll failed. Check the manager logs.", section: "github-connection" });
  }
  for (const host of fleet?.hosts ?? []) {
    if (host.status === "OFFLINE" || host.status === "UNAVAILABLE") {
      items.push({
        key: "host:" + host.name, tone: "danger", href: "#/hosts/" + enc(host.name),
        title: host.status === "OFFLINE" ? "{host} is offline" : "{host} is unavailable", values: { host: host.name },
        detail: host.error ? serverText(host.error) : undefined, seen: host.last_seen,
      });
    }
  }
  for (const host of fleet?.hosts ?? []) {
    if (host.image_status === "failed" || host.image_status === "missing") {
      items.push({
        key: "image:" + host.name, tone: "danger", href: "#/hosts/" + enc(host.name),
        title: "{host} could not prepare its runner image", values: { host: host.name },
      });
    }
  }
  const jobs = fleet?.jobs ?? [];
  items.push(...perJob(jobs.filter((job) => job.status === "OFFLINE"), (job) => ({
    key: "offline:" + job.id, tone: "danger", href: "#/jobs/" + job.id,
    title: "Job #{id} is assigned to a disconnected host", values: { id: job.id }, detail: job.repository,
  }), { key: "offline", tone: "danger", href: "#/jobs?tab=running&state=OFFLINE", title: "{n} jobs are assigned to a disconnected host" }));
  items.push(...perJob(jobs.filter((job) => job.status === "RETRY"), (job) => ({
    key: "retry:" + job.id, tone: "warning", href: "#/jobs/" + job.id,
    title: "Job #{id} is waiting to retry", values: { id: job.id }, detail: serverText(job.retry_reason) || job.repository,
  }), { key: "retry", tone: "warning", href: "#/jobs?state=RETRY", title: "{n} jobs are waiting to retry" }));
  // A diagnostic, not a state: these jobs are no longer pending, so they are on the running tab.
  items.push(...perJob(jobs.filter((job) => job.diagnostic?.code === "runner_memory_limit"), (job) => ({
    key: "memory:" + job.id, tone: "danger", href: "#/jobs/" + job.id,
    title: "Job #{id} was stopped at its memory limit", values: { id: job.id }, detail: job.repository,
  }), { key: "memory", tone: "danger", href: "#/jobs?tab=running", title: "{n} jobs were stopped at their memory limit" }));
  if (!snapshot.last_webhook && !snapshot.last_poll) {
    items.push({ key: "evidence", tone: "warning", title: "Waiting for evidence that the GitHub App is connected", section: "first-job" });
  }
  return items;
}
