// @vitest-environment happy-dom
import { act, createElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";
import MoreMenu, { type MoreMenuItem } from "./MoreMenu";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let mounted: { root: Root; el: HTMLElement } | null = null;
afterEach(() => {
  if (mounted) {
    act(() => mounted!.root.unmount());
    mounted.el.remove();
    mounted = null;
  }
});

function mount(groups: MoreMenuItem[][], busy?: string) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  act(() => root.render(createElement(MoreMenu, { groups, busy })));
  mounted = { root, el };
  return el;
}

const button = (el: HTMLElement) => el.querySelector<HTMLButtonElement>('button[aria-haspopup="menu"]')!;
const items = (el: HTMLElement) => Array.from(el.querySelectorAll<HTMLButtonElement>('[role="menuitem"]'));
const key = (k: string) =>
  act(() => {
    (document.activeElement as HTMLElement).dispatchEvent(new KeyboardEvent("keydown", { key: k, bubbles: true }));
  });

describe("MoreMenu", () => {
  const run = { a: vi.fn(), b: vi.fn(), c: vi.fn(), del: vi.fn() };
  const groups: MoreMenuItem[][] = [
    [
      { key: "a", label: "first", run: run.a },
      { key: "b", label: "second", run: run.b, disabled: true },
    ],
    [],
    [{ key: "c", label: "third", run: run.c }],
    [{ key: "del", label: "delete", run: run.del, danger: true }],
  ];

  it("opens from the keyboard onto the first item, wraps the arrows and closes on Escape with the focus back", () => {
    const el = mount(groups);
    act(() => button(el).dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true })));
    expect(button(el).getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(items(el)[0]);
    key("ArrowUp");
    expect(document.activeElement).toBe(items(el).at(-1));
    key("ArrowDown");
    expect(document.activeElement).toBe(items(el)[0]);
    key("End");
    expect(document.activeElement).toBe(items(el).at(-1));
    key("Escape");
    expect(el.querySelector('[role="menu"]')).toBeNull();
    expect(document.activeElement).toBe(button(el));
  });

  it("separates the groups, drops empty ones and sets delete apart", () => {
    const el = mount(groups);
    act(() => button(el).click());
    expect(el.querySelectorAll('[role="separator"]').length).toBe(2);
    expect(items(el).find((b) => b.textContent === "delete")!.className).toBe("danger");
  });

  it("runs the picked item and closes, and leaves a disabled one inert", () => {
    const el = mount(groups);
    act(() => button(el).click());
    act(() => items(el)[1].click());
    expect(run.b).not.toHaveBeenCalled();
    expect(items(el)[1].getAttribute("aria-disabled")).toBe("true");
    act(() => items(el)[2].click());
    expect(run.c).toHaveBeenCalledTimes(1);
    expect(el.querySelector('[role="menu"]')).toBeNull();
  });

  it("closes on an outside click", () => {
    const el = mount(groups);
    act(() => button(el).click());
    act(() => {
      document.body.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    });
    expect(el.querySelector('[role="menu"]')).toBeNull();
  });

  it("shows a running action on the button and stays shut until it ends", () => {
    const el = mount(groups, "Preparing PDF");
    expect(button(el).disabled).toBe(true);
    expect(button(el).getAttribute("aria-busy")).toBe("true");
    expect(button(el).textContent).toBe("Preparing PDF…");
  });
});
