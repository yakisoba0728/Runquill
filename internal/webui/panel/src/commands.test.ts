import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { useCurrentCommands, useScreenCommands, type ScreenCommand } from "./commands";

const list = (...ids: string[]): ScreenCommand[] => ids.map((id) => ({ id, label: id, run: () => {} }));

describe("screen commands", () => {
  it("shows a screen's and its pane's lists together and withdraws each on its own", () => {
    const current = renderHook(() => useCurrentCommands());
    const screen = renderHook(({ commands }) => useScreenCommands(commands), { initialProps: { commands: list("a", "b") } });
    const pane = renderHook(({ commands }) => useScreenCommands(commands), { initialProps: { commands: list("retry") } });
    expect(current.result.current.map((c) => c.id).sort()).toEqual(["a", "b", "retry"]);
    act(() => pane.rerender({ commands: list() }));
    expect(current.result.current.map((c) => c.id)).toEqual(["a", "b"]);
    act(() => pane.unmount());
    act(() => screen.unmount());
    expect(current.result.current).toEqual([]);
  });
});
