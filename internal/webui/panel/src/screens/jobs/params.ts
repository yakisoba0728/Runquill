// The jobs screen's address (design §2.4): #/jobs[/ID]?tab=waiting|running|cleaned|history&q&pool&repo&outcome&range.
import type { jobLists, LiveTab } from "../../fleet";

export const tabs = ["waiting", "running", "cleaned", "history"] as const;
export type Tab = (typeof tabs)[number];

export const outcomes = ["waiting", "running", "finished", "failed"] as const;
export const ranges = ["3600", "86400", "604800"] as const;
export const pageSize = 25;

/** A job id from the address, or null. */
export function jobId(id: string | undefined) {
  return id && /^\d{1,15}$/.test(id) ? Number(id) : null;
}

/** The live list holding a job, if any. */
export function holder(lists: ReturnType<typeof jobLists>, id: number): LiveTab | undefined {
  return (["waiting", "running", "cleaned"] as const).find((tab) => lists[tab].some((job) => job.id === id));
}

/**
 * The tab to show: the address's, else the live list holding the open job (it follows the job as it moves), else the
 * history for a job that left the snapshot. Before the first snapshot an open job waits on the default tab.
 */
export function pickTab(param: string | null, id: number | null, lists: ReturnType<typeof jobLists> | null): Tab {
  if (tabs.includes(param as Tab)) return param as Tab;
  if (id === null || !lists) return "waiting";
  return holder(lists, id) ?? "history";
}

/** GET /api/jobs for the history filters; since is computed now, so a refresh moves the window. */
export function historyQuery(params: URLSearchParams, before = 0, now = Date.now()) {
  const query = new URLSearchParams({ limit: String(pageSize) });
  const repo = params.get("repo")?.trim();
  if (repo) query.set("repository", repo);
  const outcome = params.get("outcome");
  if (outcomes.includes(outcome as (typeof outcomes)[number])) query.set("outcome", outcome!);
  const range = params.get("range");
  if (ranges.includes(range as (typeof ranges)[number])) query.set("since", String(Math.floor(now / 1000) - Number(range)));
  if (before) query.set("before", String(before));
  return query;
}
