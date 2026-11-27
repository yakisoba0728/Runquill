// Team & tokens: everyone manages their own API tokens; admins also enroll
// teammates, change roles and disable accounts.
import { tr } from "../i18n.js";
import {
  block, button, createTable, errorText, field, format, h, message, pageHead, roleLabel, select, state, statusLine, submitForm,
} from "../ui.js";

const scopes = [
  ["fleet:read", "Read fleet status"],
  ["hosts:write", "Manage host maintenance"],
  ["jobs:write", "Cancel and retry jobs"],
];
const scopeLabel = (scope) => tr(scopes.find(([value]) => value === scope)?.[1] || scope);
const memberName = (member) => member.login ? "@" + member.login : tr("GitHub ID {id}", { id: member.github_id });

// guarded runs action and reports a failure inline; resolves to true on success.
async function guarded(output, action) {
  message(output, "");
  try {
    await action();
    return true;
  } catch (error) {
    if (error.status !== 401) message(output, errorText(error), "failed");
    return false;
  }
}

function tokensBlock(ctx) {
  const output = statusLine();
  const secret = h("input", { type: "text", readOnly: true, id: "token-secret", "aria-describedby": "token-secret-note" });
  const issued = h("div", { class: "secret", hidden: true },
    h("p", { id: "token-secret-note" }, tr("Copy your token now. Its value is shown once.")),
    secret,
    button("Hide token", () => {
      secret.value = "";
      issued.hidden = true;
    }));
  const scope = select("scope", scopes);
  for (const option of scope.options) option.disabled = option.value !== "fleet:read" && !ctx.can("operator");
  const form = h("form", { class: "toolbar", "aria-label": tr("Create an API token") },
    field("Token name", h("input", { name: "name", maxLength: 80, required: true, placeholder: tr("Metrics collector") }), { grow: true }),
    field("Scope", scope),
    field("Valid for days", h("input", { name: "days", type: "number", min: 1, max: 90, defaultValue: 30, required: true })),
    button("Create token", null, { type: "submit", primary: true }));
  const table = createTable({
    label: "API tokens",
    key: (token) => token.id,
    empty: "No active API tokens. Create one when a script needs access.",
    columns: [
      { label: "Name", className: "long", cell: (token) => token.name },
      { label: "Scope", cell: (token) => [scopeLabel(token.scope), h("span", { class: "sub mono" }, token.scope)] },
      { label: "Expires", num: true, cell: (token) => format.day(token.expires_at) },
      { label: "", cell: (token) => h("div", { class: "row-actions" }, button("Revoke", async () => {
        if (!await ctx.confirm(tr("Revoke the token {name}?", { name: token.name }), "Revoke")) return;
        await guarded(output, async () => {
          await ctx.api.del("./api/tokens/" + encodeURIComponent(token.id));
          if (issued.dataset.token === String(token.id)) {
            secret.value = "";
            issued.hidden = true;
          }
          ctx.toast(tr("Token revoked."));
          await load();
        });
      }, { small: true, "aria-label": tr("Revoke the token {name}", { name: token.name }) })) },
    ],
  });
  async function load() {
    table.update(await ctx.api.get("./api/tokens"));
  }
  submitForm(form, output, async (data) => {
    const token = await ctx.api.post("./api/tokens", { name: data.get("name"), scope: data.get("scope"), days: Number(data.get("days")) });
    secret.value = token.secret;
    issued.dataset.token = String(token.id);
    issued.hidden = false;
    secret.focus();
    secret.select();
    form.reset();
    await load();
  });
  table.update([], "Loading…");
  void guarded(output, load).then((ok) => ok || table.update([], "API tokens are unavailable."));
  return block("API tokens", { note: "Scoped credentials for scripts and integrations." }, form, issued, table.element, output);
}

function teamBlock(ctx) {
  const output = statusLine();
  const form = h("form", { class: "toolbar", "aria-label": tr("Add a team member") },
    field("GitHub login or ID", h("input", { name: "account", maxLength: 40, placeholder: "octocat", autocomplete: "off", required: true }), { grow: true }),
    field("Role", select("role", [["viewer", "Viewer"], ["operator", "Operator"], ["admin", "Admin"]])),
    button("Save team member", null, { type: "submit", primary: true }));
  async function change(member, update, question, done) {
    if (question && !await ctx.confirm(question, update.disabled ? "Disable" : "Change role")) return false;
    return guarded(output, async () => {
      await ctx.api.put("./api/team/" + encodeURIComponent(member.github_id), update);
      ctx.toast(tr(done));
      await load();
    });
  }
  const table = createTable({
    label: "Team",
    key: (member) => member.github_id,
    columns: [
      { label: "Member", cell: (member) => memberName(member) },
      { label: "GitHub ID", num: true, cell: (member) => String(member.github_id) },
      { label: "Role", cell: (member) => {
        const role = select("role", [["viewer", "Viewer"], ["operator", "Operator"], ["admin", "Admin"]],
          { "aria-label": tr("Role for {name}", { name: memberName(member) }) });
        role.value = member.role;
        role.addEventListener("change", async () => {
          const next = role.value;
          const applied = await change(member, { role: next, disabled: member.disabled },
            tr("Change {name} to {role}?", { name: memberName(member), role: roleLabel(next) }), "Role changed.");
          if (!applied) role.value = member.role;
        });
        return role;
      } },
      { label: "Status", cell: (member) => member.disabled ? state("OFFLINE", "Disabled") : state("UP", "Active") },
      { label: "", cell: (member) => h("div", { class: "row-actions" }, button(member.disabled ? "Enable" : "Disable", () => {
        void change(member, { role: member.role, disabled: !member.disabled },
          member.disabled ? null : tr("Disable {name} and revoke their sessions and tokens?", { name: memberName(member) }),
          member.disabled ? "Team member enabled." : "Team member disabled.");
      }, { small: true, "aria-label": tr("{action} {name}", { action: tr(member.disabled ? "Enable" : "Disable"), name: memberName(member) }) })) },
    ],
  });
  let cursors = [0];
  let nextCursor = 0;
  const previous = button("Previous page", () => guarded(output, () => load(cursors.slice(0, -1))), { disabled: true });
  const next = button("Next page", () => guarded(output, () => load([...cursors, nextCursor])), { disabled: true });
  async function load(pageCursors = cursors) {
    previous.disabled = next.disabled = true;
    try {
      const members = await ctx.api.get("./api/team?after=" + encodeURIComponent(pageCursors.at(-1)));
      table.update(members);
      cursors = pageCursors;
      nextCursor = members.length === 100 ? members.at(-1).github_id : 0;
    } finally {
      previous.disabled = cursors.length === 1;
      next.disabled = !nextCursor;
    }
  }
  submitForm(form, output, async (data) => {
    const account = String(data.get("account")).trim();
    // Digits alone are a numeric account ID; write @1234 for a numeric login.
    if (/^[0-9]+$/.test(account)) {
      await ctx.api.put("./api/team/" + encodeURIComponent(account), { role: data.get("role"), disabled: false });
    } else {
      await ctx.api.post("./api/team", { login: account.replace(/^@/, ""), role: data.get("role") }, { timeout: 10000 });
    }
    ctx.toast(tr("Team member saved."));
    form.reset();
    await load();
  });
  table.update([], "Loading…");
  void guarded(output, load).then((ok) => ok || table.update([], "Team members are unavailable."));
  return block("Team", { note: "Enroll a teammate by GitHub login or numeric account ID." },
    form, table.element, h("div", { class: "actions" }, previous, next), output);
}

export default {
  id: "team",
  title: "Team & tokens",

  mount(root, ctx) {
    root.append(pageHead("Team & tokens"), ctx.can("admin") ? teamBlock(ctx) : "", tokensBlock(ctx));
  },
};
