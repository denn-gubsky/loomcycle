// adapters/ts/tests/library.test.ts — v0.10.3 Library v2 enumeration
// methods. Mirrors substrate.test.ts shape.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, errorResponse } from "./helpers.js";
import {
  AuthError,
  UnavailableError,
  type LibraryListResponse,
  type LibraryAgentDefinition,
  type LibrarySkillDefinition,
  type LibraryMcpServerDefinition,
} from "../src/index.js";

describe("listLibraryAgents", () => {
  it("GETs /v1/_library/agents and returns the typed envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        entries: [
          {
            name: "researcher",
            source: "static-only",
            in_static: true,
            in_substrate: false,
            version_count: 0,
            static_definition: {
              provider: "anthropic",
              model: "claude-opus-4-7",
              tier: "research",
              allowed_tools: ["WebSearch", "Read"],
              skills: ["literature-review"],
            },
          },
          {
            name: "summariser",
            source: "both",
            in_static: true,
            in_substrate: true,
            version_count: 3,
            active_def_id: "def_abc",
            latest_version: 3,
            last_updated: "2026-05-20T12:00:00Z",
            static_definition: {
              provider: "openai",
              model: "gpt-4o-mini",
            },
          },
          {
            name: "scrubber",
            source: "dynamic-only",
            in_static: false,
            in_substrate: true,
            version_count: 1,
            active_def_id: "def_xyz",
            latest_version: 1,
            last_updated: "2026-05-22T08:00:00Z",
          },
        ],
      }),
    ]);

    const result: LibraryListResponse<LibraryAgentDefinition> =
      await client.listLibraryAgents();

    expect(result.entries).toHaveLength(3);
    expect(result.entries[0]!.name).toBe("researcher");
    expect(result.entries[0]!.source).toBe("static-only");
    expect(result.entries[0]!.static_definition?.allowed_tools).toEqual([
      "WebSearch",
      "Read",
    ]);
    expect(result.entries[1]!.source).toBe("both");
    expect(result.entries[1]!.version_count).toBe(3);
    expect(result.entries[2]!.static_definition).toBeUndefined();

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_library/agents");
    expect((call[1] as RequestInit).method).toBe("GET");
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ entries: [] }),
    ]);
    await client.listLibraryAgents();
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(client.listLibraryAgents()).rejects.toBeInstanceOf(AuthError);
  });

  it("raises UnavailableError on 503", async () => {
    const { client } = makeClient([
      errorResponse(503, "store unwired"),
    ]);
    await expect(client.listLibraryAgents()).rejects.toBeInstanceOf(
      UnavailableError,
    );
  });

  it("handles an empty entries array (the n8n empty-dropdown case)", async () => {
    const { client } = makeClient([jsonResponse({ entries: [] })]);
    const result = await client.listLibraryAgents();
    expect(result.entries).toEqual([]);
  });

  it("passes signal through to the underlying fetch", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ entries: [] }),
    ]);
    const controller = new AbortController();
    await client.listLibraryAgents({ signal: controller.signal });
    expect((fetchMock.mock.calls[0]![1] as RequestInit).signal).toBe(
      controller.signal,
    );
  });
});

describe("listLibrarySkills", () => {
  it("GETs /v1/_library/skills with the skill-typed envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        entries: [
          {
            name: "literature-review",
            source: "static-only",
            in_static: true,
            in_substrate: false,
            version_count: 0,
            static_definition: {
              description: "Survey academic sources",
              allowed_tools: ["WebSearch", "WebFetch"],
              body: "## Literature review skill\n...",
            },
          },
        ],
      }),
    ]);
    const result: LibraryListResponse<LibrarySkillDefinition> =
      await client.listLibrarySkills();
    expect(result.entries[0]!.static_definition?.description).toBe(
      "Survey academic sources",
    );
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_library/skills",
    );
  });
});

describe("listLibraryMcpServers", () => {
  it("GETs /v1/_library/mcp-servers with the mcp-server-typed envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        entries: [
          {
            name: "jobs",
            source: "static-only",
            in_static: true,
            in_substrate: false,
            version_count: 0,
            static_definition: {
              transport: "streamable-http",
              url: "http://localhost:3000/api/mcp",
              allowed_tools: ["getAgentContext", "patchApplication"],
              discovered_tools: [{ name: "getAgentContext", description: "..." }],
            },
          },
        ],
      }),
    ]);
    const result: LibraryListResponse<LibraryMcpServerDefinition> =
      await client.listLibraryMcpServers();
    expect(result.entries[0]!.static_definition?.transport).toBe(
      "streamable-http",
    );
    expect(result.entries[0]!.static_definition?.url).toBe(
      "http://localhost:3000/api/mcp",
    );
    expect(fetchMock.mock.calls[0]![0]).toBe(
      "http://test-loomcycle:8787/v1/_library/mcp-servers",
    );
  });
});
