// Audit log (design §3.11, §4.10), admins only: sign-ins and management actions from GET /api/audit, newest first,
// 50 a page with a `before` cursor. The action family and actor filters live in the address (#/audit?action=job.&actor=x):
// a new action loads at once, the actor once typing settles. Ported from v1 audit.js.
import { Button, DataTable, Field, SearchInput, Select, type Column } from "@yakihyuk0728/stratum-ui";
import { useEffect, useMemo, useRef, useState } from "react";
import { errorText, request, type ApiError } from "../../api";
import { useScreenCommands, type ScreenCommand } from "../../commands";
import { Notice, Page } from "../../components";
import { date } from "../../format";
import { t } from "../../i18n";
import { stratum } from "../../i18n/stratum";
import { setParams, type Route } from "../../router";
import { useRequest } from "../../request";

interface Entry {
  id: number;
  actor: number;
  actor_login?: string;
  action: string;
  target: string;
  occurred_at: number;
}

const pageSize = 50;
const actions: [string, string][] = [
  ["", "All actions"], ["session.", "Sign-ins"], ["user.", "Team changes"], ["token.", "API tokens"], ["host.", "Host maintenance"],
  ["job.", "Job actions"], ["settings.update", "Settings changes"], ["queue.backup", "Queue backups"], ["migrate.", "Migrations"],
  ["runner.logs", "Runner log views"],
];

export default function Audit({ route }: { route: Route }) {
  const labels = useMemo(stratum, []);
  const action = route.params.get("action") ?? "";
  const actor = route.params.get("actor") ?? "";
  const query = new URLSearchParams({ limit: String(pageSize) });
  if (action) query.set("action", action);
  if (actor) query.set("actor", actor);
  const first = useRequest<Entry[]>("./api/audit?" + query);
  const [older, setOlder] = useState<{ rows: Entry[]; more: boolean; loading: boolean; error: string }>({ rows: [], more: false, loading: false, error: "" });
  useEffect(() => setOlder({ rows: [], more: (first.data?.length ?? 0) === pageSize, loading: false, error: "" }), [first.data]);
  const rows = useMemo(() => [...(first.data ?? []), ...older.rows], [first.data, older.rows]);

  // The actor box follows the address and writes back once typing settles.
  const [typed, setTyped] = useState(actor);
  useEffect(() => setTyped(actor), [actor]);
  useEffect(() => {
    if (typed.trim() === actor) return;
    const timer = setTimeout(() => setParams({ actor: typed.trim() }), 450);
    return () => clearTimeout(timer);
  }, [typed]);

  const loadOlder = async () => {
    if (older.loading || !rows.length) return;
    const base = first.data;
    setOlder((last) => ({ ...last, loading: true, error: "" }));
    const next = new URLSearchParams(query);
    next.set("before", String(rows.at(-1)!.id));
    try {
      const page = await request<Entry[]>("./api/audit?" + next);
      setOlder((last) => (first.data !== base ? last : { rows: [...last.rows, ...page], more: page.length === pageSize, loading: false, error: "" }));
    } catch (error) {
      if ((error as ApiError).status === 401) return;
      setOlder((last) => ({ ...last, loading: false, error: errorText(error) }));
    }
  };

  // A stable list: a new one each render would re-register and re-render the shell without end.
  const reload = useRef(first.reload);
  reload.current = first.reload;
  const commands = useMemo<ScreenCommand[]>(() => [{ id: "refresh", label: "Refresh", run: () => reload.current() }], []);
  useScreenCommands(commands);

  const columns: Column<Entry>[] = [
    { key: "time", header: t("Time"), width: 190, cell: (entry) => date(entry.occurred_at) },
    // The translated action, then its code (English has no wording of its own for the codes, so it shows the code once).
    { key: "action", header: t("Action"), cell: (entry) => t(entry.action) === entry.action ? <span className="font-mono">{entry.action}</span>
      : <>{t(entry.action)}<span className="block-sub font-mono">{entry.action}</span></> },
    { key: "actor", header: t("Actor"), cell: (entry) => (entry.actor_login ? "@" + entry.actor_login : t("GitHub ID {id}", { id: entry.actor })) },
    { key: "target", header: t("Target"), width: "minmax(160px, 1.5fr)", cell: (entry) => entry.target || "—" },
  ];

  return (
    <Page title={t("Audit log")} actions={<Button variant="secondary" onClick={first.reload} loading={first.loading && !!first.data}>{t("Refresh")}</Button>}>
      <form className="toolbar" aria-label={t("Audit filters")} onSubmit={(event) => {
        event.preventDefault();
        setParams({ actor: typed.trim() });
      }}>
        <Field label={t("Action")}>
          <Select value={action} onValueChange={(value) => setParams({ action: value })} options={actions.map(([value, label]) => ({ value, label: t(label) }))} />
        </Field>
        <Field label={t("Actor")} className="toolbar-grow">
          <SearchInput value={typed} onChange={(event) => setTyped(event.currentTarget.value)} onClear={() => setParams({ actor: null })}
            placeholder={t("Login or GitHub ID")} maxLength={100} clearLabel={labels.search.clearLabel} />
        </Field>
      </form>
      {first.error && (
        <Notice tone="danger" role="alert">
          <p>{first.error.message}</p>
          <Button variant="secondary" size="sm" onClick={first.reload}>{t("Try again")}</Button>
        </Notice>
      )}
      <DataTable aria-label={t("Audit history")} className="rows-2" columns={columns} rows={rows} getRowId={(entry) => entry.id}
        loading={first.loading && !first.data} empty={t(first.error ? "Audit history is unavailable." : "No audit entries yet.")} />
      {older.error && <Notice tone="danger" role="alert"><p>{older.error}</p></Notice>}
      {older.more && (
        <div className="table-foot">
          <Button variant="secondary" loading={older.loading} onClick={() => void loadOlder()}>{t("Load older entries")}</Button>
        </div>
      )}
    </Page>
  );
}
