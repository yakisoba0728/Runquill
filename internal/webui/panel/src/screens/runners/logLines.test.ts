import { describe, expect, it } from "vitest";
import { clean, LogBuffer, maxLineLength, maxLines } from "./logLines";

const cut = (n: number) => `[${n} cut]`;

describe("clean", () => {
  it("removes CSI, OSC and two-byte escapes", () => {
    expect(clean("\x1b[1;32mgreen\x1b[0m done", cut)).toBe("green done");
    expect(clean("\x1b]8;;https://example.com\x07link\x1b]8;;\x07", cut)).toBe("link");
    expect(clean("\x1b]0;title\x1b\\text", cut)).toBe("text");
    expect(clean("a\x1bMb", cut)).toBe("ab");
  });

  it("shows other control characters and drops a trailing CR", () => {
    expect(clean("a\x00b\x7fc\x9bd\r", cut)).toBe("a�b�c�d");
    expect(clean("tab\tkept", cut)).toBe("tab\tkept"); // tabs stay
  });

  it("cuts long lines and says how much", () => {
    const line = clean("x".repeat(maxLineLength + 5), cut);
    expect(line).toBe("x".repeat(maxLineLength) + " [5 cut]");
  });
});

describe("LogBuffer", () => {
  const lines = (n: number, from = 0) => Array.from({ length: n }, (_, i) => String(from + i));

  it("keeps at most maxLines pending and counts the rest as dropped", () => {
    const buffer = new LogBuffer();
    buffer.receive(lines(maxLines + 3));
    expect(buffer.pending).toHaveLength(maxLines);
    expect(buffer.pending[0]).toBe("3");
    expect(buffer.dropped).toBe(3);
  });

  it("removes the oldest shown lines past maxLines", () => {
    const buffer = new LogBuffer();
    buffer.receive(lines(maxLines - 2));
    expect(buffer.take()).toEqual({ lines: lines(maxLines - 2), removed: 0 });
    buffer.receive(lines(5, maxLines - 2));
    const { lines: added, removed } = buffer.take();
    expect(added).toHaveLength(5);
    expect(removed).toBe(3);
    expect(buffer.shown).toHaveLength(maxLines);
    expect(buffer.shown[0]).toBe("3");
    expect(buffer.dropped).toBe(3);
  });

  it("exports the shown lines only, with a final newline", () => {
    const buffer = new LogBuffer();
    expect(buffer.text()).toBe("");
    buffer.receive(["a", "b"]);
    buffer.take();
    buffer.receive(["c"]);
    expect(buffer.text()).toBe("a\nb\n");
    buffer.clear();
    expect([buffer.text(), buffer.dropped, buffer.pending.length]).toEqual(["", 0, 0]);
  });
});
