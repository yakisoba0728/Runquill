// Backups (design §3.8, §4.12), admins only: the schedule, last scheduled run and last verification from
// GET /api/backups/status, POST /api/backups/verify (up to 30 minutes) and the queue backup download (POST /api/backup,
// streamed with progress, recorded in the audit log). Restoring happens on the manager only. Ported from v1 backups.js.
// The sample panel shows its fixture with no verify or download.
import { Badge, Button, EmptyState, Progress, Skeleton } from "@yakihyuk0728/stratum-ui";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { saveFile } from "../../actions";
import { download, errorText, isSample, request, type ApiError } from "../../api";
import { useScreenCommands, type ScreenCommand } from "../../commands";
import { KeyValue, Notice, Page, StatusLine } from "../../components";
import { bytes, date, duration, number } from "../../format";
import { fill, serverText, t } from "../../i18n";
import { useRequest } from "../../request";
import { can } from "../../session";

interface Verification {
  name: string;
  checked_at: string;
  ok: boolean;
  error?: string;
}

interface BackupStatus {
  scheduled: boolean;
  interval_seconds?: number;
  directory?: string;
  keep?: number;
  sealing: boolean;
  remote?: { configured: boolean; bucket?: string; endpoint_host?: string; region?: string; prefix?: string; keep?: number };
  last?: {
    last_attempt?: string; last_success?: string; name?: string; size?: number; sealed?: boolean;
    remote?: string; remote_at?: string; remote_pending?: number; remote_error?: string; error?: string;
  };
  verification?: Verification;
}

// Environment variables that turn scheduled backups on, by name only.
const variables: [string, string][] = [
  ["RUNQUILL_BACKUP_INTERVAL", "How often to back up, such as 24h. At least 1m."],
  ["RUNQUILL_BACKUP_DIR", "Directory on the manager that keeps the copies."],
  ["RUNQUILL_BACKUP_KEEP", "Optional. Copies to keep in the directory (default 7)."],
  ["RUNQUILL_BACKUP_PASSPHRASE", "Optional. Seals each copy with a passphrase (or use RUNQUILL_BACKUP_PASSPHRASE_FILE)."],
  ["RUNQUILL_BACKUP_S3_ENDPOINT", "Optional. Also uploads to S3-compatible storage, with RUNQUILL_BACKUP_S3_BUCKET, RUNQUILL_BACKUP_S3_ACCESS_KEY_ID and RUNQUILL_BACKUP_S3_SECRET_ACCESS_KEY."],
];

type Row = [ReactNode, ReactNode] | false | undefined | "" | 0;
const rows = (list: Row[]) => list.filter(Boolean) as [ReactNode, ReactNode][];
const withNote = (value: ReactNode, note?: string | null) => (note ? <>{value}<span className="block-sub">{note}</span></> : value);
const onOff = (on: boolean, onLabel = "On", offLabel = "Off") => <Badge tone={on ? "success" : "neutral"} dot>{t(on ? onLabel : offLabel)}</Badge>;
const mono = (text?: string) => <span className="font-mono">{text || "—"}</span>;
const failed = (text: string) => <span className="text-danger">{serverText(text)}</span>;

function scheduleRows(status: BackupStatus) {
  const remote = status.remote ?? { configured: false };
  return rows([
    [t("Scheduled backups"), onOff(status.scheduled, "Enabled", "Disabled")],
    status.scheduled && [t("Interval"), duration(status.interval_seconds)],
    status.scheduled && [t("Copies kept"), number(status.keep)],
    status.scheduled && [t("Directory"), mono(status.directory)],
    [t("Sealing"), withNote(onOff(status.sealing), status.sealing ? t("Copies are encrypted with the backup passphrase.") : null)],
    status.scheduled && [t("Remote copy"), withNote(onOff(remote.configured),
      remote.configured ? t("{bucket} at {host}", { bucket: remote.bucket ?? "", host: remote.endpoint_host || "—" }) : null)],
    remote.configured && [t("Remote region"), mono(remote.region)],
    remote.configured && remote.prefix && [t("Remote prefix"), mono(remote.prefix)],
    remote.configured && [t("Remote copies kept"), remote.keep ? number(remote.keep) : t("All (never pruned)")],
  ]);
}

function lastRows(status: BackupStatus) {
  const last = status.last ?? {};
  // A successful run clears the error, so any error text means the latest run failed.
  const result = last.error ? <Badge tone="danger" dot>{t("Failed")}</Badge>
    : last.last_attempt ? <Badge tone="success" dot>{t("Succeeded")}</Badge> : <Badge dot>{t("No run yet")}</Badge>;
  const remote = last.remote === "uploaded" ? <Badge tone="success" dot>{t("Uploaded")}</Badge>
    : last.remote === "failed" ? <Badge tone="danger" dot>{t("Upload failed")}</Badge> : "—";
  return rows([
    [t("Result"), result],
    [t("Last attempt"), date(last.last_attempt)],
    [t("Last success"), date(last.last_success)],
    last.name && [t("File"), mono(last.name)],
    last.size && [t("Size"), bytes(last.size)],
    last.name && [t("Sealed"), t(last.sealed ? "Yes" : "No")],
    status.remote?.configured && [t("Remote upload"), withNote(remote, last.remote_at ? t("At {time}", { time: date(last.remote_at) }) : null)],
    last.remote_pending && [t("Waiting to upload"), number(last.remote_pending)],
    last.remote_error && [t("Upload error"), failed(last.remote_error)],
    last.error && [t("Error"), failed(last.error)],
  ]);
}

const verificationRows = (verification: Verification) => rows([
  [t("Result"), verification.ok ? <Badge tone="success" dot>{t("Verified")}</Badge> : <Badge tone="danger" dot>{t("Verification failed")}</Badge>],
  [t("File"), mono(verification.name)],
  [t("Checked at"), date(verification.checked_at)],
  verification.error && [t("Error"), failed(verification.error)],
]);

type Line = { text: ReactNode; tone?: "ok" | "failed" };

// A verification keeps running when the screen closes; coming back shows it running and then its result.
let verifying: Promise<Line> | null = null;

function verify(): Promise<Line> {
  verifying ??= request<Verification>("./api/backups/verify", { method: "POST", timeout: 30 * 60 * 1000 + 10000 }).then(
    (result) => ({ text: t(result?.ok ? "The latest backup was verified." : "Verification failed."), tone: result?.ok ? "ok" : "failed" } as Line),
    (error: ApiError) => ({ text: error.status === 401 ? "" : errorText(error), tone: "failed" } as Line),
  ).finally(() => (verifying = null));
  return verifying;
}

function QueueBackup({ admin, register }: { admin: boolean; register: (start: (() => void) | null) => void }) {
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState<number | null>(null);
  const [line, setLine] = useState<Line | null>(null);
  const alive = useRef(true);
  useEffect(() => () => void (alive.current = false), []);
  const start = async () => {
    if (busy) return;
    setBusy(true);
    setProgress(null);
    setLine({ text: t("Preparing queue backup…") });
    let shown = "";
    try {
      // Large queues take a while to copy and send, so there is no deadline.
      const blob = await download("./api/backup", "application/vnd.sqlite3", (received, total) => {
        if (!alive.current) return;
        if (total) setProgress(Math.floor((received * 100) / total));
        const text = total ? Math.floor((received * 100) / total) + "%" : bytes(received);
        if (text !== shown) setLine({ text: t("Downloading queue backup… {progress}", { progress: (shown = text) }) });
      });
      saveFile(blob, "runquill-queue-backup.sqlite3");
      if (alive.current) setLine({ text: t("Queue backup downloaded."), tone: "ok" });
    } catch (error) {
      if (alive.current) setLine((error as ApiError).status === 401 ? null : { text: errorText(error), tone: "failed" });
    } finally {
      if (alive.current) setBusy(false);
    }
  };
  const latest = useRef(start);
  latest.current = start;
  useEffect(() => {
    register(admin && !busy ? () => void latest.current() : null);
    return () => register(null);
  }, [admin, busy]);
  return (
    <section className="block" aria-labelledby="backup-queue">
      <div className="block-head">
        <h2 id="backup-queue">{t("Queue backup")}</h2>
        {admin && <Button variant="secondary" loading={busy} onClick={() => void start()}>{t("Download queue backup")}</Button>}
      </div>
      <p className="text-sm text-text-hint">{t(admin
        ? "A consistent, unsealed copy of the job queue database, downloaded now. It is recorded in the audit log."
        : "A consistent copy of the job queue database.")}</p>
      {!admin && <p>{t(isSample() ? "Downloads are not available in the sample panel." : "Only admins can download queue backups.")}</p>}
      {busy && <Progress value={progress} label={t("Queue backup progress")} />}
      <StatusLine {...line} />
    </section>
  );
}

export default function Backups() {
  const status = useRequest<BackupStatus>("./api/backups/status");
  const data = status.data;
  const admin = can("admin");
  const [checking, setChecking] = useState(!!verifying);
  const [verified, setVerified] = useState<Line | null>(null);
  const [downloadStart, setDownloadStart] = useState<(() => void) | null>(null);

  const runVerify = async (promise = verify()) => {
    setChecking(true);
    setVerified({ text: t("Verifying the latest local backup…") });
    const line = await promise;
    setVerified(line.text ? line : null);
    setChecking(false);
    status.reload();
  };
  useEffect(() => {
    if (verifying) void runVerify(verifying);
  }, []);

  const latest = useRef({ reload: status.reload, verify: runVerify, download: downloadStart });
  latest.current = { reload: status.reload, verify: runVerify, download: downloadStart };
  const canVerify = admin && !!data?.scheduled && !checking;
  const commands = useMemo<ScreenCommand[]>(() => [
    { id: "refresh", label: "Refresh backup status", run: () => latest.current.reload() },
    ...(canVerify ? [{ id: "verify", label: "Verify latest backup", run: () => void latest.current.verify() }] : []),
    ...(downloadStart ? [{ id: "download", label: "Download queue backup", run: () => latest.current.download?.() }] : []),
  ], [canVerify, !!downloadStart]);
  useScreenCommands(commands);

  const hasLast = data && (data.scheduled || data.last?.last_attempt || data.last?.error);
  return (
    <Page title={t("Backups")}
      description={fill("Restore a backup on the manager with {command}; the panel never restores.", { command: <code>runquill restore</code> })}
      actions={<Button variant="secondary" onClick={status.reload} loading={status.loading && !!data} disabled={checking}>{t("Refresh")}</Button>}>
      {status.error && (
        <Notice tone="danger" role="alert">
          <p>{status.error.message}</p>
          <Button variant="secondary" size="sm" onClick={status.reload}>{t("Try again")}</Button>
        </Notice>
      )}
      <div className="pair">
        <section className="block" aria-labelledby="backup-schedule">
          <h2 id="backup-schedule">{t("Schedule")}</h2>
          {data ? <KeyValue label={t("Backup schedule")} rows={scheduleRows(data)} />
            : status.error ? <p className="text-text-hint">{t("Backup status is unavailable.")}</p> : <Skeleton />}
        </section>
        <section className="block" aria-labelledby="backup-last">
          <h2 id="backup-last">{t("Last scheduled run")}</h2>
          {hasLast ? <KeyValue label={t("Last scheduled run")} rows={lastRows(data)} /> : data ? <p className="text-text-hint">{t("No scheduled run yet.")}</p> : null}
        </section>
      </div>
      {data?.scheduled && (
        <section className="block" aria-labelledby="backup-verify">
          <div className="block-head">
            <h2 id="backup-verify">{t("Backup verification")}</h2>
            {admin && <Button variant="secondary" loading={checking} onClick={() => void runVerify()}>{t("Verify latest backup")}</Button>}
          </div>
          <p className="text-sm text-text-hint">{t("Checks the latest local scheduled copy and its GitHub App identity.")}</p>
          {data.verification ? <KeyValue label={t("Last backup verification")} rows={verificationRows(data.verification)} />
            : <p className="text-text-hint">{t("No backup verification has run yet.")}</p>}
          <StatusLine {...verified} />
        </section>
      )}
      {data && !data.scheduled && (
        <section className="block" aria-labelledby="backup-enable">
          <EmptyState title={<span id="backup-enable">{t("How to enable")}</span>}
            description={t("Set these on the manager and restart it. Values stay on the server; the panel shows names only.")} />
          <KeyValue label={t("Backup environment variables")} rows={variables.map(([name, text]) => [<code>{name}</code>, t(text)])} />
        </section>
      )}
      <QueueBackup admin={admin} register={(start) => setDownloadStart(() => start)} />
    </Page>
  );
}
