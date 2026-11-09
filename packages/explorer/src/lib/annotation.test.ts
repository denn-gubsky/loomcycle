import { describe, it, expect, vi } from "vitest";
import type { Annotation } from "../types";
import {
  ABSTRACT_MAX_CHARS,
  BRIEF_MAX_CHARS,
  annotationDraftProblem,
  annotationLevelFor,
  loadAnnotations,
  setAnnotationInput,
  usableAnnotations,
  writtenByLabel,
} from "./annotation";

const rec = (over: Partial<Annotation> = {}): Annotation => ({
  chunk_id: "c1",
  document_id: "d1",
  level: "branch",
  brief: "What this section settles.",
  abstract: "A paragraph.",
  ...over,
});

describe("loadAnnotations", () => {
  it("makes no call outside the tenant scope, where no annotation can exist", async () => {
    const documentGetAnnotation = vi.fn();
    for (const scope of ["user", "agent"] as const) {
      await expect(loadAnnotations({ documentGetAnnotation }, "c1", scope)).resolves.toEqual([]);
    }
    expect(documentGetAnnotation).not.toHaveBeenCalled();
  });

  it("answers nothing when the data layer cannot read annotations", async () => {
    await expect(loadAnnotations({}, "c1", "tenant")).resolves.toEqual([]);
  });

  it("returns the chunk's records with the browse scope passed through", async () => {
    const documentGetAnnotation = vi.fn().mockResolvedValue({ annotations: [rec()] });
    const browse = { tenant: "t1" };
    const got = await loadAnnotations({ documentGetAnnotation }, "c1", "tenant", browse);
    expect(documentGetAnnotation).toHaveBeenCalledWith("c1", "tenant", browse);
    expect(got).toEqual([rec()]);
  });

  it("shows nothing for a chunk without a record (never one made from the body)", async () => {
    const documentGetAnnotation = vi.fn().mockResolvedValue({ annotations: [] });
    await expect(loadAnnotations({ documentGetAnnotation }, "c1", "tenant")).resolves.toEqual([]);
    documentGetAnnotation.mockResolvedValue({});
    await expect(loadAnnotations({ documentGetAnnotation }, "c1", "tenant")).resolves.toEqual([]);
  });

  it("propagates a read failure to the caller", async () => {
    const documentGetAnnotation = vi.fn().mockRejectedValue(new Error("boom"));
    await expect(loadAnnotations({ documentGetAnnotation }, "c1", "tenant")).rejects.toThrow("boom");
  });
});

describe("usableAnnotations", () => {
  it("keeps only this chunk's records with a brief, ordered branch, document, path", () => {
    const got = usableAnnotations(
      [
        rec({ level: "path", brief: "folder" }),
        rec({ level: "document", brief: "doc" }),
        rec({ chunk_id: "other" }),
        rec({ level: "branch", brief: "  " }),
        rec({ level: "bogus" as Annotation["level"] }),
        null,
      ],
      "c1",
    );
    expect(got.map((r) => r.level)).toEqual(["document", "path"]);
  });

  it("keeps a stale record, marked", () => {
    expect(usableAnnotations([rec({ stale: true })], "c1")[0].stale).toBe(true);
  });
});

describe("the edit", () => {
  it("is sent as a manual record with no inputs hash", () => {
    const input = setAnnotationInput(
      { id: "c1", level: "document", brief: " B ", abstract: " A " },
      "tenant",
    );
    expect(input).toEqual({
      op: "set_annotation",
      scope: "tenant",
      id: "c1",
      level: "document",
      brief: "B",
      abstract: "A",
      manual: true,
    });
    expect(input).not.toHaveProperty("inputs_sha256");
  });

  it("names a missing or over-long field before it is sent", () => {
    expect(annotationDraftProblem("", "a")).toMatch(/brief is required/);
    expect(annotationDraftProblem("b", " ")).toMatch(/abstract is required/);
    expect(annotationDraftProblem("x".repeat(BRIEF_MAX_CHARS + 1), "a")).toMatch(/240/);
    expect(annotationDraftProblem("b", "x".repeat(ABSTRACT_MAX_CHARS + 1))).toMatch(/2000/);
    expect(annotationDraftProblem("b", "a")).toBeUndefined();
  });

  it("counts characters as the runtime does (one per code point)", () => {
    // 240 emoji = 480 UTF-16 units, still within the cap.
    expect(annotationDraftProblem("😀".repeat(BRIEF_MAX_CHARS), "a")).toBeUndefined();
  });

  it("takes the document level on the root and the branch level elsewhere", () => {
    expect(annotationLevelFor(true)).toBe("document");
    expect(annotationLevelFor(false)).toBe("branch");
  });
});

describe("writtenByLabel", () => {
  it("renders each writer form", () => {
    expect(writtenByLabel("run:abc")).toBe("run abc");
    expect(writtenByLabel("principal:alice")).toBe("alice");
    expect(writtenByLabel("operator")).toBe("operator");
    expect(writtenByLabel(undefined)).toBeUndefined();
  });
});
