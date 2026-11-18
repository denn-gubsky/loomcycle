// @vitest-environment happy-dom
import { act } from "react";
import { type Root, createRoot } from "react-dom/client";
import { MemoryRouter, Outlet, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { OntologyResponse, Principal } from "../api";
import OntologySection from "./OntologyPage";

// The Ontology page's retire and delete controls (RFC GQ P2.2), mounted for real
// against a stubbed /v1/_ontology and /v1/_document.

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const state: OntologyResponse = {
  tenant: "t",
  path: "/memory/ontology",
  status: "confirmed",
  confirmed: true,
  provisioned: true,
  sections: ["classes"],
  terms: [
    { name: "Memo", source: "tenant" },
    { name: "Ticket", source: "tenant", entry_status: "retired" },
  ],
  effective: [],
  class_chunks: { Memo: "c-memo", Ticket: "c-ticket" },
};

const operator = { tenant_id: "t", subject: "ops", scopes: ["substrate:tenant"], is_admin: false, legacy: false } as Principal;
const member = { tenant_id: "t", subject: "m", scopes: [], is_admin: false, legacy: false } as Principal;

const refusal =
  'delete_chunk: entry_has_dependents: the ontology entry it would remove is in use, so nothing was deleted: class "Memo" (typed_documents 3). Retire it instead.';

type Call = { url: string; body?: Record<string, unknown> };

function stubServer(calls: Call[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined;
      calls.push({ url, body });
      if (url.startsWith("/v1/_ontology")) return new Response(JSON.stringify(state), { status: 200 });
      if (url.startsWith("/v1/_document")) {
        switch (body?.op) {
          case "get_chunk":
            return new Response(JSON.stringify({ id: body.id, revision: 3 }), { status: 200 });
          case "update_chunk":
            return new Response(JSON.stringify({ id: body.id, revision: 4 }), { status: 200 });
          case "delete_chunk":
            return new Response(JSON.stringify({ error: refusal }), { status: 400 });
        }
      }
      return new Response("404 page not found", { status: 404 });
    }),
  );
}

async function settle() {
  for (let i = 0; i < 5; i++) await act(async () => new Promise((r) => setTimeout(r, 0)));
}

async function mount(principal: Principal): Promise<{ root: Root; el: HTMLElement }> {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <MemoryRouter>
        <Routes>
          <Route element={<Outlet context={{ principal, userId: "" }} />}>
            <Route path="/" element={<OntologySection />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    ),
  );
  await settle();
  return { root, el };
}

// rowOf is the list item of the class named name in the tenant's own column.
function rowOf(el: HTMLElement, name: string): HTMLLIElement {
  const li = Array.from(el.querySelectorAll("li")).find((l) => l.querySelector("code")?.textContent === name);
  if (!li) throw new Error(`no row for ${name}`);
  return li;
}

function button(li: HTMLElement, label: string): HTMLButtonElement | undefined {
  return Array.from(li.querySelectorAll("button")).find((b) => b.textContent === label);
}

describe("OntologySection retire and delete", () => {
  let mounted: { root: Root; el: HTMLElement } | null = null;
  afterEach(() => {
    if (mounted) act(() => mounted!.root.unmount());
    mounted?.el.remove();
    mounted = null;
    vi.unstubAllGlobals();
  });

  it("offers retire on a live class and marks a retired one instead", async () => {
    stubServer([]);
    mounted = await mount(operator);
    const memo = rowOf(mounted.el, "Memo");
    expect(button(memo, "retire")).toBeDefined();
    expect(button(memo, "delete")).toBeDefined();
    const ticket = rowOf(mounted.el, "Ticket");
    expect(ticket.textContent).toContain("retired");
    expect(button(ticket, "retire")).toBeUndefined();
  });

  it("retires through update_chunk at the chunk's current revision", async () => {
    const calls: Call[] = [];
    stubServer(calls);
    mounted = await mount(operator);
    await act(async () => button(rowOf(mounted!.el, "Memo"), "retire")!.click());
    await settle();
    const update = calls.find((c) => c.body?.op === "update_chunk");
    expect(update?.body).toEqual({ op: "update_chunk", id: "c-memo", revision: 3, scope: "tenant", status: "retired" });
    expect(mounted.el.querySelector(".settings-error")).toBeNull();
  });

  it("shows the runtime's refusal when deleting an entry in use", async () => {
    const calls: Call[] = [];
    stubServer(calls);
    vi.stubGlobal("confirm", () => true);
    mounted = await mount(operator);
    await act(async () => button(rowOf(mounted!.el, "Memo"), "delete")!.click());
    await settle();
    expect(calls.find((c) => c.body?.op === "delete_chunk")?.body).toEqual({ op: "delete_chunk", id: "c-memo", scope: "tenant" });
    const err = mounted.el.querySelector(".settings-error");
    expect(err?.textContent).toContain("entry_has_dependents");
    expect(err?.textContent).toContain("Retire it instead");
  });

  it("asks before deleting, and sends nothing when the operator declines", async () => {
    const calls: Call[] = [];
    stubServer(calls);
    vi.stubGlobal("confirm", () => false);
    mounted = await mount(operator);
    await act(async () => button(rowOf(mounted!.el, "Memo"), "delete")!.click());
    await settle();
    expect(calls.some((c) => c.body?.op === "delete_chunk")).toBe(false);
  });

  it("offers a member neither retire nor delete, and still marks the retired class", async () => {
    stubServer([]);
    mounted = await mount(member);
    expect(mounted.el.querySelectorAll("li button").length).toBe(0);
    expect(rowOf(mounted.el, "Ticket").textContent).toContain("retired");
  });
});
