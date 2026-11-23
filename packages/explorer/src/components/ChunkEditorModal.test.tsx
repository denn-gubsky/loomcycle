// @vitest-environment happy-dom
import { act, createElement } from "react";
import { type Root, createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ExplorerDataProvider, type ExplorerDataLayer } from "../lib/dataLayer";
import type { ChunkDetail } from "../types";
import ChunkEditorModal from "./ChunkEditorModal";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// The shared editor stands in as a textarea that reports the props it was
// given, so the test reads which mode each box asked for and types through it.
// parseJson stays the real one: the fields' diagnostics come from it.
vi.mock("@loomcycle/code-editor", async (importOriginal) => {
  const real = await importOriginal<typeof import("@loomcycle/code-editor")>();
  return {
    ...real,
    CodeEditor: (props: {
      value: string;
      onChange: (v: string) => void;
      language?: string;
      wrap?: boolean;
      ariaLabel?: string;
      diagnostics?: { message: string }[];
    }) =>
      createElement("textarea", {
        "data-testid": "code-editor",
        "data-language": props.language,
        "data-wrap": String(props.wrap ?? true),
        "data-diagnostics": (props.diagnostics ?? []).map((d) => d.message).join("|"),
        "aria-label": props.ariaLabel,
        value: props.value,
        onChange: (e: { target: { value: string } }) => props.onChange(e.target.value),
      }),
  };
});

let mounted: { root: Root; el: HTMLElement } | null = null;
afterEach(() => {
  if (mounted) {
    act(() => mounted!.root.unmount());
    mounted.el.remove();
    mounted = null;
  }
});

const chunk = (over: Partial<ChunkDetail> = {}): ChunkDetail => ({
  id: "c1",
  document_id: "d1",
  position: 0,
  title: "Intro",
  revision: 4,
  body: "Some *text*.",
  ...over,
});

async function mount(c: ChunkDetail, data: ExplorerDataLayer, onSaved = vi.fn(), onClose = vi.fn()) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      createElement(ExplorerDataProvider, {
        value: data,
        children: createElement(ChunkEditorModal, { chunk: c, scope: "tenant", onClose, onSaved }),
      }),
    ),
  );
  mounted = { root, el };
  // The editor module loads lazily; let it (and the fields' parser) settle
  // inside act.
  for (let i = 0; i < 50 && el.querySelectorAll('[data-testid="code-editor"]').length < 2; i++) {
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
  }
  expect(el.querySelectorAll('[data-testid="code-editor"]').length).toBe(2);
  return el;
}

const editor = (el: HTMLElement, label: string) =>
  el.querySelector<HTMLTextAreaElement>(`[data-testid="code-editor"][aria-label="${label}"]`)!;

// React tracks a textarea's value through its own setter; set it the way a
// keystroke does so onChange fires.
function type(el: HTMLTextAreaElement, value: string) {
  const set = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
  act(() => {
    set.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

describe("ChunkEditorModal", () => {
  it("edits the body in the code editor's markdown mode, wrapped, and saves the edited text", async () => {
    const documentUpdateChunk = vi.fn().mockResolvedValue(chunk({ revision: 5 }));
    const onSaved = vi.fn();
    const el = await mount(chunk(), { documentUpdateChunk } as unknown as ExplorerDataLayer, onSaved);
    const body = editor(el, "Body (Markdown)");
    expect(body.dataset.language).toBe("markdown");
    expect(body.dataset.wrap).toBe("true");
    expect(editor(el, "Fields (JSON)").dataset.language).toBe("json");
    // No plain textarea is left for the body or the fields.
    expect(el.querySelectorAll("textarea:not([data-testid])").length).toBe(0);

    type(body, "# New\n\nEdited **text**.");
    await act(async () => el.querySelector<HTMLFormElement>("form")!.requestSubmit());
    expect(documentUpdateChunk).toHaveBeenCalledWith(
      "c1",
      4,
      expect.objectContaining({ body: "# New\n\nEdited **text**.", title: "Intro" }),
      "tenant",
      undefined,
    );
    expect(onSaved).toHaveBeenCalled();
  });

  it("edits a diagram's source in text mode beside its preview", async () => {
    const el = await mount(chunk({ type: "mermaid", body: "graph TD\n  A --> B" }), {} as ExplorerDataLayer);
    expect(editor(el, "Mermaid source").dataset.language).toBe("text");
    expect(el.querySelector(".chunk-editor-preview")).not.toBeNull();
  });

  it("marks a fields parse error in the editor and does not save it", async () => {
    const documentUpdateChunk = vi.fn();
    const el = await mount(chunk({ fields: { a: 1 } }), { documentUpdateChunk } as unknown as ExplorerDataLayer);
    const fields = editor(el, "Fields (JSON)");
    type(fields, '{"a": 1,');
    await vi.waitFor(() => expect(editor(el, "Fields (JSON)").dataset.diagnostics).not.toBe(""));
    const save = el.querySelector<HTMLButtonElement>('button[type="submit"]')!;
    expect(save.disabled).toBe(true);
    await act(async () => el.querySelector<HTMLFormElement>("form")!.requestSubmit());
    expect(documentUpdateChunk).not.toHaveBeenCalled();
  });
});
