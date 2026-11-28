// API tokens (design §3.7, §4.9), every role, with the browser session only. GET/POST /api/tokens, DELETE
// /api/tokens/{id}. Viewers can issue fleet:read only (the server checks too). A new secret shows once in a copy field;
// Hide token, revoking it or leaving the screen drops it. Ported from the tokens block of v1 team.js.
import { Button, DataTable, Field, Input, Select, toast, type Column } from "@yakihyuk0728/stratum-ui";
import { useEffect, useRef, useState } from "react";
import { send } from "../../actions";
import { errorText, request, type ApiError } from "../../api";
import { Notice, Page } from "../../components";
import { CopyField } from "../../components/CopyField";
import { confirm } from "../../dialogs";
import { calendarDay } from "../../format";
import { t } from "../../i18n";
import { useRequest } from "../../request";
import { can } from "../../session";

interface Token {
  id: number;
  name: string;
  scope: string;
  expires_at: number;
}

const scopes: [string, string][] = [
  ["fleet:read", "Read fleet status"],
  ["hosts:write", "Manage host maintenance"],
  ["jobs:write", "Cancel and retry jobs"],
];
const scopeLabel = (scope: string) => t(scopes.find(([value]) => value === scope)?.[1] ?? scope);

export default function Tokens() {
  const tokens = useRequest<Token[]>("./api/tokens");
  const [name, setName] = useState("");
  const [scope, setScope] = useState("fleet:read");
  const [days, setDays] = useState("30");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [issued, setIssued] = useState<{ id: number; secret: string } | null>(null);
  const [copied, setCopied] = useState("");
  const secretBox = useRef<HTMLElement>(null);
  const form = useRef<HTMLFormElement>(null);
  const operator = can("operator");

  useEffect(() => {
    if (!issued) return;
    const input = secretBox.current?.querySelector("input");
    input?.focus();
    input?.select();
  }, [issued]);

  const hide = () => {
    setIssued(null);
    setCopied("");
  };

  const create = async () => {
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      const token = await request<{ id: number; secret: string }>("./api/tokens", { method: "POST", data: { name, scope, days: Number(days) } });
      setCopied("");
      setIssued({ id: token.id, secret: token.secret });
      setName("");
      setScope("fleet:read");
      setDays("30");
      tokens.reload();
    } catch (failure) {
      if ((failure as ApiError).status !== 401) setError(errorText(failure));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (token: Token, button: HTMLElement) => {
    if (!await confirm({ title: t("Revoke the token {name}?", { name: token.name }), confirmLabel: t("Revoke"), danger: true, returnFocus: button })) return;
    if (await send("DELETE", "./api/tokens/" + encodeURIComponent(token.id)) === undefined) return;
    if (issued?.id === token.id) hide();
    toast(t("Token revoked."));
    tokens.reload();
  };

  const columns: Column<Token>[] = [
    { key: "name", header: t("Name"), cell: (token) => token.name },
    { key: "scope", header: t("Scope"), cell: (token) => <>{scopeLabel(token.scope)}<span className="block-sub font-mono">{token.scope}</span></> },
    { key: "expires", header: t("Expires"), align: "end", width: 150, cell: (token) => calendarDay(token.expires_at) },
    { key: "action", header: <span className="sr-only">{t("Actions")}</span>, align: "end", width: 120, cell: (token) => (
      <Button size="sm" variant="secondary" aria-label={t("Revoke the token {name}", { name: token.name })}
        onClick={(event) => void revoke(token, event.currentTarget)}>{t("Revoke")}</Button>
    ) },
  ];

  return (
    <Page title={t("API tokens")} description={t("Scoped credentials for scripts and integrations.")}>
      <form ref={form} className="toolbar" aria-label={t("Create an API token")} onSubmit={(event) => {
        event.preventDefault();
        void create();
      }}>
        <Field label={t("Token name")} required className="toolbar-grow">
          <Input value={name} onChange={(event) => setName(event.currentTarget.value)} maxLength={80} required placeholder={t("Metrics collector")} />
        </Field>
        <Field label={t("Scope")} description={operator ? undefined : t("Viewers can create read-only tokens.")}>
          <Select value={scope} onValueChange={setScope}
            options={scopes.map(([value, label]) => ({ value, label: t(label), disabled: value !== "fleet:read" && !operator }))} />
        </Field>
        <Field label={t("Valid for days")} required>
          <Input type="number" value={days} onChange={(event) => setDays(event.currentTarget.value)} min={1} max={90} required />
        </Field>
        <Button type="submit" loading={busy}>{t("Create token")}</Button>
        {error && <Notice tone="danger" role="alert" className="toolbar-full"><p>{error}</p></Notice>}
      </form>
      {issued && (
        <section className="block" aria-labelledby="token-new" ref={secretBox}>
          <h2 id="token-new">{t("New API token")}</h2>
          <CopyField label={t("New API token")} value={issued.secret} copyLabel={t("Copy token")} copied="Token copied."
            description={t("Copy your token now. Its value is shown once.")} report={setCopied} />
          <div className="row-actions">
            <Button variant="secondary" onClick={() => {
              hide();
              form.current?.querySelector("input")?.focus();
            }}>{t("Hide token")}</Button>
            <span role="status" className="text-sm">{copied}</span>
          </div>
        </section>
      )}
      {tokens.error && (
        <Notice tone="danger" role="alert">
          <p>{tokens.error.message}</p>
          <Button variant="secondary" size="sm" onClick={tokens.reload}>{t("Try again")}</Button>
        </Notice>
      )}
      <DataTable aria-label={t("API tokens")} className="rows-2" columns={columns} rows={tokens.data ?? []} getRowId={(token) => token.id}
        loading={tokens.loading && !tokens.data}
        empty={t(tokens.error ? "API tokens are unavailable." : "No active API tokens. Create one when a script needs access.")} />
    </Page>
  );
}
