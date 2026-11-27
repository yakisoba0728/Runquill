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

function set(next: ScreenCommand[]) {
  current = next;
  for (const fn of listeners) fn();
}

/** Pass a memoised list; it is withdrawn when the screen goes. */
export function useScreenCommands(commands: ScreenCommand[]) {
  useEffect(() => {
    set(commands);
    return () => set([]);
  }, [commands]);
}

export const useCurrentCommands = () => useSyncExternalStore((fn) => {
  listeners.add(fn);
  return () => void listeners.delete(fn);
}, () => current);
