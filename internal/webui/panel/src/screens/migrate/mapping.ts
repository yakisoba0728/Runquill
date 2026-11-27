// Workflow migration rules kept apart from the screen so they are tested (v1 migrate.js): the form check that mirrors
// the server's validateRunsOnMapping and ownedRepositoryPath, and the reading of a preview's unified diffs.
import { language, serverText, t } from "../../i18n";

export const maxRows = 50;
export const maxLabels = 10;
const repositoryPattern = /^[a-z0-9-]+\/[a-z0-9_.-]+$/i;

/** A server reason (fixed sentences and templates), with a capital first letter in English. */
export function sentence(text: unknown) {
  const out = serverText(String(text ?? "").trim());
  return out && language !== "ko" ? out[0].toUpperCase() + out.slice(1) : out;
}

export interface MappingRow {
  key: string;
  /** Comma-separated runs-on labels, as typed. */
  labels: string;
  pool: string;
}

export interface MigrateRequest {
  repository: string;
  mapping: { from: string[]; pool: string }[];
}

/** The preview request, or the first problem: where it is (repository, a row key, or "add") and why. */
export function readMapping(repository: string, rows: MappingRow[]): { request: MigrateRequest } | { at: string; problem: string } {
  const name = repository.trim();
  const slug = name.split("/")[1];
  if (!repositoryPattern.test(name) || slug === "." || slug === "..") return { at: "repository", problem: t("Enter the repository as OWNER/REPO.") };
  const mapping: MigrateRequest["mapping"] = [];
  const seen = new Set<string>();
  for (const row of rows) {
    const fail = (reason: string) => ({ at: row.key, problem: sentence(reason) });
    const labels = row.labels.split(",").map((label) => label.trim()).filter(Boolean);
    if (!labels.length || labels.length > maxLabels) return fail("each mapping needs between 1 and 10 runs-on labels");
    for (const label of labels) {
      if (label.length > 100 || /[\u0000-\u001f\u007f]/.test(label) || label.includes("${{")) {
        return fail("runs-on labels must be 1 to 100 printable characters without expressions");
      }
      const lower = label.toLowerCase();
      if (lower === "self-hosted" || lower.startsWith("runquill-")) {
        return fail("mappings move jobs from GitHub-hosted labels; self-hosted and Runquill labels are left alone");
      }
    }
    const key = [...new Set(labels.map((label) => label.toLowerCase()))].sort().join("\u0000");
    if (seen.has(key)) return fail("each runs-on label set can be mapped only once");
    seen.add(key);
    mapping.push({ from: labels, pool: row.pool });
  }
  if (!mapping.length) return { at: "add", problem: sentence("add at least one runs-on mapping") };
  return { request: { repository: name, mapping } };
}

export type DiffLine =
  | { kind: "hunk"; text: string }
  | { kind: "note" }
  | { kind: "same" | "removed" | "added"; old: number | null; new: number | null; text: string };

/** A unified diff from the server (unifiedDiff) as numbered lines; the ---/+++ file headers are dropped. */
export function diffLines(diff: string): DiffLine[] {
  const lines = String(diff || "").split("\n");
  if (lines.at(-1) === "") lines.pop();
  let oldLine = 0;
  let newLine = 0;
  return lines.slice(2).map((line): DiffLine => {
    const kind = line[0];
    const text = line.slice(1);
    if (kind === "@") {
      const match = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
      oldLine = match ? Number(match[1]) : 0;
      newLine = match ? Number(match[2]) : 0;
      return { kind: "hunk", text: line };
    }
    if (kind === "\\") return { kind: "note" };
    if (kind === "-") return { kind: "removed", old: oldLine++, new: null, text };
    if (kind === "+") return { kind: "added", old: null, new: newLine++, text };
    return { kind: "same", old: oldLine++, new: newLine++, text };
  });
}

export interface PreviewFile {
  path: string;
  changed?: boolean;
  refused?: string;
  diff?: string;
  jobs?: { job: string; line: number; labels?: string[]; changed: boolean; reason?: string }[] | null;
}

export interface Preview {
  repository: string;
  base?: string;
  hash: string;
  files?: PreviewFile[] | null;
}

export function totals(preview: Preview) {
  const files = preview.files ?? [];
  let changedJobs = 0, leftJobs = 0, changedFiles = 0, refusedFiles = 0;
  for (const file of files) {
    const jobs = file.jobs ?? [];
    const changedHere = jobs.filter((job) => job.changed).length;
    changedJobs += changedHere;
    leftJobs += jobs.length - changedHere;
    if (file.changed) changedFiles++;
    if (file.refused) refusedFiles++;
  }
  return { files: files.length, changedJobs, leftJobs, changedFiles, refusedFiles };
}
