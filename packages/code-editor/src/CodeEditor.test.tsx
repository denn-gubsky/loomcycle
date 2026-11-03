// @vitest-environment jsdom
import { act, createElement, createRef } from "react";
import { createRoot, type Root } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { EditorView } from "@codemirror/view";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { CodeEditor, type CodeEditorHandle, type CodeEditorProps } from "./CodeEditor";
import { StructuredEditor } from "./StructuredEditor";

// CodeMirror measures text through Range geometry, which jsdom does not
// implement. Empty geometry is enough for it to mount and edit; nothing here
// asserts on layout.
beforeAll(() => {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  const empty = () => ({ length: 0, item: () => null, [Symbol.iterator]: [][Symbol.iterator] }) as unknown as DOMRectList;
  Range.prototype.getClientRects ??= empty;
  Range.prototype.getBoundingClientRect ??= () => new DOMRect(0, 0, 0, 0);
});

let root: Root | null = null;
let host: HTMLElement | null = null;
afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

async function mount(props: CodeEditorProps) {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => root!.render(createElement(CodeEditor, props)));
  await vi.waitFor(() => expect(host!.querySelector(".cm-content")).not.toBeNull());
  return EditorView.findFromDOM(host!.querySelector(".cm-editor") as HTMLElement)!;
}

describe("CodeEditor", () => {
  it("renders a plain textarea before CodeMirror loads (server render)", () => {
    const html = renderToStaticMarkup(createElement(CodeEditor, { value: "a: 1", onChange: () => {}, ariaLabel: "Team" }));
    expect(html).toContain("<textarea");
    expect(html).toContain('aria-label="Team"');
    expect(html).toContain("a: 1");
  });

  it("reports the operator's edit", async () => {
    const onChange = vi.fn();
    const view = await mount({ value: "{}", onChange, language: "json" });
    act(() => view.dispatch({ changes: { from: 1, insert: '"a": 1' } }));
    expect(onChange).toHaveBeenCalledWith('{"a": 1}');
  });

  it("takes a new value from outside without echoing it as an edit", async () => {
    const onChange = vi.fn();
    const props: CodeEditorProps = { value: "one", onChange };
    const view = await mount(props);
    await act(async () => root!.render(createElement(CodeEditor, { ...props, value: "two" })));
    expect(view.state.doc.toString()).toBe("two");
    expect(onChange).not.toHaveBeenCalled();
  });

  it("wraps by default and unwraps on request", async () => {
    const props: CodeEditorProps = { value: "x", onChange: () => {} };
    await mount(props);
    expect(host!.querySelector(".cm-lineWrapping")).not.toBeNull();
    await act(async () => root!.render(createElement(CodeEditor, { ...props, wrap: false })));
    expect(host!.querySelector(".cm-lineWrapping")).toBeNull();
  });

  it("indents a wrapped line under its own indentation", async () => {
    await mount({ value: "a:\n    deep: x\nb: y", onChange: () => {}, language: "yaml" });
    const lines = [...host!.querySelectorAll(".cm-line")] as HTMLElement[];
    expect(lines[1].style.textIndent).toBe("-4ch");
    expect(lines[0].style.textIndent).toBe("");
  });

  it("is not editable when read-only", async () => {
    await mount({ value: "x", onChange: () => {}, readOnly: true });
    expect(host!.querySelector(".cm-content")!.getAttribute("contenteditable")).toBe("false");
  });

  it("colours syntax with class names a host can theme", async () => {
    await mount({ value: '{"key": "v", "n": 1}', onChange: () => {}, language: "json" });
    await vi.waitFor(() => expect(host!.querySelector(".tok-propertyName")).not.toBeNull());
    expect(host!.querySelector(".tok-string")).not.toBeNull();
    expect(host!.querySelector(".tok-number")).not.toBeNull();
  });

  it("colours YAML values, not only keys", async () => {
    await mount({ value: "name: parse\nprompt: |-\n  line one\n", onChange: () => {}, language: "yaml" });
    await vi.waitFor(() => expect(host!.querySelector(".tok-propertyName")).not.toBeNull());
    const values = [...host!.querySelectorAll(".tok-string")].map((e) => e.textContent?.trim());
    expect(values).toEqual(expect.arrayContaining(["parse", "line one"]));
  });

  it("marks a diagnostic in the text", async () => {
    await mount({ value: "abc def", onChange: () => {}, diagnostics: [{ from: 4, to: 7, severity: "error", message: "bad" }] });
    await vi.waitFor(() => expect(host!.querySelector(".cm-lintRange-error")).not.toBeNull());
  });

  it("goto places the cursor without reporting it as the operator's move", async () => {
    const onCursor = vi.fn();
    const handle = createRef<CodeEditorHandle>();
    await mount({ value: "line one\nline two", onChange: () => {}, onCursor, handle });
    act(() => handle.current!.goto(9));
    expect(handle.current!.cursor()).toBe(9);
    expect(onCursor).not.toHaveBeenCalled();
  });
});

describe("StructuredEditor", () => {
  async function mountStructured(value: string, onChange = vi.fn(), onParseError = vi.fn()) {
    host = document.createElement("div");
    document.body.appendChild(host);
    root = createRoot(host);
    await act(async () => root!.render(createElement(StructuredEditor, { value, onChange, onParseError, defaultFormat: "yaml" })));
    await vi.waitFor(() => expect(host!.querySelector(".cm-content")).not.toBeNull());
    const view = EditorView.findFromDOM(host!.querySelector(".cm-editor") as HTMLElement)!;
    return { view, onChange, onParseError };
  }

  it("shows the host's JSON as YAML and hands edits back as JSON", async () => {
    const { view, onChange } = await mountStructured('{\n  "name": "t",\n  "prompt": "a\\nb"\n}\n');
    expect(view.state.doc.toString()).toBe("name: t\nprompt: |-\n  a\n  b\n");
    act(() => view.dispatch({ changes: { from: 6, to: 7, insert: "renamed" } }));
    expect(JSON.parse(onChange.mock.calls.at(-1)![0])).toEqual({ name: "renamed", prompt: "a\nb" });
  });

  it("an invalid YAML edit reports the error and does not change the host's value", async () => {
    const { view, onChange, onParseError } = await mountStructured('{"name": "t"}');
    act(() => view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: "name: [" } }));
    expect(onChange).not.toHaveBeenCalled();
    expect(onParseError).toHaveBeenLastCalledWith(expect.objectContaining({ line: 1 }));
    expect(host!.querySelector(".lc-ce-error")?.textContent).toMatch(/line 1/);
  });

  it("refuses to switch format while the text does not parse", async () => {
    const { view } = await mountStructured('{"name": "t"}');
    act(() => view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: "name: [" } }));
    const jsonButton = [...host!.querySelectorAll("button")].find((b) => b.textContent === "JSON")!;
    act(() => jsonButton.click());
    expect(jsonButton.getAttribute("aria-pressed")).toBe("false");
    expect(view.state.doc.toString()).toBe("name: [");
    expect(host!.querySelector(".lc-ce-error")?.textContent).toMatch(/before switching/);
  });
});
