// Runners (design §4.5): every runner container or Pod Runquill observes, and the leases it holds without an
// observation, with a detail pane. Ported from panelweb/views/runners.js. The pane has Details and, for operators and
// admins (and the sample panel, as in v1), Logs: #/runners/NAME/logs (v1 views/logs.js, RunnerLogs.tsx).
import { DataTable, EmptyState, SplitView, Tabs, useSplitViewNavigation, type Column } from "@yakihyuk0728/stratum-ui";
import { useMemo, useRef, type ReactNode } from "react";
import { isSample } from "../../api";
import { useScreenCommands, type ScreenCommand } from "../../commands";
import { KeyValue, Notice, Page, StatusBadge } from "../../components";
import { isGitHubJobURL, poolName, runnerRows, runnerStatus, type RunnerRow } from "../../fleet";
import { date } from "../../format";
import { t } from "../../i18n";
import { stratum } from "../../i18n/stratum";
import { useLive } from "../../live";
import { format, navigate, type Route } from "../../router";
import { can } from "../../session";
import type { Job } from "../../types";
import { RunnerLogs, unreadable } from "./RunnerLogs";

const labels = stratum();
const enc = encodeURIComponent;

function RunnerState({ runner }: { runner: RunnerRow }) {
  return runner.runtime_state ? <StatusBadge status={runnerStatus(runner)} /> : <StatusBadge status="UNKNOWN" label={t("Lease not observed")} />;
}

const image = (runner: RunnerRow) => runner.runner_image || runner.job?.runner_image;
const os = (runner: RunnerRow) => runner.runner_os || runner.job?.runner_os;

function JobLink({ runner }: { runner: RunnerRow }) {
  const job = runner.job;
  if (!job) return <span className="text-text-hint">{t("Untracked runner")}</span>;
  return <a href={"#/jobs/" + job.id}>{job.repository + " #" + job.id}</a>;
}

/** Runner logs: operators and admins with a session; the sample panel shows its fixed lines to everyone (v1). */
const logsAllowed = () => isSample() || can("operator");
const logsHref = (name: string) => "#/runners/" + enc(name) + "/logs";

const columns: Column<RunnerRow>[] = [
  { key: "name", header: t("Runner"), sticky: true, width: 220, cell: (runner) => <span className="font-mono">{runner.name}</span> },
  { key: "host", header: t("Host"), cell: (runner) => runner.host || "—" },
  { key: "pool", header: t("Pool"), cell: (runner) => poolName(runner.pool || runner.job?.pool) },
  { key: "state", header: t("Runtime state"), width: 200, cell: (runner) => <RunnerState runner={runner} /> },
  // A start time, not a running duration, so rows stay unchanged between snapshots.
  { key: "provisioned", header: t("Provisioned"), align: "end", cell: (runner) => date(runner.job?.timing?.provisioned_at) },
  { key: "image", header: t("Runner image"), width: "minmax(200px, 2fr)", cell: (runner) => (
    <>{image(runner) ? <span className="font-mono">{image(runner)}</span> : "—"}{os(runner) && <span className="block-sub">{t("Runner OS {name}", { name: os(runner)! })}</span>}</>
  ) },
  { key: "job", header: t("GitHub job"), width: "minmax(200px, 2fr)", cell: (runner) => <JobLink runner={runner} /> },
];

const logsColumn: Column<RunnerRow> = {
  key: "logs", header: t("Logs"), width: 110, cell: (runner) => unreadable(runner) ? null
    : <a href={logsHref(runner.name)} aria-label={t("View logs for {runner}", { runner: runner.name })}>{t("View logs")}</a>,
};

/** Which job the log belongs to, and where its step logs are (v1 logs pane head): the runner log is not the job log. */
function StepLogs({ job, host }: { job: Job; host?: string }) {
  return (
    <p className="summary-line">
      {job.repository + " #" + job.id + " · " + (host || "—") + " · "}
      {isGitHubJobURL(job.url)
        ? <a href={job.url} target="_blank" rel="noopener noreferrer" aria-label={t("Step logs for {repo} job {id} on GitHub", { repo: job.repository, id: job.id })}>{t("Step logs on GitHub")}</a>
        : t("Step logs are on GitHub.")}
    </p>
  );
}

function RunnerPane({ name, runner, tab, open }: { name: string; runner?: RunnerRow; tab: "details" | "logs"; open: boolean }) {
  // Logs keep their last lines once the runner has gone, so the tab stays for a runner that had them.
  const reason = runner ? unreadable(runner) : "";
  const hadLogs = useRef(false);
  if (runner && !reason) hadLogs.current = true;
  const logs = logsAllowed() && (hadLogs.current || !!reason);
  const shownTab = logs ? tab : "details";
  let body: ReactNode = <p className="text-sm text-text-hint">{t("The runner {name} is no longer listed.", { name })}</p>;
  if (runner) {
    const job = runner.job;
    const rows: [ReactNode, ReactNode][] = [
      [t("Host"), runner.host ? <a href={"#/hosts/" + enc(runner.host)}>{runner.host}</a> : "—"],
      [t("Pool"), poolName(runner.pool || job?.pool)],
      [t("Runtime state"), <RunnerState runner={runner} />],
      [t("Provisioned"), date(job?.timing?.provisioned_at)],
      [t("Runner image"), image(runner) ? <span className="font-mono">{image(runner)}</span> : "—"],
      [t("Runner OS"), os(runner) || "—"],
      [t("GitHub job"), <JobLink runner={runner} />],
    ];
    body = (
      <div className="pane-sections">
        <KeyValue rows={rows} />
        {isGitHubJobURL(job?.url) && <p className="m-0"><a href={job.url} target="_blank" rel="noopener noreferrer">{t("Open in GitHub")}</a></p>}
      </div>
    );
  }
  if (!logs) {
    return (
      <>
        <SplitView.PaneHeader title={name} />
        <SplitView.PaneBody>{body}</SplitView.PaneBody>
      </>
    );
  }
  const select = (value: string) => navigate(value === "logs" ? logsHref(name) : "#/runners/" + enc(name), { replace: true });
  return (
    <Tabs value={shownTab} onValueChange={select} className="pane-tabs">
      <SplitView.PaneHeader title={name} />
      <Tabs.List aria-label={t("Runner")}>
        <Tabs.Tab value="details">{t("Details")}</Tabs.Tab>
        <Tabs.Tab value="logs">{t("Logs")}</Tabs.Tab>
      </Tabs.List>
      <SplitView.PaneBody>
        <Tabs.Panels>
          <Tabs.Panel value="details">{shownTab === "details" && body}</Tabs.Panel>
          <Tabs.Panel value="logs">
            {shownTab === "logs" && (reason ? <Notice role="status"><p>{t(reason)}</p></Notice>
              : <>{runner?.job && <StepLogs job={runner.job} host={runner.host} />}<RunnerLogs name={name} active={open} gone={!runner} /></>)}
          </Tabs.Panel>
        </Tabs.Panels>
      </SplitView.PaneBody>
    </Tabs>
  );
}

export default function Runners({ route }: { route: Route }) {
  const snapshot = useLive((s) => s.snapshot);
  const rows = useMemo(() => runnerRows(snapshot), [snapshot]);
  const name = route.id ?? null;
  const observed = rows.filter((runner) => runner.runtime_state).length;
  const open = (next: string, replace = false) => navigate(format({ screen: "runners", id: next, params: new URLSearchParams() }), { replace });
  const navigation = useSplitViewNavigation(rows.map((runner) => runner.name), name, (next) => open(next, true));
  const shown = useRef(name);
  if (name !== null) shown.current = name;
  const fleet = snapshot?.fleet;
  const allowed = logsAllowed();
  const commands = useMemo<ScreenCommand[]>(() => (allowed ? rows : [])
    .filter((runner) => !unreadable(runner) && !(runner.name === name && route.sub === "logs"))
    .map((runner) => ({ id: "logs:" + runner.name, label: "View logs for {runner}", values: { runner: runner.name }, run: () => navigate(logsHref(runner.name)) })),
  [rows, name, route.sub, allowed]);
  useScreenCommands(commands);
  return (
    <Page title={t("Runners")} description={fleet ? t("{observed} observed, {leases} unobserved leases", { observed, leases: rows.length - observed }) : undefined}>
      <SplitView className="page-split" open={name !== null} onOpenChange={(next) => !next && navigate("#/runners")} navigation={navigation} labels={labels.split}>
        <SplitView.List>
          <div className="tab-content">
            <DataTable
              className="rows-2"
              aria-label={t("Runner inventory")}
              columns={allowed ? [...columns, logsColumn] : columns}
              rows={rows}
              getRowId={(runner) => runner.name}
              onRowAction={(runner) => open(runner.name)}
              loading={!fleet}
              empty={<EmptyState title={t(fleet?.docker_error ? "Runner observations are unavailable." : "No runners or retained leases.")} />}
            />
          </div>
        </SplitView.List>
        <SplitView.Pane>
          {shown.current !== null && (
            <RunnerPane key={shown.current} name={shown.current} runner={rows.find((runner) => runner.name === shown.current)}
              tab={route.sub === "logs" ? "logs" : "details"} open={name !== null} />
          )}
        </SplitView.Pane>
      </SplitView>
    </Page>
  );
}
