// G8: Stratum's built-in wording is Korean, and it has no global way to change it, so every use passes its labels.
// They are made here, once per page load (the language never changes without a reload).
import type { ReactNode } from "react";
import { fill, t } from ".";

export const stratum = () => ({
  shell: { drawerLabel: t("Menu"), menuLabel: t("Open menu") },
  nav: { "aria-label": t("Panel") },
  brand: { closeLabel: t("Close menu") },
  rail: { collapseLabel: t("Collapse sidebar"), expandLabel: t("Expand sidebar") },
  palette: {
    label: t("Commands"),
    placeholder: t("Type a screen, command, runner, job or host"),
    emptyLabel: t("No matching commands."),
    recentLabel: t("Recent"),
    statusLabel: (n: number) => (n === 0 ? t("No matching commands.") : n === 1 ? t("1 result") : t("{n} results", { n })),
    hints: t("↑ ↓ to move, Enter to open, Esc to close"),
  },
  toaster: { label: t("Notifications"), closeLabel: t("Close") },
  confirm: { cancelLabel: t("Cancel") },
  table: { empty: t("No entries.") },
  // Only the labels a pane without unsaved changes shows: no pane edits anything (settings edit on a page, with SaveBar).
  split: { close: t("Close"), back: t("Back to the list"), prev: t("Previous item"), next: t("Next item"), resize: t("Detail pane width") },
  search: { clearLabel: t("Clear search") },
  saveBar: {
    dirty: (n: ReactNode) => fill("Unsaved changes: {n}", { n }),
    guard: (n: ReactNode) => fill("Unsaved changes ({n}) kept you here. Save them, or discard them and leave.", { n }),
    saving: t("Saving…"),
    saved: t("Settings saved."),
    error: (n: ReactNode, error: unknown) => fill("Not saved: {error} Unsaved changes ({n}) are kept.", {
      n, error: error instanceof Error && error.message ? error.message : t("Request failed."),
    }),
    save: t("Save"),
    retry: t("Try again"),
    revert: t("Revert"),
    discard: t("Discard and leave"),
    region: t("Unsaved changes"),
  },
});
