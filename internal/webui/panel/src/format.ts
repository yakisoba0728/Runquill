// Numbers, durations and relative times in the page language, ported from format in panelweb/ui.js.
// shortcut: only what phase 1 shows; bytes, dates and times arrive with the screens that use them.
import { useSyncExternalStore } from "react";
import { language, t } from "./i18n";

export const number = (value: number | null | undefined) =>
  typeof value === "number" && Number.isFinite(value) ? value.toLocaleString(language) : "—";

export function duration(seconds: number | null | undefined) {
  if (typeof seconds !== "number" || !Number.isFinite(seconds)) return "—";
  seconds = Math.max(0, Math.round(seconds));
  const minutes = Math.floor(seconds / 60);
  const hours = Math.floor(minutes / 60);
  if (hours) return minutes % 60 ? t("{hours}h {minutes}m", { hours, minutes: minutes % 60 }) : t("{hours}h", { hours });
  if (minutes) return t("{minutes}m {seconds}s", { minutes, seconds: seconds % 60 });
  return t("{seconds}s", { seconds });
}

export function relative(value: string | undefined | null, now = Date.now()) {
  const time = value ? Date.parse(value) : NaN;
  if (!Number.isFinite(time)) return t("Not observed");
  const seconds = Math.max(0, Math.floor((now - time) / 1000));
  if (seconds < 5) return t("Just now");
  if (seconds < 60) return t("{n}s ago", { n: seconds });
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return t("{n}m ago", { n: minutes });
  const hours = Math.floor(minutes / 60);
  return hours < 24 ? t("{n}h ago", { n: hours }) : t("{n}d ago", { n: Math.floor(hours / 24) });
}

// One shared one-second clock for text that ages ("3s ago"); only the components that read it re-render.
let now = Date.now();
const ticking = new Set<() => void>();
let clock: ReturnType<typeof setInterval> | undefined;

function subscribe(listener: () => void) {
  ticking.add(listener);
  if (!clock) now = Date.now();
  clock ??= setInterval(() => {
    now = Date.now();
    for (const fn of ticking) fn();
  }, 1000);
  return () => {
    ticking.delete(listener);
    if (!ticking.size) {
      clearInterval(clock);
      clock = undefined;
    }
  };
}

export const useNow = () => useSyncExternalStore(subscribe, () => now);
