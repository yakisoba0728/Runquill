// The usage screen's address (design §2.4): #/usage?range=7|2|30|90 or from=&to= (custom UTC days), group, repo, metric;
// #/usage?tab=rightsizing&group=pool|job&days=1..7.
import { utcDay } from "../../format";

export const presets = ["2", "7", "30", "90"] as const;
export const groups = ["day", "repository", "pool", "host"] as const;
export const maxDays = 400;

export interface UsageRange {
  from: string;
  to: string;
  /** A preset's day count, or "custom" when the address names the dates. */
  preset: string;
  /** Why the dates cannot be loaded, as an English key; "" when they can. */
  problem: string;
}

const dayKey = /^\d{4}-\d{2}-\d{2}$/;
const days = (from: string, to: string) => Math.round((Date.parse(to + "T00:00:00Z") - Date.parse(from + "T00:00:00Z")) / 86400000);

/** from/to in the address make a custom range; otherwise a preset ending today (UTC), 30 days by default. */
export function usageRange(params: URLSearchParams, now = Date.now()): UsageRange {
  const from = params.get("from");
  const to = params.get("to");
  if (from !== null || to !== null) {
    const valid = !!from && !!to && dayKey.test(from) && dayKey.test(to) && from <= to && days(from, to) < maxDays;
    return { from: from ?? "", to: to ?? "", preset: "custom", problem: valid ? "" : "Choose a start date on or before the end date, at most 400 days apart." };
  }
  const range = params.get("range");
  const preset = presets.includes(range as (typeof presets)[number]) ? range! : "30";
  return { from: utcDay(1 - Number(preset), now), to: utcDay(0, now), preset, problem: "" };
}

export const usageGroup = (params: URLSearchParams) => {
  const group = params.get("group");
  return groups.includes(group as (typeof groups)[number]) ? group! : "day";
};

/** GET /api/usage for the address. */
export function usageQuery(params: URLSearchParams, now = Date.now()) {
  const range = usageRange(params, now);
  const query = new URLSearchParams({ from: range.from, to: range.to, group: usageGroup(params) });
  const repo = params.get("repo")?.trim();
  if (repo) query.set("repository", repo);
  return query;
}

/** GET /api/usage/rightsizing for the address: group pool|job, days 1-7 (default 7). */
export function rightsizingQuery(params: URLSearchParams) {
  const group = params.get("group") === "job" ? "job" : "pool";
  const days = Number(params.get("days"));
  return new URLSearchParams({ group, days: String(Number.isInteger(days) && days >= 1 && days <= 7 ? days : 7) });
}
