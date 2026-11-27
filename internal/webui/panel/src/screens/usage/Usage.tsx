// Usage (design §3.10, §4.13): finished-job totals over UTC days from GET /api/usage, grouped by day, repository, pool
// or host, and CPU and memory right-sizing from GET /api/usage/rightsizing. Ported from panelweb/views/usage.js.
// Dates are native date inputs: Stratum 0.1.1's DateRangePicker has Korean calendar labels it does not let us replace.
import { Button, DataTable, EmptyState, Field, Input, SegmentedControl, Select, Skeleton, Tabs, type Column } from "@yakihyuk0728/stratum-ui";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useScreenCommands, type ScreenCommand } from "../../commands";
import { KeyValue, Meter, Notice, Page, StatusBadge } from "../../components";
import { BarChart } from "../../components/BarChart";
import { poolName } from "../../fleet";
import { cpus, day, duration, mib, number, percent } from "../../format";
import { t } from "../../i18n";
import { setParams, type Route } from "../../router";
import { useRequest } from "../../request";
import type { RightsizingReport, RightsizingResource, UsageReport, UsageTotals } from "../../types";
import { groups, presets, rightsizingQuery, usageGroup, usageQuery, usageRange } from "./params";

type Row = UsageTotals & { key: string };

// Wait fields are 0 when no finished job in the row had a known wait.
const wait = (row: UsageTotals, seconds: number) => (row.wait_count ? duration(seconds) : "—");

const metrics: Record<string, { label: string; value: (row: Row) => number; text: (row: Row) => string }> = {
  jobs: { label: "Finished jobs", value: (row) => row.jobs, text: (row) => number(row.jobs) },
  failed: { label: "Failed", value: (row) => row.failed, text: (row) => number(row.failed) },
  runner: { label: "Runner time", value: (row) => row.runner_seconds, text: (row) => duration(row.runner_seconds) },
  p50: { label: "Median wait", value: (row) => (row.wait_count ? row.wait_p50_seconds : 0), text: (row) => wait(row, row.wait_p50_seconds) },
  p95: { label: "95th percentile wait", value: (row) => (row.wait_count ? row.wait_p95_seconds : 0), text: (row) => wait(row, row.wait_p95_seconds) },
};
const groupLabels: Record<string, string> = { day: "Day", repository: "Repository", pool: "Pool", host: "Host" };
const groupCommands: Record<string, string> = { day: "Group usage by day", repository: "Group usage by repository", pool: "Group usage by pool", host: "Group usage by host" };
const tableLabels: Record<string, string> = { day: "Usage by day", repository: "Usage by repository", pool: "Usage by pool", host: "Usage by host" };

function keyLabel(group: string, key: string) {
  if (group === "day") return day(key);
  if (group === "pool") return poolName(key);
  return key || t(group === "host" ? "No host" : "Unknown");
}

function UsageTab({ params }: { params: URLSearchParams }) {
  const range = usageRange(params);
  const group = usageGroup(params);
  const metric = metrics[params.get("metric") ?? ""] ?? metrics.jobs;
  const path = range.problem ? null : "./api/usage?" + usageQuery(params);
  const loaded = useRequest<UsageReport>(path);
  const data = loaded.data;
  const [repo, setRepo] = useState(params.get("repo") ?? "");
  useEffect(() => setRepo(params.get("repo") ?? ""), [params.get("repo")]);
  const totals = data?.totals;
  const rows = useMemo(() => (data && totals?.jobs ? (data.group === "day" ? [...data.series].reverse() : data.series) : []), [data, totals]);
  const top = Math.max(1, ...rows.map(metric.value));
  // Cells must not read data: the table still draws its leaving rows after a new range drops it.
  const shownGroup = data?.group ?? group;
  const columns: Column<Row>[] = [
    { key: "key", header: t(groupLabels[shownGroup]), sticky: true, width: 170, cell: (row) => keyLabel(shownGroup, row.key) },
    { key: "jobs", header: t("Finished jobs"), align: "end", cell: (row) => number(row.jobs) },
    { key: "failed", header: t("Failed"), align: "end", cell: (row) => number(row.failed) },
    { key: "runner", header: t("Runner time"), align: "end", cell: (row) => duration(row.runner_seconds) },
    { key: "p50", header: t("Median wait"), align: "end", cell: (row) => wait(row, row.wait_p50_seconds) },
    { key: "p95", header: t("95th percentile wait"), align: "end", cell: (row) => wait(row, row.wait_p95_seconds) },
    { key: "bar", header: t("Relative to largest"), width: "minmax(140px, 1fr)", cell: (row) => (
      <Meter value={metric.value(row)} max={top} label={t("{metric} for {key}: {value}", { metric: t(metric.label), key: keyLabel(shownGroup, row.key), value: metric.text(row) })} />
    ) },
  ];
  const totalRows: [ReactNode, ReactNode][] = totals ? [
    [t("Finished jobs"), number(totals.jobs)],
    [t("Failed in Runquill"), <>{number(totals.failed)}{totals.jobs > 0 && <span className="block-sub">{t("{percent} of jobs", { percent: percent(totals.failed / totals.jobs, 1) })}</span>}</>],
    [t("Runner time"), <>{duration(totals.runner_seconds)}{totals.jobs > 0 && <span className="block-sub">{t("{time} per job", { time: duration(totals.runner_seconds / totals.jobs) })}</span>}</>],
    [t("Jobs with a known wait"), number(totals.wait_count)],
    [t("Average wait"), wait(totals, totals.wait_avg_seconds)],
    [t("Median wait"), wait(totals, totals.wait_p50_seconds)],
    [t("95th percentile wait"), wait(totals, totals.wait_p95_seconds)],
  ] : [];

  return (
    <>
      <form className="toolbar" aria-label={t("Usage filters")} onSubmit={(event) => {
        event.preventDefault();
        setParams({ repo: repo.trim() });
      }}>
        <Field label={t("Range")} className="toolbar-wide">
          <SegmentedControl size="sm" value={range.preset} onValueChange={(value) => setParams(value === "custom"
            ? { range: null, from: range.from, to: range.to } : { range: value === "30" ? null : value, from: null, to: null })}
          options={[...presets.map((value) => ({ value, label: t(value === "2" ? "Yesterday and today" : "Last {n} days", { n: value }) })),
            { value: "custom", label: t("Custom dates") }]} />
        </Field>
        <Field label={t("From (UTC)")}>
          <Input type="date" value={range.from} required onChange={(event) => setParams({ range: null, from: event.currentTarget.value, to: range.to })} />
        </Field>
        <Field label={t("To (UTC)")}>
          <Input type="date" value={range.to} required onChange={(event) => setParams({ range: null, from: range.from, to: event.currentTarget.value })} />
        </Field>
        <Field label={t("Group by")}>
          <Select value={group} onValueChange={(value) => setParams({ group: value === "day" ? null : value })}
            options={groups.map((value) => ({ value, label: t(groupLabels[value]) }))} />
        </Field>
        <Field label={t("Repository")} className="toolbar-grow">
          <Input type="search" value={repo} onChange={(event) => setRepo(event.currentTarget.value)} placeholder="OWNER/REPO" maxLength={140} />
        </Field>
        <Field label={t("Bar shows")}>
          <Select value={params.get("metric") ?? "jobs"} onValueChange={(value) => setParams({ metric: value === "jobs" ? null : value })}
            options={Object.entries(metrics).map(([value, m]) => ({ value, label: t(m.label) }))} />
        </Field>
        <Button type="submit" variant="secondary">{t("Apply filters")}</Button>
      </form>
      {range.problem && <Notice tone="danger" role="alert"><p>{t(range.problem)}</p></Notice>}
      {loaded.error && (
        <Notice tone="danger" role="alert">
          <p>{loaded.error.message}</p>
          <Button variant="secondary" size="sm" onClick={loaded.reload}>{t("Try again")}</Button>
        </Notice>
      )}
      <section className="block-flat" aria-labelledby="usage-totals">
        <div className="block-head">
          <h2 id="usage-totals">{t("Totals")}</h2>
          {data && <span className="text-sm text-text-hint">{t("{from} to {to}, UTC days", { from: day(data.from, true), to: day(data.to, true) })}</span>}
        </div>
        {totals ? <KeyValue label={t("Usage totals")} rows={totalRows} /> : !loaded.error && !range.problem && <Skeleton />}
      </section>
      <section className="block-flat" aria-labelledby="usage-breakdown">
        <h2 id="usage-breakdown">{t("Breakdown")}</h2>
        {data?.truncated && <Notice role="status"><p>{t("Showing the {n} busiest groups. Totals include every group.", { n: number(data.series.length) })}</p></Notice>}
        {data?.group === "day" && totals!.jobs > 0 && <BarChart rows={data.series} value={metric.value} text={metric.text} metric={t(metric.label)} />}
        <DataTable aria-label={t(tableLabels[shownGroup])} columns={columns} rows={rows} getRowId={(row) => row.key || "-"}
          loading={loaded.loading && !data} empty={<EmptyState title={t("No finished jobs in this range.")} />} />
      </section>
      <p className="summary-line">{t("Counts finished jobs by the UTC day they ended. Failed means Runquill gave up on the job or it was cancelled; GitHub's own job conclusion is not counted. Runner time runs from provisioning to cleanup. Waits run from queueing to provisioning and are estimated from histogram buckets.")}</p>
    </>
  );
}

// Right-sizing verdicts (job_resources.go rightsizingResource.finish): a tight limit is a problem, an oversized one a waste.
const verdicts: Record<string, [string, string]> = {
  tight: ["FAILED", "Tight"], oversized: ["WAITING", "Oversized"], ok: ["DONE", "Fits"], too_few_jobs: ["", "Too few jobs"], unlimited: ["", "No limit"],
};

function Verdict({ resource, text }: { resource?: RightsizingResource; text: (n: number) => string }) {
  if (!resource) return <span className="text-text-hint">{t("Not sampled")}</span>;
  const [status, label] = verdicts[resource.verdict] ?? ["", resource.verdict];
  return (
    <>
      <StatusBadge status={status} label={t(label)} />
      <span className="block-sub">{resource.limit > 0 ? t("Peak {peak} of {limit}", { peak: text(resource.peak_max), limit: text(resource.limit) })
        : t("Peak {peak}", { peak: text(resource.peak_max) })}</span>
      {typeof resource.peak_ratio === "number" && (
        <span className="block-sub">{t("Highest peak {percent} of its limit", { percent: percent(resource.peak_ratio) })}</span>
      )}
    </>
  );
}

function RightsizingTab({ params }: { params: URLSearchParams }) {
  const query = rightsizingQuery(params);
  const loaded = useRequest<RightsizingReport>("./api/usage/rightsizing?" + query);
  const data = loaded.data;
  const pool = (data?.group ?? query.get("group")) === "pool";
  type Group = RightsizingReport["groups"][number];
  const columns: Column<Group>[] = [
    // The job group key is "owner/repo runner-label" (one label per workflow job).
    { key: "key", header: t("Group"), sticky: true, width: 220, cell: (row) => pool ? poolName(row.key)
      : <>{row.key.split(" ")[0]}{row.key.split(" ")[1] && <span className="block-sub font-mono">{row.key.split(" ")[1]}</span>}</> },
    { key: "sampled", header: t("Sampled jobs"), align: "end", cell: (row) => t("{sampled} of {total}", { sampled: number(row.sampled_jobs), total: number(row.jobs) }) },
    { key: "cpu", header: t("CPU"), width: "minmax(180px, 1.5fr)", cell: (row) => <Verdict resource={row.cpu} text={cpus} /> },
    { key: "memory", header: t("Memory"), width: "minmax(180px, 1.5fr)", cell: (row) => <Verdict resource={row.memory} text={mib} /> },
    { key: "oom", header: t("Memory kills"), align: "end", cell: (row) => row.oom_kills ? <StatusBadge status="FAILED" label={number(row.oom_kills)} /> : "0" },
  ];
  return (
    <>
      <div className="toolbar" role="group" aria-label={t("Right-sizing filters")}>
        <Field label={t("Group by")}>
          <Select value={query.get("group")!} onValueChange={(value) => setParams({ group: value === "pool" ? null : value })}
            options={[{ value: "pool", label: t("Pool") }, { value: "job", label: t("Workflow job") }]} />
        </Field>
        <Field label={t("Finished in")}>
          <Select value={query.get("days")!} onValueChange={(value) => setParams({ days: value === "7" ? null : value })}
            options={[1, 2, 3, 5, 7].map((n) => ({ value: String(n), label: n === 1 ? t("Last day") : t("Last {n} days", { n }) }))} />
        </Field>
      </div>
      <p className="summary-line">{t("Compares recent jobs' CPU and memory peaks with their limits. Runquill samples runners on the local Docker host only.")}</p>
      {loaded.error && (
        <Notice tone="danger" role="alert">
          <p>{loaded.error.message}</p>
          <Button variant="secondary" size="sm" onClick={loaded.reload}>{t("Try again")}</Button>
        </Notice>
      )}
      {data?.truncated && <Notice role="status"><p>{t("Showing the {n} groups with the most jobs.", { n: number(data.groups.length) })}</p></Notice>}
      <DataTable className="rows-3" aria-label={t("CPU and memory right-sizing")} columns={columns} rows={data?.groups ?? []} getRowId={(row) => row.key || "-"}
        loading={loaded.loading && !data} empty={<EmptyState title={t("No finished jobs with resource samples in this range.")} />} />
      <p className="summary-line">{t("Tight: a peak above 80% of the limit, or a memory kill. Oversized: every peak below 25% of the limit, over at least 3 sampled jobs.")}</p>
    </>
  );
}

export default function Usage({ route }: { route: Route }) {
  const tab = route.params.get("tab") === "rightsizing" ? "rightsizing" : "usage";
  const group = usageGroup(route.params);
  const commands = useMemo<ScreenCommand[]>(() => [
    ...([["7", "Show usage for the last 7 days"], ["30", "Show usage for the last 30 days"], ["90", "Show usage for the last 90 days"]] as const)
      .map(([n, label]) => ({ id: "range:" + n, label, run: () => setParams({ tab: null, range: n === "30" ? null : n, from: null, to: null }) })),
    ...groups.filter((value) => tab === "rightsizing" || value !== group).map((value) => ({
      id: "group:" + value, label: groupCommands[value], run: () => setParams({ tab: null, days: null, group: value === "day" ? null : value }),
    })),
  ], [tab, group]);
  useScreenCommands(commands);
  return (
    // The two tabs read group differently, so switching drops it.
    <Tabs value={tab} onValueChange={(value) => setParams({ tab: value === "usage" ? null : value, group: null, days: null })}>
      <Page title={t("Usage")} tabs={(
        <Tabs.List aria-label={t("Usage")}>
          <Tabs.Tab value="usage">{t("Usage")}</Tabs.Tab>
          <Tabs.Tab value="rightsizing">{t("Right-sizing")}</Tabs.Tab>
        </Tabs.List>
      )}>
        <Tabs.Panels>
          <Tabs.Panel value="usage" className="usage-panel">{tab === "usage" && <UsageTab params={route.params} />}</Tabs.Panel>
          <Tabs.Panel value="rightsizing" className="usage-panel">{tab === "rightsizing" && <RightsizingTab params={route.params} />}</Tabs.Panel>
        </Tabs.Panels>
      </Page>
    </Tabs>
  );
}
