// Hosts (design §3.5, §4.6): worker hosts (or Kubernetes targets), the same capacity per pool, server resources and
// elastic limits, with a host detail pane. Ported from panelweb/views/hosts.js. Resources and elastic limits poll only
// while their tab is shown.
// shortcut: read-only; pause/resume assignments and worker enrollment join the pane footer in phase 3.
import { Badge, Button, DataTable, EmptyState, Skeleton, SplitView, Tabs, useSplitViewNavigation, type Column } from "@yakihyuk0728/stratum-ui";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import type { ApiError } from "../../api";
import { Ago, KeyValue, Meter, Notice, Page, StatusBadge } from "../../components";
import { availableSlots, poolName, pools, runnerRows, workerState, type PoolRow } from "../../fleet";
import { bytes, cpus, date, duration, mib, number, timeOfDay } from "../../format";
import { fill, serverText, t } from "../../i18n";
import { stratum } from "../../i18n/stratum";
import { useLive } from "../../live";
import { format, navigate, setParams, type Route } from "../../router";
import { useRequest } from "../../request";
import type { ElasticReport, Host, ResourceReport } from "../../types";

const labels = stratum();
const enc = encodeURIComponent;
const tabs = ["hosts", "pools", "resources", "elastic"] as const;
type Tab = (typeof tabs)[number];

// image_status is set only for local and ssh:// hosts while RUNQUILL_IMAGE_PULL is missing or always.
const imageStates: Record<string, string> = { ready: "Image ready", pulling: "Pulling image", missing: "Image missing", failed: "Image pull failed" };
const round2 = (n: number) => Math.round(n * 100) / 100;

function HostState({ host }: { host: Host }) {
  return (
    <>
      <StatusBadge status={workerState(host)} />
      {imageStates[host.image_status ?? ""] && <span className="block-sub">{t(imageStates[host.image_status!])}</span>}
      {host.last_seen && <span className="block-sub">{t("Last seen")} <Ago at={host.last_seen} /></span>}
      {(host.warm || host.warm_target) ? <span className="block-sub">{t("{n} of {target} warm runners idle", { n: host.warm ?? 0, target: host.warm_target ?? 0 })}</span> : null}
    </>
  );
}

const hostColumns: Column<Host>[] = [
  { key: "name", header: t("Host"), sticky: true, width: 200, cell: (host) => (
    <>{host.name}{host.error && <span className="block-sub">{serverText(host.error)}</span>}</>
  ) },
  { key: "pool", header: t("Pool"), cell: (host) => poolName(host.pool) },
  { key: "state", header: t("State"), width: "minmax(180px, 1.5fr)", cell: (host) => <HostState host={host} /> },
  { key: "limit", header: t("Slot limit"), align: "end", width: 100, cell: (host) => number(host.slot_limit) },
  { key: "occupied", header: t("Occupied"), align: "end", width: 100, cell: (host) => number(host.occupied_slots) },
  { key: "reserved", header: t("Reserved"), align: "end", width: 100, cell: (host) => number(host.reserved_slots) },
  { key: "available", header: t("Available"), align: "end", width: 100, cell: (host) => number(availableSlots(host)) },
];

const poolColumns: Column<PoolRow>[] = [
  { key: "name", header: t("Pool"), sticky: true, width: 160, cell: (pool) => poolName(pool.name) },
  { key: "ready", header: t("Ready hosts"), align: "end", cell: (pool) => t("{ready} of {total}", { ready: pool.ready, total: pool.hosts }) },
  { key: "occupied", header: t("Occupied"), align: "end", cell: (pool) => number(pool.occupied) },
  { key: "reserved", header: t("Reserved"), align: "end", cell: (pool) => number(pool.reserved) },
  { key: "free", header: t("Available"), align: "end", cell: (pool) => number(pool.free) },
  { key: "limit", header: t("Slot limit"), align: "end", cell: (pool) => number(pool.limit) },
  { key: "warm", header: t("Warm runners"), align: "end", cell: (pool) => pool.warmTarget
    ? <>{t("{n} idle", { n: number(pool.warm) })}<span className="block-sub">{t("{n} per host", { n: number(pool.warmTarget) })}</span></>
    : pool.warm ? t("{n} idle", { n: number(pool.warm) }) : <span className="text-text-hint">{t("Off")}</span> },
  { key: "usage", header: t("Usage"), width: "minmax(140px, 1fr)", cell: (pool) => (
    <Meter value={Math.min(pool.limit, pool.occupied + pool.reserved)} max={pool.limit} label={t("{host} capacity", { host: poolName(pool.name) })} />
  ) },
];

function RequestError({ error, reload }: { error: ApiError; reload: () => void }) {
  return (
    <Notice tone="danger" role="alert">
      <p>{error.status === 403 ? t("You do not have access to this operation.") : error.message}</p>
      <Button variant="secondary" size="sm" onClick={reload}>{t("Try again")}</Button>
    </Notice>
  );
}

const notAvailable = (note?: string) => <span className="text-text-hint">{t("Not available")}{note && <span className="block-sub">{t(note)}</span>}</span>;

/** "used / total" over a meter; total may be unknown. */
function Share({ used, total, text, label }: { used: number; total?: number; text: (n: number) => string; label: string }) {
  if (!total || total <= 0) return <>{text(used)}</>;
  return <>{t("{used} / {total}", { used: text(used), total: text(total) })}<Meter value={Math.min(total, used)} max={total} label={label} /></>;
}

type ResourceEntry = NonNullable<ResourceReport["hosts"]>[number];
const unavailableText: Record<string, string> = {
  agent: "Outbound worker: the manager cannot read this host.",
  kubernetes: "Kubernetes: the manager sees Pods, not the nodes they run on.",
};

function Reserved({ entry, kind }: { entry: ResourceEntry; kind: "cpu" | "memory" }) {
  const reserved = entry.reserved;
  if (!reserved) return notAvailable();
  const perRunner = kind === "cpu" ? reserved.runner_cpus : reserved.runner_memory_bytes;
  if (!perRunner) return <>{t("Unlimited")}<span className="block-sub">{t("No per-runner limit")}</span></>;
  const text = kind === "cpu" ? cpus : bytes;
  return (
    <>
      <Share used={kind === "cpu" ? reserved.cpus : reserved.memory_bytes} total={kind === "cpu" ? entry.cpus : entry.memory_bytes}
        text={kind === "cpu" ? (n) => number(round2(n)) : bytes} label={t(kind === "cpu" ? "Reserved CPU" : "Reserved memory")} />
      <span className="block-sub">{t("{amount} per runner", { amount: text(perRunner) })}</span>
    </>
  );
}

function Disk({ entry, measured }: { entry: ResourceEntry; measured: boolean }) {
  if (entry.unavailable) return notAvailable();
  if (entry.disk_error) return <><StatusBadge status="FAILED" label={t("Not measured")} /><span className="block-sub">{serverText(entry.disk_error)}</span></>;
  const disk = entry.disk;
  if (!disk) return <span className="text-text-hint">{t(measured ? "Not measured" : "Not measured yet")}</span>;
  return (
    <>
      {t("Images {images}", { images: bytes(disk.image_bytes) })}
      <span className="block-sub">{t("{n} pulled images", { n: number(disk.images) })}</span>
      <span className="block-sub">{t("Cache {size} in {n} volumes", { size: disk.cache_bytes == null ? t("size unknown") : bytes(disk.cache_bytes), n: number(disk.cache_volumes) })}</span>
    </>
  );
}

function resourceColumns(hosts: Map<string, Host>, measured: boolean): Column<ResourceEntry>[] {
  return [
    { key: "host", header: t("Host"), sticky: true, width: 200, cell: (entry) => (
      <>{entry.host}{entry.unavailable && <span className="block-sub">{t(unavailableText[entry.unavailable] ?? "Not available")}</span>}
        {entry.error && <span className="block-sub">{serverText(entry.error)}</span>}</>
    ) },
    { key: "capacity", header: t("Capacity"), align: "end", cell: (entry) => entry.cpus
      ? <>{cpus(entry.cpus)}<span className="block-sub">{bytes(entry.memory_bytes)}</span></> : notAvailable() },
    { key: "slots", header: t("Slots used"), align: "end", cell: (entry) => {
      const host = hosts.get(entry.host);
      return host ? <Share used={host.occupied_slots + host.reserved_slots} total={host.slot_limit} text={number} label={t("Slots used")} /> : "—";
    } },
    { key: "cpu", header: t("Reserved CPU"), align: "end", width: "minmax(150px, 1fr)", cell: (entry) => <Reserved entry={entry} kind="cpu" /> },
    { key: "memory", header: t("Reserved memory"), align: "end", width: "minmax(170px, 1fr)", cell: (entry) => <Reserved entry={entry} kind="memory" /> },
    { key: "usage", header: t("Runner usage"), align: "end", cell: (entry) => entry.usage
      ? <>{cpus(entry.usage.cpus)}<span className="block-sub">{bytes(entry.usage.memory_bytes)}</span></>
      : entry.unavailable ? notAvailable() : <span className="text-text-hint">{t("Not sampled")}</span> },
    { key: "disk", header: t("Disk"), align: "end", width: "minmax(170px, 1fr)", cell: (entry) => <Disk entry={entry} measured={measured} /> },
  ];
}

/** GET /api/hosts/resources every 30s (5s while a disk measurement runs, 60s after an error); ?disk=1 measures. */
function useResources() {
  const [measure, setMeasure] = useState(false);
  const loaded = useRequest<ResourceReport>("./api/hosts/resources" + (measure ? "?disk=1" : ""), {
    poll: (data, error) => (error ? 60000 : data?.disk_measuring ? 5000 : 30000),
  });
  // Once the measurement is done, plain requests return it from the manager's 10-minute cache.
  useEffect(() => {
    if (measure && loaded.data && !loaded.data.disk_measuring) setMeasure(false);
  }, [measure, loaded.data]);
  return { ...loaded, measure: () => setMeasure(true), measuring: measure || !!loaded.data?.disk_measuring };
}

function Resources({ hosts }: { hosts: Host[] }) {
  const report = useResources();
  const byName = useMemo(() => new Map(hosts.map((host) => [host.name, host])), [hosts]);
  const data = report.data;
  return (
    <section className="block-flat" aria-labelledby="resources-title">
      <div className="block-head">
        <h2 id="resources-title">{t("Server resources")}</h2>
        <Button variant="secondary" size="sm" loading={report.measuring} disabled={report.measuring} onClick={report.measure}>{t("Measure disk use")}</Button>
      </div>
      <p className="summary-line">{t("Reserved is what runners holding a slot were promised. Usage comes from the elastic limits loop.")}</p>
      {report.error && <RequestError error={report.error} reload={report.reload} />}
      <DataTable className="rows-3" aria-label={t("Server resources per host")} columns={resourceColumns(byName, !!data?.disk_measured_at)} rows={data?.hosts ?? []}
        getRowId={(entry) => entry.host} loading={!data && !report.error} empty={<EmptyState title={t("No hosts configured.")} />} />
      <p className="summary-line" role="status">
        {data?.disk_measuring || report.measuring ? t("Measuring disk use…")
          : data?.disk_measured_at ? t("Disk use measured at {time}. Measurements are kept for 10 minutes.", { time: timeOfDay(data.disk_measured_at) })
            : t("Disk use is measured only when you ask, because it walks every volume on each host.")}
      </p>
    </section>
  );
}

const modes: Record<string, string> = { off: "Off", observe: "Observe only", apply: "Apply" };

function Elastic() {
  // The report's own interval, at least 5s; a minute after an error.
  const report = useRequest<ElasticReport>("./api/elastic", {
    poll: (data, error) => (error || !data?.interval_seconds ? 60000 : Math.max(5000, data.interval_seconds * 1000)),
  });
  const data = report.data;
  const mode = data && modes[data.mode] ? data.mode : "off";
  const amount = (resource: string, value: number) => (resource === "memory" ? mib(value) : cpus(value));
  return (
    <section className="block-flat" aria-labelledby="elastic-title">
      <h2 id="elastic-title">{t("Elastic limits")}</h2>
      <p className="summary-line">{t("Runners may borrow CPU and memory that no other runner was promised.")}</p>
      {report.error && <RequestError error={report.error} reload={report.reload} />}
      {!data ? !report.error && <Skeleton /> : mode === "off" ? (
        <EmptyState title={t("Off")} description={fill("Elastic limits are off. Set {variable} on the manager to see what Runquill would lend, then {apply} to change runner limits.",
          { variable: <code>RUNQUILL_ELASTIC=observe</code>, apply: <code>apply</code> })} />
      ) : (
        <>
          <KeyValue label={t("Elastic limits")} rows={[
            [t("Mode"), <>{<Badge tone={mode === "apply" ? "success" : "neutral"} dot>{t(modes[mode])}</Badge>}
              {mode === "observe" && <span className="block-sub">{t("Decisions are recorded but runner limits are not changed.")}</span>}</>],
            [t("Check interval"), duration(data.interval_seconds)],
          ]} />
          <h3>{t("Surplus per host")}</h3>
          <DataTable className="rows-2" aria-label={t("Elastic surplus per host")} rows={data.hosts ?? []} getRowId={(host) => host.host}
            empty={<EmptyState title={t("No hosts sampled yet.")} />} columns={[
              { key: "host", header: t("Host"), sticky: true, width: 160, cell: (host) => (
                <>{host.host}{host.memory_overcommitted && <span className="block-sub"><StatusBadge status="FAILED" label={t("Memory overcommitted")} /></span>}</>
              ) },
              { key: "cpus", header: t("Host CPUs"), align: "end", cell: (host) => number(host.cpus) },
              { key: "surplus", header: t("Surplus CPUs"), align: "end", cell: (host) => number(round2(host.surplus_cpus)) },
              { key: "lent", header: t("Lent CPUs"), align: "end", cell: (host) => number(round2(host.lent_cpus)) },
              { key: "memory", header: t("Host memory"), align: "end", cell: (host) => mib(host.memory_mib) },
              { key: "surplusMemory", header: t("Surplus memory"), align: "end", cell: (host) => mib(host.surplus_memory_mib) },
              { key: "extra", header: t("Extra memory"), align: "end", cell: (host) => mib(host.extra_memory_mib) },
              { key: "held", header: t("Held slots"), align: "end", cell: (host) => number(host.held_slots) },
            ]} />
          <h3>{t("Recent decisions")}</h3>
          <DataTable className="rows-2" aria-label={t("Recent elastic decisions")} rows={data.decisions ?? []} getRowId={(d) => d.at + d.host + d.runner + d.resource}
            empty={<EmptyState title={t("No decisions yet.")} />} columns={[
              { key: "at", header: t("Time"), align: "end", width: 110, cell: (d) => timeOfDay(d.at) },
              { key: "runner", header: t("Runner"), width: "minmax(180px, 1.5fr)", cell: (d) => <><span className="font-mono">{d.runner}</span><span className="block-sub">{d.host}</span></> },
              { key: "resource", header: t("Resource"), width: 100, cell: (d) => t(d.resource === "memory" ? "Memory" : "CPU") },
              { key: "change", header: t("Change"), align: "end", cell: (d) => t("{from} → {to}", { from: amount(d.resource, d.from), to: amount(d.resource, d.to) }) },
              { key: "reason", header: t("Reason"), width: "minmax(240px, 3fr)", cell: (d) => (
                <>{d.reason ? serverText(d.reason) : "—"}{d.forced && <span className="block-sub">{t("Reclaimed at once to keep host guarantees.")}</span>}</>
              ) },
              { key: "applied", header: t("Applied"), cell: (d) => d.error
                ? <><StatusBadge status="FAILED" /><span className="block-sub">{serverText(d.error)}</span></>
                : d.applied ? <StatusBadge status="DONE" label={t("Applied")} /> : <Badge>{t("Observed only")}</Badge> },
            ]} />
        </>
      )}
    </section>
  );
}

function HostPane({ name, host }: { name: string; host?: Host }) {
  const snapshot = useLive((s) => s.snapshot);
  const runners = useMemo(() => runnerRows(snapshot).filter((runner) => runner.host === name), [snapshot, name]);
  const resources = useRequest<ResourceReport>("./api/hosts/resources");
  const entry = resources.data?.hosts?.find((candidate) => candidate.host === name);
  let body: ReactNode = <p className="text-sm text-text-hint">{t("The host {name} is no longer reported.", { name })}</p>;
  if (host) {
    const state = workerState(host);
    const rows: [ReactNode, ReactNode][] = [
      [t("State"), <HostState host={host} />],
      [t("Pool"), poolName(host.pool)],
      [t("Slots"), <>{t("{used} of {limit}", { used: number(host.occupied_slots + host.reserved_slots), limit: number(host.slot_limit) })}
        <Meter value={Math.min(host.slot_limit, host.occupied_slots + host.reserved_slots)} max={host.slot_limit} label={t("{host} capacity", { host: name })} />
        <span className="block-sub">{[t("{n} free", { n: availableSlots(host) }), t("{n} reserved", { n: host.reserved_slots })].join(", ")}</span></>],
      [t("Warm runners"), host.warm_target ? t("{n} of {target} warm runners idle", { n: host.warm ?? 0, target: host.warm_target }) : t("Off")],
    ];
    if (entry && !entry.unavailable) {
      rows.push([t("Capacity"), entry.cpus ? <>{cpus(entry.cpus)}<span className="block-sub">{bytes(entry.memory_bytes)}</span></> : notAvailable()],
        [t("Runner usage"), entry.usage ? <>{cpus(entry.usage.cpus)}<span className="block-sub">{bytes(entry.usage.memory_bytes)}</span></> : t("Not sampled")]);
    } else if (entry?.unavailable) {
      rows.push([t("Server resources"), notAvailable(unavailableText[entry.unavailable])]);
    }
    body = (
      <div className="pane-sections">
        {(state === "OFFLINE" || state === "UNAVAILABLE") && (
          <Notice tone="danger" role="status">
            <p>{host.error ? serverText(host.error) : state === "OFFLINE" && host.last_seen ? fill("Last seen {time}", { time: <Ago at={host.last_seen} /> }) : t(state === "OFFLINE" ? "Offline" : "Unavailable")}</p>
            {host.error && host.last_seen && <p className="text-sm">{t("Last seen")} {date(host.last_seen)}</p>}
          </Notice>
        )}
        <KeyValue rows={rows} />
        <section aria-labelledby="host-runners">
          <h3 id="host-runners">{t("Runners")}</h3>
          {runners.length ? (
            <ul className="pane-list">{runners.map((runner) => (
              <li key={runner.name}><a className="font-mono" href={"#/runners/" + enc(runner.name)}>{runner.name}</a></li>
            ))}</ul>
          ) : <p className="text-sm text-text-hint">{t("No runners on this host.")}</p>}
        </section>
      </div>
    );
  }
  return (
    <>
      <SplitView.PaneHeader title={name} />
      <SplitView.PaneBody>{body}</SplitView.PaneBody>
    </>
  );
}

export default function Hosts({ route }: { route: Route }) {
  const fleet = useLive((s) => s.snapshot?.fleet);
  const hosts = fleet?.hosts ?? [];
  const param = route.params.get("tab");
  const tab: Tab = tabs.includes(param as Tab) ? (param as Tab) : "hosts";
  const name = route.id ?? null;
  const kubernetes = fleet?.backend === "kubernetes";
  const open = (next: string, replace = false) => navigate(format({ screen: "hosts", id: next, params: route.params }), { replace });
  const navigation = useSplitViewNavigation(hosts.map((host) => host.name), name, (next) => open(next, true));
  const shown = useRef(name);
  if (name !== null) shown.current = name;
  const poolRows = useMemo(() => pools(hosts), [hosts]);
  const tabNames: Record<Tab, string> = { hosts: kubernetes ? "Runtime targets" : "Hosts", pools: "Worker pools", resources: "Server resources", elastic: "Elastic limits" };
  const empty = <EmptyState title={t("No host observations yet.")} />;

  const content: Record<Tab, () => ReactNode> = {
    hosts: () => (
      <DataTable className="rows-3" aria-label={t(tabNames.hosts)} columns={hostColumns} rows={hosts} getRowId={(host) => host.name}
        onRowAction={(host) => open(host.name)} loading={!fleet} empty={empty} />
    ),
    pools: () => (
      <>
        <p className="summary-line">{t("Host capacity before the fleet slot limit applies.")}</p>
        <DataTable className="rows-2" aria-label={t("Worker pools")} columns={poolColumns} rows={poolRows} getRowId={(pool) => pool.name} loading={!fleet} empty={empty} />
      </>
    ),
    resources: () => <Resources hosts={hosts} />,
    elastic: () => <Elastic />,
  };

  return (
    <Tabs value={tab} onValueChange={(value) => setParams({ tab: value === "hosts" ? null : value })}>
      <Page title={t(kubernetes ? "Runtime targets" : "Hosts")} tabs={(
        <Tabs.List aria-label={t("Hosts")}>
          {tabs.map((value) => <Tabs.Tab key={value} value={value}>{t(tabNames[value])}</Tabs.Tab>)}
        </Tabs.List>
      )}>
        <SplitView className="page-split" open={name !== null} labels={labels.split} navigation={navigation}
          onOpenChange={(next) => !next && navigate(format({ screen: "hosts", params: route.params }))}>
          <SplitView.List>
            <Tabs.Panels>
              {tabs.map((value) => (
                <Tabs.Panel key={value} value={value} className="tab-content">{value === tab && content[value]()}</Tabs.Panel>
              ))}
            </Tabs.Panels>
          </SplitView.List>
          <SplitView.Pane>
            {shown.current !== null && <HostPane key={shown.current} name={shown.current} host={hosts.find((host) => host.name === shown.current)} />}
          </SplitView.Pane>
        </SplitView>
      </Page>
    </Tabs>
  );
}
