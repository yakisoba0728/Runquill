// Audit: sign-ins and management actions from GET /api/audit, newest first,
// filtered by action family and actor, 50 per page.
import { tr } from "../i18n.js";
import { button, createTable, errorText, field, h, message, pageHead, select, statusLine, time } from "../ui.js";

const pageSize = 50;
let parts = null;

async function load(more = false) {
  const current = parts;
  if (!current) return;
  const ticket = ++current.ticket;
  message(current.output, "");
  current.more.disabled = true;
  if (!more) current.table.update([], "Loading…");
  const data = new FormData(current.form);
  const query = new URLSearchParams({ limit: String(pageSize) });
  for (const name of ["action", "actor"]) {
    const value = String(data.get(name) || "").trim();
    if (value) query.set(name, value);
  }
  if (more && current.cursor) query.set("before", String(current.cursor));
  try {
    const entries = await current.ctx.api.get("./api/audit?" + query);
    if (parts !== current || ticket !== current.ticket) return;
    current.rows = more ? current.rows.concat(entries) : entries;
    current.cursor = entries.length ? entries[entries.length - 1].id : 0;
    current.more.hidden = entries.length < pageSize;
    current.loaded = true;
    current.table.update(current.rows, "No audit entries yet.");
  } catch (error) {
    if (parts !== current || ticket !== current.ticket || error.status === 401) return;
    if (!more) current.table.update([], "Audit history is unavailable.");
    message(current.output, errorText(error), "failed");
  } finally {
    current.more.disabled = false;
  }
}

export default {
  id: "audit",
  title: "Audit",

  mount(root, ctx) {
    const form = h("form", { class: "toolbar", "aria-label": tr("Audit filters") },
      field("Action", select("action", [["", "All actions"], ["session.", "Sign-ins"], ["user.", "Team changes"],
        ["token.", "API tokens"], ["host.", "Host maintenance"], ["job.", "Job actions"], ["settings.update", "Settings changes"],
        ["queue.backup", "Queue backups"], ["migrate.", "Migrations"], ["runner.logs", "Runner log views"]])),
      field("Actor", h("input", { type: "search", name: "actor", placeholder: tr("Login or GitHub ID"), maxLength: 100 }), { grow: true }),
      button("Apply filters", null, { type: "submit" }));
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      void load();
    });
    form.elements.action.addEventListener("change", () => void load());
    const table = createTable({
      label: "Audit history",
      key: (entry) => entry.id,
      columns: [
        { label: "Time", num: true, cell: (entry) => time(entry.occurred_at) },
        { label: "Action", cell: (entry) => [tr(entry.action), h("span", { class: "sub mono" }, entry.action)] },
        { label: "Actor", className: "long", cell: (entry) => entry.actor_login ? "@" + entry.actor_login : tr("GitHub ID {id}", { id: entry.actor }) },
        { label: "Target", className: "wrap", cell: (entry) => entry.target || "—" },
      ],
    });
    const more = button("Load older entries", () => void load(true), { hidden: true });
    const output = statusLine();
    root.append(
      pageHead("Audit", button("Refresh", () => void load())),
      h("div", { class: "block" }, form, table.element, h("div", { class: "table-foot" }, more), output),
    );
    parts = { ctx, form, table, more, output, rows: [], cursor: 0, loaded: false, ticket: 0 };
    void load();
  },

  unmount() {
    parts = null;
  },
};

