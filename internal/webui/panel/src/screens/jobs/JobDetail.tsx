// The job detail pane (design §3.3): state, reason, stage timing, resources, details and timeline. A job in the live
// snapshot shows its live fields; GET /api/jobs/{id} adds resources and execution time, and stands in once the job
// has left the snapshot. Operators retry or cancel a job waiting without a runner from the footer or the palette.
import { Button, Skeleton, SplitView, toast } from "@yakihyuk0728/stratum-ui";
import { useEffect, useMemo, useRef, type ReactNode } from "react";
import { post, useBusy } from "../../actions";
import type { ApiError } from "../../api";
import { useScreenCommands, type ScreenCommand } from "../../commands";
import { KeyValue, Notice, StatusBadge } from "../../components";
import { ResourceMeters } from "../../components/ResourceMeters";
import { StageBar } from "../../components/StageBar";
import { confirm } from "../../dialogs";
import { isGitHubJobURL, jobReason, jobStatus, pendingJob, poolName } from "../../fleet";
import { timeOfDay, date } from "../../format";
import { serverText, t } from "../../i18n";
import { useRequest } from "../../request";
import { can } from "../../session";
import type { HistoryJob, Job } from "../../types";

const enc = encodeURIComponent;

/** A history row's outcome as a status code. */
export const outcomeStatus = (job: HistoryJob) => (job.outcome === "waiting" ? (job.retry_at ? "RETRY" : "WAITING") : job.outcome.toUpperCase());

/** The history's reason: why it failed, or why a waiting job last retried. */
export const historyReason = (job: HistoryJob) =>
  serverText(job.failure || job.diagnostic?.message || (job.outcome === "waiting" ? job.retry_reason : "") || "");

export type Action = "retry" | "cancel";

export const actionText: Record<Action, { label: string; command: string; question: string; done: string }> = {
  retry: {
    label: "Retry now", command: "Retry job #{id} now", done: "The job will be retried on the next scheduler pass.",
    question: "Retry job #{id} now? Runquill tries to start a runner on its next pass.",
  },
  cancel: {
    label: "Cancel job", command: "Cancel job #{id}", done: "Job cancelled.",
    question: "Cancel job #{id} in Runquill? It is marked failed and no runner starts for it. The run stays queued on GitHub until you cancel it there.",
  },
};

/**
 * Retry and cancel for operators (v1 jobActionButtons): only for a job waiting without a runner, retry only while it
 * has a retry delay. The pane footer, the palette and the waiting tab's row menu offer the same.
 */
export function jobActions(live?: Job, history?: HistoryJob): Action[] {
  const waiting = live ? pendingJob(live) && !live.runner : history?.outcome === "waiting";
  return !can("operator") || !waiting ? [] : (live ?? history)?.retry_at ? ["retry", "cancel"] : ["cancel"];
}

/** Asks first (cancel as a danger); returnFocus takes focus after the dialog when its opener is gone. */
export const askJobAction = (kind: Action, id: number, returnFocus?: HTMLElement | null) => confirm({
  title: t("Job #{id}", { id }), description: t(actionText[kind].question, { id }), confirmLabel: t(actionText[kind].label),
  danger: kind === "cancel", returnFocus,
});

/** Sends the action; true once the server applied it (a failure is a toast). */
export async function applyJobAction(kind: Action, id: number) {
  if ((await post("./api/jobs/" + id + "/" + kind)) === undefined) return false;
  toast(t(actionText[kind].done), { tone: "success" });
  return true;
}

/** The pane footer's buttons; onDone runs after the server applied the change. */
function JobActions({ id, live, history, open, onDone }: {
  id: number; live?: Job; history?: HistoryJob; open: boolean; onDone: () => void;
}) {
  const kinds = jobActions(live, history);
  const [busy, run] = useBusy<Action>();
  const buttons = useRef<Partial<Record<Action, HTMLButtonElement | null>>>({});
  const act = async (kind: Action) => {
    if (!(await askJobAction(kind, id, buttons.current[kind]))) return;
    await run(kind, async () => {
      if (await applyJobAction(kind, id)) onDone();
    });
  };
  const latest = useRef(act);
  latest.current = act;
  const commands = useMemo<ScreenCommand[]>(() => (open ? kinds : []).map((kind) => (
    { id: "job-" + kind, label: actionText[kind].command, values: { id: String(id) }, run: () => void latest.current(kind) }
  )), [open, kinds.join(), id]);
  useScreenCommands(commands);
  if (!kinds.length) return null;
  return (
    <SplitView.PaneFooter className="pane-actions">
      {kinds.map((kind) => (
        <Button key={kind} ref={(button) => void (buttons.current[kind] = button)} variant={kind === "cancel" ? "danger" : "secondary"} size="sm" loading={busy === kind} onClick={() => void act(kind)}
          aria-label={t("{action} for job {id}", { action: t(actionText[kind].label), id })}>{t(actionText[kind].label)}</Button>
      ))}
    </SplitView.PaneFooter>
  );
}

export function JobPane({ id, live, refresh, open, onDone }: { id: number; live?: Job; refresh: number; open: boolean; onDone: () => void }) {
  const status = live ? jobStatus(live) : "";
  const loaded = useRequest<HistoryJob>("./api/jobs/" + id);
  // Load again when the live state moves on (v1 loadInsight keyed by id and state) or on Refresh.
  const first = useRef(true);
  useEffect(() => {
    if (first.current) first.current = false;
    else loaded.reload();
  }, [status, refresh]);
  // A job not in the history yet has nothing recorded: 404 is "no record", not an error.
  const missing = loaded.error?.status === 404;
  const error = missing ? undefined : loaded.error;
  const history = loaded.data;
  const repository = live?.repository ?? history?.repository;

  let body: ReactNode;
  if (!live && !history) {
    body = loaded.loading ? <Skeleton /> : error ? <ErrorNotice error={error} reload={loaded.reload} />
      : <p className="text-sm text-text-hint">{t("Job #{id} is not in the queue or the 7-day job history.", { id })}</p>;
  } else {
    body = <Facts live={live} history={history} loading={loaded.loading && !history} error={error} reload={loaded.reload} />;
  }
  return (
    <>
      <SplitView.PaneHeader title={t("Job #{id}", { id })} subtitle={repository} />
      <SplitView.PaneBody>{body}</SplitView.PaneBody>
      <JobActions id={id} live={live} history={history} open={open} onDone={onDone} />
    </>
  );
}

function ErrorNotice({ error, reload }: { error: ApiError; reload: () => void }) {
  return (
    <Notice tone="danger" role="alert">
      <p>{error.status === 403 ? t("You do not have access to this operation.") : error.message}</p>
      <Button variant="secondary" size="sm" onClick={reload}>{t("Try again")}</Button>
    </Notice>
  );
}

function Facts({ live, history, loading, error, reload }: {
  live?: Job; history?: HistoryJob; loading: boolean; error?: ApiError; reload: () => void;
}) {
  const url = live?.url ?? history?.url;
  // v1: the reason, then the retry reason when it says something else.
  const retry = live?.retry_reason ? serverText(live.retry_reason) : "";
  const why = live ? jobReason(live) : historyReason(history!);
  const reason = [why, retry !== why ? retry : ""].filter(Boolean).join(" ");
  // Live timing grows with the queue; execution time comes only from the history.
  const timing = live ? { ...live.timing, execution_seconds: live.timing?.execution_seconds ?? history?.timing?.execution_seconds } : history?.timing;
  const pool = live?.pool ?? history?.pool;
  const host = live?.runner_host ?? history?.host;
  const label = live?.runner_label ?? history?.runner_label;
  const os = live?.runner_os ?? history?.runner_os;
  const source = timing?.discovery_source;
  const rows: [ReactNode, ReactNode][] = [
    [t("Discovered by"), source ? t(source === "poll" ? "GitHub API polling" : "Webhook") : "—"],
    [t("Pool"), pool ? poolName(pool) : "—"],
    [t("Host"), host ? <a href={"#/hosts/" + enc(host)}>{host}</a> : "—"],
  ];
  if (live) rows.push([t("Runner"), live.runner ? <a className="font-mono" href={"#/runners/" + enc(live.runner)}>{live.runner}</a> : "—"]);
  rows.push([t("Runner label"), label ? <code>{label}</code> : "—"], [t("Runner OS"), os || "—"]);
  if (live) rows.push([t("Runner image"), live.runner_image ? <span className="font-mono">{live.runner_image}</span> : "—"]);
  const retryAt = live?.retry_at ?? (history?.outcome === "waiting" ? history.retry_at : undefined);
  if (retryAt) rows.push([t("Retry time"), timeOfDay(retryAt)]);
  // A live job's own fields are current; the history's outcome and times stand in once it has left the snapshot.
  if (history && !live) {
    rows.push([t("Outcome"), <StatusBadge status={outcomeStatus(history)} />], [t("Queued"), date(history.queued_at)],
      [t("Started"), date(history.started_at)], [t("Finished"), date(history.finished_at)]);
  }
  const events = ([
    ["Created on GitHub", timing?.github_created_at],
    ["Discovered", timing?.discovered_at],
    ["JIT requested", timing?.jit_requested_at],
    ["Runner provisioned", timing?.jit_registered_at || timing?.provisioned_at],
    ["Runtime accepted launch", timing?.runner_launch_at],
    ["Job started on GitHub", timing?.github_started_at],
    ["Completion observed", live?.finished_at ?? history?.finished_at],
    ["Runner cleaned up", live?.cleaned_at ?? history?.cleaned_at],
  ] as const).filter(([, at]) => at);
  return (
    <div className="pane-sections">
      <div className="pane-lead">
        <StatusBadge status={live ? jobStatus(live) : outcomeStatus(history!)} />
        {isGitHubJobURL(url) && <a href={url} target="_blank" rel="noopener noreferrer">{t("Open in GitHub")}</a>}
      </div>
      {reason && <Notice role="status"><p>{reason}</p></Notice>}
      <section aria-labelledby="job-timing">
        <h3 id="job-timing">{t("Stage timing")}</h3>
        <StageBar timing={timing} />
      </section>
      <section aria-labelledby="job-resources">
        <h3 id="job-resources">{t("Resources")}</h3>
        {loading ? <p className="text-sm text-text-hint">{t("Loading resource samples…")}</p>
          : error ? <ErrorNotice error={error} reload={reload} /> : <ResourceMeters resources={history?.resources} />}
      </section>
      <section aria-labelledby="job-details">
        <h3 id="job-details">{t("Details")}</h3>
        <KeyValue rows={rows} />
      </section>
      {events.length > 0 && (
        <section aria-labelledby="job-timeline">
          <h3 id="job-timeline">{t("Timeline")}</h3>
          <KeyValue rows={events.map(([label, at]) => [t(label), <time dateTime={at!}>{timeOfDay(at)}</time>])} />
        </section>
      )}
    </div>
  );
}
