import { describe, expect, it } from "vitest";
import type { LoomcycleClient } from "@loomcycle/client";
import { parseUniverseView, viewFields, viewPath, type UniverseViewState } from "./views";
import { dataLayerFromClient } from "./dataLayer";

const FULL: UniverseViewState = {
  mode: "meaning",
  planar: true,
  depth: 3,
  focus: false,
  hiddenKinds: ["cites", "mentions"],
  type: "rfc",
  status: "draft",
  camera: { position: [1.5, -2, 30], target: [0, 0.25, -1] },
  selection: { documentId: "d1", chunkId: "c9" },
};

describe("saved universe views", () => {
  it("TestView_RoundTripsThroughTheRootFields", () => {
    const v = parseUniverseView("d42", { id: "r1", title: "Drafts by meaning", revision: 2, fields: viewFields(FULL) });
    expect(v).not.toBeNull();
    expect(v!.state).toEqual(FULL);
    expect(v!.title).toBe("Drafts by meaning");
    expect(v!.rootChunkId).toBe("r1");
  });

  it("TestView_ABoardViewIsNotAUniverseView", () => {
    const board = { query: { source: "documents", status: "draft" }, layout: { kind: "kanban", groupBy: "status" } };
    expect(parseUniverseView("d", { id: "r", fields: board })).toBeNull();
    expect(parseUniverseView("d", { id: "r" })).toBeNull();
  });

  it("TestView_AGarbledViewOpensWithDefaults", () => {
    const v = parseUniverseView("d", {
      id: "r",
      fields: {
        layout: { kind: "universe", mode: "sideways", depth: 9, hiddenKinds: [1, "x"], camera: { position: [1, 2] } },
      },
    });
    expect(v!.state).toEqual({ mode: "structure", planar: false, depth: 1, focus: true, hiddenKinds: ["x"] });
  });

  it("TestViewPath_FilesUnderViewsUniverseBySlug", () => {
    expect(viewPath("Drafts — by Meaning!", () => 0.5)).toBe("/views/universe/drafts-by-meaning-7fff");
    expect(viewPath("  ", () => 0)).toBe("/views/universe/view-0000");
  });
});

describe("saved views over the client", () => {
  function fake(roots: Record<string, unknown>) {
    const calls: Record<string, unknown>[] = [];
    const client = {
      document: async (input: Record<string, unknown>) => {
        calls.push(input);
        switch (input.op) {
          case "query_documents":
            return {
              documents: Object.keys(roots).map((id) => ({ document_id: id, root_chunk_id: `root-${id}` })),
            };
          case "get_chunk": {
            const id = String(input.id).replace("root-", "");
            return { id: input.id, title: `View ${id}`, revision: 2, fields: roots[id] };
          }
          case "create_document":
            return { document_id: "new", root_chunk_id: "root-new" };
          default:
            return {};
        }
      },
    } as unknown as LoomcycleClient;
    return { calls, data: dataLayerFromClient(client) };
  }

  it("TestListViews_KeepsOnlyUniverseViews", async () => {
    const { data } = fake({
      a: viewFields(FULL),
      b: { query: { source: "documents" }, layout: { kind: "table" } },
    });
    const views = await data.listViews!("user");
    expect(views.map((v) => v.documentId)).toEqual(["a"]);
  });

  it("TestSaveView_CreatesAViewDocumentAndWritesItsFields", async () => {
    const { calls, data } = fake({});
    const v = await data.saveView!("tenant", "Shared map", FULL);
    expect(calls[0]).toMatchObject({ op: "create_document", scope: "tenant", title: "Shared map", type: "view" });
    expect(String(calls[0].path)).toMatch(/^\/views\/universe\/shared-map-[0-9a-f]{4}$/);
    expect(calls[1]).toEqual({ op: "update_chunk", scope: "tenant", id: "root-new", revision: 1, fields: viewFields(FULL) });
    expect(v.documentId).toBe("new");
  });
});
