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

  // A deployed team's sweep is typed: no cast to read runnable or an issue.
  it("types the deployed version's runnable and issues", async () => {
    const { client } = makeClient([
      jsonResponse({
        name: "triage", matches: true, deployed: true, current_sha256: "sha256:abc",
        current_def_id: "tdf_2", version: 2, runnable: false,
        issues: [{
          kind: "channel_undeclared", severity: "unrunnable", state: "intake", field: "source",
          channel: "inbox", path: "states[0].handler.source.channel", detail: "channel \"inbox\" is no longer declared",
        }],
      }),
    ]);
    const res = await client.verifyTeam("triage", "sha256:abc");
    expect(res.runnable).toBe(false);
    expect(res.issues?.[0]?.kind).toBe("channel_undeclared");
    expect(res.issues?.[0]?.path).toBe("states[0].handler.source.channel");
  });

  // A draft is sent as the overlay a save would send, never with a hash.
  it("checks a draft: sends the overlay and the draft's options, no hash", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "triage", matches: false, deployed: true, current_sha256: "sha256:abc",
        current_def_id: "tdf_2", version: 2, valid: false, runnable: false,
        checked_as: "fork", parent_def_id: "tdf_2", content_sha256: "sha256:def",
        issues: [{ kind: "graph_invalid", severity: "refused", path: "entry", detail: "team definition: `entry` is required" }],
      }),
    ]);
    const overlay = { states: [] };
    const res = await client.verifyTeam("triage", { overlay, as: "fork", parentDefId: "tdf_2", description: "why" });
    expect(res.valid).toBe(false);
    expect(res.checked_as).toBe("fork");
    expect(res.issues[0]?.severity).toBe("refused");

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body).toEqual({ op: "verify", name: "triage", overlay, as: "fork", parent_def_id: "tdf_2", description: "why" });
  });
});

// ---- the debug surface: a walk is a run, and can be armed while it runs ----

describe("runTeam debug arguments", () => {
  it("passes mode + breakpoints through as snake_case", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "triage", def_id: "team_1", run_id: "r_1", status: "running" }),
    ]);

    const res = await client.runTeam({
      name: "triage",
      input: "go",
      mode: "detach",
      breakpoints: ["wave", "review:before_dispatch"],
    });
    // Detaching returns the HANDLE, not a trace — that is the point: op=run is
    // otherwise synchronous, so there is no moment at which a caller can arm a
    // breakpoint or read a pause on a walk that is still running.
    expect(res.run_id).toBe("r_1");
    expect(res.status).toBe("running");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_teamdef");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body).toMatchObject({
      op: "run",
      name: "triage",
      input: "go",
      mode: "detach",
      breakpoints: ["wave", "review:before_dispatch"],
    });
  });

  it("omits mode + breakpoints when unset, so an ordinary run is unchanged", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "triage", def_id: "team_1", status: "completed", steps: [] }),
    ]);

    await client.runTeam({ name: "triage", input: "go" });

    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect("mode" in body).toBe(false);
    expect("breakpoints" in body).toBe(false);
  });

  it("surfaces run_id on the synchronous path too", async () => {
    // A caller that waits for the walk can still debug it from a second
    // connection — the id is what every run surface keys on.
    const { client } = makeClient([
      jsonResponse({ name: "triage", def_id: "team_1", run_id: "r_2", status: "completed", steps: [] }),
    ]);
    const res = await client.runTeam({ name: "triage", input: "go" });
    expect(res.run_id).toBe("r_2");
  });
});

describe("runTeam vars", () => {
  it("passes vars through, and omits them when unset", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "triage", def_id: "team_1", status: "completed", steps: [] }),
      jsonResponse({ name: "triage", def_id: "team_1", status: "completed", steps: [] }),
    ]);

    await client.runTeam({ name: "triage", input: "go", vars: { tone: "casual", lang: "" } });
    const sent = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(sent).toMatchObject({ op: "run", name: "triage", input: "go", vars: { tone: "casual", lang: "" } });

    await client.runTeam({ name: "triage", input: "go" });
    const bare = JSON.parse((fetchMock.mock.calls[1]![1] as RequestInit).body as string);
    expect("vars" in bare).toBe(false);
  });
});

describe("getRunBreakpoints / setRunBreakpoints", () => {
  it("GETs the armed set for a run", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "r_1", armed: ["wave:before_dispatch"] }),
    ]);

    const res = await client.getRunBreakpoints("r_1");
    expect(res.armed).toEqual(["wave:before_dispatch"]);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs/r_1/breakpoints");
    expect((call[1] as RequestInit).method).toBe("GET");
  });

  it("PUTs the WHOLE set, not a delta", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "r_1", armed: ["wave:before_dispatch", "wave:review"] }),
    ]);

    await client.setRunBreakpoints("r_1", ["wave"]);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/runs/r_1/breakpoints");
    expect((call[1] as RequestInit).method).toBe("PUT");
    expect(JSON.parse((call[1] as RequestInit).body as string)).toEqual({ breakpoints: ["wave"] });
  });

  it("sends an empty list as the off switch rather than omitting the field", async () => {
    // Omitting it would read as "no change" on a PUT that means "replace".
    const { client, fetchMock } = makeClient([jsonResponse({ run_id: "r_1", armed: [] })]);
    await client.setRunBreakpoints("r_1", []);
    expect(JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string)).toEqual({
      breakpoints: [],
    });
  });

  it("sends review_ttl_seconds with the set when asked, and reads it back", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "r_1", armed: ["wave:review"], review_ttl_seconds: 600 }),
    ]);
    const res = await client.setRunBreakpoints("r_1", ["wave:review"], { reviewTtlSeconds: 600 });
    expect(JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string)).toEqual({
      breakpoints: ["wave:review"],
      review_ttl_seconds: 600,
    });
    expect(res.review_ttl_seconds).toBe(600);
  });

  it("sends 0 as no deadline rather than dropping it", async () => {
    // Omitting it would read as "leave the deadline as it is".
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "r_1", armed: [], review_ttl_seconds: 0 }),
    ]);
    await client.setRunBreakpoints("r_1", [], { reviewTtlSeconds: 0 });
    expect(JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string)).toEqual({
      breakpoints: [],
      review_ttl_seconds: 0,
    });
  });

  it("leaves the deadline out when not asked to change it", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ run_id: "r_1", armed: ["wave:before_dispatch"], review_ttl_seconds: 90 }),
    ]);
    await client.setRunBreakpoints("r_1", ["wave"], { signal: new AbortController().signal });
    const body = JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string);
    expect(body).not.toHaveProperty("review_ttl_seconds");
  });

  it("percent-encodes the run id into the path", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ run_id: "a/b", armed: [] })]);
    await client.getRunBreakpoints("a/b");
    expect(fetchMock.mock.calls[0]![0]).toBe("http://test-loomcycle:8787/v1/runs/a%2Fb/breakpoints");
  });

  it("propagates a 404 for a run with no live walk", async () => {
    // An arming that silently did nothing is worse than a refusal, so the
    // client must not swallow this into an empty set.
    const { client } = makeClient([
      // errorResponse takes a bodyText STRING: passing the object sent
      // "[object Object]" as the body, so the assertion below could only ever
      // check that something threw.
      errorResponse(
        404,
        JSON.stringify({ code: "no_live_walk", error: "no live team walk for that run_id" }),
      ),
    ]);
    await expect(client.setRunBreakpoints("r_gone", ["wave"])).rejects.toMatchObject({
      status: 404,
      bodyText: expect.stringContaining("no_live_walk"),
    });
  });
});

describe("listTeamChannels", () => {
  it("GETs the team's channels and returns their definitions and counts", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        team: "triage",
        channels: [
          {
            name: "journal",
            scope: "user",
            hold: true,
            declared_in: "active",
            def_id: "tdf_1",
            version: 3,
            message_count: 4,
            held_count: 2,
          },
        ],
      }),
    ]);

    const res = await client.listTeamChannels("triage");
    expect(res.team).toBe("triage");
    expect(res.channels[0]!.name).toBe("journal");
    expect(res.channels[0]!.declared_in).toBe("active");
    expect(res.channels[0]!.held_count).toBe(2);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_teamdef/triage/channels");
    expect((call[1] as RequestInit).method).toBe("GET");
  });

  it("names the tenant only when asked to", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ team: "triage", channels: [] })]);
    const res = await client.listTeamChannels("triage", { tenant: "acme" });
    expect(res.channels).toEqual([]);
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_teamdef/triage/channels?tenant=acme",
    );
  });
});

describe("peekTeamChannel", () => {
  it("GETs the peek by team and local name, with its query options", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        team: "triage",
        name: "journal",
        scope: "user",
        declared_in: "active",
        messages: [{ id: "m1", value: { note: "hi" }, published_at: "2026-10-08T00:00:00Z" }],
      }),
    ]);

    const res = await client.peekTeamChannel("triage", "journal", {
      userId: "alice",
      fromCursor: "cur_2",
      maxMessages: 5,
    });
    expect(res.messages[0]!.value).toEqual({ note: "hi" });
    expect(res.declared_in).toBe("active");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe(
      "http://test-loomcycle:8787/v1/_teamdef/triage/channels/journal/peek?user_id=alice&from_cursor=cur_2&max_messages=5",
    );
    expect((call[1] as RequestInit).method).toBe("GET");
  });

  it("sends no query when no option is set, and escapes both names", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ team: "a b", name: "x/y", scope: "tenant", declared_in: "retired", messages: [] }),
    ]);
    await client.peekTeamChannel("a b", "x/y");
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_teamdef/a%20b/channels/x%2Fy/peek",
    );
  });
});

describe("releaseTeamChannel", () => {
  it("POSTs the release by team and local name with its count", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ team: "triage", name: "inbox", released: ["m1", "m2"], released_count: 2, still_held: 3 }),
    ]);

    const res = await client.releaseTeamChannel("triage", "inbox", { count: 2, userId: "alice" });
    expect(res.released).toEqual(["m1", "m2"]);
    expect(res.still_held).toBe(3);

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_teamdef/triage/channels/inbox/release");
    expect((call[1] as RequestInit).method).toBe("POST");
    expect(JSON.parse((call[1] as RequestInit).body as string)).toEqual({ count: 2, user_id: "alice" });
  });

  it("sends an empty body to release one, and names the tenant only when asked", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ team: "triage", name: "inbox", released: [], released_count: 0, still_held: 0 }),
    ]);
    const res = await client.releaseTeamChannel("triage", "inbox", { tenant: "acme" });
    expect(res.released).toEqual([]);
    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe(
      "http://test-loomcycle:8787/v1/_teamdef/triage/channels/inbox/release?tenant=acme",
    );
    expect(JSON.parse((call[1] as RequestInit).body as string)).toEqual({});
  });
});
