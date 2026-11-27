// Runner log text (v1 views/logs.js): lines are untrusted, so escape sequences go and other control characters become
// visible; the view keeps at most maxLines, counting what it drops.

export const maxLines = 5000; // lines kept on screen and in the pause buffer
export const maxLineLength = 4000; // characters shown per line; the rest is cut
export const followLimit = 15 * 60 * 1000; // the server ends a follow after 15 minutes
export const limitMarker = "[Runquill: log output limit reached]";
export const timestamp = /^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)) (.*)$/s;

/** Removes ANSI/VT escape sequences and makes other control characters visible; cut(n) words the cut-off note. */
export function clean(line: string, cut: (n: number) => string) {
  let text = line.endsWith("\r") ? line.slice(0, -1) : line;
  text = text
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "") // CSI: colours, cursor moves
    .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?/g, "") // OSC: titles, hyperlinks
    .replace(/\x1b[@-_]/g, "") // other two-byte escapes
    .replace(/[\x00-\x08\x0b-\x1f\x7f\x9b]/g, "\ufffd");
  return text.length > maxLineLength ? text.slice(0, maxLineLength) + " " + cut(text.length - maxLineLength) : text;
}

/** Lines waiting to be shown (all of them while paused) and the lines shown, each capped at maxLines. */
export class LogBuffer {
  pending: string[] = [];
  shown: string[] = [];
  dropped = 0;

  receive(lines: string[]) {
    this.pending.push(...lines);
    const extra = this.pending.length - maxLines;
    if (extra > 0) {
      this.pending.splice(0, extra);
      this.dropped += extra;
    }
  }

  /** Moves the pending lines to the shown ones: the lines to append, and how many to remove from the top. */
  take() {
    const lines = this.pending; // receive() keeps it within maxLines, so only older shown lines are removed
    this.pending = [];
    const removed = Math.max(0, this.shown.length + lines.length - maxLines);
    this.shown.splice(0, removed);
    this.shown.push(...lines);
    this.dropped += removed;
    return { lines, removed };
  }

  clear() {
    this.pending = [];
    this.shown = [];
    this.dropped = 0;
  }

  /** The shown lines as a file (Copy and Download keep only these). */
  text() {
    return this.shown.join("\n") + (this.shown.length ? "\n" : "");
  }
}
