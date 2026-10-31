import { describe, expect, it } from "vitest";
import {
  buildRelationTree,
  conceptSummary,
  instanceStateLabel,
  classDirectiveNotes,
  factsUnderOlderVocabulary,
  ontologyHealthFindings,
  ontologyTabs,
  pickOntologyTab,
  relationSummary,
  typedFieldLabel,
} from "./ontologySections";
import type { OntologyResponse } from "../api";

describe("typedFieldLabel", () => {
  it("leaves an untyped field as its bare name", () => {
    expect(typedFieldLabel({ name: "summary", type: "text" })).toBe("summary");
  });
  it("renders types and flags the way they are written", () => {
    expect(
      typedFieldLabel({ name: "rank", type: "concept", range: "normative-rank", required: true }),
    ).toBe("rank: concept(normative-rank), required");
    expect(typedFieldLabel({ name: "state", type: "enum", enum: ["a", "b"] })).toBe("state: enum(a, b)");
    expect(typedFieldLabel({ name: "title", type: "text", identity: true })).toBe("title: identity");
  });
});

describe("classDirectiveNotes", () => {
  it("marks inherited directives so the panel never claims a declaration", () => {
    const notes = classDirectiveNotes({
      name: "Law",
      source: "tenant",
      citation_grammar: "law-ua-v2",
      inherited_directives: { identifier_pattern: "(?P<n>\\d+)", definitions: "^Article 1\\b" },
      inherited_title_identity: true,
    });
    expect(notes).toContain("citations: law-ua-v2");
    expect(notes).toContain("identifier pattern: (?P<n>\\d+) (inherited)");
    expect(notes).toContain("definitions: ^Article 1\\b (inherited)");
    expect(notes).toContain("title is its identity (inherited)");
  });
  it("is empty for a plain class", () => {
    expect(classDirectiveNotes({ name: "incident", source: "tenant" })).toEqual([]);
  });
});

describe("factsUnderOlderVocabulary", () => {
  it("counts older and unknown versions apart", () => {
    const r = {
      ontology_version: 5,
      fact_versions: [
        { version: 0, count: 7 },
        { version: 3, count: 2 },
        { version: 5, count: 10 },
      ],
    } as OntologyResponse;
    expect(factsUnderOlderVocabulary(r)).toEqual({ older: 2, unknown: 7 });
  });
});

describe("relationSummary", () => {
  it("describes direction, ends, traits and qualifiers", () => {
    expect(
      relationSummary({
        name: "stricter_than",
        source: "tenant",
        inverse: "looser_than",
        domain: ["Provision"],
        range: ["Provision"],
        asymmetric: true,
        qualifiers: [{ name: "aspect", values: ["security", "privacy"] }],
        discriminator: "aspect",
      }),
    ).toBe(
      "inverse looser_than · Provision → Provision · asymmetric · qualifiers aspect(security|privacy) · distinct per aspect",
    );
    expect(relationSummary({ name: "related", source: "base", symmetric: true })).toBe("either direction");
  });
});

describe("buildRelationTree", () => {
  it("nests sub-kinds and keeps orphans as roots", () => {
    const tree = buildRelationTree([
      { name: "cites", parent: "references", source: "base" },
      { name: "references", source: "reserved" },
      { name: "amends", parent: "changes", source: "tenant" },
    ]);
    expect(tree.map((n) => n.name)).toEqual(["amends", "references"]);
    expect(tree[1].children.map((n) => n.name)).toEqual(["cites"]);
  });
});

describe("conceptSummary", () => {
  it("leads with the tag and marks file-plan policy as not enforced", () => {
    expect(
      conceptSummary({
        scheme: "file-plan",
        name: "compliance",
        path: ["compliance"],
        tag: "file-plan/compliance",
        retention: "10y",
        acl: "compliance",
      }),
    ).toBe("file-plan/compliance · retention 10y, access compliance (not enforced yet)");
    expect(
      conceptSummary({ scheme: "rank", name: "Law", path: ["Law"], tag: "rank/law", rank: 1, notation: "L" }),
    ).toBe("rank/law · L · rank 1");
  });
});

describe("instanceStateLabel", () => {
  it("speaks operator words and carries an error's reason", () => {
    expect(instanceStateLabel({ name: "a", state: "pending" })).toBe("adopted on the next confirm");
    expect(instanceStateLabel({ name: "a", state: "renamed" })).toBe("renamed — keeps its key");
    expect(instanceStateLabel({ name: "a", state: "error", note: "no @a class" })).toBe(
      "not adopted: no @a class",
    );
  });
});

// The smallest response the server sends for a provisioned, section-less ontology.
function flat(extra: Partial<OntologyResponse> = {}): OntologyResponse {
  return {
    tenant: "t",
    path: "/memory/ontology",
    status: "confirmed",
    confirmed: true,
    provisioned: true,
    terms: [],
    effective: [{ name: "person", source: "seed" }],
    ...extra,
  } as OntologyResponse;
}

describe("ontologyTabs", () => {
  it("gives a section-less ontology only its classes", () => {
    expect(ontologyTabs(flat()).map((t) => t.id)).toEqual(["classes"]);
  });

  it("adds a tab per section the server reported, in document order", () => {
    const tabs = ontologyTabs(
      flat({
        sections: ["classes", "relations", "schemes", "instances"],
        document_classes: [{ name: "Law", source: "tenant" }],
        relations: [],
        schemes: [{ name: "doc-kind", concepts: [] }],
        instances: { instances: [{ name: "Board", state: "unchanged" }], adopted: 0, linked: 0, renamed: 0, statements: 0 },
        grammars: [{ name: "nbu-act" }],
      } as Partial<OntologyResponse>),
    );
    expect(tabs.map((t) => t.id)).toEqual([
      "classes",
      "document-classes",
      "relations",
      "schemes",
      "instances",
      "grammars",
    ]);
    expect(tabs.find((t) => t.id === "instances")?.count).toBe(1);
  });

  it("keeps a declared but empty Relations section, which still holds the standard kinds", () => {
    expect(ontologyTabs(flat({ relations: [] })).map((t) => t.id)).toContain("relations");
  });

  it("warns on a disabled grammar and on a scheme concept missing its tag", () => {
    const tabs = ontologyTabs(
      flat({
        grammars: [{ name: "ok" }, { name: "bad", issue: "pattern does not compile" }],
        schemes: [{ name: "s", concepts: [] }],
        scheme_mirror_stale: ["s/a"],
      } as Partial<OntologyResponse>),
    );
    expect(tabs.find((t) => t.id === "grammars")?.warn).toBe(true);
    expect(tabs.find((t) => t.id === "schemes")?.warn).toBe(true);
    expect(tabs.find((t) => t.id === "classes")?.warn).toBe(false);
  });

  it("shows Health only when it has a finding", () => {
    expect(ontologyTabs(flat()).some((t) => t.id === "health")).toBe(false);
    const tabs = ontologyTabs(flat({ undeclared_types: [{ type: "memo", count: 3 }] }));
    expect(tabs.find((t) => t.id === "health")).toMatchObject({ count: 1, warn: true });
  });
});

describe("ontologyHealthFindings", () => {
  it("counts each kind of finding once", () => {
    const r = flat({
      ontology_version: 5,
      undeclared_types: [
        { type: "memo", count: 3 },
        { type: "note", count: 1 },
      ],
      fact_versions: [
        { version: 4, count: 10 },
        { version: 0, count: 2 },
      ],
      undated_statements: 7,
    });
    expect(ontologyHealthFindings(r)).toBe(3);
  });

  it("finds nothing when every fact is on the current vocabulary", () => {
    expect(ontologyHealthFindings(flat({ ontology_version: 5, fact_versions: [{ version: 5, count: 9 }] }))).toBe(0);
  });
});

describe("pickOntologyTab", () => {
  const tabs = ontologyTabs(flat({ relations: [] }));
  it("opens the tab the URL names when the ontology has it", () => {
    expect(pickOntologyTab("relations", tabs)).toBe("relations");
  });
  it("falls back to classes for an absent or unknown tab", () => {
    expect(pickOntologyTab(null, tabs)).toBe("classes");
    expect(pickOntologyTab("schemes", tabs)).toBe("classes");
    expect(pickOntologyTab("nonsense", tabs)).toBe("classes");
  });
});
