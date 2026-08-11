import { describe, expect, it } from "vitest";
import { ontologyEditHref } from "./ontologyEditHref";

describe("ontologyEditHref", () => {
  // The regression. Without scope=tenant the viewer opens the tenant document at user
  // scope, the read 422s, and the panel's edit affordance lands on an empty page with no
  // create buttons — which is indistinguishable, to an operator, from "editing is not
  // possible here".
  it("carries scope=tenant, because the ontology is a tenant document", () => {
    expect(ontologyEditHref("abc123")).toBe("/documents/abc123?scope=tenant");
  });

  it("escapes the id rather than interpolating it raw", () => {
    expect(ontologyEditHref("a/b?c")).toBe("/documents/a%2Fb%3Fc?scope=tenant");
  });
});
