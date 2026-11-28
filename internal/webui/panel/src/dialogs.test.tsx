import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { confirm, ConfirmHost } from "./dialogs";

afterEach(cleanup);

// jsdom has no modal <dialog> or matchMedia.
HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
  this.setAttribute("open", "");
};
HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
  this.removeAttribute("open");
  this.dispatchEvent(new Event("close"));
};
window.matchMedia ??= ((query: string) => ({ matches: false, media: query, addEventListener() {}, removeEventListener() {} })) as never;

describe("confirm", () => {
  it("resolves true on confirm and false on cancel", async () => {
    render(<ConfirmHost />);
    let answer = confirm({ title: "Cancel job #1?", confirmLabel: "Cancel job", danger: true });
    fireEvent.click(await screen.findByRole("button", { name: "Cancel job" }));
    await expect(answer).resolves.toBe(true);

    answer = confirm({ title: "Retry job #1?", confirmLabel: "Retry now" });
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    await expect(answer).resolves.toBe(false);
  });

  it("answers false when no host is mounted", async () => {
    await act(async () => {
      await expect(confirm({ title: "x", confirmLabel: "y" })).resolves.toBe(false);
    });
  });
});
