// Numbers, durations and relative times in the page language, ported from format in panelweb/ui.js.
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

/** Milliseconds below a second as "450ms", longer as a duration. */
export const millis = (value: number | null | undefined) =>
  typeof value !== "number" || !Number.isFinite(value) ? "—" : value < 1000 ? t("{n}ms", { n: value }) : duration(value / 1000);

export function bytes(value: number | null | undefined) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return value.toLocaleString(language, { maximumFractionDigits: unit ? 1 : 0 }) + " " + units[unit];
}

export const mib = (value: number | null | undefined) => bytes(typeof value === "number" ? value * 1048576 : value);
export const cpus = (value: number) => t("{n} CPUs", { n: number(Math.round(value * 100) / 100) });
export const percent = (value: number, digits = 0) => value.toLocaleString(language, { style: "percent", maximumFractionDigits: digits });

const toDate = (value: string | null | undefined) => {
  const date = value ? new Date(value) : null;
  return date && !Number.isNaN(date.getTime()) ? date : null;
};

/** Local wall-clock time with seconds. */
export const timeOfDay = (value: string | null | undefined) =>
  toDate(value)?.toLocaleTimeString(language, { hour: "2-digit", minute: "2-digit", second: "2-digit" }) ?? "—";

/** Local month, day and time. */
export const date = (value: string | null | undefined) =>
  toDate(value)?.toLocaleString(language, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }) ?? "—";

/** A UTC day key (YYYY-MM-DD), the usage API unit, as a short or (long) a full date. */
export function day(key: string, long = false) {
  const value = new Date(key + "T00:00:00Z");
  if (Number.isNaN(value.getTime())) return key;
  return value.toLocaleDateString(language, long
    ? { timeZone: "UTC", year: "numeric", month: "short", day: "numeric" }
    : { timeZone: "UTC", month: "short", day: "numeric", weekday: "short" });
}

/** Today's UTC day key, moved by offset days. */
export function utcDay(offset = 0, now = Date.now()) {
  return new Date(now + offset * 86400000).toISOString().slice(0, 10);
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
