// Hash routing (design §2.4, §5.4): #/screen/id/sub?query. Old v1 addresses are rewritten once with replaceState.
// shortcut: no leave guard yet; the settings screen (phase 4) adds useLeaveGuard here.
import { useSyncExternalStore } from "react";

export interface Route {
  screen: string;
  id?: string;
  sub?: string;
  params: URLSearchParams;
}

/** null for a hash that is not a route (an in-page anchor); an empty hash is the first screen. */
export function parse(hash: string): Route | null {
  if (hash === "" || hash === "#" || hash === "#/") return { screen: "", params: new URLSearchParams() };
  if (!hash.startsWith("#/")) return null;
  const at = hash.indexOf("?");
  const path = hash.slice(2, at < 0 ? undefined : at);
  let parts: string[];
  try {
    parts = path.split("/").map(decodeURIComponent);
  } catch {
    parts = [path.split("/")[0]];
  }
  return { screen: parts[0], id: parts[1] || undefined, sub: parts[2] || undefined, params: new URLSearchParams(at < 0 ? "" : hash.slice(at + 1)) };
}

const enc = encodeURIComponent;

/** The new address for a v1 address, or null. */
export function legacy(route: Route, admin: boolean): string | null {
  const { screen, id, params } = route;
  if (id) return null;
  const job = params.get("job");
  const runner = params.get("runner");
  const host = params.get("host");
  if (screen === "queue") return job ? "#/jobs/" + enc(job) : "#/jobs";
  if (screen === "jobs" && job) return "#/jobs/" + enc(job);
  if (screen === "runners" && runner) return "#/runners/" + enc(runner);
  if (screen === "logs") return runner ? "#/runners/" + enc(runner) + "/logs" : "#/runners";
  if (screen === "hosts" && host) return "#/hosts/" + enc(host);
  if (screen === "team" && !admin) return "#/tokens";
  return null;
}

/** Where a hash lands: the route to show, and the address to replace it with when that differs. */
export function resolve(hash: string, visible: readonly string[], admin: boolean): { route: Route; redirect?: string } | null {
  const parsed = parse(hash);
  if (!parsed) return null;
  const moved = legacy(parsed, admin);
  const route = moved ? parse(moved)! : parsed;
  if (visible.includes(route.screen)) return moved ? { route, redirect: moved } : { route };
  const first = "#/" + visible[0];
  return { route: parse(first)!, redirect: first };
}

// The store: hashchange, plus our own replaceState calls (which fire no event).
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((fn) => fn());
let focusPending = false;
let visits = 0;

function subscribe(fn: () => void) {
  listeners.add(fn);
  if (listeners.size === 1) addEventListener("hashchange", onHashChange);
  return () => {
    listeners.delete(fn);
    if (!listeners.size) removeEventListener("hashchange", onHashChange);
  };
}

function onHashChange() {
  focusPending = true;
  visits++;
  emit();
}

export const useHash = () => useSyncExternalStore(subscribe, () => location.hash);
/** Moves on every navigation, also when the same address is opened again. */
export const useVisit = () => useSyncExternalStore(subscribe, () => visits);

/** Opens a route; the same address again re-focuses it. replace changes the address without a history entry. */
export function navigate(hash: string, { replace = false } = {}) {
  if (replace) {
    history.replaceState(history.state, "", hash);
    emit();
  } else if (location.hash === hash) {
    onHashChange();
  } else {
    location.hash = hash;
  }
}

/** For the page heading: true once after a navigation, so the new screen takes focus (not on first load). */
export function takeFocus() {
  const pending = focusPending;
  focusPending = false;
  return pending;
}
