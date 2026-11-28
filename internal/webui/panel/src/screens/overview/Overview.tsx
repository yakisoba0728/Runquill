// Overview (design §3.2, §4.3): what needs attention, capacity, host health, GitHub connection, first-job progress and
// the jobs waiting longest. Ported from panelweb/views/overview.js.
import { Badge, DataTable, Disclosure, EmptyState, Skeleton, type Column } from "@yakihyuk0728/stratum-ui";
import { useMemo, type ReactNode } from "react";
import { attention, type AttentionItem } from "../../attention";
import { Ago, Clip, KeyValue, Meter, Page, StatTile, StatusBadge, Steps, UpdatedAgo } from "../../components";
import { isGitHubJobURL, jobStatus, pendingJob, poolName, summary, type Summary } from "../../fleet";
import { duration, number } from "../../format";
import { fill, serverText, t } from "../../i18n";
import { useLive } from "../../live";
import type { Job, Snapshot } from "../../types";

const jump = (id: string) => {
  const target = document.getElementById(id);
  target?.focus({ preventScroll: true });
  target?.scrollIntoView({ block: "start" });
};

function Attention({ items, at }: { items: AttentionItem[]; at?: string }) {
  if (!items.length) {
    return <p className="text-sm text-text-hint">{t("Nothing needs attention.")} {at && <UpdatedAgo at={at} />}</p>;
  }
  return (
    <ul className="attention">
      {items.map((item) => {
        const title = t(item.title, item.values);
        const detail = item.detail ?? (item.seen ? fill("Last seen {time}", { time: <Ago at={item.seen} /> }) : null);
        const body = <><Badge tone={item.tone} dot>{title}</Badge>{detail && <span className="attention-detail">{detail}</span>}</>;
        return (
          <li key={item.key}>
            {item.href ? <a href={item.href}>{body}</a>
              : <button type="button" onClick={() => jump(item.section!)}>{body}</button>}
          </li>
        );
      })}
    </ul>
  );
}

function Capacity({ counts }: { counts: Summary | null }) {
  const wait = <Skeleton className="w-16" />;
  return (
    <section className="stats" aria-label={t("Fleet capacity")}>
      <StatTile label={t("Running runners")} value={counts ? number(counts.running) : wait} />
      <StatTile label={t("Queued jobs")} value={counts ? number(counts.queued) : wait}>
        {counts && <div className="text-xs text-text-hint">{t("{ready} ready, {retrying} retrying", { ready: counts.ready, retrying: counts.retrying })}</div>}
      </StatTile>
      <StatTile label={t("Longest observed wait")} value={counts ? (counts.oldestWait >= 0 ? duration(counts.oldestWait) : "—") : wait} />
      <StatTile label={t("Slots in use")} value={counts ? t("{used} of {limit}", { used: number(counts.used), limit: number(counts.limit) }) : wait}>
        {counts && (
          <>
            <Meter value={counts.used} max={counts.limit} label={t("Slots in use")} />
            <div className="text-xs text-text-hint">
              {[t("{n} free", { n: counts.free }), t("{n} reserved", { n: counts.reserved }),
                counts.unassigned ? t("{n} unassigned", { n: counts.unassigned }) : ""].filter(Boolean).join(", ")}
            </div>
          </>
        )}
      </StatTile>
    </section>
  );
}

function Hosts({ counts }: { counts: Summary | null }) {
  const known = counts?.hostsKnown;
  const tag = (n: number, label: string, tone: "success" | "neutral" | "danger" | "warning") => (
    <Badge tone={n ? tone : "neutral"} dot={!!n}>{t(label)} {number(n)}</Badge>
  );
  return (
    <section className="block" aria-labelledby="hosts-title">
      <h2 id="hosts-title">{t("Hosts")}</h2>
      {!counts ? <Skeleton /> : !known ? <p className="text-sm text-text-hint">—</p> : (
        <div className="host-strip">
          <span className="text-sm"><strong className="tabular-nums">{number(counts.hosts)}</strong> {t(counts.kubernetes ? "Runtime targets" : "Worker hosts")}</span>
          {tag(counts.hostsUp, "Ready", "success")}
          {tag(counts.hostsOffline, "Offline", "neutral")}
          {tag(counts.hostsUnavailable, "Unavailable", "danger")}
          {tag(counts.hostsCordoned, "On maintenance", "warning")}
        </div>
      )}
    </section>
  );
}

function GitHub({ snapshot }: { snapshot: Snapshot | null }) {
  const scope = snapshot?.fleet?.repository_scope;
  const connected = !!(snapshot?.last_webhook || snapshot?.last_poll);
  const rows: [string, ReactNode][] = [];
  if (scope === "selected" || scope === "all") rows.push([t("Configured access"), t(scope === "selected" ? "Only selected repositories" : "All repositories")]);
  // RUNQUILL_ALLOW_PUBLIC_REPOSITORIES is a deployment setting, so the panel only reports it.
  if (snapshot?.fleet?.public_repositories) rows.push([t("Public repositories"), t("Allowed, fork runs refused")]);
  rows.push(
    [t("App connection"), <Badge tone={connected ? "success" : "neutral"} dot>{t(connected ? "Activity verified" : "Awaiting evidence")}</Badge>],
    [t("Last matched webhook"), snapshot?.webhook ? <Ago at={snapshot.last_webhook} /> : t("Disabled")],
    [t("Last successful poll"), !snapshot?.polling ? t("Disabled") : snapshot.poll_failed
      ? <Badge tone="danger" dot>{fill("Failed, last success {time}", { time: <Ago at={snapshot.last_poll} /> })}</Badge> : <Ago at={snapshot.last_poll} />],
  );
  return (
    <section className="block" aria-labelledby="github-connection">
      <h2 id="github-connection" tabIndex={-1}>{t("GitHub connection")}</h2>
      {snapshot ? <KeyValue rows={rows} /> : <Skeleton />}
    </section>
  );
}

function FirstJob({ snapshot }: { snapshot: Snapshot }) {
  const connected = !!(snapshot.last_webhook || snapshot.last_poll);
  const clean = snapshot.first_job_clean;
  const steps = [
    { title: t("Connect the GitHub App"), done: connected,
      note: t(connected ? "Runquill observed a GitHub delivery or a successful poll." : "Run setup and install the App on your personal account.") },
    { title: t("Run the first workflow"), done: !!snapshot.first_job_seen,
      note: snapshot.first_job_seen ? t("Runquill discovered the first-job workflow.")
        : fill("In a private repository, run {command}, commit the workflow, then select Run workflow on GitHub.", { command: <code>runquill workflow init</code> }) },
    { title: t("Confirm cleanup"), done: !!clean,
      note: clean ? t("Runner cleanup confirmed for job #{id}.", { id: clean.id }) : t("When the job finishes, Runquill removes its runner. Check the job result on GitHub.") },
  ];
  const links = (
    <p className="links text-sm">
      <a href="https://github.com/yakisoba0728/Runquill#verify-your-first-job" target="_blank" rel="noopener noreferrer">{t("Setup guide")}</a>
      {isGitHubJobURL(clean?.url) && <a href={clean.url} target="_blank" rel="noopener noreferrer">{t("View first job on GitHub")}</a>}
    </p>
  );
  const done = steps.every((step) => step.done);
  return (
    <section className="block" aria-labelledby="first-job">
      {done ? (
        <Disclosure>
          <Disclosure.Trigger level={2} id="first-job">{t("First job setup complete")}</Disclosure.Trigger>
          <Disclosure.Content><Steps steps={steps} />{links}</Disclosure.Content>
        </Disclosure>
      ) : (
        <>
          <h2 id="first-job" tabIndex={-1}>{t("First job setup")}</h2>
          <Steps steps={steps} />
          {links}
        </>
      )}
    </section>
  );
}

function Waiting({ snapshot }: { snapshot: Snapshot | null }) {
  const pending = useMemo(() => (snapshot?.fleet?.jobs ?? []).filter(pendingJob)
    .sort((a, b) => (a.queue_position || Infinity) - (b.queue_position || Infinity)), [snapshot]);
  const shown = pending.slice(0, 8);
  const columns: Column<Job>[] = [
    { key: "position", header: t("Position"), align: "end", width: 90, cell: (job) => job.queue_position ?? "—" },
    { key: "repository", header: t("Repository"), width: "minmax(180px, 2fr)", cell: (job) => (
      <a href={"#/jobs/" + job.id} aria-label={t("{repo} job {id}", { repo: job.repository, id: job.id })}>{job.repository}</a>
    ) },
    { key: "id", header: t("Job"), width: 90, cell: (job) => "#" + job.id },
    { key: "pool", header: t("Pool"), cell: (job) => poolName(job.pool) },
    { key: "status", header: t("State"), cell: (job) => <StatusBadge status={jobStatus(job)} /> },
    { key: "reason", header: t("Reason"), width: "minmax(200px, 3fr)", cell: (job) => {
      const reason = serverText(job.diagnostic?.message || job.retry_reason || "");
      return reason ? <Clip text={reason} /> : "—";
    } },
    { key: "wait", header: t("Queue time"), align: "end", cell: (job) => duration(job.timing?.runquill_queue_seconds) },
  ];
  return (
    <section className="block" aria-labelledby="waiting-title">
      <div className="block-head">
        <h2 id="waiting-title">{t("Waiting jobs")}</h2>
        <a className="text-sm" href="#/jobs">{t("Open jobs")}</a>
      </div>
      <DataTable
        aria-labelledby="waiting-title"
        columns={columns}
        rows={shown}
        getRowId={(job) => job.id}
        density="compact"
        loading={!snapshot}
        empty={<EmptyState title={t("The queue is empty.")} />}
      />
      {pending.length > shown.length && (
        <p className="text-sm text-text-hint">{t("Showing {shown} of {total} waiting jobs. Open the queue for the rest.", { shown: shown.length, total: pending.length })}</p>
      )}
    </section>
  );
}

export default function Overview() {
  const snapshot = useLive((s) => s.snapshot);
  const counts = useMemo(() => summary(snapshot), [snapshot]);
  const items = useMemo(() => attention(snapshot), [snapshot]);
  return (
    <Page title={t("Overview")}>
      <section className="block" aria-labelledby="attention-title">
        <h2 id="attention-title">{items.length ? t("Needs attention ({n})", { n: items.length }) : t("Needs attention")}</h2>
        {snapshot ? <Attention items={items} at={snapshot.sampled_at} /> : <Skeleton />}
      </section>
      <Capacity counts={counts} />
      <Hosts counts={counts} />
      <div className="pair">
        <GitHub snapshot={snapshot} />
        {snapshot ? <FirstJob snapshot={snapshot} /> : <section className="block"><Skeleton /></section>}
      </div>
      <Waiting snapshot={snapshot} />
    </Page>
  );
}
