import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { onUnauthorized } from "./api";
import { useRequest } from "./request";

afterEach(() => {
  vi.unstubAllGlobals();
  onUnauthorized(() => {});
});

const server = (answer: (path: string) => [number, string]) =>
  vi.stubGlobal("fetch", vi.fn(async (path: string) => {
    const [status, body] = answer(path);
    return new Response(body, { status });
  }));

describe("useRequest", () => {
  it("loads, keeps data while the same path reloads, and drops it when the path changes", async () => {
    let n = 0;
    server((path) => [200, JSON.stringify({ path, n: ++n })]);
    const { result, rerender } = renderHook(({ path }) => useRequest<{ path: string; n: number }>(path), { initialProps: { path: "./api/a" } });
    expect(result.current.loading).toBe(true);
    await waitFor(() => expect(result.current.data).toEqual({ path: "./api/a", n: 1 }));
    act(() => result.current.reload());
    expect(result.current.data?.n).toBe(1);
    await waitFor(() => expect(result.current.data?.n).toBe(2));
    rerender({ path: "./api/b" });
    expect(result.current.data).toBeUndefined();
    await waitFor(() => expect(result.current.data).toEqual({ path: "./api/b", n: 3 }));
  });

  it("reports errors with their status, but leaves a 401 to the sign-in screen", async () => {
    server((path) => (path.endsWith("/missing") ? [404, "Job not found."] : [401, "Sign in."]));
    const signedOut = vi.fn();
    onUnauthorized(signedOut);
    const missing = renderHook(() => useRequest("./api/jobs/missing"));
    await waitFor(() => expect(missing.result.current.error?.status).toBe(404));
    const unauthorized = renderHook(() => useRequest("./api/jobs/1"));
    await waitFor(() => expect(signedOut).toHaveBeenCalled());
    expect(unauthorized.result.current.error).toBeUndefined();
  });

  it("loads nothing for a null path", () => {
    const network = vi.fn();
    vi.stubGlobal("fetch", network);
    const { result } = renderHook(() => useRequest(null));
    expect(result.current.loading).toBe(false);
    expect(network).not.toHaveBeenCalled();
  });
});
