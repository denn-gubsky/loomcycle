// @vitest-environment happy-dom
import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import { describe, expect, it, vi } from "vitest";
import CreateMenu from "./CreateMenu";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

describe("CreateMenu", () => {
  it("TestCreateMenu_OffersFolderAndDocumentUnderOneButton", () => {
    const onNewFolder = vi.fn();
    const onNewDocument = vi.fn();
    const el = document.createElement("div");
    document.body.appendChild(el);
    const root = createRoot(el);
    act(() => root.render(createElement(CreateMenu, { dir: "/rfcs", onNewFolder, onNewDocument })));

    const button = el.querySelector<HTMLButtonElement>('button[aria-haspopup="menu"]')!;
    expect(button.title).toBe("New folder or document in /rfcs");
    expect(el.querySelector('[role="menu"]')).toBeNull();
    act(() => button.click());
    const items = Array.from(el.querySelectorAll<HTMLButtonElement>('[role="menuitem"]'));
    expect(items.map((b) => b.textContent)).toEqual(["folder", "document"]);
    act(() => items[1].click());
    expect(onNewDocument).toHaveBeenCalledTimes(1);
    expect(onNewFolder).not.toHaveBeenCalled();
    expect(el.querySelector('[role="menu"]')).toBeNull();

    act(() => root.unmount());
    el.remove();
  });
});
