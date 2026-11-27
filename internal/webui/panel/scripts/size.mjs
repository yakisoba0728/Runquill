// Measures the built panel against the size budget (design §6.8) with gzip -9 and fails when it is over.
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import { gzipSync } from "node:zlib";

const out = new URL("../../panelv2/", import.meta.url).pathname;
const kb = (bytes) => (bytes / 1024).toFixed(1) + " KB";
const gz = (file) => gzipSync(readFileSync(join(out, file)), { level: 9 }).length;

const html = readFileSync(join(out, "index.html"), "utf8");
const entry = /src="\.\/(assets\/panel-[^"]+\.js)"/.exec(html)[1];
const css = [...html.matchAll(/href="\.\/(assets\/[^"]+\.css)"/g)].map((m) => m[1]);
// Static imports of the entry (shared chunks); dynamic import() chunks load later.
const shared = [...readFileSync(join(out, entry), "utf8").matchAll(/(?:^|[;\n])import(?:[^"'`()]*?from)?\s*["'`]\.\/([^"'`]+)["'`]/g)]
  .map((m) => "assets/" + m[1]);
const first = ["index.html", entry, ...shared, ...css];

const files = readdirSync(join(out, "assets")).map((name) => "assets/" + name);
const total = ["index.html", "theme.js", ...files].reduce((sum, file) => sum + statSync(join(out, file)).size, 0);
const rows = [
  ["first screen (index.html, entry, shared chunks, CSS)", first.reduce((sum, file) => sum + gz(file), 0), 150 * 1024],
  ["CSS", css.reduce((sum, file) => sum + gz(file), 0), 35 * 1024],
  ...files.filter((file) => file.endsWith(".js") && !first.includes(file)).map((file) => [file + " (lazy)", gz(file), 30 * 1024]),
  ["panelv2/ raw total", total, 1200 * 1024],
];

let over = false;
for (const [name, size, limit] of rows) {
  const bad = size > limit;
  over ||= bad;
  console.log(`${bad ? "OVER" : "ok  "}  ${kb(size).padStart(9)} / ${kb(limit).padStart(9)}  ${name}`);
}
if (over) {
  console.error("The panel is over its size budget (docs/design/panel-v2.md §6.8).");
  process.exit(1);
}
