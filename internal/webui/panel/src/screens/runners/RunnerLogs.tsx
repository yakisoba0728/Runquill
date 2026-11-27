// G3 LogView: a runner container's own output from GET /api/runners/{name}/logs?tail=N[&follow=1] (operators and admins
// with a session; the sample panel answers fixed lines). Ported from v1 views/logs.js. The response is plain text: a
// tail, or with follow a chunked stream that ends at the server's limits. Lines are untrusted: written as text only,
// escape sequences removed (logLines.ts). New lines collect and are appended once per frame, outside React, because a
// follow can add thousands; at most 5,000 stay. At phone width the action buttons fold into one Menu (panel.css).
import { Button, Menu, Select, Switch, toast } from "@yakihyuk0728/stratum-ui";
import { useEffect, useMemo, useRef, useState } from "react";
import { saveFile } from "../../actions";
import { isSample, stream, type ApiError } from "../../api";
import { useScreenCommands, type ScreenCommand } from "../../commands";
import { runnerRows } from "../../fleet";
import { number, timeOfDay } from "../../format";
import { t } from "../../i18n";
import { live } from "../../live";
import { clean, followLimit, limitMarker, LogBuffer, timestamp } from "./logLines";

const tails = ["100", "200", "500", "1000", "2000"];
const tailNames: Record<string, string> = { "100": "100 lines", "200": "200 lines", "500": "500 lines", "1000": "1,000 lines", "2000": "2,000 lines" };

/** Why the server cannot read this runner's logs (runner_logs.go), or "" when it can. */
export function unreadable(runner: { host?: string; job_id?: number }) {
  if (runner.host === "kubernetes") return "Not available for Kubernetes runners.";
  if (runner.host?.startsWith("agent://")) return "Not available for agent:// workers.";
  if (!runner.host || !runner.job_id) return "This runner is not active in Runquill.";
  return "";
}

const stillRunning = (name: string) => runnerRows(live.get().snapshot).some((runner) => runner.name === name && runner.job_id);

function lineNode(text: string) {
  const line = document.createElement("div");
  line.className = "log-line";
  const match = timestamp.exec(text);
  if (!match) {
    line.textContent = text;
    return line;
  }
  const time = document.createElement("span");
  time.className = "log-time";
  time.title = match[1];
  time.textContent = timeOfDay(match[1]);
  line.append(time, " " + match[2]);
  return line;
}

type Tone = "" | "waiting" | "failed";

/** active: the pane is open on this tab. The stream runs only then; gone: the runner left the snapshot. */
export function RunnerLogs({ name, active, gone }: { name: string; active: boolean; gone: boolean }) {
  const sample = isSample();
  const [tail, setTail] = useState("200");
  const [follow, setFollow] = useState(!sample);
  const [wrap, setWrap] = useState(true);
  const [paused, setPaused] = useState(false);
  const [status, setStatus] = useState<{ text: string; tone: Tone }>({ text: "", tone: "" });
  const [streaming, setStreaming] = useState(false);
  const [reconnect, setReconnect] = useState(false);
  const [jump, setJump] = useState(false);
  const [round, setRound] = useState(0); // Reconnect
  const [, setCount] = useState(0); // re-renders the line counts
  const output = useRef<HTMLDivElement>(null);
  const buffer = useRef(new LogBuffer()).current;
  const frame = useRef({ id: 0, timer: 0 as ReturnType<typeof setTimeout> | 0, paused: false });
  const controller = useRef<AbortController | null>(null);

  const unschedule = () => {
    cancelAnimationFrame(frame.current.id);
    clearTimeout(frame.current.timer);
    frame.current.id = 0;
    frame.current.timer = 0;
  };
  const flush = () => {
    unschedule();
    const out = output.current;
    if (!out || frame.current.paused || !buffer.pending.length) return setCount((n) => n + 1);
    // Follow the tail only when the reader is already at the bottom.
    const atBottom = out.scrollHeight - out.scrollTop - out.clientHeight < 24;
    const { lines, removed } = buffer.take();
    const fragment = document.createDocumentFragment();
    for (const line of lines) fragment.append(lineNode(line));
    for (let i = 0; i < removed; i++) out.firstChild?.remove();
    out.append(fragment);
    if (atBottom) out.scrollTop = out.scrollHeight;
    else setJump(true);
    setCount((n) => n + 1);
  };
  // A frame is the normal trigger; the timer covers browsers that stop frames for a page that is not painted.
  const schedule = () => {
    if (frame.current.id || frame.current.paused) return;
    frame.current.id = requestAnimationFrame(flush);
    frame.current.timer = setTimeout(flush, 250);
  };
  const receive = (lines: string[]) => {
    buffer.receive(lines);
    if (frame.current.paused) setCount((n) => n + 1);
    else schedule();
  };
  const clear = () => {
    unschedule();
    buffer.clear();
    output.current?.replaceChildren();
    setJump(false);
    setCount((n) => n + 1);
  };
  const pause = (next: boolean) => {
    frame.current.paused = next;
    setPaused(next);
    if (!next) schedule();
  };
  const stop = () => {
    controller.current?.abort();
    controller.current = null;
    setStreaming(false);
  };

  useEffect(() => {
    if (!active) return;
    const current = new AbortController();
    controller.current = current;
    const following = follow && !sample;
    const path = "./api/runners/" + encodeURIComponent(name) + "/logs?tail=" + tail + (following ? "&follow=1" : "");
    const cut = (n: number) => t("[{n} characters cut]", { n });
    const shownTail = t("Showing the last {n} lines.", { n: tail });
    clear();
    setReconnect(false);
    setStatus({ text: t("Connecting…"), tone: "" });
    setStreaming(following);
    const started = Date.now();
    (async () => {
      let response: Response;
      try {
        response = await stream(path, { signal: current.signal });
      } catch (error) {
        if (current.signal.aborted) return;
        setStreaming(false);
        const status = (error as ApiError).status;
        if (status === 401) return;
        setStatus({ text: (error as ApiError).message, tone: "failed" });
        // A lost connection, 429 (too many streams) and server errors may pass; 501 and 404 do not.
        setReconnect([0, 429, 502, 503].includes(status));
        return;
      }
      let limited = false;
      try {
        setStatus({ text: following ? t("Following new output.") : shownTail, tone: "" });
        const reader = response.body!.pipeThrough(new TextDecoderStream()).getReader();
        current.signal.addEventListener("abort", () => void reader.cancel().catch(() => {}));
        let rest = "";
        for (;;) {
          const { value, done } = await reader.read();
          if (done || current.signal.aborted) break;
          const pieces = (rest + value).split("\n");
          rest = pieces.pop()!;
          if (pieces.includes(limitMarker)) limited = true;
          if (pieces.length) receive(pieces.map((line) => clean(line, cut)));
        }
        if (current.signal.aborted) return;
        if (rest) receive([clean(rest, cut)]);
        setStreaming(false);
        if (!following) {
          setStatus({ text: shownTail, tone: "" });
          setReconnect(!sample);
          return;
        }
        const ended = !stillRunning(name);
        if (ended) setStatus({ text: t("The runner finished, so its log stream ended."), tone: "" });
        else if (limited) setStatus({ text: t("The stream stopped at the server's 8 MiB limit. Reconnect to continue from the latest lines."), tone: "waiting" });
        else if (Date.now() - started >= followLimit - 5000) setStatus({ text: t("Follow streams end after 15 minutes. Reconnect to continue."), tone: "waiting" });
        else setStatus({ text: t("The log stream ended. The last lines may explain why."), tone: "waiting" });
        setReconnect(!ended);
      } catch {
        if (current.signal.aborted) return;
        setStreaming(false);
        setStatus({ text: t("The log stream was interrupted. Check the connection and reconnect."), tone: "failed" });
        setReconnect(true);
      }
    })();
    return () => {
      current.abort();
      unschedule();
    };
  }, [active, name, tail, follow, round]);

  // docker logs --follow ends by itself when the container stops; abort in case the stream is still open.
  useEffect(() => {
    if (!gone || !active) return;
    stop();
    setStatus({ text: t("This runner finished and was removed. Its last output is kept below."), tone: "" });
    setReconnect(false);
  }, [gone, active]);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(buffer.text());
      toast(t("Copied {n} lines.", { n: number(buffer.shown.length) }), { tone: "success" });
    } catch {
      toast(t("The browser did not allow copying. Use Download instead."), { tone: "danger" });
    }
  };
  const download = () => saveFile(new Blob([buffer.text()], { type: "text/plain;charset=utf-8" }), name + ".log");
  const stopFollowing = () => {
    stop();
    setStatus({ text: t("Stopped. The lines shown are kept."), tone: "" });
    setReconnect(true);
  };

  const latest = useRef({ pause, download, stopFollowing });
  latest.current = { pause, download, stopFollowing };
  const commands = useMemo<ScreenCommand[]>(() => !active ? [] : [
    { id: "log-pause", label: paused ? "Resume log output" : "Pause log output", run: () => latest.current.pause(!paused) },
    ...(reconnect ? [{ id: "log-reconnect", label: "Reconnect to the runner log", run: () => setRound((n) => n + 1) }] : []),
    ...(streaming ? [{ id: "log-stop", label: "Stop following the runner log", run: () => latest.current.stopFollowing() }] : []),
    { id: "log-download", label: "Download the runner log", run: () => latest.current.download() },
  ], [active, paused, reconnect, streaming]);
  useScreenCommands(commands);

  const pending = buffer.pending.length;
  return (
    <section className="log-view" aria-label={t("Runner log")}>
      <p className="summary-line">{t("This is the runner container's own output: registration, job pickup and shutdown. Job step logs stay on GitHub.")}</p>
      <div className="toolbar log-toolbar">
        <Select size="sm" aria-label={t("Tail")} value={tail} onValueChange={setTail}
          options={tails.map((value) => ({ value, label: t(tailNames[value]) }))} />
        <Switch size="sm" label={t("Follow")} checked={follow && !sample} disabled={sample} onCheckedChange={setFollow} />
        <Switch size="sm" label={t("Wrap lines")} checked={wrap} onCheckedChange={setWrap} />
        <span className="log-menu">
          <Menu align="end" trigger={<Button variant="secondary" size="sm">{t("Actions")}<Menu.Chevron /></Button>}>
            <Menu.Item onSelect={() => pause(!paused)}>{t(paused ? "Resume" : "Pause")}</Menu.Item>
            {streaming && <Menu.Item onSelect={stopFollowing}>{t("Stop")}</Menu.Item>}
            {reconnect && <Menu.Item onSelect={() => setRound((n) => n + 1)}>{t("Reconnect")}</Menu.Item>}
            <Menu.Item onSelect={() => void copy()}>{t("Copy")}</Menu.Item>
            <Menu.Item onSelect={download}>{t("Download")}</Menu.Item>
            <Menu.Item onSelect={clear}>{t("Clear")}</Menu.Item>
          </Menu>
        </span>
      </div>
      <div className="toolbar log-toolbar log-actions">
        <Button variant="secondary" size="sm" aria-pressed={paused} onClick={() => pause(!paused)}>{t(paused ? "Resume" : "Pause")}</Button>
        {streaming && <Button variant="secondary" size="sm" onClick={stopFollowing}>{t("Stop")}</Button>}
        {reconnect && <Button variant="secondary" size="sm" onClick={() => setRound((n) => n + 1)}>{t("Reconnect")}</Button>}
        <Button variant="secondary" size="sm" onClick={() => void copy()}>{t("Copy")}</Button>
        <Button variant="secondary" size="sm" onClick={download}>{t("Download")}</Button>
        <Button variant="secondary" size="sm" onClick={clear}>{t("Clear")}</Button>
      </div>
      {sample && <p className="summary-line">{t("The sample panel shows fixed sample lines and cannot follow output.")}</p>}
      <div className="log-frame">
        {/* Filled outside React (flush); the reader scrolls it with the keyboard. */}
        <div ref={output} className={"log-output" + (wrap ? " wrap" : "")} tabIndex={0} role="region" aria-label={t("Runner log output")}
          onScroll={(event) => {
            const out = event.currentTarget;
            if (out.scrollHeight - out.scrollTop - out.clientHeight < 24) setJump(false);
          }} />
        {jump && (
          <Button className="log-jump" size="sm" onClick={() => {
            const out = output.current!;
            out.scrollTop = out.scrollHeight;
            setJump(false);
            out.focus();
          }}>{t("Jump to latest")}</Button>
        )}
      </div>
      {/* Counts change with every line, so they are not a live region; the status line is. */}
      <p className="summary-line log-meta">
        {t("{n} lines", { n: number(buffer.shown.length) })}
        {paused && pending > 0 && <> · {t("{n} new lines waiting", { n: number(pending) })}</>}
      </p>
      {buffer.dropped > 0 && (
        <p className="summary-line">{t("{n} older lines were dropped to keep the view responsive. Download keeps only the lines shown.", { n: number(buffer.dropped) })}</p>
      )}
      <p className="log-status" role="status" data-tone={status.tone || undefined}>{status.text}</p>
    </section>
  );
}
