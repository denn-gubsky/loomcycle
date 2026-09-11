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

// The version-lifecycle ops (list / promote / retire / verify). They existed on
// the substrate tool from the start and were reachable over HTTP, but a client
// that could author a team could not put one in force or check it for drift —
// so a workflow kept in source control had no way to ask "is what I have what
// is deployed?" without hand-rolling the POST.

describe("listTeamVersions", () => {
  it("POSTs op=list by name and returns the lineage", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "triage",
        versions: [
          { def_id: "tdf_2", name: "triage", version: 2, parent_def_id: "tdf_1" },
          { def_id: "tdf_1", name: "triage", version: 1 },
        ],
      }),
    ]);

    const res = await client.listTeamVersions("triage");
    expect(res.versions).toHaveLength(2);
    expect(res.versions[0]!.parent_def_id).toBe("tdf_1");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_teamdef");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("list");
    expect(body.name).toBe("triage");
  });

  it("tolerates a name with no versions", async () => {
    const { client } = makeClient([jsonResponse({ name: "ghost", versions: [] })]);
    const res = await client.listTeamVersions("ghost");
    expect(res.versions).toEqual([]);
  });
});

describe("promoteTeam", () => {
  it("POSTs op=promote by def_id", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ def_id: "tdf_2", name: "triage", promoted: true }),
    ]);

    const res = await client.promoteTeam("tdf_2");
    expect(res.promoted).toBe(true);

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.op).toBe("promote");
    expect(body.def_id).toBe("tdf_2");
  });
});

describe("retireTeam", () => {
  it("POSTs op=retire with the required retired flag", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ def_id: "tdf_1", retired: true })]);

    const res = await client.retireTeam("tdf_1", true);
    expect(res.retired).toBe(true);

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.op).toBe("retire");
    expect(body.def_id).toBe("tdf_1");
    expect(body.retired).toBe(true);
  });

  // Retiring is reversible; `retired: false` must reach the wire as false
  // rather than being dropped as a falsy value, or un-retiring would silently
  // become a no-op that reports success.
  it("sends retired:false to un-retire", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ def_id: "tdf_1", retired: false })]);

    await client.retireTeam("tdf_1", false);

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.retired).toBe(false);
    expect("retired" in body).toBe(true);
  });
});

describe("verifyTeam", () => {
  it("POSTs op=verify with the local hash and reports a match", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "triage",
        matches: true,
        deployed: true,
        current_sha256: "sha256:abc",
        current_def_id: "tdf_2",
        version: 2,
      }),
    ]);

    const res = await client.verifyTeam("triage", "sha256:abc");
    expect(res.matches).toBe(true);

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.op).toBe("verify");
    expect(body.name).toBe("triage");
    expect(body.content_sha256).toBe("sha256:abc");
  });

  // An absent team is an ANSWER, not an error — and it is a different answer
  // from a deployed version whose hash differs. A caller that conflated them
  // would report drift on a team that was never deployed.
  it("reports deployed:false for a name with no active version", async () => {
    const { client } = makeClient([
      jsonResponse({
        name: "ghost",
        matches: false,
        deployed: false,
        current_sha256: "",
        current_def_id: "",
        version: 0,
      }),
    ]);

    const res = await client.verifyTeam("ghost", "sha256:abc");
    expect(res.deployed).toBe(false);
    expect(res.matches).toBe(false);
  });
});
