// Migrate (admin): rewrite runs-on in one repository's workflow files so its
// jobs run on Runquill, through a single reviewed pull request.
// GET /api/migrate/status, POST /api/migrate/preview, POST /api/migrate/open.
// Linear flow: 1 permissions, 2 repository and label mapping, 3 preview, 4 open.
// The repository, labels and diff are user input or repository content: text only.
import { language, serverText, tr } from "../i18n.js";
import { poolName } from "../fleet.js";
import { block, button, errorText, fill, format, h, kv, loadStyle, message, pageHead, state, statusLine } from "../ui.js";

// Previews read up to 50 files and opening commits each changed file; the
// server allows 90 seconds.
const slowRequest = { timeout: 95000 };
const maxRows = 50;
const maxLabels = 10;
const repositoryPattern = /^[a-z0-9-]+\/[a-z0-9_.-]+$/i;
const branchPrefix = "runquill/migrate-runs-on-";
const permissionNames = { contents: "Contents", pull_requests: "Pull requests", workflows: "Workflows" };

// sentence translates a server reason (fixed sentences and the templates in
// i18n.js) and, in English, starts it with a capital.
function sentence(text) {
  const out = serverText(String(text || "").trim());
  return out && language !== "ko" ? out[0].toUpperCase() + out.slice(1) : out;
}

// failure reads an error from the migrate handlers: their JSON bodies carry a
// code and, for an existing branch, its pull request; plain-text errors
// (auth, busy, unconfigured) have neither.
function failure(error) {
  return { text: error?.code ? sentence(error.message) : errorText(error), code: error?.code || "", pullRequest: error?.pullRequest || "" };
}

function pullRequestURL(url) {
  return typeof url === "string" && /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/\d+$/.test(url);
}

function count(n, one, many) {
  return tr(n === 1 ? one : many, { n: n.toLocaleString(language) });
}

function jobCount(n) { return count(n, "{n} job", "{n} jobs"); }
function fileCount(n) { return count(n, "{n} file", "{n} files"); }

let parts = null;

// Step 1: permissions.

function renderStatus() {
  const { status, permRows, permNote, permOutput, recheck } = parts;
  for (const [name, row] of permRows) {
    const cell = row.cells[2];
    if (status.state === "loading") cell.replaceChildren(h("span", { class: "muted" }, tr("Checking…")));
    else if (status.state === "error") cell.replaceChildren(h("span", { class: "muted" }, tr("Unknown")));
    else cell.replaceChildren(status.missing.includes(name) ? state("", "Missing", "failed") : state("", "Granted", "ok"));
  }
  permNote.hidden = status.state !== "ready" || status.missing.length === 0;
  if (status.state === "error") message(permOutput, status.error, "failed");
  else message(permOutput, "");
  recheck.hidden = status.state !== "error";
  renderOpen();
}

async function loadStatus() {
  const current = parts;
  current.status = { state: "loading", missing: [] };
  renderStatus();
  try {
    const result = await current.ctx.api.get("./api/migrate/status");
    if (parts !== current) return;
    const missing = Array.isArray(result?.missing) ? result.missing.map(String) : [];
    current.status = { state: "ready", missing };
  } catch (error) {
    if (parts !== current || error.status === 401) return;
    current.status = { state: "error", missing: [], error: failure(error).text };
  }
  renderStatus();
}

// Step 2: repository and mapping.

function poolOptions() {
  const names = [...parts.pools].filter((name) => name !== "default").sort((a, b) => a.localeCompare(b));
  return ["default", ...names];
}

function addPools(names) {
  if (!parts) return;
  let added = false;
  for (const name of names) {
    if (typeof name !== "string" || !/^[a-z0-9][a-z0-9-]{0,31}$/.test(name) || parts.pools.has(name)) continue;
    parts.pools.add(name);
    added = true;
  }
  if (!added) return;
  const options = poolOptions();
  for (const select of parts.rowsBody.querySelectorAll("select")) {
    const value = select.value;
    select.replaceChildren(...options.map((name) => h("option", { value: name }, poolName(name))));
    select.value = options.includes(value) ? value : "default";
  }
}

let rowCount = 0;

function addRow(label = "", pool = "default") {
  const id = ++rowCount;
  const input = h("input", {
    type: "text", name: "labels", value: label, maxLength: 1100, spellcheck: false, autocomplete: "off",
    placeholder: "ubuntu-latest", class: "mono", "aria-label": tr("Runs-on labels, mapping {n}", { n: id }),
  });
  const options = poolOptions();
  if (!options.includes(pool)) addPools([pool]);
  const select = h("select", { name: "pool", "aria-label": tr("Pool, mapping {n}", { n: id }) },
    ...poolOptions().map((name) => h("option", { value: name }, poolName(name))));
  select.value = pool;
  const remove = button("Remove", () => {
    const next = row.nextElementSibling || row.previousElementSibling;
    row.remove();
    changed();
    renderRows();
    next?.querySelector("input")?.focus();
  }, { small: true, "aria-label": tr("Remove mapping {n}", { n: id }) });
  const row = h("tr", {},
    h("td", { class: "migrate-labels" }, input),
    h("td", { class: "migrate-arrow", "aria-hidden": "true" }, "→"),
    h("td", {}, select),
    h("td", {}, h("div", { class: "row-actions" }, remove)));
  parts.rowsBody.append(row);
  renderRows();
  return input;
}

function renderRows() {
  const rows = [...parts.rowsBody.rows];
  for (const row of rows) row.querySelector("button").disabled = rows.length === 1;
  parts.addRowButton.disabled = rows.length >= maxRows;
}

// readForm validates like validateRunsOnMapping and ownedRepositoryPath and
// returns the request body, or marks the first problem and returns null.
function readForm() {
  const { repository, rowsBody, formOutput } = parts;
  const fail = (control, text) => {
    control.setAttribute("aria-invalid", "true");
    control.focus();
    message(formOutput, text, "failed");
    return null;
  };
  for (const control of parts.form.querySelectorAll("[aria-invalid]")) control.removeAttribute("aria-invalid");
  const name = repository.value.trim();
  const slug = name.split("/")[1];
  if (!repositoryPattern.test(name) || slug === "." || slug === "..") return fail(repository, tr("Enter the repository as OWNER/REPO."));
  const mapping = [];
  const seen = new Set();
  for (const row of rowsBody.rows) {
    const input = row.querySelector("input");
    const labels = input.value.split(",").map((label) => label.trim()).filter(Boolean);
    if (!labels.length || labels.length > maxLabels) return fail(input, sentence("each mapping needs between 1 and 10 runs-on labels"));
    for (const label of labels) {
      if (label.length > 100 || /[\u0000-\u001f\u007f]/.test(label) || label.includes("${{")) {
        return fail(input, sentence("runs-on labels must be 1 to 100 printable characters without expressions"));
      }
      const lower = label.toLowerCase();
      if (lower === "self-hosted" || lower.startsWith("runquill-")) {
        return fail(input, sentence("mappings move jobs from GitHub-hosted labels; self-hosted and Runquill labels are left alone"));
      }
    }
    const key = [...new Set(labels.map((label) => label.toLowerCase()))].sort().join("\u0000");
    if (seen.has(key)) return fail(input, sentence("each runs-on label set can be mapped only once"));
    seen.add(key);
    mapping.push({ from: labels, pool: row.querySelector("select").value });
  }
  if (!mapping.length) return fail(parts.addRowButton, sentence("add at least one runs-on mapping"));
  message(formOutput, "");
  return { repository: name, mapping };
}

// Any edit after a preview makes it describe something else: drop it.
function changed() {
  if (!parts || parts.busy) return;
  if (parts.preview || parts.opened) {
    parts.preview = null;
    parts.request = null;
    parts.opened = null;
    renderPreview();
    message(parts.formOutput, tr("The choices changed. Preview again to see the new changes."));
  }
}

// Step 3: preview.

async function runPreview(request, { automatic = false } = {}) {
  const current = parts;
  current.busy = true;
  current.preview = null;
  current.opened = null;
  if (!automatic) {
    current.retried = false;
    message(current.openOutput, "");
  }
  setBusy();
  renderPreview();
  message(current.formOutput, tr(current.ctx.demo ? "Loading the sample preview…" : "Reading workflow files from GitHub…"));
  try {
    const result = current.ctx.demo
      ? await current.ctx.api.get("./api/migrate/preview")
      : await current.ctx.api.post("./api/migrate/preview", request, slowRequest);
    if (parts !== current) return;
    current.preview = result;
    current.request = request;
    message(current.formOutput, automatic ? tr("The preview was refreshed with the current workflow files. Review it again before opening the pull request.") : "");
  } catch (error) {
    if (parts !== current) return;
    message(current.formOutput, error.status === 401 ? "" : failure(error).text, "failed");
  } finally {
    if (parts === current) {
      current.busy = false;
      setBusy();
      renderPreview();
    }
  }
}

function totals(preview) {
  const files = preview.files || [];
  let changedJobs = 0, leftJobs = 0, changedFiles = 0, refusedFiles = 0;
  for (const file of files) {
    const jobs = file.jobs || [];
    const changedHere = jobs.filter((job) => job.changed).length;
    changedJobs += changedHere;
    leftJobs += jobs.length - changedHere;
    if (file.changed) changedFiles++;
    if (file.refused) refusedFiles++;
  }
  return { files: files.length, changedJobs, leftJobs, changedFiles, refusedFiles };
}

function diffTable(path, diff) {
  const body = h("tbody");
  const lines = String(diff || "").split("\n");
  if (lines.at(-1) === "") lines.pop();
  let oldLine = 0, newLine = 0;
  // Lines 0 and 1 are the ---/+++ file headers.
  for (const line of lines.slice(2)) {
    const kind = line[0];
    const text = line.slice(1);
    if (kind === "@") {
      const match = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
      oldLine = match ? Number(match[1]) : 0;
      newLine = match ? Number(match[2]) : 0;
      body.append(h("tr", { class: "migrate-hunk" }, h("td", { colSpan: 4 }, line)));
      continue;
    }
    if (kind === "\\") {
      body.append(h("tr", { class: "migrate-note" }, h("td", { colSpan: 3 }), h("td", {}, tr("No newline at end of file"))));
      continue;
    }
    let old = "", next = "", marker = " ", tone = "", spoken = "";
    if (kind === "-") { old = oldLine++; marker = "-"; tone = "failed"; spoken = tr("Removed"); }
    else if (kind === "+") { next = newLine++; marker = "+"; tone = "ok"; spoken = tr("Added"); }
    else { old = oldLine++; next = newLine++; }
    body.append(h("tr", { dataset: tone ? { state: tone } : {} },
      h("td", { class: "num" }, String(old)),
      h("td", { class: "num" }, String(next)),
      h("td", { class: "migrate-marker" }, marker, spoken ? h("span", { class: "visually-hidden" }, spoken) : null),
      h("td", { class: "migrate-code" }, text)));
  }
  return h("div", { class: "table-wrap", role: "region", tabindex: "0", "aria-label": tr("Changes in {path}", { path }) },
    h("table", { class: "migrate-diff" },
      h("caption", { class: "visually-hidden" }, tr("Changes in {path}", { path })),
      h("thead", {}, h("tr", {},
        h("th", { scope: "col", class: "num" }, tr("Old")),
        h("th", { scope: "col", class: "num" }, tr("New")),
        h("th", { scope: "col" }, h("span", { class: "visually-hidden" }, tr("Change"))),
        h("th", { scope: "col" }, tr("Line")))),
      body));
}

function leftAloneTable(path, jobs) {
  return h("div", { class: "table-wrap", role: "region", tabindex: "0", "aria-label": tr("Jobs left alone in {path}", { path }) },
    h("table", {},
      h("caption", { class: "visually-hidden" }, tr("Jobs left alone in {path}", { path })),
      h("thead", {}, h("tr", {},
        h("th", { scope: "col" }, tr("Job")),
        h("th", { scope: "col" }, tr("Current runs-on")),
        h("th", { scope: "col" }, tr("Reason")))),
      h("tbody", {}, ...jobs.map((job) => h("tr", {},
        h("td", { class: "id" }, String(job.job), h("span", { class: "sub" }, tr("line {line}", { line: job.line }))),
        h("td", { class: "id" }, job.labels?.length ? job.labels.join(", ") : "—"),
        h("td", { class: "wrap" }, sentence(job.reason) || "—"))))));
}

function fileSection(file) {
  const jobs = file.jobs || [];
  const changedJobs = jobs.filter((job) => job.changed).length;
  const left = jobs.filter((job) => !job.changed);
  const summary = file.refused
    ? tr("Left alone: {reason}", { reason: sentence(file.refused) })
    : tr("{changed} changed, {left} left alone", { changed: jobCount(changedJobs), left: jobCount(left.length) });
  return h("section", { class: "migrate-file", "aria-label": file.path },
    h("div", { class: "migrate-file-head" },
      h("h3", { class: "mono" }, file.path),
      h("span", { class: "muted" }, summary)),
    file.diff ? diffTable(file.path, file.diff) : null,
    left.length ? leftAloneTable(file.path, left) : null);
}

function renderPreview() {
  const { preview, previewBlock, previewBody } = parts;
  previewBlock.hidden = !preview;
  if (!preview) {
    previewBody.replaceChildren();
    renderOpen();
    return;
  }
  const sum = totals(preview);
  const content = [];
  if (parts.ctx.demo) {
    content.push(h("p", { class: "inline-message" },
      tr("Sample preview. The sample panel cannot reach GitHub, so this is fixed sample output and no pull request can be opened.")));
  }
  content.push(kv([
    ["Repository", preview.repository || "—"],
    ["Default branch", preview.base || "—"],
    ["Workflow files read", format.number(sum.files)],
    ["Jobs to change", sum.changedJobs ? tr("{jobs} in {files}", { jobs: jobCount(sum.changedJobs), files: fileCount(sum.changedFiles) }) : format.number(0)],
    ["Jobs left alone", format.number(sum.leftJobs)],
    ["Files left alone", format.number(sum.refusedFiles)],
  ], { label: "Preview totals" }));
  if (!sum.files) {
    content.push(h("p", { class: "inline-message" }, tr("The repository has no workflow files in .github/workflows, so there is nothing to change.")));
  } else if (!sum.changedJobs) {
    content.push(h("p", { class: "inline-message" }, tr("No job matches the mapping, so nothing would change and no pull request will be opened.")));
  }
  for (const file of preview.files || []) content.push(fileSection(file));
  previewBody.replaceChildren(...content);
  renderOpen();
}

// Step 4: open the pull request.

function renderOpen() {
  if (!parts?.openBlock) return;
  const { preview, opened, status, openBlock, openSummary, openButton, openHint, ctx } = parts;
  const sum = preview ? totals(preview) : null;
  const canOpen = ctx.can("admin") && !ctx.demo;
  const ready = canOpen && sum && sum.changedJobs > 0;
  openBlock.hidden = !(ready || opened);
  openSummary.hidden = !ready;
  openButton.hidden = !ready;
  if (ready) {
    openSummary.textContent = tr("Opens one pull request on the new branch {branch} in {repo}, changing only runs-on in {jobs} across {files}. Nothing is merged.", {
      branch: branchPrefix + String(preview.hash || "").slice(0, 12), repo: preview.repository,
      jobs: jobCount(sum.changedJobs), files: fileCount(sum.changedFiles),
    });
  }
  const missing = status.state === "ready" && status.missing.length > 0;
  openButton.disabled = parts.busy || missing;
  openHint.hidden = !ready || !missing;
}

async function openPullRequest() {
  const current = parts;
  const { preview, request, ctx } = current;
  if (!preview || !request || current.busy) return;
  const sum = totals(preview);
  const branch = branchPrefix + String(preview.hash).slice(0, 12);
  const question = tr("Open a pull request in {repo}? It changes runs-on in {jobs} across {files} on the new branch {branch}. Nothing is merged.", {
    repo: preview.repository, jobs: jobCount(sum.changedJobs), files: fileCount(sum.changedFiles), branch,
  });
  if (!await ctx.confirm(question, "Open pull request")) return;
  if (parts !== current || current.preview !== preview) return;
  current.busy = true;
  setBusy();
  message(current.openOutput, tr("Opening the pull request on GitHub…"));
  let rerun = false;
  try {
    const pr = await ctx.api.post("./api/migrate/open", { ...request, hash: preview.hash }, slowRequest);
    if (parts !== current) return;
    const url = String(pr?.url || "");
    current.opened = url;
    current.preview = null;
    current.request = null;
    current.repository.value = "";
    const link = pullRequestURL(url)
      ? h("a", { href: url, target: "_blank", rel: "noopener noreferrer" }, url)
      : h("span", { class: "mono" }, url || "—");
    current.openOutput.replaceChildren(...fill("Opened pull request {link}. Review and merge it on GitHub when the pools are ready.", { link }));
    current.openOutput.dataset.state = "ok";
    current.openOutput.hidden = false;
    ctx.toast(tr("Pull request opened."));
  } catch (error) {
    if (parts !== current) return;
    if (error.status === 401) { message(current.openOutput, ""); return; }
    const info = failure(error);
    if (info.code === "stale" && !current.retried) {
      current.retried = true;
      rerun = true;
      message(current.openOutput, tr("The workflow files changed since the preview. Previewing again…"), "failed");
    } else if (info.code === "branch_exists") {
      const extra = pullRequestURL(info.pullRequest)
        ? fill("Existing pull request: {link}", { link: h("a", { href: info.pullRequest, target: "_blank", rel: "noopener noreferrer" }, info.pullRequest) })
        : [tr("Delete the branch {branch} in GitHub to try again.", { branch })];
      current.openOutput.replaceChildren(info.text, " ", ...extra);
      current.openOutput.dataset.state = "failed";
      current.openOutput.hidden = false;
    } else {
      message(current.openOutput, info.text, "failed");
    }
  } finally {
    if (parts === current) {
      current.busy = false;
      setBusy();
      renderPreview();
    }
  }
  if (rerun && parts === current) {
    await runPreview(request, { automatic: true });
    if (parts === current && current.preview) message(current.openOutput, tr("The workflow files changed since the preview. The preview above is now current; review it and open the pull request again."), "failed");
  }
}

function setBusy() {
  const { busy, fieldset, previewButton } = parts;
  fieldset.disabled = busy;
  previewButton.disabled = busy;
  renderOpen();
}

export default {
  id: "migrate",
  title: "Migrate",

  mount(root, ctx) {
    loadStyle("migrate");
    rowCount = 0;

    // 1. Permissions.
    const permRows = new Map(Object.entries(permissionNames).map(([name, label]) => [name, h("tr", {},
      h("td", {}, tr(label)), h("td", {}, tr("Read and write")), h("td", {}, "—"))]));
    const permTable = h("div", { class: "table-wrap", role: "region", tabindex: "0", "aria-label": tr("GitHub App permissions") },
      h("table", {},
        h("caption", { class: "visually-hidden" }, tr("GitHub App permissions")),
        h("thead", {}, h("tr", {},
          h("th", { scope: "col" }, tr("Permission")),
          h("th", { scope: "col" }, tr("Access")),
          h("th", { scope: "col" }, tr("Granted")))),
        h("tbody", {}, ...permRows.values())));
    const permNote = h("div", { class: "inline-message migrate-note-text", hidden: true },
      h("p", {}, tr("Migration is optional, and opening the pull request needs the GitHub App to have write access to Contents, Pull requests and Workflows.")),
      h("p", {}, ...fill("Create a new App with {command}, or add the three permissions to the existing App in its GitHub App settings and accept the request on the installation. You can still preview, which needs read access to Contents.", {
        command: h("code", {}, "runquill setup --migrate"),
      })));
    const permOutput = statusLine();
    const recheck = button("Check again", () => void loadStatus(), { small: true, hidden: true });

    // 2. Choose.
    const repository = h("input", {
      type: "text", name: "repository", placeholder: "OWNER/REPO", maxLength: 140, spellcheck: false,
      autocomplete: "off", required: true, class: "mono",
    });
    const rowsBody = h("tbody");
    const addRowButton = button("Add mapping", () => { addRow().focus(); changed(); }, { small: true });
    const mappingTable = h("div", { class: "table-wrap migrate-mapping", role: "region", "aria-label": tr("Label mapping") },
      h("table", {},
        h("caption", { class: "visually-hidden" }, tr("Label mapping")),
        h("thead", {}, h("tr", {},
          h("th", { scope: "col" }, tr("Jobs that run on")),
          h("th", { scope: "col" }, h("span", { class: "visually-hidden" }, tr("Moves to"))),
          h("th", { scope: "col" }, tr("Pool")),
          h("th", { scope: "col" }, h("span", { class: "visually-hidden" }, tr("Actions"))))),
        rowsBody));
    const previewButton = button("Preview changes", null, { type: "submit", primary: true });
    const formOutput = statusLine();
    const fieldset = h("fieldset", { class: "migrate-fieldset" },
      h("legend", { class: "visually-hidden" }, tr("Repository and label mapping")),
      h("div", { class: "toolbar" }, h("label", { class: "field grow migrate-repo" }, h("span", {}, tr("Repository")), repository)),
      mappingTable,
      h("p", { class: "muted migrate-hint" }, tr("A row matches jobs whose runs-on lists exactly these labels. Separate several labels with commas.")),
      h("div", { class: "table-foot" }, addRowButton));
    const form = h("form", { class: "migrate-form", novalidate: true },
      fieldset, h("div", { class: "form-actions" }, previewButton), formOutput);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      if (parts.busy) return;
      const request = readForm();
      if (request) void runPreview(request);
    });
    form.addEventListener("input", changed);
    form.addEventListener("change", changed);

    // 3. Preview and 4. Open.
    const previewBody = h("div", { class: "migrate-preview" });
    const openSummary = h("p", { class: "migrate-summary" });
    const openHint = h("p", { class: "muted", hidden: true }, tr("Grant the three write permissions in step 1 first."));
    const openButton = button("Open pull request", () => void openPullRequest(), { primary: true });
    const openOutput = statusLine();

    const previewBlock = block("3. Preview", {}, h("div", { "aria-live": "polite" }, previewBody));
    previewBlock.hidden = true;
    const openBlock = block("4. Open pull request", {}, openSummary, openHint, h("div", { class: "form-actions" }, openButton), openOutput);
    openBlock.hidden = true;

    root.append(
      pageHead("Migrate"),
      block("1. Permissions", {}, permTable, permNote, permOutput, h("div", { class: "table-foot" }, recheck)),
      block("2. Repository and labels", { note: "Runquill rewrites only runs-on, in one pull request you review. Nothing is opened until you confirm." }, form),
      previewBlock,
      openBlock,
    );

    parts = {
      ctx, status: { state: "loading", missing: [] }, permRows, permNote, permOutput, recheck,
      form, fieldset, repository, rowsBody, addRowButton, previewButton, formOutput,
      previewBlock, previewBody, openBlock, openSummary, openHint, openButton, openOutput,
      pools: new Set(["default"]), preview: null, request: null, opened: null, busy: false, retried: false,
    };
    addPools(snapshotPools(ctx.snapshot()));
    if (ctx.demo) {
      repository.value = "garden-lab/papertrail";
      addRow("ubuntu-latest", "default");
      addRow("ubuntu-24.04", "build");
    } else {
      addRow("ubuntu-latest", "default");
      void loadSettingsPools();
    }
    renderStatus();
    void loadStatus();
    if (ctx.demo) void runPreview(null);
  },

  update(snapshot) {
    addPools(snapshotPools(snapshot));
  },

  unmount() {
    parts = null;
  },
};

function snapshotPools(snapshot) {
  return (snapshot?.fleet?.hosts || []).map((host) => host.pool || "default");
}

// Configured pools may have no live host yet; settings lists them all.
async function loadSettingsPools() {
  const current = parts;
  try {
    const settings = await current.ctx.api.get("./api/settings");
    if (parts === current) addPools(Object.keys(settings?.policy?.worker_pools || {}));
  } catch {
    // The snapshot's pools are enough to work with.
  }
}
