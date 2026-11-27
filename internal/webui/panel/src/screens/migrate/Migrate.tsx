// Workflow migration (design §3.9, §4.11), admins only: rewrite runs-on in one repository's workflow files so its jobs
// run on Runquill, through one reviewed pull request. GET /api/migrate/status, POST /api/migrate/preview and
// /api/migrate/open (95 s each). Four steps: permissions, repository and label mapping, preview, open. The repository,
// labels and diff are user input or repository content: text only. Ported from v1 migrate.js. The sample panel shows a
// fixed preview (read with GET) and no open step.
import { Accordion, Badge, Button, DataTable, Field, Input, Select, toast, type Column } from "@yakihyuk0728/stratum-ui";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { errorText, isSample, request, type ApiError } from "../../api";
import { KeyValue, Notice, Page, StatusLine } from "../../components";
import { confirm } from "../../dialogs";
import { poolName } from "../../fleet";
import { number } from "../../format";
import { fill, language, t } from "../../i18n";
import { useLive } from "../../live";
import { can } from "../../session";
import { DiffView } from "./DiffView";
import { maxRows, readMapping, sentence, totals, type MappingRow, type MigrateRequest, type Preview, type PreviewFile } from "./mapping";

// Previews read up to 50 files and opening commits each changed file; the server allows 90 seconds.
const slow = 95000;
const branchPrefix = "runquill/migrate-runs-on-";
const permissions: [string, string][] = [["contents", "Contents"], ["pull_requests", "Pull requests"], ["workflows", "Workflows"]];

const count = (n: number, one: string, many: string) => t(n === 1 ? one : many, { n: n.toLocaleString(language) });
const jobCount = (n: number) => count(n, "{n} job", "{n} jobs");
const fileCount = (n: number) => count(n, "{n} file", "{n} files");
const pullRequestURL = (url: unknown): url is string =>
  typeof url === "string" && /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/\d+$/.test(url);
const link = (url: string) => <a href={url} target="_blank" rel="noopener noreferrer">{url}</a>;
// Migrate handlers answer JSON with a code; plain-text errors (auth, busy, unconfigured) have none.
const failure = (error: unknown) => (error as ApiError)?.code ? sentence((error as ApiError).message) : errorText(error);

let rowKeys = 0;
const newRow = (labels = "", pool = "default"): MappingRow => ({ key: "m" + ++rowKeys, labels, pool });

type Status = { state: "loading" | "ready" | "error"; missing: string[]; error?: string };
type Line = { text?: ReactNode; tone?: "ok" | "failed" };

function Permissions({ status, recheck }: { status: Status; recheck: () => void }) {
  const granted = (name: string) => status.state === "loading" ? <span className="text-text-hint">{t("Checking…")}</span>
    : status.state === "error" ? <span className="text-text-hint">{t("Unknown")}</span>
    : status.missing.includes(name) ? <Badge tone="danger" dot>{t("Missing")}</Badge> : <Badge tone="success" dot>{t("Granted")}</Badge>;
  const columns: Column<[string, string]>[] = [
    { key: "permission", header: t("Permission"), cell: ([, label]) => t(label) },
    { key: "access", header: t("Access"), cell: () => t("Read and write") },
    { key: "granted", header: t("Granted"), cell: ([name]) => granted(name) },
  ];
  return (
    <section className="block" aria-labelledby="migrate-1">
      <h2 id="migrate-1">{t("1. Permissions")}</h2>
      <DataTable aria-label={t("GitHub App permissions")} columns={columns} rows={permissions} getRowId={([name]) => name} />
      {status.state === "ready" && status.missing.length > 0 && (
        <Notice tone="warning">
          <p>{t("Migration is optional, and opening the pull request needs the GitHub App to have write access to Contents, Pull requests and Workflows.")}</p>
          <p>{fill("Create a new App with {command}, or add the three permissions to the existing App in its GitHub App settings and accept the request on the installation. You can still preview, which needs read access to Contents.", {
            command: <code>runquill setup --migrate</code>,
          })}</p>
        </Notice>
      )}
      {status.state === "error" && (
        <Notice tone="danger" role="alert">
          <p>{status.error}</p>
          <Button variant="secondary" size="sm" onClick={recheck}>{t("Check again")}</Button>
        </Notice>
      )}
    </section>
  );
}

function FileChanges({ file }: { file: PreviewFile }) {
  const jobs = file.jobs ?? [];
  const left = jobs.filter((job) => !job.changed);
  const columns: Column<(typeof left)[number]>[] = [
    { key: "job", header: t("Job"), cell: (job) => <><span className="font-mono">{job.job}</span><span className="block-sub">{t("line {line}", { line: job.line })}</span></> },
    { key: "labels", header: t("Current runs-on"), cell: (job) => <span className="font-mono">{job.labels?.length ? job.labels.join(", ") : "—"}</span> },
    { key: "reason", header: t("Reason"), width: "minmax(200px, 2fr)", cell: (job) => sentence(job.reason) || "—" },
  ];
  return (
    <>
      {file.diff && <DiffView path={file.path} diff={file.diff} />}
      {left.length > 0 && (
        <DataTable aria-label={t("Jobs left alone in {path}", { path: file.path })} className="rows-2" columns={columns} rows={left}
          getRowId={(job) => job.job + ":" + job.line} />
      )}
    </>
  );
}

function PreviewBlock({ preview }: { preview: Preview }) {
  const sum = totals(preview);
  const files = preview.files ?? [];
  return (
    <section className="block" aria-labelledby="migrate-3">
      <h2 id="migrate-3">{t("3. Preview")}</h2>
      {isSample() && <Notice tone="info"><p>{t("Sample preview. The sample panel cannot reach GitHub, so this is fixed sample output and no pull request can be opened.")}</p></Notice>}
      <KeyValue label={t("Preview totals")} rows={[
        [t("Repository"), preview.repository || "—"],
        [t("Default branch"), preview.base || "—"],
        [t("Workflow files read"), number(sum.files)],
        [t("Jobs to change"), sum.changedJobs ? t("{jobs} in {files}", { jobs: jobCount(sum.changedJobs), files: fileCount(sum.changedFiles) }) : number(0)],
        [t("Jobs left alone"), number(sum.leftJobs)],
        [t("Files left alone"), number(sum.refusedFiles)],
      ]} />
      {!sum.files && <p>{t("The repository has no workflow files in .github/workflows, so there is nothing to change.")}</p>}
      {sum.files > 0 && !sum.changedJobs && <p>{t("No job matches the mapping, so nothing would change and no pull request will be opened.")}</p>}
      {files.length > 0 && (
        <Accordion multiple defaultValue={files.filter((file) => file.diff).map((file) => file.path)} className="file-list">
          {files.map((file) => {
            const jobs = file.jobs ?? [];
            const changed = jobs.filter((job) => job.changed).length;
            return (
              <Accordion.Item key={file.path} value={file.path} disabled={!file.diff && !(jobs.length - changed)}>
                <Accordion.Trigger level={3}>
                  <span className="file-head">
                    <span className="font-mono">{file.path}</span>
                    <span className="text-sm text-text-hint">{file.refused
                      ? t("Left alone: {reason}", { reason: sentence(file.refused) })
                      : t("{changed} changed, {left} left alone", { changed: jobCount(changed), left: jobCount(jobs.length - changed) })}</span>
                  </span>
                </Accordion.Trigger>
                <Accordion.Content><div className="file-body"><FileChanges file={file} /></div></Accordion.Content>
              </Accordion.Item>
            );
          })}
        </Accordion>
      )}
    </section>
  );
}

export default function Migrate() {
  const sample = isSample();
  const admin = can("admin");
  const [status, setStatus] = useState<Status>({ state: "loading", missing: [] });
  const [repository, setRepository] = useState(sample ? "garden-lab/papertrail" : "");
  const [rows, setRows] = useState<MappingRow[]>(() => sample ? [newRow("ubuntu-latest"), newRow("ubuntu-24.04", "build")] : [newRow("ubuntu-latest")]);
  const [invalid, setInvalid] = useState("");
  const [preview, setPreview] = useState<{ result: Preview; request: MigrateRequest | null } | null>(null);
  const [opened, setOpened] = useState("");
  const [busy, setBusy] = useState(false);
  const [formLine, setFormLine] = useState<Line>({});
  const [openLine, setOpenLine] = useState<Line>({});
  const [settingsPools, setSettingsPools] = useState<string[]>([]);
  const alive = useRef(true);
  useEffect(() => () => void (alive.current = false), []);

  // Pools: the snapshot's hosts and, in a live panel, every configured pool (one may have no host yet).
  const hostPools = useLive((s) => (s.snapshot?.fleet?.hosts ?? []).map((host) => host.pool || "default").join("\n"));
  const pools = useMemo(() => {
    const names = new Set([...hostPools.split("\n"), ...settingsPools].filter((name) => /^[a-z0-9][a-z0-9-]{0,31}$/.test(name) && name !== "default"));
    return ["default", ...[...names].sort((a, b) => a.localeCompare(b))];
  }, [hostPools, settingsPools]);

  const loadStatus = async () => {
    setStatus({ state: "loading", missing: [] });
    try {
      const result = await request<{ missing?: unknown[] }>("./api/migrate/status");
      if (alive.current) setStatus({ state: "ready", missing: Array.isArray(result?.missing) ? result.missing.map(String) : [] });
    } catch (error) {
      if (alive.current && (error as ApiError).status !== 401) setStatus({ state: "error", missing: [], error: failure(error) });
    }
  };

  const runPreview = async (body: MigrateRequest | null, automatic = false) => {
    setBusy(true);
    setPreview(null);
    setOpened("");
    if (!automatic) setOpenLine({});
    setFormLine({ text: t(sample ? "Loading the sample preview…" : "Reading workflow files from GitHub…") });
    try {
      const result = sample ? await request<Preview>("./api/migrate/preview")
        : await request<Preview>("./api/migrate/preview", { method: "POST", data: body, timeout: slow });
      if (!alive.current) return null;
      setPreview({ result, request: body });
      const sum = totals(result);
      // The preview itself is not a live region; this one line says it is ready.
      setFormLine({ tone: "ok", text: automatic
        ? t("The preview was refreshed with the current workflow files. Review it again before opening the pull request.")
        : t("Preview ready: {jobs} jobs change in {files} files.", { jobs: sum.changedJobs, files: sum.changedFiles }) });
      return result;
    } catch (error) {
      if (alive.current) setFormLine((error as ApiError).status === 401 ? {} : { tone: "failed", text: failure(error) });
      return null;
    } finally {
      if (alive.current) setBusy(false);
    }
  };

  useEffect(() => {
    void loadStatus();
    if (sample) void runPreview(null);
    else request<{ policy?: { worker_pools?: Record<string, unknown> } }>("./api/settings")
      .then((settings) => alive.current && setSettingsPools(Object.keys(settings?.policy?.worker_pools ?? {})), () => {});
  }, []);

  // Any edit after a preview makes it describe something else: drop it.
  const changed = () => {
    if (busy || !(preview || opened)) return;
    setPreview(null);
    setOpened("");
    setFormLine({ text: t("The choices changed. Preview again to see the new changes.") });
  };
  const setRow = (key: string, patch: Partial<MappingRow>) => {
    setRows((list) => list.map((row) => (row.key === key ? { ...row, ...patch } : row)));
    if (invalid === key) setInvalid("");
    changed();
  };
  const focusRow = (key: string | undefined) => requestAnimationFrame(() =>
    document.querySelector<HTMLElement>(key ? `[data-mapping="${key}"] input` : "#migrate-add")?.focus());

  const submit = () => {
    if (busy) return;
    const read = readMapping(repository, rows);
    if ("problem" in read) {
      setInvalid(read.at);
      setFormLine({ tone: "failed", text: read.problem });
      focusRow(read.at === "repository" ? undefined : read.at === "add" ? undefined : read.at);
      if (read.at === "repository") requestAnimationFrame(() => document.querySelector<HTMLElement>("#migrate-repository")?.focus());
      return;
    }
    setInvalid("");
    void runPreview(read.request);
  };

  const sum = preview ? totals(preview.result) : null;
  const ready = admin && !sample && !!sum && sum.changedJobs > 0;
  const missing = status.state === "ready" && status.missing.length > 0;
  const branch = preview ? branchPrefix + String(preview.result.hash || "").slice(0, 12) : "";
  const retried = useRef(false);

  const open = async (button: HTMLElement) => {
    if (!preview?.request || !sum || busy) return;
    const { result, request: body } = preview;
    const question = t("Open a pull request in {repo}? It changes runs-on in {jobs} across {files} on the new branch {branch}. Nothing is merged.", {
      repo: result.repository, jobs: jobCount(sum.changedJobs), files: fileCount(sum.changedFiles), branch,
    });
    if (!await confirm({ title: question, confirmLabel: t("Open pull request"), returnFocus: button })) return;
    if (!alive.current) return;
    setBusy(true);
    setOpenLine({ text: t("Opening the pull request on GitHub…") });
    let rerun = false;
    try {
      const pr = await request<{ url?: string }>("./api/migrate/open", { method: "POST", data: { ...body, hash: result.hash }, timeout: slow });
      if (!alive.current) return;
      const url = String(pr?.url || "");
      setOpened(url);
      setPreview(null);
      setRepository("");
      retried.current = false;
      setOpenLine({ tone: "ok", text: fill("Opened pull request {link}. Review and merge it on GitHub when the pools are ready.", {
        link: pullRequestURL(url) ? link(url) : <span className="font-mono">{url || "—"}</span>,
      }) });
      toast(t("Pull request opened."));
    } catch (error) {
      if (!alive.current) return;
      const code = (error as ApiError).code;
      if ((error as ApiError).status === 401) setOpenLine({});
      else if (code === "stale" && !retried.current) {
        retried.current = true;
        rerun = true;
        setOpenLine({ tone: "failed", text: t("The workflow files changed since the preview. Previewing again…") });
      } else if (code === "branch_exists") {
        const pr = (error as ApiError).pullRequest;
        setOpenLine({ tone: "failed", text: <>{failure(error)} {pullRequestURL(pr)
          ? fill("Existing pull request: {link}", { link: link(pr) }) : t("Delete the branch {branch} in GitHub to try again.", { branch })}</> });
      } else setOpenLine({ tone: "failed", text: failure(error) });
    } finally {
      if (alive.current) setBusy(false);
    }
    if (rerun && alive.current && await runPreview(body, true)) {
      setOpenLine({ tone: "failed", text: t("The workflow files changed since the preview. The preview above is now current; review it and open the pull request again.") });
    }
  };

  return (
    <Page title={t("Workflow migration")}>
      <Permissions status={status} recheck={() => void loadStatus()} />

      <section className="block" aria-labelledby="migrate-2">
        <h2 id="migrate-2">{t("2. Repository and labels")}</h2>
        <p className="text-sm text-text-hint">{t("Runquill rewrites only runs-on, in one pull request you review. Nothing is opened until you confirm.")}</p>
        <form className="migrate-form" noValidate onSubmit={(event) => {
          event.preventDefault();
          submit();
        }}>
          <fieldset disabled={busy} className="plain-fieldset">
            <legend className="sr-only">{t("Repository and label mapping")}</legend>
            <Field label={t("Repository")} className="migrate-repository" htmlFor="migrate-repository">
              <Input className="font-mono" value={repository} placeholder="OWNER/REPO" maxLength={140} spellCheck={false}
                autoComplete="off" required aria-invalid={invalid === "repository" || undefined}
                onChange={(event) => {
                  setRepository(event.currentTarget.value);
                  if (invalid === "repository") setInvalid("");
                  changed();
                }} />
            </Field>
            <ul className="mapping-list" aria-label={t("Label mapping")}>
              {rows.map((row, i) => (
                <li key={row.key} className="mapping-row" data-mapping={row.key}>
                  <Input className="font-mono mapping-labels" value={row.labels} placeholder="ubuntu-latest" maxLength={1100} spellCheck={false}
                    autoComplete="off" aria-label={t("Runs-on labels, mapping {n}", { n: i + 1 })} aria-invalid={invalid === row.key || undefined}
                    onChange={(event) => setRow(row.key, { labels: event.currentTarget.value })} />
                  <span aria-hidden className="text-text-hint">→</span>
                  <Select value={pools.includes(row.pool) ? row.pool : "default"} aria-label={t("Pool, mapping {n}", { n: i + 1 })}
                    options={pools.map((name) => ({ value: name, label: poolName(name) }))} onValueChange={(pool) => setRow(row.key, { pool })} />
                  <Button type="button" variant="secondary" size="sm" disabled={rows.length === 1} aria-label={t("Remove mapping {n}", { n: i + 1 })}
                    onClick={() => {
                      const next = rows[i + 1] ?? rows[i - 1];
                      setRows(rows.filter((r) => r.key !== row.key));
                      changed();
                      focusRow(next?.key);
                    }}>{t("Remove")}</Button>
                </li>
              ))}
            </ul>
            <p className="text-sm text-text-hint">{t("A row matches jobs whose runs-on lists exactly these labels. Separate several labels with commas.")}</p>
            <div className="row-actions">
              <Button id="migrate-add" type="button" variant="secondary" size="sm" disabled={rows.length >= maxRows} onClick={() => {
                const row = newRow();
                setRows([...rows, row]);
                changed();
                focusRow(row.key);
              }}>{t("Add mapping")}</Button>
            </div>
          </fieldset>
          <div className="row-actions">
            <Button type="submit" loading={busy && !preview}>{t("Preview changes")}</Button>
          </div>
          <StatusLine {...formLine} />
        </form>
      </section>

      {preview && <PreviewBlock preview={preview.result} />}

      {(ready || opened) && (
        <section className="block" aria-labelledby="migrate-4">
          <h2 id="migrate-4">{t("4. Open pull request")}</h2>
          {ready && preview && sum && (
            <>
              <p>{t("Opens one pull request on the new branch {branch} in {repo}, changing only runs-on in {jobs} across {files}. Nothing is merged.", {
                branch, repo: preview.result.repository, jobs: jobCount(sum.changedJobs), files: fileCount(sum.changedFiles),
              })}</p>
              {missing && <p className="text-text-hint">{t("Grant the three write permissions in step 1 first.")}</p>}
              <div className="row-actions">
                <Button disabled={missing} loading={busy} onClick={(event) => void open(event.currentTarget)}>{t("Open pull request")}</Button>
              </div>
            </>
          )}
          <StatusLine {...openLine} />
        </section>
      )}
    </Page>
  );
}
