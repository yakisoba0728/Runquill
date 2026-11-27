// REST for screens (design §5.3): one hook that loads a path, cancels the previous load when the path changes or the
// screen goes, and can poll. A 401 ends the session elsewhere (api.ts), so it is not shown here.
import { useEffect, useState } from "react";
import { isSample, request, type ApiError } from "./api";

export interface Loaded<T> {
  data?: T;
  error?: ApiError;
  loading: boolean;
  reload(): void;
}

interface Options<T> {
  /** Milliseconds until the next load after one ends; omit for no polling. Never polls in the sample panel. */
  poll?: (data?: T, error?: ApiError) => number;
  timeout?: number;
}

/** null path: nothing to load. Data stays while the same path reloads, and is dropped when the path changes. */
export function useRequest<T>(path: string | null, { poll, timeout }: Options<T> = {}): Loaded<T> {
  const [state, setState] = useState<{ path: string | null; data?: T; error?: ApiError; loading: boolean }>({ path, loading: !!path });
  const [round, setRound] = useState(0);
  const [visible, setVisible] = useState(!document.hidden);

  useEffect(() => {
    if (!poll) return;
    const onVisibility = () => setVisible(!document.hidden);
    document.addEventListener("visibilitychange", onVisibility);
    return () => document.removeEventListener("visibilitychange", onVisibility);
  }, [!!poll]);

  useEffect(() => {
    if (!path) return setState({ path, loading: false });
    // A hidden tab pauses polling; becoming visible loads again.
    if (poll && !visible && round > 0) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    setState((last) => (last.path === path ? { ...last, loading: true } : { path, loading: true }));
    request<T>(path, { signal: controller.signal, timeout }).then(
      (data) => {
        setState({ path, data, loading: false });
        if (poll && !isSample()) timer = setTimeout(() => setRound((n) => n + 1), poll(data));
      },
      (error: ApiError) => {
        if (controller.signal.aborted || error.status === 401) return;
        setState((last) => ({ path, data: last.path === path ? last.data : undefined, error, loading: false }));
        if (poll && !isSample()) timer = setTimeout(() => setRound((n) => n + 1), poll(undefined, error));
      },
    );
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [path, round, visible]);

  const current = state.path === path;
  return {
    data: current ? state.data : undefined,
    error: current ? state.error : undefined,
    loading: current ? state.loading : !!path,
    reload: () => setRound((n) => n + 1),
  };
}
