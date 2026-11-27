// ResourceMeters: a job's CPU and memory peaks against their limits, from GET /api/jobs/{id} (v1 jobinsight.js).
import { Badge } from "@yakihyuk0728/stratum-ui";
import { cpus, mib, number } from "../format";
import { t } from "../i18n";
import type { JobResources } from "../types";

/** One resource: its peak against the guaranteed limit, and the highest raised limit when elastic limits lent more. */
function Resource({ label, peak, limit, raised, text, notes }: {
  label: string; peak?: number; limit?: number; raised?: number; text: (n: number) => string; notes: (string | null)[];
}) {
  const sampled = typeof peak === "number" && Number.isFinite(peak);
  const top = Math.max(limit ?? 0, raised ?? 0);
  // A 0 limit is unlimited.
  const value = !sampled ? t("Not sampled")
    : limit && limit > 0 ? t("{used} of {limit} limit", { used: text(peak), limit: text(limit) })
      : limit === 0 ? t("{used}, unlimited", { used: text(peak) }) : text(peak);
  return (
    <div>
      <dt>{t(label)}</dt>
      <dd>
        <span>{value}</span>
        {sampled && top > 0 && (
          <meter className="meter" min={0} max={top} value={Math.min(top, peak)} high={(limit || top) * 0.8} optimum={0} aria-hidden />
        )}
        {notes.filter(Boolean).map((note) => <span key={note} className="block-sub">{note}</span>)}
      </dd>
    </div>
  );
}

export function ResourceMeters({ resources }: { resources?: JobResources | null }) {
  if (!resources) {
    return <p className="text-sm text-text-hint">{t("No resource samples for this job. Runquill samples runners on the local Docker host only.")}</p>;
  }
  const r = resources;
  const raised = (max: number | undefined, limit: number | undefined, text: (n: number) => string) =>
    max && limit && max > limit ? t("Raised to {limit} by elastic limits", { limit: text(max) }) : null;
  return (
    <div className="resources">
      {r.oom_killed && (
        <p className="text-sm">
          <Badge tone="danger" dot>{t("Killed for memory")}</Badge>{" "}
          <span className="text-text-hint">{t("The container engine reported an out-of-memory kill. Check the worker pool memory limit.")}</span>
        </p>
      )}
      <dl className="kv resource-list" aria-label={t("Resources")}>
        <Resource label="CPU peak" peak={r.cpu_peak} limit={r.cpu_limit} raised={r.cpu_limit_max} text={cpus}
          notes={[typeof r.cpu_average === "number" ? t("Average {value}", { value: cpus(r.cpu_average) }) : null, raised(r.cpu_limit_max, r.cpu_limit, cpus)]} />
        <Resource label="Memory peak" peak={r.memory_peak_mib} limit={r.memory_limit_mib} raised={r.memory_limit_max_mib} text={mib}
          notes={[raised(r.memory_limit_max_mib, r.memory_limit_mib, mib)]} />
      </dl>
      <p className="text-xs text-text-hint">{r.samples === 1 ? t("From 1 sample.") : t("From {n} samples.", { n: number(r.samples) })}</p>
    </div>
  );
}
