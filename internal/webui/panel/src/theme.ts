// Appearance: system, light or dark on <html data-theme>, saved as runquill-theme. public/theme.js applies it before
// the first paint; Stratum reads data-theme="light"|"dark" and follows the OS otherwise.
import { useSyncExternalStore } from "react";

export type Theme = "system" | "light" | "dark";
const listeners = new Set<() => void>();

const current = (): Theme => {
  const value = document.documentElement.dataset.theme;
  return value === "light" || value === "dark" ? value : "system";
};

export function setTheme(value: Theme) {
  document.documentElement.dataset.theme = value;
  try {
    localStorage.setItem("runquill-theme", value);
  } catch {}
  listeners.forEach((fn) => fn());
}

export const useTheme = () => useSyncExternalStore((fn) => {
  listeners.add(fn);
  return () => void listeners.delete(fn);
}, current);

/** The same page in the other language (?lang=), as a full reload. */
export function languageURL(to: "en" | "ko") {
  const target = new URL(location.href);
  target.searchParams.set("lang", to);
  return target.href;
}
