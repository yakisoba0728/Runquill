// Translations. Keys are the English sentences; ko.json maps them to Korean (converted once from the v1 panelweb/i18n.js).
// The server sends English sentences too, so serverText translates them with the same table: a known first line, or a
// template from server-patterns.json. Korean data is loaded only for Korean pages, before the first render.
import { createElement, Fragment, type ReactNode } from "react";

export type Language = "en" | "ko";
type Values = Record<string, string | number>;

export let language: Language = "en";
let words: Record<string, string> = {};
let patterns: { key: string; names: string[]; pattern: RegExp }[] = [];

/** ?lang=ko|en (remembered), else the saved choice, else the browser language. */
export function preferredLanguage(href = location.href): Language {
  const requested = new URL(href).searchParams.get("lang");
  const valid = (value: string | null): value is Language => value === "ko" || value === "en";
  try {
    if (valid(requested)) localStorage.setItem("runquill-language", requested);
    const saved = localStorage.getItem("runquill-language");
    if (valid(saved)) return saved;
    return navigator.language.startsWith("ko") ? "ko" : "en";
  } catch {
    return valid(requested) ? requested : "en";
  }
}

export function compilePattern(key: string) {
  const names: string[] = [];
  const source = key.split(/(\{[a-z]+\})/).map((part) => {
    const token = /^\{([a-z]+)\}$/.exec(part);
    if (!token) return part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    names.push(token[1]);
    return "(.+?)";
  }).join("");
  return { key, names, pattern: new RegExp("^" + source + "$", "s") };
}

/** Sets the page language; Korean loads its catalogue first. Call once before rendering. */
export async function setLanguage(next: Language) {
  language = next;
  document.documentElement.lang = next;
  if (next !== "ko") return;
  const [catalogue, templates] = await Promise.all([import("./ko.json"), import("./server-patterns.json")]);
  words = catalogue.default;
  patterns = templates.default.map(compilePattern);
}

/** For tests: install a catalogue without the dynamic import. */
export function setCatalogue(next: Language, catalogue: Record<string, string>, templates: string[] = []) {
  language = next;
  words = catalogue;
  patterns = templates.map(compilePattern);
}

export function t(message: string, values: Values = {}): string {
  const text = Object.hasOwn(words, message) ? words[message] : message;
  return text.replace(/\{([a-z]+)\}/g, (token, key) => (Object.hasOwn(values, key) ? String(values[key]) : token));
}

/** t() for sentences with elements in them: fill('Run {command}', { command: <code>…</code> }). */
export function fill(message: string, values: Record<string, ReactNode>): ReactNode {
  const parts = t(message).split(/(\{[a-z]+\})/).map((part) => {
    const token = /^\{([a-z]+)\}$/.exec(part);
    return token && Object.hasOwn(values, token[1]) ? values[token[1]] : part;
  });
  return createElement(Fragment, null, ...parts);
}

/** Translates a server message: a known first line (details after it kept as sent), a known template, or as sent. */
export function serverText(message: unknown): string {
  const text = String(message ?? "").trim();
  const [head, ...detail] = text.split("\n");
  if (Object.hasOwn(words, head)) return [t(head), ...detail].join("\n");
  if (language !== "ko") return text;
  for (const { key, names, pattern } of patterns) {
    const match = pattern.exec(text);
    if (!match) continue;
    return t(key, Object.fromEntries(names.map((name, i) => [name, name === "detail" ? serverText(match[i + 1]) : match[i + 1]])));
  }
  return text;
}
