// Team (design §3.7, §4.8), admins only: enroll by GitHub login or numeric ID, change roles, disable and enable, 100
// members a page with an `after` cursor. Ported from v1 team.js; API tokens have their own screen.
import { Badge, Button, DataTable, Field, Input, Select, toast, type Column } from "@yakihyuk0728/stratum-ui";
import { useEffect, useRef, useState } from "react";
import { send } from "../../actions";
import { errorText, request, type ApiError } from "../../api";
import { Notice, Page } from "../../components";
import { confirm } from "../../dialogs";
import { t } from "../../i18n";
import type { Role } from "../../types";

interface Member {
  github_id: number;
  login?: string;
  role: Role;
  disabled: boolean;
}

const pageSize = 100;
const roles: [Role, string][] = [["viewer", "Viewer"], ["operator", "Operator"], ["admin", "Admin"]];
const roleOptions = () => roles.map(([value, label]) => ({ value, label: t(label) }));
const roleLabel = (role: string) => t(roles.find(([value]) => value === role)?.[1] ?? role);
const memberName = (member: Member) => (member.login ? "@" + member.login : t("GitHub ID {id}", { id: member.github_id }));

function AddMember({ done }: { done: () => Promise<void> }) {
  const [account, setAccount] = useState("");
  const [role, setRole] = useState<Role>("viewer");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <form className="toolbar" aria-label={t("Add a team member")} onSubmit={async (event) => {
      event.preventDefault();
      if (busy) return;
      setBusy(true);
      setError("");
      const name = account.trim();
      try {
        // Digits alone are a numeric account ID; write @1234 for a numeric login.
        if (/^[0-9]+$/.test(name)) await request("./api/team/" + encodeURIComponent(name), { method: "PUT", data: { role, disabled: false } });
        else await request("./api/team", { method: "POST", data: { login: name.replace(/^@/, ""), role }, timeout: 10000 });
        toast(t("Team member saved."));
        setAccount("");
        setRole("viewer");
        await done();
      } catch (failure) {
        if ((failure as ApiError).status !== 401) setError(errorText(failure));
      } finally {
        setBusy(false);
      }
    }}>
      <Field label={t("GitHub login or ID")} required className="toolbar-grow">
        <Input value={account} onChange={(event) => setAccount(event.currentTarget.value)} maxLength={40} placeholder="octocat"
          autoComplete="off" spellCheck={false} required />
      </Field>
      <Field label={t("Role")}>
        <Select value={role} onValueChange={(value) => setRole(value as Role)} options={roleOptions()} />
      </Field>
      <Button type="submit" loading={busy}>{t("Save team member")}</Button>
      {error && <Notice tone="danger" role="alert" className="toolbar-full"><p>{error}</p></Notice>}
    </form>
  );
}

export default function Team() {
  const [cursors, setCursors] = useState([0]);
  const [members, setMembers] = useState<Member[] | null>(null);
  const [next, setNext] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiError | null>(null);
  const [roleChoice, setRoleChoice] = useState<Record<number, Role>>({});
  const table = useRef<HTMLDivElement>(null);
  const paged = useRef(false);
  const current = useRef(cursors);
  current.current = cursors;

  // A page button that turned disabled under focus (first or last page) hands focus to the table.
  useEffect(() => {
    if (paged.current && document.activeElement === document.body) table.current?.focus({ preventScroll: true });
    paged.current = false;
  }, [cursors, next]);

  // Loads a page (the cursor list to it); an empty page past the current one keeps the current page (v1).
  const load = async (page = current.current) => {
    setLoading(true);
    setError(null);
    try {
      const list = await request<Member[]>("./api/team?after=" + encodeURIComponent(page.at(-1)!));
      if (list.length || page.length <= current.current.length) {
        setMembers(list);
        setCursors(page);
        setRoleChoice({});
        setNext(list.length === pageSize ? list.at(-1)!.github_id : 0);
      }
    } catch (failure) {
      if ((failure as ApiError).status !== 401) setError(failure as ApiError);
    } finally {
      setLoading(false);
    }
  };
  const turn = (page: number[]) => {
    if (loading) return;
    paged.current = true;
    void load(page);
  };
  useEffect(() => void load(), []);

  const change = async (member: Member, update: { role: Role; disabled: boolean }, done: string, question?: string, returnFocus?: HTMLElement | null) => {
    if (question && !await confirm({ title: question, confirmLabel: t(update.disabled ? "Disable" : "Change role"), danger: update.disabled, returnFocus })) return false;
    if (await send("PUT", "./api/team/" + encodeURIComponent(member.github_id), update) === undefined) return false;
    toast(t(done));
    await load();
    return true;
  };

  const columns: Column<Member>[] = [
    { key: "member", header: t("Member"), cell: memberName },
    { key: "id", header: t("GitHub ID"), align: "end", width: 120, cell: (member) => String(member.github_id) },
    // Choosing a role does nothing until Change role is pressed, so arrow keys can browse the options.
    { key: "role", header: t("Role"), width: "minmax(260px, 1.4fr)", cell: (member) => {
      const choice = roleChoice[member.github_id] ?? member.role;
      return (
        <div className="row-actions">
          <Select size="sm" aria-label={t("Role for {name}", { name: memberName(member) })} value={choice} options={roleOptions()}
            onValueChange={(value) => setRoleChoice((all) => ({ ...all, [member.github_id]: value as Role }))} />
          {choice !== member.role && (
            <Button size="sm" variant="secondary" aria-label={t("Change role for {name}", { name: memberName(member) })} onClick={async (event) => {
              const button = event.currentTarget;
              const applied = await change(member, { role: choice, disabled: member.disabled }, "Role changed.",
                t("Change {name} to {role}?", { name: memberName(member), role: roleLabel(choice) }), button);
              if (!applied) setRoleChoice((all) => ({ ...all, [member.github_id]: member.role }));
            }}>{t("Change role")}</Button>
          )}
        </div>
      );
    } },
    { key: "status", header: t("Status"), width: 120, cell: (member) => member.disabled
      ? <Badge tone="danger" dot>{t("Disabled")}</Badge> : <Badge tone="success" dot>{t("Active")}</Badge> },
    { key: "action", header: <span className="sr-only">{t("Actions")}</span>, align: "end", width: 130, cell: (member) => (
      <Button size="sm" variant="secondary" aria-label={t("{action} {name}", { action: t(member.disabled ? "Enable" : "Disable"), name: memberName(member) })}
        onClick={(event) => void change(member, { role: member.role, disabled: !member.disabled },
          member.disabled ? "Team member enabled." : "Team member disabled.",
          member.disabled ? undefined : t("Disable {name} and revoke their sessions and tokens?", { name: memberName(member) }), event.currentTarget)}>
        {t(member.disabled ? "Enable" : "Disable")}
      </Button>
    ) },
  ];

  return (
    <Page title={t("Team")} description={t("Enroll a teammate by GitHub login or numeric account ID.")}>
      <AddMember done={() => load()} />
      {error && (
        <Notice tone="danger" role="alert">
          <p>{error.message}</p>
          <Button variant="secondary" size="sm" onClick={() => void load()}>{t("Try again")}</Button>
        </Notice>
      )}
      <div ref={table} tabIndex={-1}>
        <DataTable aria-label={t("Team")} columns={columns} rows={members ?? []} getRowId={(member) => member.github_id}
          loading={loading && !members} empty={t(error ? "Team members are unavailable." : "No entries.")} />
      </div>
      <div className="row-actions table-pager">
        <Button variant="secondary" size="sm" disabled={cursors.length === 1} onClick={() => turn(cursors.slice(0, -1))}>{t("Previous page")}</Button>
        <Button variant="secondary" size="sm" disabled={!next} onClick={() => turn([...cursors, next])}>{t("Next page")}</Button>
      </div>
    </Page>
  );
}
