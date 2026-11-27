// Palette commands of the screen on show (v1 view.commands()): the screen registers them, the shell lists them.
import { useEffect, useSyncExternalStore } from "react";

export interface ScreenCommand {
  id: string;
  /** English i18n key. */
  label: string;
  values?: Record<string, string>;
  run: () => void;
}

let current: ScreenCommand[] = [];
const listeners = new Set<() => void>();
// A screen and its open detail pane may each register a list; the palette shows them together.
const lists = new Set<{ commands: ScreenCommand[] }>();

function update() {
  current = [...lists].flatMap((entry) => entry.commands);
  for (const fn of listeners) fn();
}

/** Pass a memoised list; it is withdrawn when the component goes. */
export function useScreenCommands(commands: ScreenCommand[]) {
  useEffect(() => {
    const entry = { commands };
    lists.add(entry);
    update();
    return () => {
      lists.delete(entry);
      update();
    };
  }, [commands]);
}

export const useCurrentCommands = () => useSyncExternalStore((fn) => {
  listeners.add(fn);
  return () => void listeners.delete(fn);
}, () => current);
