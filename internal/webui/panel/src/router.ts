// Hash routing (design §2.4, §5.4): #/screen/id/sub?query. Old v1 addresses are rewritten once with replaceState.
// A screen with unsaved changes can hold a leave guard: a move to another screen is undone and the screen is told.
import { useEffect, useRef, useSyncExternalStore } from "react";

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

/** The address of a route: #/screen/id/sub?params, empty params left out. */
export function format(route: Route): string {
  const path = [route.screen, route.id, route.sub].filter((part) => part !== undefined && part !== "").map((part) => enc(part!));
  const params = new URLSearchParams([...route.params].filter(([, value]) => value !== ""));
  const query = params.toString();
  return "#/" + path.join("/") + (query ? "?" + query : "");
}

/** route with params changed: a null or "" value removes the param. */
export function withParams(route: Route, patch: Record<string, string | null | undefined>): Route {
  const params = new URLSearchParams(route.params);
  for (const [key, value] of Object.entries(patch)) {
    if (value == null || value === "") params.delete(key);
    else params.set(key, value);
  }
  return { ...route, params };
}

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

// The store: hashchange, plus our own replaceState calls (which fire no event). The hash shown is the accepted one, not
// location.hash: setting location.hash changes it at once while hashchange (and the leave guard) comes a task later.
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((fn) => fn());
let focusPending = false;
let visits = 0;
let accepted = location.hash;
let shown = parse(accepted)?.screen;
let guard: ((retry: () => void) => void) | null = null;
let bypass = false;

function subscribe(fn: () => void) {
  listeners.add(fn);
  if (listeners.size === 1) addEventListener("hashchange", onHashChange);
  return () => {
    listeners.delete(fn);
    if (!listeners.size) removeEventListener("hashchange", onHashChange);
  };
}

/** True when a move from one address to another changes the screen (opening a detail or a filter does not). */
export function changesScreen(from: string | undefined, to: string) {
  const next = parse(to)?.screen;
  return next !== undefined && next !== from;
}

// The page heading takes focus only when the screen changes, so opening or closing a SplitView detail keeps the
// focus the SplitView gives it.
function onHashChange() {
  const target = location.hash;
  if (guard && !bypass && changesScreen(shown, target)) {
    // Blocked (a link, the palette or Back): put the address back and let the screen offer a way on.
    history.replaceState(history.state, "", accepted || location.pathname + location.search);
    guard(() => {
      bypass = true;
      location.hash = target;
    });
    return;
  }
  bypass = false;
  if (changesScreen(shown, target)) focusPending = true;
  accepted = target;
  shown = parse(target)?.screen ?? shown;
  visits++;
  emit();
}

/**
 * While blocked, leaving for another screen is undone and onBlocked gets retry(), which leaves anyway (call it after
 * dropping the changes). The design's SaveBar guard (§3.6): no confirm dialog.
 */
export function useLeaveGuard(blocked: boolean, onBlocked: (retry: () => void) => void) {
  const latest = useRef(onBlocked);
  latest.current = onBlocked;
  useEffect(() => {
    if (!blocked) return;
    const entry = (retry: () => void) => latest.current(retry);
    guard = entry;
    return () => {
      if (guard === entry) guard = null;
    };
  }, [blocked]);
}

export const useHash = () => useSyncExternalStore(subscribe, () => accepted);
/** Moves on every navigation, also when the same address is opened again. */
export const useVisit = () => useSyncExternalStore(subscribe, () => visits);

/** Opens a route; the same address again re-focuses it. replace changes the address without a history entry. */
export function navigate(hash: string, { replace = false } = {}) {
  if (replace) {
    history.replaceState(history.state, "", hash);
    accepted = hash;
    shown = parse(hash)?.screen ?? shown;
    emit();
  } else if (accepted === hash) {
    focusPending = true;
    visits++;
    emit();
  } else {
    location.hash = hash;
  }
}

/** Changes the current address's params without a history entry (filters, tabs). */
export function setParams(patch: Record<string, string | null | undefined>) {
  const route = parse(accepted);
  if (route) navigate(format(withParams(route, patch)), { replace: true });
}

/** For the page heading: true once after a navigation, so the new screen takes focus (not on first load). */
export function takeFocus() {
  const pending = focusPending;
  focusPending = false;
  return pending;
}
