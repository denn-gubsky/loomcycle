import { describe, expect, it } from "vitest";
import { LoomcycleError } from "../src/errors.js";
import {
  errorResponse,
  jsonResponse,
  makeClient,
  noContentResponse,
} from "./helpers.js";

// v0.11.5 — Channel admin CRUD (createChannel / updateChannel /
// deleteChannel). The adapter is a thin wrapper around POST / PATCH /
// DELETE, so the tests focus on:
//
//   1. URL path + method + body shape are right (n8n integration
//      uses these to build channels from a typed node config).
//   2. Refusals — yaml-immutable and name-in-use — surface as a
//      typed error the consumer can catch.

describe("createChannel", () => {
  it("POSTs to /v1/_channels with the full body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "briefing-ready",
        description: "Researcher signals editor",
        scope: "global",
        semantic: "queue",
        default_ttl: 3600,
        max_messages: 100,
        message_count: 0,
        source: "runtime",
      }, 201),
    ]);

    const desc = await client.createChannel({
      name: "briefing-ready",
      description: "Researcher signals editor",
      scope: "global",
      semantic: "queue",
      default_ttl: 3600,
      max_messages: 100,
    });

    expect(desc.name).toBe("briefing-ready");
    expect(desc.source).toBe("runtime");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_channels");
    expect(init.method).toBe("POST");
    const body = JSON.parse(init.body as string);
    expect(body).toEqual({
      name: "briefing-ready",
      description: "Researcher signals editor",
      scope: "global",
      semantic: "queue",
      default_ttl: 3600,
      max_messages: 100,
    });
  });

  it("surfaces 409 channel_yaml_immutable as a typed error", async () => {
    const { client } = makeClient([
      errorResponse(409, `{"code":"channel_yaml_immutable","error":"channel is declared in operator yaml"}`),
    ]);
    await expect(
      client.createChannel({ name: "_system/alarms", scope: "global", semantic: "queue" }),
    ).rejects.toBeInstanceOf(LoomcycleError);
  });
});

describe("updateChannel", () => {
  it("PATCHes the name path with the partial body", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "patch-me",
        description: "updated",
        scope: "global",
        semantic: "queue",
        max_messages: 500,
        message_count: 7,
        source: "runtime",
      }),
    ]);

    const desc = await client.updateChannel("patch-me", {
      description: "updated",
      max_messages: 500,
    });

    expect(desc.description).toBe("updated");
    expect(desc.max_messages).toBe(500);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_channels/patch-me");
    expect(init.method).toBe("PATCH");
    const body = JSON.parse(init.body as string);
    expect(body).toEqual({ description: "updated", max_messages: 500 });
  });

  it("URL-encodes the name segment", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ name: "a/b", message_count: 0 })]);
    await client.updateChannel("a/b", { description: "x" });
    const [url] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_channels/a%2Fb");
  });
});

describe("deleteChannel", () => {
  it("DELETEs the name path", async () => {
    const { client, fetchMock } = makeClient([noContentResponse()]);
    await client.deleteChannel("delete-me");
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_channels/delete-me");
    expect(init.method).toBe("DELETE");
  });

  it("surfaces 404 as NotFoundError", async () => {
    const { client } = makeClient([
      errorResponse(404, `{"code":"channel_not_found","error":"channel not found"}`),
    ]);
    await expect(client.deleteChannel("missing")).rejects.toBeInstanceOf(LoomcycleError);
  });
});

// F20 — purge clears buffered messages (allowed on yaml channels, unlike
// delete) and returns the cleared count.
describe("purgeChannel", () => {
  it("POSTs to /{name}/purge and returns the cleared count", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "team-updates", purged: 7 }),
    ]);
    const res = await client.purgeChannel("team-updates");
    expect(res).toEqual({ name: "team-updates", purged: 7 });
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("http://test-loomcycle:8787/v1/_channels/team-updates/purge");
    expect(init.method).toBe("POST");
  });

  it("surfaces 404 as a typed error", async () => {
    const { client } = makeClient([
      errorResponse(404, `{"code":"channel_not_found","error":"channel not found"}`),
    ]);
    await expect(client.purgeChannel("ghost")).rejects.toBeInstanceOf(LoomcycleError);
  });
});
