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
    { name: "Ledger", source: "tenant", locked: true },
  ],
  effective: [],
  class_chunks: { Memo: "c-memo", Ticket: "c-ticket", Ledger: "c-ledger" },
};

const operator = { tenant_id: "t", subject: "ops", scopes: ["substrate:tenant"], is_admin: false, legacy: false } as Principal;
const member = { tenant_id: "t", subject: "m", scopes: [], is_admin: false, legacy: false } as Principal;

const refusal =
  'delete_chunk: entry_has_dependents: the ontology entry it would remove is in use, so nothing was deleted: class "Memo" (typed_documents 3). Retire it instead.';

type Call = { url: string; body?: Record<string, unknown> };

// stubServer answers each plan with the next fingerprint (fp1, fp2, …); an update
// carrying a fingerprint listed in stale answers 409 plan_stale, as the runtime does
// after another change to the ontology (RFC GQ P2.4).
function stubServer(calls: Call[], stale: string[] = [], ontology: OntologyResponse = state) {
  let plans = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined;
      calls.push({ url, body });
      if (url.startsWith("/v1/_ontology/plan")) {
        plans++;
        const counts = { typed_documents: { n: plans + 2, effect: "stale", tenant_scope: plans + 2 } };
        const entry = { chunk_id: "c-memo", name: "Memo", section: "classes", kind: "class" };
        return new Response(
          JSON.stringify({ fingerprint: `fp${plans}`, complete: true, changes: [{ op: "retire", entry, risk: "stales", counts }] }),
          { status: 200 },
        );
      }
      if (url.startsWith("/v1/_ontology")) return new Response(JSON.stringify(ontology), { status: 200 });
      if (url.startsWith("/v1/_document")) {
        switch (body?.op) {
          case "get_chunk":
            return new Response(JSON.stringify({ id: body.id, revision: 3 }), { status: 200 });
          case "update_chunk":
            if (stale.includes(String(body.plan_fingerprint))) {
              return new Response(
                JSON.stringify({ code: "plan_stale", error: "update_chunk: plan_stale: …", fingerprint: "fp-now" }),
                { status: 409 },
              );
            }
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

async function mount(principal: Principal, entry = "/"): Promise<{ root: Root; el: HTMLElement }> {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () =>
    root.render(
      <MemoryRouter initialEntries={[entry]}>
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

  it("marks a locked class and offers neither retire nor delete on it (RFC GQ P2.3)", async () => {
    stubServer([]);
    mounted = await mount(operator);
    const ledger = rowOf(mounted.el, "Ledger");
    expect(ledger.textContent).toContain("· locked");
    expect(button(ledger, "retire")).toBeUndefined();
    expect(button(ledger, "delete")).toBeUndefined();
    expect(rowOf(mounted.el, "Memo").textContent).not.toContain("locked");
  });

  it("plans the retire, shows its counts, and retires with the plan's fingerprint", async () => {
    const calls: Call[] = [];
    stubServer(calls);
    const confirm = vi.fn((_text: string) => true);
    vi.stubGlobal("confirm", confirm);
    mounted = await mount(operator);
    await act(async () => button(rowOf(mounted!.el, "Memo"), "retire")!.click());
    await settle();
    expect(calls.find((c) => c.url.startsWith("/v1/_ontology/plan"))?.body).toEqual({
      changes: [{ op: "retire", entry: "c-memo" }],
    });
    expect(confirm.mock.calls[0]?.[0]).toContain("typed documents: 3");
    const update = calls.find((c) => c.body?.op === "update_chunk");
    expect(update?.body).toEqual({
      op: "update_chunk",
      id: "c-memo",
      revision: 3,
      scope: "tenant",
      status: "retired",
      plan_fingerprint: "fp1",
    });
    expect(mounted.el.querySelector(".settings-error")).toBeNull();
  });

  it("re-plans once when the retire answers plan_stale", async () => {
    const calls: Call[] = [];
    stubServer(calls, ["fp1"]);
    const confirm = vi.fn((_text: string) => true);
    vi.stubGlobal("confirm", confirm);
    mounted = await mount(operator);
    await act(async () => button(rowOf(mounted!.el, "Memo"), "retire")!.click());
    await settle();
    const sent = calls.filter((c) => c.body?.op === "update_chunk").map((c) => c.body?.plan_fingerprint);
    expect(sent).toEqual(["fp1", "fp2"]);
    expect(confirm).toHaveBeenCalledTimes(2);
    expect(confirm.mock.calls[1]?.[0]).toContain("typed documents: 4");
    expect(mounted.el.querySelector(".settings-error")).toBeNull();
  });

  it("retires nothing when the operator declines the plan", async () => {
    const calls: Call[] = [];
    stubServer(calls);
    vi.stubGlobal("confirm", () => false);
    mounted = await mount(operator);
    await act(async () => button(rowOf(mounted!.el, "Memo"), "retire")!.click());
    await settle();
    expect(calls.some((c) => c.body?.op === "update_chunk")).toBe(false);
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

describe("OntologySection change proposal accept (RFC GQ P4.3)", () => {
  let mounted: { root: Root; el: HTMLElement } | null = null;
  afterEach(() => {
    if (mounted) act(() => mounted!.root.unmount());
    mounted?.el.remove();
    mounted = null;
    vi.unstubAllGlobals();
  });

  const withProposal: OntologyResponse = {
    ...state,
    proposals: [
      {
        chunk_id: "p-retire",
        name: "retire: Memo",
        status: "proposed",
        change: { op: "retire", target: "c-memo", target_name: "Memo", section: "classes", kind: "class" },
      },
    ],
  };

  function acceptButton(el: HTMLElement): HTMLButtonElement {
    const b = Array.from(el.querySelectorAll("button")).find((x) => x.textContent === "accept");
    if (!b) throw new Error("no accept button");
    return b;
  }

  it("plans the proposal by its id, shows the counts, and accepts with the plan's fingerprint", async () => {
    const calls: Call[] = [];
    stubServer(calls, [], withProposal);
    const confirm = vi.fn((_text: string) => true);
    vi.stubGlobal("confirm", confirm);
    mounted = await mount(operator);
    await act(async () => acceptButton(mounted!.el).click());
    await settle();
    expect(calls.find((c) => c.url.startsWith("/v1/_ontology/plan"))?.body).toEqual({
      changes: [{ op: "proposal", id: "p-retire" }],
    });
    expect(confirm.mock.calls[0]?.[0]).toContain("typed documents: 3");
    expect(calls.find((c) => c.url.startsWith("/v1/_ontology/proposals"))?.body).toEqual({
      chunk_id: "p-retire",
      action: "accept",
      plan_fingerprint: "fp1",
    });
  });

  it("accepts nothing when the operator declines the plan", async () => {
    const calls: Call[] = [];
    stubServer(calls, [], withProposal);
    vi.stubGlobal("confirm", () => false);
    mounted = await mount(operator);
    await act(async () => acceptButton(mounted!.el).click());
    await settle();
    expect(calls.some((c) => c.url.startsWith("/v1/_ontology/proposals"))).toBe(false);
  });
});

describe("OntologySection releases (RFC GQ P3.7)", () => {
  let mounted: { root: Root; el: HTMLElement } | null = null;
  afterEach(() => {
    if (mounted) act(() => mounted!.root.unmount());
    mounted?.el.remove();
    mounted = null;
    vi.unstubAllGlobals();
  });

  const releases = [
    { seq: 3, content_version: "cccccccccccccccc", fingerprint: "f3", reason: "apply", actor: "ops", created_at: 1764000000000000000, entries: 12 },
    { seq: 2, content_version: "bbbbbbbbbbbbbbbb", fingerprint: "f2", reason: "confirm", created_at: 1763990000000000000, entries: 11 },
  ];
  const diffAnswer = {
    tenant: "t",
    from: { seq: 2, content_version: "bbbbbbbbbbbbbbbb" },
    to: { seq: 3, content_version: "cccccccccccccccc" },
    changes: [
      {
        chunk_id: "c-memo",
        kind: "class",
        change: "changed",
        changed: ["renamed", "meaning"],
        before: { name: "Memo", meaning: "a note", directives: {} },
        after: { name: "Note", meaning: "a short note", directives: {} },
      },
    ],
    issues: [],
  };

  // stubReleases answers the ontology and its release routes; refuse makes every
  // release route answer 403 forbidden, as the runtime does for a member.
  function stubReleases(calls: string[], refuse = false) {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        calls.push(url);
        if (url.startsWith("/v1/_ontology/releases")) {
          if (refuse) {
            return new Response(JSON.stringify({ code: "forbidden", error: "ontology releases: operator only" }), {
              status: 403,
              statusText: "Forbidden",
            });
          }
          if (url.startsWith("/v1/_ontology/releases/diff")) return new Response(JSON.stringify(diffAnswer), { status: 200 });
          if (url === "/v1/_ontology/releases") return new Response(JSON.stringify({ tenant: "t", releases }), { status: 200 });
          return new Response(
            JSON.stringify({ tenant: "t", release: releases[0], export_md: "# Ontology\n\n## Note\n" }),
            { status: 200 },
          );
        }
        if (url.startsWith("/v1/_ontology")) {
          return new Response(JSON.stringify({ ...state, content_version: "cccccccccccccccc" }), { status: 200 });
        }
        return new Response("404 page not found", { status: 404 });
      }),
    );
  }

  function buttonByText(el: HTMLElement, label: string): HTMLButtonElement {
    const b = Array.from(el.querySelectorAll("button")).find((x) => x.textContent === label);
    if (!b) throw new Error(`no button ${label}`);
    return b;
  }

  it("lists each release with its reason, content version and actor, marking the one in force", async () => {
    const calls: string[] = [];
    stubReleases(calls);
    mounted = await mount(operator, "/?section=releases");
    const rows = Array.from(mounted.el.querySelectorAll("table.ontology-releases tbody tr"));
    expect(rows.map((r) => r.querySelector("td")?.textContent)).toEqual(["3", "2"]);
    expect(rows[0].textContent).toContain("change applied");
    expect(rows[0].textContent).toContain("cccccccccccccccc in force");
    expect(rows[0].textContent).toContain("ops");
    expect(rows[1].textContent).toContain("confirmed");
    expect(rows[1].textContent).not.toContain("in force");
  });

  it("diffs a release from the one before it and shows each changed field before and after", async () => {
    const calls: string[] = [];
    stubReleases(calls);
    mounted = await mount(operator, "/?section=releases");
    await act(async () => buttonByText(mounted!.el, "diff from 2").click());
    await settle();
    expect(calls).toContain("/v1/_ontology/releases/diff?from=2&to=3");
    const diff = mounted.el.querySelector(".ontology-release-diff")!;
    expect(diff.querySelector("h4")?.textContent).toBe("From release 2 to release 3: 1 changed, 0 added, 0 removed");
    const cells = Array.from(diff.querySelectorAll("tbody tr")).map((tr) =>
      Array.from(tr.querySelectorAll("td")).map((td) => td.textContent),
    );
    expect(cells).toEqual([
      ["Note class · changed", "name", "Memo", "Note"],
      ["meaning", "a note", "a short note"],
    ]);
  });

  it("compares a release with the ontology in force and opens one as Markdown", async () => {
    const calls: string[] = [];
    stubReleases(calls);
    mounted = await mount(operator, "/?section=releases");
    await act(async () => buttonByText(mounted!.el, "Compare").click());
    await settle();
    expect(calls).toContain("/v1/_ontology/releases/diff?from=3&to=current");
    await act(async () => Array.from(mounted!.el.querySelectorAll("button")).find((b) => b.textContent === "view")!.click());
    await settle();
    expect(calls).toContain("/v1/_ontology/releases/3");
    expect(mounted.el.querySelector(".ontology-release-md")?.textContent).toContain("## Note");
  });

  it("shows a refusal as not permitted and offers no release", async () => {
    stubReleases([], true);
    mounted = await mount(member, "/?section=releases");
    const alert = mounted.el.querySelector(".acl-denied");
    expect(alert?.querySelector(".acl-denied-head")?.textContent).toBe("Not permitted");
    expect(alert?.querySelector(".settings-muted")?.textContent).toBe("ontology releases: operator only");
    expect(mounted.el.querySelector("table.ontology-releases")).toBeNull();
    expect(mounted.el.querySelector(".settings-error")).toBeNull();
  });

  it("reads no release until the tab is opened", async () => {
    const calls: string[] = [];
    stubReleases(calls);
    mounted = await mount(operator);
    expect(calls.some((u) => u.startsWith("/v1/_ontology/releases"))).toBe(false);
    expect(Array.from(mounted.el.querySelectorAll("[role=tab]")).map((t) => t.textContent)).toContain("releases");
  });
});

describe("OntologySection rename job (RFC GQ P5.6)", () => {
  let mounted: { root: Root; el: HTMLElement } | null = null;
  afterEach(() => {
    if (mounted) act(() => mounted!.root.unmount());
    mounted?.el.remove();
    mounted = null;
    vi.unstubAllGlobals();
  });

  const running = {
    id: "j1",
    kind: "class_rename",
    entry: "c-memo",
    from: ["Memo"],
    to: "Note",
    state: "running",
    step: "chunks_type",
    done: { documents_type: 4 },
    remaining: 12,
    scope: "user",
    scopes_done: 1,
    scopes_total: 3,
  };

  // stubJob answers the ontology and its job routes: the status with job, a resume
  // with resumed (or a 409 job_busy when resumed is null).
  function stubJob(calls: Call[], job: unknown, resumed: unknown = { ...running, state: "done", step: "", scopes_done: 3 }) {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        calls.push({ url, body: init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined });
        if (url === "/v1/_ontology/job") return new Response(JSON.stringify({ tenant: "t", job }), { status: 200 });
        if (url === "/v1/_ontology/job/resume") {
          if (resumed === null) {
            return new Response(JSON.stringify({ code: "job_busy", error: "a class rename is being checked right now" }), {
              status: 409,
              statusText: "Conflict",
            });
          }
          return new Response(JSON.stringify({ tenant: "t", job: resumed }), { status: 200 });
        }
        if (url.startsWith("/v1/_ontology")) return new Response(JSON.stringify(state), { status: 200 });
        return new Response("404 page not found", { status: 404 });
      }),
    );
  }

  const panel = (el: HTMLElement) => el.querySelector("[aria-label='ontology job']");

  it("shows an unfinished job's progress to an operator and resumes it with its id", async () => {
    const calls: Call[] = [];
    stubJob(calls, running);
    mounted = await mount(operator);
    const p = panel(mounted.el);
    expect(p?.textContent).toContain("Class rename: Memo → Note");
    expect(p?.textContent).toContain("step chunks_type in the user scope · 1 of 3 scopes done · 12 rows left in this scope");
    const resume = Array.from(p!.querySelectorAll("button")).find((b) => b.textContent === "Resume");
    expect(resume).toBeDefined();
    await act(async () => resume!.click());
    await settle();
    const post = calls.find((c) => c.url === "/v1/_ontology/job/resume");
    expect(post?.body).toEqual({ id: "j1" });
    expect(panel(mounted.el)?.textContent).toContain("finished");
    expect(Array.from(panel(mounted.el)!.querySelectorAll("button")).map((b) => b.textContent)).not.toContain("Resume");
  });

  it("shows a blocked job's reason and a refused resume's code", async () => {
    stubJob([], { ...running, state: "blocked", error: "a user scope: identity_keys: a row could not be rewritten" }, null);
    mounted = await mount(operator);
    expect(panel(mounted.el)?.textContent).toContain("a row could not be rewritten");
    const resume = Array.from(panel(mounted.el)!.querySelectorAll("button")).find((b) => b.textContent === "Resume");
    await act(async () => resume!.click());
    await settle();
    expect(panel(mounted.el)?.textContent).toContain("a class rename is being checked right now");
  });

  it("offers no resume for a finished job, and shows nothing without one", async () => {
    stubJob([], { ...running, state: "done", step: "", scopes_done: 3 });
    mounted = await mount(operator);
    expect(panel(mounted.el)?.textContent).toContain("Last rename job");
    expect(panel(mounted.el)?.querySelector("button")).toBeNull();
    act(() => mounted!.root.unmount());
    mounted.el.remove();
    vi.unstubAllGlobals();

    stubJob([], null);
    mounted = await mount(operator);
    expect(panel(mounted.el)).toBeNull();
  });

  it("reads no job for a member", async () => {
    const calls: Call[] = [];
    stubJob(calls, running);
    mounted = await mount(member);
    expect(calls.some((c) => c.url.startsWith("/v1/_ontology/job"))).toBe(false);
    expect(panel(mounted.el)).toBeNull();
  });
});
