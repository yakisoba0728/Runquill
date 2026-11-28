// Jobs (design §3.3, §3.4, §4.4): the live queue (waiting, running, cleaned up) from the snapshot and the 7-day
// history from GET /api/jobs, one detail pane for all four. Ported from panelweb/views/queue.js and jobs.js. Admins
// check a repository from the header ("Missing a job?"); operators retry and cancel from the pane (JobDetail.tsx) and
// from a waiting row's menu.
import {
  Button, DataTable, EmptyState, Field, Input, Menu, SearchInput, Select, SplitView, Tabs, useSplitViewNavigation, type Column,
} from "@yakihyuk0728/stratum-ui";
import { useEffect, useMemo, useRef, useState, type MouseEvent, type ReactNode } from "react";
import { flushSync } from "react-dom";
import { useBusy } from "../../actions";
import { request, type ApiError } from "../../api";
import { useScreenCommands, type ScreenCommand } from "../../commands";
import { Clip, Notice, Page, StatusBadge } from "../../components";
import { RowMenu, rowMenuTabs } from "../../components/RowMenu";
import { filterJobs, isGitHubJobURL, jobLists, jobReason, jobStatus, poolName, statusLabel } from "../../fleet";
import { timeOfDay, date, duration, number } from "../../format";
import { fill, t } from "../../i18n";
import { stratum } from "../../i18n/stratum";
import { live as liveStore, useLive } from "../../live";
import { format, navigate, setParams, withParams, type Route } from "../../router";
import type { HistoryJob, Job } from "../../types";
import { refocus } from "../../dialogs";
import { can } from "../../session";
import { Diagnose } from "./Diagnose";
import { actionText, applyJobAction, askJobAction, historyReason, jobActions, JobPane, outcomeStatus, type Action } from "./JobDetail";
import { historyQuery, holder, jobId, outcomes, pickTab, ranges, tabs, type Tab } from "./params";

const labels = stratum();
const tabNames: Record<Tab, string> = { waiting: "Waiting", running: "Running", cleaned: "Cleaned up", history: "History" };
// Stratum's DataTable renders every row it gets; a live tab shows this many at a time (and always the open job).
const rowCap = 200;
const emptyText: Record<Exclude<Tab, "history">, string> = {
  waiting: "The queue is empty.", running: "No assigned jobs.", cleaned: "No confirmed runner cleanup yet.",
};

/** The address of a job (or of the list, id null) on this screen, keeping the filters. */
const jobHref = (route: Route, id: number | null, tab: Tab | null) =>
  format(withParams({ screen: "jobs", id: id === null ? undefined : String(id), params: route.params }, { tab }));

function liveColumns(tab: Tab, menu?: (job: Job) => ReactNode): Column<Job>[] {
  return [
    { key: "repository", header: t("Repository"), sticky: true, width: 180, cell: (job) => job.repository },
    { key: "id", header: t("Job"), width: 90, cell: (job) => "#" + job.id },
    ...(tab === "waiting" ? [{ key: "position", header: t("Position"), align: "end" as const, width: 90, cell: (job: Job) => job.queue_position ?? "—" }] : []),
    { key: "pool", header: t("Pool"), cell: (job) => (
      <>{poolName(job.pool)}{job.runner_os && <Clip className="block-sub" text={t("Runner OS {name}", { name: job.runner_os })} />}</>
    ) },
    { key: "status", header: t("State"), width: 160, cell: (job) => (
      <><StatusBadge status={jobStatus(job)} />{job.retry_at && <Clip className="block-sub" text={t("Retry time {time}", { time: timeOfDay(job.retry_at) })} />}</>
    ) },
    { key: "reason", header: t("Reason"), width: "minmax(220px, 3fr)", cell: (job) => jobReason(job) ? <Clip className="clamp-2" text={jobReason(job)} /> : "—" },
    // The queue time grows each snapshot; only this cell changes, and rows keep their id.
    { key: "wait", header: t("Queue time"), align: "end", width: 110, cell: (job) => duration(job.timing?.runquill_queue_seconds) },
    { key: "host", header: t("Host"), cell: (job) => job.runner_host || "—" },
    ...(tab === "waiting" && menu ? [{ key: "actions", header: <span className="sr-only">{t("Actions")}</span>, align: "end" as const, width: 56, cell: menu }] : []),
  ];
}

const historyColumns: Column<HistoryJob>[] = [
  { key: "repository", header: t("Repository"), sticky: true, width: 180, cell: (job) => isGitHubJobURL(job.url)
    ? <a href={job.url} target="_blank" rel="noopener noreferrer" aria-label={t("{repo} job {id} on GitHub", { repo: job.repository, id: job.id })}>{job.repository}</a>
    : job.repository },
  { key: "id", header: t("Job"), width: "minmax(140px, 1fr)", cell: (job) => (
    <>{"#" + job.id}{job.runner_label && <Clip className="block-sub font-mono" text={job.runner_label} />}</>
  ) },
  { key: "outcome", header: t("Outcome"), width: "minmax(200px, 2fr)", cell: (job) => {
    const reason = historyReason(job);
    return (
      <>
        <StatusBadge status={outcomeStatus(job)} />
        {reason && <Clip className="block-sub" text={job.failure || job.diagnostic ? t("Reason: {reason}", { reason }) : t("Last retry: {reason}", { reason })} />}
        {job.retry_at && <Clip className="block-sub" text={t("Retry time {time}", { time: timeOfDay(job.retry_at) })} />}
      </>
    );
  } },
  { key: "pool", header: t("Pool"), cell: (job) => (
    <>{job.pool ? poolName(job.pool) : "—"}{job.runner_os && <Clip className="block-sub" text={t("Runner OS {name}", { name: job.runner_os })} />}</>
  ) },
  { key: "host", header: t("Host"), cell: (job) => job.host || "—" },
  { key: "queued", header: t("Queued"), align: "end", cell: (job) => date(job.queued_at), sort: (a, b) => (a.queued_at ?? "").localeCompare(b.queued_at ?? "") },
  { key: "started", header: t("Started"), align: "end", cell: (job) => date(job.started_at) },
  { key: "finished", header: t("Finished"), align: "end", cell: (job) => date(job.finished_at) },
  { key: "wait", header: t("Queue time"), align: "end", width: 110, cell: (job) => duration(job.wait_seconds) },
];

/** GET /api/jobs, 25 at a time with a before cursor. Reloads when the filters or round change. */
function useHistory(params: URLSearchParams, active: boolean, round: number) {
  const key = ["repo", "outcome", "range"].map((name) => params.get(name) ?? "").join("\n");
  const [state, setState] = useState<{ rows: HistoryJob[]; next: number; loading: boolean; error?: ApiError }>({ rows: [], next: 0, loading: true });
  const more = useRef<AbortController | null>(null);
  useEffect(() => {
    if (!active) return;
    const controller = new AbortController();
    setState({ rows: [], next: 0, loading: true });
    request<{ jobs: HistoryJob[]; next?: number }>("./api/jobs?" + historyQuery(params), { signal: controller.signal }).then(
      (page) => setState({ rows: page.jobs ?? [], next: page.next ?? 0, loading: false }),
      (error: ApiError) => !controller.signal.aborted && error.status !== 401 && setState({ rows: [], next: 0, loading: false, error }),
    );
    return () => {
      controller.abort();
      more.current?.abort();
    };
  }, [key, active, round]);
  const loadMore = () => {
    more.current?.abort();
    const controller = (more.current = new AbortController());
    setState((last) => ({ ...last, loading: true }));
    request<{ jobs: HistoryJob[]; next?: number }>("./api/jobs?" + historyQuery(params, state.next), { signal: controller.signal }).then(
      (page) => setState((last) => ({ rows: [...last.rows, ...(page.jobs ?? [])], next: page.next ?? 0, loading: false })),
      (error: ApiError) => !controller.signal.aborted && error.status !== 401 && setState((last) => ({ ...last, loading: false, error })),
    );
  };
  return { ...state, loadMore };
}

export default function Jobs({ route }: { route: Route }) {
  const snapshot = useLive((s) => s.snapshot);
  // Like v1, the lists wait for a snapshot with fleet data.
  const lists = useMemo(() => (snapshot?.fleet ? jobLists(snapshot) : null), [snapshot]);
  const id = jobId(route.id);
  const tab = pickTab(route.params.get("tab"), id, lists);
  const query = route.params.get("q") ?? "";
  const pool = route.params.get("pool") ?? "";
  const state = route.params.get("state") ?? "";
  const [round, setRound] = useState(0);
  const [diagnose, setDiagnose] = useState(false);
  const admin = can("admin");
  const operator = can("operator");
  const diagnoseButton = useRef<HTMLButtonElement>(null);
  const history = useHistory(route.params, tab === "history", round);

  const pools = useMemo(() => [...new Set([...(lists ? [...lists.waiting, ...lists.running, ...lists.cleaned] : []),
    ...(snapshot?.fleet?.hosts ?? [])].map((item) => item.pool || "default"))].sort(), [lists, snapshot]);
  // The states in the tab, and the address's even when no job has it now.
  const states = useMemo(() => (lists && tab !== "history" ? [...new Set([...lists[tab].map(jobStatus), ...(state ? [state] : [])])].sort() : []),
    [lists, tab, state]);
  const rows = useMemo(() => (lists && tab !== "history" ? filterJobs(lists[tab], query, pool, state) : []), [lists, tab, query, pool, state]);
  // j/k and the pane's arrows walk every row; the table shows the first rows up to the limit, and up to the open job.
  const ids = tab === "history" ? history.rows.map((job) => job.id) : rows.map((job) => job.id);
  const [limit, setLimit] = useState(rowCap);
  useEffect(() => setLimit(rowCap), [tab, query, pool, state]);
  const openAt = id === null || tab === "history" ? -1 : ids.indexOf(id);
  const shownRows = useMemo(() => rows.slice(0, Math.max(limit, openAt + 1)), [rows, limit, openAt]);
  const showMore = (event: MouseEvent<HTMLElement>) => {
    // The button goes with the last rows: focus the first row it revealed instead.
    const list = event.currentTarget.closest(".tab-content");
    const first = shownRows.length;
    flushSync(() => setLimit(first + rowCap));
    list?.querySelectorAll<HTMLElement>('.data-table-body > [role="row"]')[first]?.focus();
  };

  // Opening from a live tab drops ?tab so the pane's tab follows the job; history keeps it.
  const open = (next: number, replace = false) =>
    navigate(jobHref(route, next, tab === "history" || (lists && holder(lists, next) !== tab) ? tab : null), { replace });
  const close = () => navigate(jobHref(route, null, tab === "waiting" ? null : tab));
  const openId = useRef(id);
  openId.current = id;
  const closeRef = useRef(close);
  closeRef.current = close;
  const navigation = useSplitViewNavigation(ids, id, (next) => open(next, true));
  const filtered = !!(query || pool || state);
  const clearFilters = () => setParams({ q: null, pool: null, state: null });

  // With a job open, a missing tab follows the job, so a chosen tab is always written.
  const showTab = (value: string) => setParams({ tab: value === "waiting" && id === null ? null : value });
  const commands = useMemo<ScreenCommand[]>(() => [
    { id: "waiting", label: "Show waiting jobs", run: () => showTab("waiting") },
    { id: "running", label: "Show running jobs", run: () => setParams({ tab: "running" }) },
    { id: "cleaned", label: "Show cleaned-up jobs", run: () => setParams({ tab: "cleaned" }) },
    { id: "history", label: "Show job history", run: () => setParams({ tab: "history" }) },
    ...(filtered || route.params.get("repo") || route.params.get("outcome") || route.params.get("range")
      ? [{ id: "clear", label: "Clear job filters", run: () => setParams({ q: null, pool: null, state: null, repo: null, outcome: null, range: null }) }] : []),
    ...(tab === "history" && history.next && !history.loading ? [{ id: "more", label: "Load older jobs", run: history.loadMore }] : []),
    ...(admin ? [{ id: "diagnose", label: "Check repository", run: () => setDiagnose(true) }] : []),
  ], [filtered, route, tab, id, history.next, history.loading, admin]);
  useScreenCommands(commands);

  // A waiting row's menu: the pane's retry and cancel (same question, request and toast).
  const [, runRow] = useBusy<Action>();
  const rowAction = async (kind: Action, job: Job) => {
    const trigger = document.activeElement as HTMLElement | null; // Menu.Item runs onSelect with focus back on the trigger
    const table = trigger?.closest<HTMLElement>(".data-table");
    if (!(await askJobAction(kind, job.id, trigger))) return;
    await runRow(kind, async () => {
      if (!(await applyJobAction(kind, job.id))) return;
      if (openId.current === job.id) closeRef.current();
      await liveStore.refresh();
      setRound((n) => n + 1);
      // The job leaves the queue with its row: keep focus in the table (its current row), else on the tab.
      refocus(table?.querySelector<HTMLElement>('[role="row"][tabindex="0"]') ?? document.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]'));
    });
  };
  const rowMenu = (job: Job) => {
    const kinds = jobActions(job);
    return kinds.length > 0 && (
      <RowMenu label={t("{action} for job {id}", { action: t("Actions"), id: job.id })}>
        {kinds.map((kind) => (
          <Menu.Item key={kind} tone={kind === "cancel" ? "danger" : undefined} onSelect={() => void rowAction(kind, job)}>{t(actionText[kind].label)}</Menu.Item>
        ))}
      </RowMenu>
    );
  };

  // Keep the last pane content through the SplitView's closing slide.
  const shown = useRef(id);
  if (id !== null) shown.current = id;
  const live = shown.current === null || !lists ? undefined
    : [...lists.waiting, ...lists.running, ...lists.cleaned].find((job) => job.id === shown.current);

  const counts = lists && { waiting: lists.waiting.length, running: lists.running.length, cleaned: lists.cleaned.length };
  const tabList = (
    <Tabs.List aria-label={t("Choose a job list")}>
      {tabs.map((value) => (
        <Tabs.Tab key={value} value={value} badge={value !== "history" && counts ? counts[value] : undefined}>{t(tabNames[value])}</Tabs.Tab>
      ))}
    </Tabs.List>
  );

  const liveContent = tab !== "history" && (
    <>
      <div className="toolbar">
        <JobSearch query={query} />
        <Select aria-label={t("Pool")} value={pool} onValueChange={(value) => setParams({ pool: value })}
          options={[{ value: "", label: t("All pools") }, ...pools.map((name) => ({ value: name, label: poolName(name) }))]} />
        <Select aria-label={t("State")} value={state} onValueChange={(value) => setParams({ state: value })}
          options={[{ value: "", label: t("All states") }, ...states.map((value) => ({ value, label: statusLabel(value) }))]} />
      </div>
      <DataTable
        className="rows-2"
        aria-label={t(tabNames[tab])}
        columns={liveColumns(tab, operator ? rowMenu : undefined)}
        rows={shownRows}
        getRowId={(job) => job.id}
        onRowAction={(job) => open(job.id)}
        onFocus={tab === "waiting" && operator ? rowMenuTabs : undefined}
        loading={!lists}
        empty={filtered
          ? <EmptyState title={t("No jobs match these filters.")} actions={<Button variant="secondary" size="sm" onClick={clearFilters}>{t("Clear filters")}</Button>} />
          : <EmptyState title={t(emptyText[tab])} description={tab === "waiting"
            ? fill("Missing a job? Run {command} to check GitHub App access, its job label and delivery.", { command: <code>runquill diagnose OWNER/REPO</code> })
            : undefined} actions={tab === "waiting" && admin
            ? <Button variant="secondary" size="sm" onClick={() => setDiagnose(true)}>{t("Check repository")}</Button> : undefined} />}
      />
      {rows.length > shownRows.length && (
        <div className="table-foot">
          <span className="text-sm text-text-hint">{t("{shown} of {total} shown", { shown: number(shownRows.length), total: number(rows.length) })}</span>
          <Button variant="secondary" size="sm" onClick={showMore}>{t("Show more")}</Button>
        </div>
      )}
    </>
  );

  const historyContent = tab === "history" && (
    <>
      <p className="summary-line">{t("Runquill keeps finished jobs for seven days. Job logs stay on GitHub.")}</p>
      <HistoryFilters params={route.params} onRefresh={() => setRound((n) => n + 1)} />
      {history.error && (
        <Notice tone="danger" role="alert">
          <p>{t("Job history is unavailable.")} {history.error.message}</p>
          <Button variant="secondary" size="sm" onClick={() => setRound((n) => n + 1)}>{t("Try again")}</Button>
        </Notice>
      )}
      <DataTable
        className="rows-3"
        aria-label={t("Job history")}
        columns={historyColumns}
        rows={history.rows}
        getRowId={(job) => job.id}
        onRowAction={(job) => open(job.id)}
        loading={history.loading && !history.rows.length}
        empty={<EmptyState title={t("No jobs match these filters.")} />}
      />
      <div className="table-foot">
        <span className="text-sm text-text-hint">{!history.loading && t("{n} shown", { n: number(history.rows.length) })}</span>
        {history.next > 0 && <Button variant="secondary" size="sm" loading={history.loading} onClick={history.loadMore}>{t("Load older jobs")}</Button>}
      </div>
    </>
  );

  return (
    <Tabs value={tab} onValueChange={showTab}>
      <Page title={t("Jobs")} description={t("The live queue and the 7-day job history")} tabs={tabList}
        actions={admin && <Button ref={diagnoseButton} variant="secondary" onClick={() => setDiagnose(true)}>{t("Check repository")}</Button>}>
        <SplitView className="page-split" open={id !== null} onOpenChange={(next) => !next && close()} navigation={navigation} labels={labels.split}>
          <SplitView.List>
            <Tabs.Panels>
              {tabs.map((value) => (
                <Tabs.Panel key={value} value={value} className="tab-content">
                  {value === tab && (value === "history" ? historyContent : liveContent)}
                </Tabs.Panel>
              ))}
            </Tabs.Panels>
          </SplitView.List>
          <SplitView.Pane>
            {shown.current !== null && <JobPane key={shown.current} id={shown.current} live={live} refresh={round} open={id !== null} onDone={() => {
              // The job leaves the queue (v1): close the pane, read the snapshot now and reload the history.
              close();
              void liveStore.refresh();
              setRound((n) => n + 1);
            }} />}
          </SplitView.Pane>
        </SplitView>
      </Page>
      {admin && <Diagnose open={diagnose} onOpenChange={(next) => {
        setDiagnose(next);
        if (!next) refocus(diagnoseButton.current); // opened from the palette, which is gone
      }} />}
    </Tabs>
  );
}

/**
 * The live tabs' search. Typing stays in the box and writes ?q= once it pauses: each address change re-renders the
 * whole screen, which takes long with a large queue.
 */
function JobSearch({ query }: { query: string }) {
  const [text, setText] = useState(query);
  const sent = useRef(query);
  useEffect(() => {
    // Clear filters, the palette or back/forward changed the address; the box already holds what it wrote itself.
    if (query !== sent.current) setText((sent.current = query));
  }, [query]);
  useEffect(() => {
    if (text === sent.current) return;
    const timer = setTimeout(() => setParams({ q: (sent.current = text) }), 250);
    return () => clearTimeout(timer);
  }, [text]);
  return (
    <SearchInput value={text} onChange={(event) => setText(event.currentTarget.value)} onClear={() => {
      setText("");
      setParams({ q: (sent.current = "") });
    }} placeholder={t("Repository or job ID")} aria-label={t("Filter jobs")} clearLabel={labels.search.clearLabel} className="toolbar-grow" />
  );
}

function HistoryFilters({ params, onRefresh }: { params: URLSearchParams; onRefresh: () => void }) {
  const [repo, setRepo] = useState(params.get("repo") ?? "");
  useEffect(() => setRepo(params.get("repo") ?? ""), [params.get("repo")]);
  return (
    <form className="toolbar" aria-label={t("Job filters")} onSubmit={(event) => {
      event.preventDefault();
      setParams({ repo: repo.trim() });
    }}>
      <Field label={t("Repository")} className="toolbar-grow">
        <Input type="search" value={repo} onChange={(event) => setRepo(event.currentTarget.value)} placeholder="OWNER/REPO" maxLength={140} />
      </Field>
      <Field label={t("Outcome")}>
        <Select value={params.get("outcome") ?? ""} onValueChange={(value) => setParams({ outcome: value })}
          options={[{ value: "", label: t("All outcomes") }, ...outcomes.map((value) => ({ value, label: t(value[0].toUpperCase() + value.slice(1)) }))]} />
      </Field>
      <Field label={t("Time range")}>
        <Select value={params.get("range") ?? ""} onValueChange={(value) => setParams({ range: value })}
          options={[{ value: "", label: t("Any time") }, ...ranges.map((value, i) => ({ value, label: t(["Last hour", "Last 24 hours", "Last 7 days"][i]) }))]} />
      </Field>
      <Button type="submit" variant="secondary">{t("Apply filters")}</Button>
      <Button type="button" variant="secondary" onClick={onRefresh}>{t("Refresh")}</Button>
    </form>
  );
}
