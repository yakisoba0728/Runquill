import { describe, expect, it } from "vitest";
import { diffLines, readMapping } from "./mapping";

const row = (labels: string, pool = "default", key = "r" + labels) => ({ key, labels, pool });

describe("migrate mapping", () => {
  it("builds the request", () => {
    expect(readMapping(" garden-lab/forecast ", [row("ubuntu-latest, ubuntu-24.04", "build")])).toEqual({
      request: { repository: "garden-lab/forecast", mapping: [{ from: ["ubuntu-latest", "ubuntu-24.04"], pool: "build" }] },
    });
  });

  it.each([
    ["forecast", [row("ubuntu-latest")], "repository", "Enter the repository as OWNER/REPO."],
    ["o/..", [row("ubuntu-latest")], "repository", "Enter the repository as OWNER/REPO."],
    ["o/r", [row(" , ")], "r , ", "Each mapping needs between 1 and 10 runs-on labels"],
    ["o/r", [row(Array.from({ length: 11 }, (_, i) => "l" + i).join(","))], expect.any(String), "Each mapping needs between 1 and 10 runs-on labels"],
    ["o/r", [row("${{ matrix.os }}")], expect.any(String), "Runs-on labels must be 1 to 100 printable characters without expressions"],
    ["o/r", [row("Self-Hosted")], expect.any(String), "Mappings move jobs from GitHub-hosted labels; self-hosted and Runquill labels are left alone"],
    ["o/r", [row("runquill-x")], expect.any(String), "Mappings move jobs from GitHub-hosted labels; self-hosted and Runquill labels are left alone"],
    ["o/r", [row("a, b", "default", "1"), row("B,A", "build", "2")], "2", "Each runs-on label set can be mapped only once"],
    ["o/r", [], "add", "Add at least one runs-on mapping"],
  ])("refuses %s %j", (repository, rows, at, problem) => {
    expect(readMapping(repository, rows)).toEqual({ at, problem });
  });

  it("numbers diff lines from each hunk", () => {
    const diff = "--- a/x\n+++ b/x\n@@ -4,3 +4,3 @@\n jobs:\n-  a: 1\n+  a: 2\n\\ No newline at end of file\n";
    expect(diffLines(diff)).toEqual([
      { kind: "hunk", text: "@@ -4,3 +4,3 @@" },
      { kind: "same", old: 4, new: 4, text: "jobs:" },
      { kind: "removed", old: 5, new: null, text: "  a: 1" },
      { kind: "added", old: null, new: 5, text: "  a: 2" },
      { kind: "note" },
    ]);
  });
});
