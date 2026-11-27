// The signed-in frame (design §2.2, §4.2): nav with counts, live status, user menu, command palette, notices,
// the sample-panel band and the current screen.
import {
  AppShell, Badge, CommandPalette, IconButton, Menu, Nav, Select, Toaster, toast, type CommandItem,
} from "@yakihyuk0728/stratum-ui";
import { Suspense, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { request, type ApiError } from "./api";
import { attention } from "./attention";
import { useCurrentCommands } from "./commands";
import { Notice, ScreenBoundary, UpdatedAgo } from "./components";
import type { Sample, Scenario } from "./demo";
import { ConfirmHost } from "./dialogs";
import { pendingJob, summary } from "./fleet";
import { useNow } from "./format";
import { fill, language, serverText, t } from "./i18n";
import { stratum } from "./i18n/stratum";
import { live, useLive, type Connection } from "./live";
import { navigate, parse, resolve, useHash, type Route } from "./router";
import { hasRole, visibleScreens } from "./routes";
import { languageURL, setTheme, useTheme, type Theme } from "./theme";
import type { User } from "./types";

const status: Record<Connection, [string, "neutral" | "success" | "warning" | "danger"]> = {
  connecting: ["Connecting", "neutral"],
  live: ["Live", "success"],
  polling: ["Polling", "success"],
  reconnecting: ["Reconnecting", "warning"],
  lost: ["Connection lost", "danger"],
  hidden: ["Paused while hidden", "neutral"],
  sample: ["Sample data", "neutral"],
};

const SearchIcon = () => (
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <circle cx="11" cy="11" r="8" /><path d="m21 21-4.3-4.3" />
  </svg>
);

const mac = /mac|iphone|ipad/i.test(navigator.platform || navigator.userAgent);

function Updated() {
  const at = useLive((s) => s.snapshot?.sampled_at);
  return at ? <span className="live-age"><UpdatedAgo at={at} /></span> : null;
}

/** G4: connection state (announced only when it changes) and the snapshot's age (not announced). */
function LiveStatus() {
  const [label, tone] = status[useLive((s) => s.connection)];
  return (
    <div className="live-status">
      <Badge tone={tone} dot><span role="status">{t(label)}</span></Badge>
      <Updated />
    </div>
  );
}

/** The phone bar's copy: a dot with a name, no live region (the footer's status already announces). */
function LiveDot() {
  const [label, tone] = status[useLive((s) => s.connection)];
  return <span className="live-dot" data-tone={tone} title={t(label)}><span className="sr-only">{t(label)}</span></span>;
}

function Countdown({ at }: { at: number }) {
  const now = useNow();
  return <span className="tabular-nums">{Math.max(0, Math.ceil((at - now) / 1000))}</span>;
}

/** Above the page: lost connection (not a live region: its countdown would be read every second), server problems. */
function Notices() {
  const connection = useLive((s) => s.connection);
  const retryAt = useLive((s) => s.retryAt);
  const hasSnapshot = useLive((s) => !!s.snapshot);
  const dockerError = useLive((s) => s.snapshot?.fleet?.docker_error);
  const pollFailed = useLive((s) => !!s.snapshot?.poll_failed);
  if (connection !== "lost" && !dockerError && !pollFailed) return null;
  return (
    <div className="notices">
      {connection === "lost" && (
        <Notice tone="danger">{fill(hasSnapshot ? "Connection lost. Retrying in {n}s. Showing the last available snapshot."
          : "Connection lost. Retrying in {n}s.", { n: <Countdown at={retryAt} /> })}</Notice>
      )}
      {dockerError && <Notice role="status">{serverText(dockerError)}</Notice>}
      {pollFailed && <Notice role="status">{t("The latest GitHub poll failed. Check the manager logs.")}</Notice>}
    </div>
  );
}

function DemoBand({ sample }: { sample: Sample }) {
  // main.tsx puts the first scenario's snapshot in before the first render.
  const [scenario, setScenario] = useState<Scenario>("first");
  const note = useMemo(() => sample(scenario).note, [sample, scenario]);
  return (
    <div className="demo-band">
      <Select
        size="sm"
        aria-label={t("Scenario")}
        value={scenario}
        onValueChange={(value) => {
          const next = sample(value as Scenario);
          setScenario(value as Scenario);
          live.sample(next.snapshot);
          navigate(next.focus);
        }}
        options={[{ value: "first", label: t("First job") }, { value: "missed", label: t("Webhook missed") }, { value: "offline", label: t("Worker offline") }]}
      />
      <span className="text-sm text-text-hint">{t(note)}</span>
      <a className="text-sm" href="https://github.com/yakisoba0728/Runquill#install-a-public-release" target="_blank" rel="noopener noreferrer">{t("Install Runquill")}</a>
    </div>
  );
}

async function signOut() {
  try {
    await request("./api/logout", { method: "POST" });
    location.assign("./");
  } catch (error) {
    if ((error as ApiError).status !== 401) toast((error as ApiError).message, { tone: "danger" });
  }
}

const roleNames = { viewer: "Viewer", operator: "Operator", admin: "Admin" } as const;

export function Shell({ user, sample }: { user: User | null; sample?: Sample }) {
  const demo = !!sample;
  const labels = useMemo(stratum, []);
  const visible = useMemo(() => visibleScreens(user, demo), [user, demo]);
  const ids = useMemo(() => visible.map((screen) => screen.id), [visible]);
  const hash = useHash();
  const resolved = useMemo(() => resolve(hash, ids, hasRole(user, "admin")), [hash, ids, user]);
  // An in-page anchor (resolved null) keeps the screen that was showing.
  const last = useRef<Route>(parse("#/" + ids[0])!);
  if (resolved) last.current = resolved.route;
  const route = last.current;
  const screen = visible.find((candidate) => candidate.id === route.screen) ?? visible[0];
  useLayoutEffect(() => {
    if (resolved?.redirect) navigate(resolved.redirect, { replace: true });
  }, [resolved]);

  const snapshot = useLive((s) => s.snapshot);
  const counts = useMemo(() => summary(snapshot), [snapshot]);
  const alerts = useMemo(() => attention(snapshot).length, [snapshot]);
  const down = counts ? counts.hostsOffline + counts.hostsUnavailable : 0;
  const count = (n: number | undefined, label: string, tone?: "warning" | "danger") => n ? (
    <><Badge tone={tone} count>{n}</Badge><span className="sr-only"> {t(label)}</span></>
  ) : null;
  const badges: Record<string, ReactNode> = {
    overview: count(alerts, "need attention", "warning"),
    jobs: count(counts?.queued, "queued"),
    runners: count(counts?.running, "running"),
    hosts: count(down, "offline", "danger"),
  };

  const theme = useTheme();
  const other = language === "ko" ? "en" : "ko";
  const [palette, setPalette] = useState(false);
  useEffect(() => {
    // "/" outside text fields opens the palette too (⌘K / Ctrl+K is the palette's own).
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== "/" || event.defaultPrevented || event.isComposing || event.metaKey || event.ctrlKey || event.altKey) return;
      const target = event.target;
      if (target instanceof Element && target.closest("input, textarea, select, [contenteditable], dialog, [role=listbox], [role=menu]")) return;
      event.preventDefault();
      setPalette(true);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  const screenCommands = useCurrentCommands();
  const items = useMemo(() => {
    // Each entry also matches its English wording, so "hosts" finds 호스트 in the Korean panel.
    const english = (text: string, values: Record<string, string> = {}) => text.replace(/\{([a-z]+)\}/g, (token, key) => values[key] ?? token);
    // The screen's own commands first, under its name.
    const list: CommandItem[] = screenCommands.map((c) => ({
      id: "screen-command:" + c.id, group: t(screen.title), label: t(c.label, c.values), keywords: [english(c.label, c.values)], onSelect: c.run,
    }));
    list.push(...visible.map((s) => ({
      id: "screen:" + s.id, group: t("Screen"), label: t("Go to {screen}", { screen: t(s.title) }),
      keywords: [english("Go to {screen}", { screen: s.title })], onSelect: () => navigate("#/" + s.id),
    })));
    const command = (id: string, group: string, label: string, onSelect: () => void) =>
      list.push({ id, group: t(group), label: t(label), keywords: [label], onSelect });
    command("language", "Language", other === "en" ? "Switch to English" : "Switch to Korean", () => location.assign(languageURL(other)));
    command("theme:system", "Appearance", "Use the system theme", () => setTheme("system"));
    command("theme:light", "Appearance", "Use the light theme", () => setTheme("light"));
    command("theme:dark", "Appearance", "Use the dark theme", () => setTheme("dark"));
    if (!demo) {
      command("refresh", "Panel", "Refresh now", () => void live.refresh());
      command("signout", "Panel", "Sign out", () => void signOut());
    }
    // shortcut: Stratum's palette lists every item on an empty search and has no limit (G7), so snapshot items go
    // last and stop at 50; add searchOnly/limit when Stratum has them.
    const jumps: CommandItem[] = [
      ...(snapshot?.runner_inventory ?? []).filter((r) => r.runtime_state === "running")
        .map((r) => ({ id: "runner:" + r.name, group: t("Runner"), label: r.name, onSelect: () => navigate("#/runners/" + encodeURIComponent(r.name)) })),
      ...(snapshot?.fleet?.jobs ?? []).filter(pendingJob)
        .map((j) => ({ id: "job:" + j.id, group: t("Waiting job"), label: j.repository + " #" + j.id, onSelect: () => navigate("#/jobs/" + j.id) })),
      ...(snapshot?.fleet?.hosts ?? [])
        .map((h) => ({ id: "host:" + h.name, group: t("Host"), label: h.name, onSelect: () => navigate("#/hosts/" + encodeURIComponent(h.name)) })),
    ];
    return [...list, ...jumps.slice(0, 50)];
  }, [visible, snapshot, demo, other, screenCommands, screen]);

  const groups = (["Operations", "Administration"] as const).map((group) => [group, visible.filter((s) => s.group === group)] as const)
    .filter(([, list]) => list.length);

  const sidebar = (
    <Nav {...labels.nav}>
      <Nav.Brand href="#/overview" {...labels.brand}>runquill</Nav.Brand>
      <Nav.List>
        {groups.map(([group, list]) => (
          <Nav.Section key={group} label={t(group)} defaultOpen>
            {list.map((s) => (
              <Nav.Item key={s.id} label={t(s.title)} href={"#/" + s.id} current={s.id === screen.id} badge={badges[s.id]} />
            ))}
          </Nav.Section>
        ))}
      </Nav.List>
      <Nav.Footer>
        <button type="button" className="nav-item" onClick={() => setPalette(true)} aria-haspopup="dialog" aria-keyshortcuts={mac ? "Meta+K /" : "Control+K /"}>
          <SearchIcon />
          <span className="nav-label">{t("Commands")}</span>
          <kbd className="nav-kbd">{mac ? "⌘K" : "Ctrl K"}</kbd>
        </button>
        <LiveStatus />
        <Menu
          align="start"
          trigger={(
            <button type="button" className="nav-item">
              <span className="nav-label">{user ? <><strong>@{user.login}</strong> · {t(roleNames[user.role])}</> : t("Preferences")}</span>
              <Menu.Chevron />
            </button>
          )}
        >
          {user && <Menu.Item onSelect={() => navigate("#/tokens")}>{t("API tokens")}</Menu.Item>}
          {user && <Menu.Separator />}
          <Menu.Item lang={other} onSelect={() => location.assign(languageURL(other))}>{other === "en" ? "English" : "한국어"}</Menu.Item>
          <Menu.Separator />
          <Menu.Label>{t("Appearance")}</Menu.Label>
          <Menu.RadioGroup value={theme} onValueChange={(value) => setTheme(value as Theme)}>
            <Menu.RadioItem value="system">{t("System")}</Menu.RadioItem>
            <Menu.RadioItem value="light">{t("Light")}</Menu.RadioItem>
            <Menu.RadioItem value="dark">{t("Dark")}</Menu.RadioItem>
          </Menu.RadioGroup>
          {user && <Menu.Separator />}
          {user && <Menu.Item onSelect={() => void signOut()}>{t("Sign out")}</Menu.Item>}
        </Menu>
        <a className="nav-item" href="https://github.com/yakisoba0728/Runquill" target="_blank" rel="noopener noreferrer">
          <span className="nav-label">{t("Source on GitHub")}</span>
        </a>
      </Nav.Footer>
    </Nav>
  );

  const Content = screen.component;
  return (
    <>
      <a className="skip-link" href="#main" onClick={(event) => {
        event.preventDefault();
        const heading = document.querySelector<HTMLElement>(".shell-main .page-title");
        if (heading) {
          heading.tabIndex = -1;
          heading.focus();
        }
      }}>{t("Skip to content")}</a>
      <AppShell
        sidebar={sidebar}
        rail={false} // shortcut: no nav icons yet, so no icon rail; add icons and Nav.RailToggle together
        {...labels.shell}
        barActions={<><IconButton label={t("Commands")} icon={<SearchIcon />} onClick={() => setPalette(true)} /><LiveDot /></>}
      >
        {sample && <DemoBand sample={sample} />}
        <Notices />
        <ScreenBoundary key={screen.id}>
          <Suspense fallback={null}><Content route={route} /></Suspense>
        </ScreenBoundary>
      </AppShell>
      <CommandPalette items={items} open={palette} onOpenChange={setPalette} {...labels.palette} />
      <Toaster {...labels.toaster} />
      <ConfirmHost />
    </>
  );
}
