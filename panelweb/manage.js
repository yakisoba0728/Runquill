import { language, tr } from "./i18n.js";

const byID = (id) => document.getElementById(id);
byID("login-host").textContent = location.host;

export async function panelRequest(path, method = "GET", data, timeout = 7000) {
  const response = await fetch(path, {
    method, cache: "no-store", signal: AbortSignal.timeout(timeout),
    headers: data === undefined ? undefined : { "Content-Type": "application/json" },
    body: data === undefined ? undefined : JSON.stringify(data),
  });
  if (!response.ok) {
    const error = new Error((await response.text()).trim() || "Request failed.");
    error.status = response.status;
    throw error;
  }
  return response.status === 204 ? null : response.json();
}

function message(id, value) {
  const target = byID(id);
  target.textContent = tr(value || "");
  target.hidden = !value;
}

export function showLogin(error) {
  const wasAuthenticated = document.body.hasAttribute("data-authenticated");
  delete document.body.dataset.authenticated;
  document.title = tr("Sign in to Runquill");
  byID("access-nav").hidden = true;
  byID("settings-nav").hidden = true;
  byID("signout").hidden = true;
  byID("token-secret").value = "";
  byID("issued-token").hidden = true;
  message("login-message", error);
  byID("sign-in").hidden = false;
  if (wasAuthenticated) byID("login-title").focus();
}

function bindSettings() {
  const form = byID("settings-form");
  const hostList = byID("settings-hosts");
  const poolList = byID("settings-pools");
  let revision = 0;

  function addEntry(template, list) {
    const row = byID(template).content.firstElementChild.cloneNode(true);
    row.querySelector("[data-remove]").addEventListener("click", () => row.remove());
    list.append(row);
    return row;
  }

  function addHost(address = "", limit = "") {
    const row = addEntry("settings-host", hostList);
    const input = row.querySelector("[name=address]");
    input.value = address;
    row.querySelector("[name=limit]").value = limit;
    if (address.startsWith("agent://")) {
      input.readOnly = true;
      row.querySelector("[data-remove]").hidden = true;
    }
    return input;
  }

  function addPool(name = "", pool = {}) {
    const row = addEntry("settings-pool", poolList);
    const field = (key) => row.querySelector("[name=" + key + "]");
    field("pool_name").value = name;
    field("cpus").value = pool.cpus || "";
    field("memory").value = pool.memory_mib || "";
    field("image").value = pool.image || "";
    field("hosts").value = (pool.hosts || []).join("\n");
    field("restrict").checked = Array.isArray(pool.repositories);
    field("repositories").value = (pool.repositories || []).join("\n");
    const sync = () => { field("repositories").disabled = !field("restrict").checked; };
    field("restrict").addEventListener("change", sync);
    sync();
    return field("pool_name");
  }

  function populate({ backend, webhook_enabled, policy }) {
    revision = policy.revision;
    form.elements.slot_limit.value = policy.slot_limit;
    form.elements.poll_interval.value = policy.poll_interval;
    form.elements.runner_image.value = policy.runner_image;
    byID("settings-webhook").textContent = tr(webhook_enabled ?
      "GitHub webhooks are enabled. Set polling to 0 for webhook-only discovery." :
      "GitHub webhooks are disabled. Keep polling enabled to receive jobs.");
    byID("settings-docker").hidden = backend !== "docker";
    hostList.replaceChildren();
    poolList.replaceChildren();
    for (const address of policy.docker_hosts || []) addHost(address, policy.host_limits?.[address]);
    for (const [name, pool] of Object.entries(policy.worker_pools || {})) addPool(name, pool);
    form.hidden = false;
  }

  async function load() {
    populate(await panelRequest("./api/settings"));
  }

  function rows(list) {
    return [...list.children].map((row) => Object.fromEntries([...row.querySelectorAll("[name]")]
      .map((field) => [field.name, field.type === "checkbox" ? field.checked : field.value.trim()])));
  }

  const values = (value) => value.split(/[,\n]/).map((part) => part.trim()).filter(Boolean);
  bindForm("settings-form", "settings-message", async (data) => {
    const hosts = rows(hostList);
    const pools = rows(poolList);
    if (new Set(pools.map((pool) => pool.pool_name)).size !== pools.length) throw new Error(tr("Use a different name for each worker pool."));
    const saved = await panelRequest("./api/settings", "PUT", {
      revision,
      slot_limit: Number(data.get("slot_limit")),
      poll_interval: data.get("poll_interval").trim(),
      runner_image: data.get("runner_image").trim(),
      docker_hosts: hosts.map((row) => row.address),
      host_limits: Object.fromEntries(hosts.filter((row) => row.limit).map((row) => [row.address, Number(row.limit)])),
      worker_pools: Object.fromEntries(pools.map((row) => [row.pool_name, {
        hosts: values(row.hosts), repositories: row.restrict ? values(row.repositories) : null,
        cpus: Number(row.cpus), memory_mib: Number(row.memory), image: row.image,
      }])),
    }, 10000);
    populate(saved);
    message("settings-message", "Settings saved.");
  });
  byID("settings-reload").addEventListener("click", () => void report("settings-message", load));
  byID("settings-add-host").addEventListener("click", () => addHost().focus());
  byID("settings-add-pool").addEventListener("click", () => addPool().focus());
  byID("settings").hidden = false;
  byID("settings-nav").hidden = false;
  void report("settings-message", load);
}

function listItem(parent, title, details, button, action) {
  const li = document.createElement("li");
  const info = document.createElement("div");
  const heading = document.createElement("strong");
  const meta = document.createElement("small");
  heading.textContent = tr(title);
  meta.textContent = tr(details);
  info.append(heading, meta);
  li.append(info);
  if (button) {
    const control = document.createElement("button");
    control.type = "button";
    control.className = "button";
    control.textContent = tr(button);
    control.addEventListener("click", action);
    li.append(control);
  }
  parent.append(li);
}

async function report(id, act) {
  message(id, "");
  try {
    await act();
  } catch (error) {
    if (error.status === 401) showLogin();
    else message(id, error.message || "Request failed.");
  }
}

function bindForm(id, messageID, action) {
  const form = byID(id);
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = form.querySelector("button[type=submit]");
    button.disabled = true;
    await report(messageID, () => action(new FormData(form)));
    button.disabled = false;
  });
}

async function tokens() {
  const list = byID("token-list");
  const entries = await panelRequest("./api/tokens");
  list.replaceChildren();
  if (!entries.length) listItem(list, "No active API tokens", "Create a token when a script needs access.");
  for (const token of entries) {
    listItem(list, token.name, tr("{scope} · expires {date}", { scope: token.scope, date: new Date(token.expires_at * 1000).toLocaleDateString(language) }), "Revoke", () => {
      if (!window.confirm(tr("Revoke the token {name}?", { name: token.name }))) return;
      void report("token-message", async () => {
        await panelRequest("./api/tokens/" + token.id, "DELETE");
        message("token-message", "Token revoked.");
        await tokens();
      });
    });
  }
}

async function team() {
  const list = byID("team-list");
  const entries = await panelRequest("./api/team");
  list.replaceChildren();
  for (const member of entries) {
    listItem(list, member.login ? "@" + member.login : tr("GitHub ID {id}", { id: member.github_id }),
      tr(member.role) + (member.disabled ? " · " + tr("disabled") : "") + " · ID " + member.github_id,
      member.disabled ? "Enable" : "Disable", () => {
        const label = member.login ? "@" + member.login : "ID " + member.github_id;
        if (!member.disabled && !window.confirm(tr("Disable {name} and revoke their sessions and tokens?", { name: label }))) return;
        void report("team-message", async () => {
          await panelRequest("./api/team/" + member.github_id, "PUT", {
            role: member.role, disabled: !member.disabled,
          });
          message("team-message", member.disabled ? "Team member enabled." : "Team member disabled.");
          await team();
        });
      });
  }
}

async function audit() {
  const list = byID("audit-list");
  const entries = await panelRequest("./api/audit");
  list.replaceChildren();
  if (!entries.length) listItem(list, "No audit entries yet", "");
  for (const event of entries) {
    listItem(list, event.action, new Date(event.occurred_at * 1000).toLocaleString(language) + " · GitHub ID " + event.actor +
      (event.target ? " · " + event.target : ""));
  }
}

function bindOperations() {
  byID("operations-panel").hidden = false;
  byID("download-backup").addEventListener("click", () => void report("operations-message", async () => {
    const response = await fetch("./api/backup", { method: "POST", cache: "no-store", signal: AbortSignal.timeout(10000) });
    if (!response.ok) throw new Error(await response.text());
    const url = URL.createObjectURL(await response.blob());
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = "runquill-queue-backup.sqlite3";
    anchor.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    message("operations-message", "Queue backup downloaded.");
  }));
  bindForm("diagnose-form", "operations-message", async (form) => {
    const result = byID("diagnose-result");
    result.hidden = true;
    const response = await panelRequest("./api/diagnose", "POST", { repository: form.get("repository") }, 10000);
    result.textContent = response.report;
    result.hidden = false;
    message("operations-message", response.ok ? "Repository check complete." : "Repository check needs attention.");
  });
}

export async function initializeManagement() {
  let user;
  try {
    user = await panelRequest("./api/me");
  } catch (error) {
    showLogin(error.status === 401 ? "" : "The panel could not check your session. Try again later.");
    return null;
  }
  document.body.dataset.authenticated = "true";
  byID("sign-in").hidden = true;
  byID("access").hidden = false;
  byID("access-nav").hidden = false;
  byID("signout").hidden = false;
  byID("mode").textContent = tr(user.role);
  byID("current-user").textContent = tr("Signed in as @{name} · {role}", { name: user.login, role: tr(user.role) });
  byID("maint-scope").disabled = user.role === "viewer";
  byID("signout").addEventListener("click", () => void report("host-action", async () => {
    await panelRequest("./api/logout", "POST");
    window.location.assign("./");
  }));
  byID("hide-token").addEventListener("click", () => {
    byID("token-secret").value = "";
    byID("issued-token").hidden = true;
  });
  bindForm("token-form", "token-message", async (form) => {
    const token = await panelRequest("./api/tokens", "POST", {
      name: form.get("name"), scope: form.get("scope"), days: Number(form.get("days")),
    });
    const secret = byID("token-secret");
    secret.value = token.secret;
    byID("issued-token").hidden = false;
    secret.focus();
    secret.select();
    await tokens();
  });
  if (user.role === "admin") {
    bindOperations();
    bindSettings();
    byID("team-panel").hidden = false;
    byID("audit-panel").hidden = false;
    bindForm("team-form", "team-message", async (form) => {
      await panelRequest("./api/team/" + encodeURIComponent(form.get("github_id")), "PUT", {
        role: form.get("role"), disabled: false,
      });
      message("team-message", "Team member saved.");
      await team();
    });
    byID("refresh-audit").addEventListener("click", () => void report("audit-message", audit));
    void report("team-message", team);
    void report("audit-message", audit);
  }
  void report("token-message", tokens);
  return user;
}

export function updateHost(host) {
  return report("host-action", async () => {
    await panelRequest("./api/hosts/cordon", "POST", { host: host.name, cordoned: !host.cordoned });
    message("host-action", host.cordoned ? "New assignments resumed." : "New assignments paused. Existing jobs can finish.");
  });
}
