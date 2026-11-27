import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { serverText, setCatalogue, t } from ".";
import ko from "./ko.json";
import patterns from "./server-patterns.json";

afterEach(() => setCatalogue("en", {}));

describe("i18n", () => {
  it("fills placeholders and leaves unknown ones", () => {
    setCatalogue("ko", { "{n} free": "여유 {n}" });
    expect(t("{n} free", { n: 2 })).toBe("여유 2");
    expect(t("{n} free")).toBe("여유 {n}");
    expect(t("Not a key {x}", { x: "y" })).toBe("Not a key y");
  });

  it("translates a server message's first line and keeps the detail", () => {
    setCatalogue("ko", ko, patterns);
    expect(serverText("Request failed.\ndetail line")).toBe(ko["Request failed."] + "\ndetail line");
  });

  it("translates a server template, nested details too", () => {
    setCatalogue("ko", ko, patterns);
    const text = serverText('worker pool build: warm must be between 0 and 8');
    expect(text).toBe(t("worker pool {name}: {detail}", { name: "build", detail: t("warm must be between 0 and {n}", { n: "8" }) }));
    expect(text).not.toContain("warm must");
  });

  it("leaves English and unknown text as sent", () => {
    expect(serverText("  Anything at all \n")).toBe("Anything at all");
  });

  it("has a Korean entry for the English keys written in the source", () => {
    const keys = new Set<string>();
    const walk = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap((entry) =>
      entry.isDirectory() ? walk(join(dir, entry.name)) : /\.tsx?$/.test(entry.name) && !entry.name.includes(".test.") ? [join(dir, entry.name)] : []);
    for (const file of walk(join(__dirname, ".."))) {
      if (file.includes("/demo/")) continue; // sample fixtures: server-like English data, not keys
      const source = readFileSync(file, "utf8");
      // t("…"), fill("…"), table fields (title: "…") and both arms of a ternary (? "…" : "…").
      for (const match of source.matchAll(/\b(?:t|fill)\(\s*"((?:[^"\\]|\\.)+)"|\b(?:title|label|key): "([A-Z][^"]*)"|[?:] "([A-Z{][^"]*)"/g)) {
        keys.add(match[1] ?? match[2] ?? match[3]);
      }
    }
    // Codes and fixed text that are not translated.
    const literal = new Set(["POST", "Meta+K /", "Control+K /", "Ctrl K", "English", "UTC",
      "CORDONED", "RUNNING", "PAUSED", "STARTING", "CLEANUP", "UNKNOWN", "RETRY", "WAITING"]);
    const missing = [...keys].filter((key) => !Object.hasOwn(ko, key) && !literal.has(key));
    expect(missing).toEqual([]);
  });
});
