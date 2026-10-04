import { afterEach, describe, expect, it, vi } from "vitest";
import { renderTeamDiagram, type Agent } from "../api";
import { graphCaption, walkTeamOf } from "./walkView";

// What the diagram request addresses the team by. The server resolves a NAME
// in the caller's own tenant, so an administrator asking for another tenant's
// team by name gets "team not found"; a version id reaches it.

function diagramFetch() {
  const fetchMock = vi.fn(
    async (_url: string, _init?: RequestInit) => new Response('{"name":"t","def_id":"d","format":"mermaid","diagram":"x"}', { status: 200 }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function sentBody(fetchMock: ReturnType<typeof diagramFetch>): Record<string, unknown> {
  expect(fetchMock).toHaveBeenCalledTimes(1);
  const [url, init] = fetchMock.mock.calls[0];
  expect(url).toBe("/v1/_teamdef");
  return JSON.parse(String(init?.body));
}

function walk(spec?: Record<string, unknown>): Pick<Agent, "agent_id" | "spec"> {
  return { agent_id: "team:pcparts", spec };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("renderTeamDiagram", () => {
  it("asks for the version by def_id, not the name, when a version id is known", async () => {
    const f = diagramFetch();
    await renderTeamDiagram({ name: "pcparts", defId: "tdf_1" }, "build");
    expect(sentBody(f)).toEqual({ op: "render_diagram", def_id: "tdf_1", highlight_state: "build" });
  });

  it("asks by name when no version id is known", async () => {
    const f = diagramFetch();
    await renderTeamDiagram({ name: "pcparts" });
    expect(sentBody(f)).toEqual({ op: "render_diagram", name: "pcparts" });
  });
});

describe("a walk's diagram", () => {
  const recorded = walk({ team: { name: "pcparts", def_id: "tdf_ran", version: 3, def_tenant: "loomcycle-dev" } });

  it("is drawn from the version the walk recorded", async () => {
    expect(walkTeamOf(recorded)).toEqual({ name: "pcparts", defId: "tdf_ran", version: 3 });
    const f = diagramFetch();
    await renderTeamDiagram(walkTeamOf(recorded), "build");
    expect(sentBody(f)).toEqual({ op: "render_diagram", def_id: "tdf_ran", highlight_state: "build" });
  });

  it("falls back to the team's name for a walk that recorded no version", async () => {
    for (const old of [walk(), walk({}), walk({ team: { name: "pcparts", def_id: "" } }), walk({ team: "pcparts" })]) {
      expect(walkTeamOf(old)).toEqual({ name: "pcparts" });
    }
    const f = diagramFetch();
    await renderTeamDiagram(walkTeamOf(walk()));
    expect(sentBody(f)).toEqual({ op: "render_diagram", name: "pcparts" });
  });

  it("is captioned with the version it shows", () => {
    expect(graphCaption(walkTeamOf(recorded))).toBe(
      "Drawn from version 3 of the team's definition, the version this walk ran.",
    );
    expect(graphCaption({ name: "pcparts", defId: "tdf_ran" })).toBe(
      "Drawn from the version of the team's definition this walk ran.",
    );
    const old = graphCaption(walkTeamOf(walk()));
    expect(old).toContain("the team's current definition");
    expect(old).toContain("before walks recorded the version they ran");
    expect(graphCaption(undefined)).toBe("");
  });
});
