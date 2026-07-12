// adapters/ts/tests/teams.test.ts — RFC AP Agent Teams client methods
// (listTeams / renderTeamDiagram / getTeamDef / createTeam / forkTeam /
// deleteTeam / runTeam). Mirror of the substrate + parity test patterns:
// assert the URL + method + snake_case body the runtime expects.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, errorResponse } from "./helpers.js";
import { AuthError, SubstrateToolRefusedError } from "../src/index.js";

describe("listTeams", () => {
  it("GETs /v1/_teamdef/names and returns the summaries", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        names: [{ name: "triage", version_count: 2, latest_version: 2 }],
      }),
    ]);

    const res = await client.listTeams();
    expect(res.names?.[0]!.name).toBe("triage");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_teamdef/names");
    expect((call[1] as RequestInit).method).toBe("GET");
  });

  it("tolerates a null names list (empty tenant)", async () => {
    const { client } = makeClient([jsonResponse({ names: null })]);
    const res = await client.listTeams();
    expect(res.names).toBeNull();
  });
});

describe("renderTeamDiagram", () => {
  it("POSTs op=render_diagram + highlight_state to /v1/_teamdef", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "triage", def_id: "team_1", format: "mermaid", diagram: "stateDiagram-v2" }),
    ]);

    const res = await client.renderTeamDiagram("triage", { highlightState: "review" });
    expect(res.diagram).toContain("stateDiagram");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_teamdef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("render_diagram");
    expect(body.name).toBe("triage");
    expect(body.highlight_state).toBe("review");
  });
});

describe("getTeamDef", () => {
  it("POSTs op=get by def_id and returns the record incl. definition", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ def_id: "team_1", name: "triage", version: 1, definition: { entry: "start" } }),
    ]);

    const res = await client.getTeamDef("team_1");
    expect((res.definition as Record<string, unknown>).entry).toBe("start");

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.op).toBe("get");
    expect(body.def_id).toBe("team_1");
  });
});

describe("createTeam / forkTeam", () => {
  it("createTeam POSTs op=create with the overlay graph", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ def_id: "team_1", name: "triage", version: 1 }),
    ]);

    const res = await client.createTeam("triage", { entry: "start", states: [], transitions: [] });
    expect(res.version).toBe(1);

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.op).toBe("create");
    expect(body.name).toBe("triage");
    expect(body.overlay.entry).toBe("start");
  });

  it("forkTeam POSTs op=fork with the overlay graph", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ def_id: "team_2", name: "triage", version: 2 }),
    ]);

    await client.forkTeam("triage", { entry: "start", states: [], transitions: [] });
    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.op).toBe("fork");
  });

  it("createTeam surfaces a 422 graph refusal as SubstrateToolRefusedError", async () => {
    const { client } = makeClient([
      errorResponse(422, JSON.stringify({ code: "tool_refused", tool: "TeamDef", error: "entry state not found" })),
    ]);
    await expect(client.createTeam("bad", { entry: "nope" })).rejects.toBeInstanceOf(
      SubstrateToolRefusedError,
    );
  });
});

describe("deleteTeam", () => {
  it("POSTs op=delete by name", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ name: "triage", deleted: true })]);
    const res = await client.deleteTeam("triage");
    expect(res.deleted).toBe(true);
    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.op).toBe("delete");
    expect(body.name).toBe("triage");
  });
});

describe("runTeam", () => {
  it("POSTs op=run with name + input and returns the trace", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "triage",
        def_id: "team_1",
        status: "completed",
        final_output: "done",
        steps: [{ state: "start", agent: "worker", next: "" }],
      }),
    ]);

    const res = await client.runTeam({ name: "triage", input: "handle this" });
    expect(res.status).toBe("completed");
    expect(res.steps).toHaveLength(1);

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.op).toBe("run");
    expect(body.name).toBe("triage");
    expect(body.input).toBe("handle this");
    // Absent target fields must be omitted (not sent as null/undefined keys).
    expect("def_id" in body).toBe(false);
  });

  it("targets a specific version by def_id", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "triage", def_id: "team_9", status: "completed", steps: [] }),
    ]);
    await client.runTeam({ defId: "team_9" });
    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.def_id).toBe("team_9");
    expect("name" in body).toBe(false);
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(client.runTeam({ name: "triage" })).rejects.toBeInstanceOf(AuthError);
  });
});
