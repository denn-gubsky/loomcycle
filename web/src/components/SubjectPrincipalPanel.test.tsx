import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import { bindSubjectPrincipal, loadSubjectPrincipal, subjectPrincipalOf } from "../api";
import { SubjectPrincipalControl } from "./SubjectPrincipalPanel";

afterEach(() => vi.unstubAllGlobals());

const noop = () => {};

describe("SubjectPrincipalControl", () => {
  it("shows the bound user and offers to clear the binding", () => {
    const html = renderToStaticMarkup(
      createElement(SubjectPrincipalControl, { principal: "u-ada", busy: false, onBind: noop, onClear: noop }),
    );
    expect(html).toContain("Bound to user <code>u-ada</code>");
    expect(html).toContain("Clear binding");
    expect(html).not.toContain("Bind to a user");
  });

  it("offers a bind, folded, when the subject is not bound", () => {
    const html = renderToStaticMarkup(
      createElement(SubjectPrincipalControl, { principal: "", busy: false, onBind: noop, onClear: noop }),
    );
    expect(html).toContain("Bind to a user");
    expect(html).toContain("subject id");
    expect(html).not.toContain("Clear binding");
  });
});

describe("subjectPrincipalOf", () => {
  it("reads the binding from a root's fields, and nothing from anything else", () => {
    expect(subjectPrincipalOf({ principal: " u-ada ", color: "blue" })).toBe("u-ada");
    expect(subjectPrincipalOf({ color: "blue" })).toBe("");
    expect(subjectPrincipalOf({ principal: 7 })).toBe("");
    expect(subjectPrincipalOf(null)).toBe("");
    expect(subjectPrincipalOf(["u-ada"])).toBe("");
  });
});

describe("bindSubjectPrincipal", () => {
  it("POSTs the subject and user to the binding route with the tenant focus", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    await bindSubjectPrincipal("d1", "u-ada", "globex");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toContain("/v1/_ontology/subjects/principal?tenant=globex");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ subject: "d1", principal: "u-ada" });
  });
});

describe("loadSubjectPrincipal", () => {
  it("reads the binding from the document's root chunk in the tenant scope", async () => {
    const fetchMock = vi.fn(async (_url: string, init?: RequestInit) => {
      const body = JSON.parse(String(init?.body));
      const out =
        body.op === "get_document"
          ? { document_id: "d1", root_chunk_id: "r1" }
          : { id: "r1", fields: { principal: "u-ada" } };
      return new Response(JSON.stringify(out), { status: 200 });
    });
    vi.stubGlobal("fetch", fetchMock);
    expect(await loadSubjectPrincipal("d1", "globex")).toBe("u-ada");
    const bodies = fetchMock.mock.calls.map((c) => JSON.parse(String(c[1]?.body)));
    expect(bodies).toEqual([
      { op: "get_document", id: "d1", scope: "tenant" },
      { op: "get_chunk", id: "r1", scope: "tenant" },
    ]);
    expect(String(fetchMock.mock.calls[0][0])).toContain("tenant=globex");
  });
});
