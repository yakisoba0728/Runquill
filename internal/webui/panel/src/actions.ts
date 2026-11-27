// Operator and admin writes (design §2.1): the server decides, so a refusal (403 included) shows as a danger toast with
// its message. A 401 ends the session in api.ts and shows nothing here. Buttons show a pending state with Stratum's
// loading (they stay focusable), and a write already running ignores a second start (a click or the palette).
import { toast } from "@yakihyuk0728/stratum-ui";
import { useRef, useState } from "react";
import { errorText, request, type ApiError } from "./api";

/** POSTs and resolves to the answer (null for 204), or undefined after a failure it reported. */
export async function post<T = null>(path: string, data?: unknown, timeout?: number): Promise<T | null | undefined> {
  try {
    return await request<T>(path, { method: "POST", data, timeout });
  } catch (error) {
    if ((error as ApiError).status !== 401) toast(errorText(error), { tone: "danger" });
    return undefined;
  }
}

/** Which write (by key) is running, and run(key, task), which does nothing while another runs. */
export function useBusy<K extends string>() {
  const [busy, setBusy] = useState<K | null>(null);
  const running = useRef(false);
  const run = async (key: K, task: () => Promise<unknown>) => {
    if (running.current) return;
    running.current = true;
    setBusy(key);
    try {
      await task();
    } finally {
      running.current = false;
      setBusy(null);
    }
  };
  return [busy, run] as const;
}
