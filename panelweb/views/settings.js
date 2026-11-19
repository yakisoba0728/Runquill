// Settings: the operational policy from GET/PUT /api/settings (applies
// without a restart). Admins only. Queue backups live on the Backups screen.
import { ApiError } from "../api.js";
import { tr } from "../i18n.js";
import { block, button, errorText, field, fill, h, intro, message, pageHead, statusLine, submitForm } from "../ui.js";

let parts = null;

function hostRow(list, address = "", limit = "") {
  const input = h("input", { name: "address", placeholder: "local or ssh://user@host", maxLength: 260, required: true,
    "aria-label": tr("Worker address"), class: "mono" });
  input.value = address;
  const limitInput = h("input", { name: "limit", type: "number", min: 1, placeholder: tr("Use fleet limit"), "aria-label": tr("Worker slot limit") });
  limitInput.value = limit ?? "";
  const remove = button("Remove", async () => {
    const name = input.value.trim();
    if (name && !await parts.ctx.confirm(tr("Remove the worker {name} from these settings? The change applies when you save.", { name }), "Remove")) return;
    row.remove();
  }, { small: true });
  // Outbound agents enroll with deployment credentials, not from the panel.
  if (address.startsWith("agent://")) {
    input.readOnly = true;
    remove.hidden = true;
  }
  const row = h("tr", {}, h("td", {}, input), h("td", {}, limitInput), h("td", { class: "num" }, remove));
  list.append(row);
  return input;
}

function poolEntry(list, originals, name = "", pool = {}) {
  const input = (props, value) => {
    const element = h(props.rows ? "textarea" : "input", props);
    element.value = value ?? "";
    return element;
  };
  const poolName = input({ name: "pool_name", pattern: "[a-z0-9][a-z0-9\\-]{0,31}", required: true, class: "mono" }, name);
  const restrict = h("input", { type: "checkbox", name: "restrict", checked: Array.isArray(pool.repositories) });
  const repositories = input({ name: "repositories", rows: 2, placeholder: tr("One OWNER/REPO per line") }, (pool.repositories || []).join("\n"));
  const sync = () => { repositories.disabled = !restrict.checked; };
  restrict.addEventListener("change", sync);
  sync();
  const legend = h("legend", {}, name || tr("New pool"));
  poolName.addEventListener("input", () => { legend.textContent = poolName.value || tr("New pool"); });
  const entry = h("fieldset", { class: "entry" }, legend,
    h("div", { class: "entry-grid" },
      field("Pool name", poolName),
      field("CPU limit", input({ name: "cpus", type: "number", min: 0.01, step: "any" }, pool.cpus || "")),
      field("Memory limit (MiB)", input({ name: "memory", type: "number", min: 6 }, pool.memory_mib || "")),
      field("Pool runner image", input({ name: "image", maxLength: 256, placeholder: tr("Use default image") }, pool.image), { className: "span" }),
      field("Worker addresses", input({ name: "hosts", rows: 2, placeholder: tr("One worker address per line") }, (pool.hosts || []).join("\n")), { className: "half" }),
      field("Allowed repositories", repositories, { className: "half" })),
    h("div", { class: "entry-foot" },
      h("label", { class: "check" }, h("input", { type: "checkbox", name: "privileged", checked: pool.privileged !== false }), tr("Run Docker-in-Docker (privileged)")),
      h("label", { class: "check" }, h("input", { type: "checkbox", name: "cache", checked: pool.cache === true }), tr("Keep a shared build cache")),
      h("label", { class: "check" }, restrict, tr("Restrict repositories")),
      button("Remove pool", async () => {
        const value = poolName.value.trim();
        if (value && !await parts.ctx.confirm(tr("Remove the pool {name} from these settings? The change applies when you save.", { name: value }), "Remove")) return;
        entry.remove();
      }, { small: true })));
  // Each entry keeps the object it was loaded from, so fields this editor
  // does not show (such as max_per_repository) are saved back unchanged.
  originals.set(entry, pool);
  list.append(entry);
  return poolName;
}

function values(container, selector) {
  return [...container.querySelectorAll(selector)].map((row) => Object.fromEntries([...row.querySelectorAll("[name]")]
    .map((input) => [input.name, input.type === "checkbox" ? input.checked : input.value.trim()])));
}

function settingsForm(ctx) {
  const originals = new WeakMap();
  let revision = 0;
  const output = statusLine();
  const hostList = h("tbody");
  const poolList = h("div");
  const webhook = h("p", { class: "hint" });
  const fleetFields = {
    slot_limit: h("input", { id: "set-slot-limit", name: "slot_limit", type: "number", inputMode: "numeric", min: 1, required: true }),
    poll_interval: h("input", { id: "set-poll", name: "poll_interval", placeholder: "30s", maxLength: 32, required: true, "aria-describedby": "set-poll-hint" }),
    runner_image: h("input", { id: "set-image", name: "runner_image", maxLength: 256, required: true, class: "mono" }),
  };
  webhook.id = "set-poll-hint";
  const docker = h("div", { hidden: true },
    block("Docker workers", {
      note: "SSH workers use the manager's configured SSH credentials. Outbound agents use deployment credentials.",
      actions: [button("Add worker", () => hostRow(hostList).focus())],
    }, h("div", { class: "table-wrap" }, h("table", {},
      h("caption", { class: "visually-hidden" }, tr("Docker workers")),
      h("thead", {}, h("tr", {}, h("th", { scope: "col" }, tr("Worker address")), h("th", { scope: "col" }, tr("Worker slot limit")),
        h("th", { scope: "col" }, h("span", { class: "visually-hidden" }, tr("Actions"))))),
      hostList))),
    block("Worker pools", { actions: [button("Add pool", () => poolEntry(poolList, originals).focus())] }, poolList));
  const form = h("form", { "aria-label": tr("Operational settings"), hidden: true },
    block("Fleet", {}, h("div", { class: "form-grid" },
      h("label", { for: "set-slot-limit" }, tr("Fleet slot limit")), fleetFields.slot_limit,
      h("label", { for: "set-poll" }, tr("GitHub polling interval")), fleetFields.poll_interval, webhook,
      h("label", { for: "set-image" }, tr("Default runner image")), fleetFields.runner_image)),
    docker,
    h("div", { class: "block" }, h("div", { class: "form-actions" }, button("Save settings", null, { type: "submit", primary: true }))));

  function populate({ backend, webhook_enabled, policy }) {
    revision = policy.revision;
    fleetFields.slot_limit.value = policy.slot_limit;
    fleetFields.poll_interval.value = policy.poll_interval;
    fleetFields.runner_image.value = policy.runner_image;
    webhook.textContent = tr(webhook_enabled ? "GitHub webhooks are enabled. Set polling to 0 for webhook-only discovery."
      : "GitHub webhooks are disabled. Keep polling enabled to receive jobs.");
    docker.hidden = backend !== "docker";
    hostList.replaceChildren();
    poolList.replaceChildren();
    for (const address of policy.docker_hosts || []) hostRow(hostList, address, policy.host_limits?.[address]);
    for (const [name, pool] of Object.entries(policy.worker_pools || {})) poolEntry(poolList, originals, name, pool);
    form.hidden = false;
  }

  const lines = (value) => value.split(/[,\n]/).map((part) => part.trim()).filter(Boolean);
  submitForm(form, output, async (data) => {
    const hosts = values(hostList, "tr");
    const fields = values(poolList, "fieldset");
    const pools = [...poolList.children].map((entry, i) => ({ original: originals.get(entry) || {}, ...fields[i] }));
    if (new Set(pools.map((pool) => pool.pool_name)).size !== pools.length) throw new ApiError(0, "Use a different name for each worker pool.");
    const saved = await ctx.api.put("./api/settings", {
      revision,
      slot_limit: Number(data.get("slot_limit")),
      poll_interval: String(data.get("poll_interval")).trim(),
      runner_image: String(data.get("runner_image")).trim(),
      docker_hosts: hosts.map((row) => row.address),
      host_limits: Object.fromEntries(hosts.filter((row) => row.limit).map((row) => [row.address, Number(row.limit)])),
      worker_pools: Object.fromEntries(pools.map((row) => [row.pool_name, {
        ...row.original,
        hosts: lines(row.hosts), repositories: row.restrict ? lines(row.repositories) : null,
        cpus: Number(row.cpus), memory_mib: Number(row.memory), image: row.image,
        // An absent value means the privileged default; keep it absent unless it changes.
        privileged: row.privileged ? (row.original.privileged === false ? true : row.original.privileged) : false,
        cache: row.cache,
      }])),
    }, { timeout: 10000 });
    populate(saved);
    message(output, tr("Settings saved."), "ok");
  });

  const loading = h("p", { class: "placeholder" }, tr("Loading…"));
  async function load() {
    message(output, "");
    try {
      populate(await ctx.api.get("./api/settings"));
    } catch (error) {
      if (error.status !== 401) message(output, errorText(error), "failed");
    } finally {
      loading.hidden = true;
    }
  }
  return { form, output, load, loading };
}

export default {
  id: "settings",
  title: "Settings",

  mount(root, ctx) {
    parts = { ctx };
    const settings = settingsForm(ctx);
    root.append(
      pageHead("Settings", button("Reload saved settings", () => void settings.load())),
      intro("Changes apply without restarting the manager."),
      settings.loading,
      settings.form,
      h("div", { class: "block" }, settings.output),
      h("p", { class: "block muted" }, ...fill("Queue backups are on the {link} screen.", {
        link: h("a", { href: "#/backups" }, tr("Backups")),
      })),
    );
    void settings.load();
  },

  unmount() {
    parts = null;
  },
};
