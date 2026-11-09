import { describe, expect, it, vi } from "vitest";
import type { Principal } from "../api";
import {
  ANNOTATOR_AGENT,
  canRegenerateAnnotations,
  regenerateAnnotation,
  runFailure,
} from "./annotationRegen";

const target = { documentId: "d1", chunkId: "c1", level: "branch" as const, scope: "tenant" as const };
const frame = (data: unknown) => ({ event: "message", data: JSON.stringify(data) });

describe("regenerateAnnotation", () => {
  it("starts the annotator on the target's document with a forced rewrite", async () => {
    const start = vi.fn().mockResolvedValue(undefined);
    await regenerateAnnotation(target, start);
    const [req] = start.mock.calls[0];
    expect(req.agent).toBe(ANNOTATOR_AGENT);
    expect(JSON.parse(req.prompt)).toEqual({
      mode: "document",
      document_id: "d1",
      write: "annotate",
      force: true,
    });
  });

  it("regenerates a folder's record with the folder's path", async () => {
    const start = vi.fn().mockResolvedValue(undefined);
    await regenerateAnnotation({ ...target, level: "path", folder: "/kb/products" }, start);
    expect(JSON.parse(start.mock.calls[0][0].prompt)).toEqual({
      mode: "path",
      path: "/kb/products",
      write: "annotate",
      force: true,
    });
  });

  it("rejects with the run's error when the run fails", async () => {
    const start = vi.fn(async (_req, h) => {
      h.onFrame(frame({ type: "text", text: "working" }));
      h.onFrame(frame({ type: "error", error: "unknown agent kb/annotator" }));
    });
    await expect(regenerateAnnotation(target, start)).rejects.toThrow("unknown agent kb/annotator");
  });

  it("resolves when the run completes", async () => {
    const start = vi.fn(async (_req, h) => h.onFrame(frame({ type: "done" })));
    await expect(regenerateAnnotation(target, start)).resolves.toBeUndefined();
  });
});

describe("runFailure", () => {
  it("ignores frames that are not errors, and malformed ones", () => {
    expect(runFailure(frame({ type: "done" }))).toBeUndefined();
    expect(runFailure({ event: "message", data: "not json" })).toBeUndefined();
    expect(runFailure(frame({ type: "error" }))).toBe("The annotator run failed.");
  });
});

describe("canRegenerateAnnotations", () => {
  const p = (over: Partial<Principal>): Principal =>
    ({ tenant_id: "t", subject: "s", scopes: [], is_admin: false, legacy: false, ...over }) as Principal;
  it("is an operator's action", () => {
    expect(canRegenerateAnnotations(p({ is_admin: true }))).toBe(true);
    expect(canRegenerateAnnotations(p({ scopes: ["substrate:tenant"] }))).toBe(true);
    expect(canRegenerateAnnotations(p({ scopes: ["runs:create"] }))).toBe(false);
    expect(canRegenerateAnnotations(null)).toBe(false);
  });
});
