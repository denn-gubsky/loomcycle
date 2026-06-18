// adapters/ts/tests/volumes.test.ts — v0.35.0 RFC AH dynamic
// filesystem-volume methods (volumeDef + listVolumes +
// listEphemeralVolumes). Mirrors substrate.test.ts / library.test.ts.

import { describe, expect, it } from "vitest";

import { jsonResponse, makeClient, errorResponse } from "./helpers.js";
import {
  AuthError,
  SubstrateToolRefusedError,
  UnavailableError,
  type PersistentVolumesResponse,
  type EphemeralVolumesResponse,
} from "../src/index.js";

describe("volumeDef", () => {
  it("posts JSON to /v1/_volumedef and returns the row envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        name: "repo-a",
        path: "/pool/_shared/repo-a",
        mode: "rw",
      }),
    ]);

    const result = (await client.volumeDef({
      op: "create",
      name: "repo-a",
      mode: "rw",
    })) as Record<string, unknown>;

    expect(result.name).toBe("repo-a");
    expect(result.mode).toBe("rw");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_volumedef");
    expect((call[1] as RequestInit).method).toBe("POST");
    const body = JSON.parse((call[1] as RequestInit).body as string);
    expect(body.op).toBe("create");
    expect(body.name).toBe("repo-a");
    expect(body.mode).toBe("rw");
  });

  it("forwards bearer auth in the Authorization header", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ok: true })]);
    await client.volumeDef({ op: "list" });
    const headers = (fetchMock.mock.calls[0]![1] as RequestInit)
      .headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-bearer");
  });

  it("supports the destructive delete + purge ops", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "repo-a", deleted: true }),
      jsonResponse({ name: "repo-a", purged: true }),
    ]);
    await client.volumeDef({ op: "delete", name: "repo-a" });
    await client.volumeDef({ op: "purge", name: "repo-a" });
    expect(JSON.parse((fetchMock.mock.calls[0]![1] as RequestInit).body as string).op).toBe(
      "delete",
    );
    expect(JSON.parse((fetchMock.mock.calls[1]![1] as RequestInit).body as string).op).toBe(
      "purge",
    );
  });

  it("raises SubstrateToolRefusedError on 422 with code=tool_refused", async () => {
    const refusalBody = JSON.stringify({
      code: "tool_refused",
      tool: "VolumeDef",
      error: "name 'pool' collides with a static volume; static volumes are immutable",
    });
    const { client } = makeClient([
      () =>
        new Response(refusalBody, {
          status: 422,
          headers: { "Content-Type": "application/json" },
        }),
    ]);

    let caught: unknown;
    try {
      await client.volumeDef({ op: "create", name: "pool", mode: "rw" });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(SubstrateToolRefusedError);
    const err = caught as SubstrateToolRefusedError;
    expect(err.tool).toBe("VolumeDef");
    expect(err.message).toContain("collides with a static volume");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(
      client.volumeDef({ op: "create", name: "x", mode: "rw" }),
    ).rejects.toBeInstanceOf(AuthError);
  });
});

describe("listVolumes", () => {
  it("GETs /v1/_volumes and returns the typed envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        entries: [
          {
            name: "pool",
            source: "static",
            path: "/srv/pool",
            mode: "rw",
            default: false,
            dynamic_root: true,
          },
          {
            name: "repo-a",
            source: "dynamic",
            path: "/srv/pool/acme/repo-a",
            mode: "rw",
            default: false,
            dynamic_root: false,
            created_at: "2026-06-18T00:00:00.000000000Z",
          },
        ],
      }),
    ]);

    const resp: PersistentVolumesResponse = await client.listVolumes();
    expect(resp.entries).toHaveLength(2);
    expect(resp.entries[0]!.source).toBe("static");
    expect(resp.entries[0]!.dynamic_root).toBe(true);
    expect(resp.entries[1]!.source).toBe("dynamic");
    expect(resp.entries[1]!.created_at).toBeTruthy();

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_volumes");
    expect((call[1] as RequestInit).method ?? "GET").toBe("GET");
  });

  it("tolerates redacted (empty) host paths for a non-operator caller", async () => {
    const { client } = makeClient([
      jsonResponse({
        entries: [
          { name: "pool", source: "static", path: "", mode: "rw", default: true, dynamic_root: false },
        ],
      }),
    ]);
    const resp = await client.listVolumes();
    expect(resp.entries[0]!.path).toBe("");
    expect(resp.entries[0]!.name).toBe("pool");
  });

  it("raises UnavailableError on 503 (store unwired)", async () => {
    const { client } = makeClient([errorResponse(503, "store_unavailable")]);
    await expect(client.listVolumes()).rejects.toBeInstanceOf(UnavailableError);
  });
});

describe("listEphemeralVolumes", () => {
  it("GETs /v1/_volumes/ephemeral and returns the typed envelope", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({
        entries: [
          {
            name: "work",
            root_run_id: "run-1",
            path: "/srv/pool/_ephemeral/run-1/work",
            mode: "rw",
            created_at: "2026-06-18T00:00:00.000000000Z",
          },
        ],
      }),
    ]);

    const resp: EphemeralVolumesResponse = await client.listEphemeralVolumes();
    expect(resp.entries).toHaveLength(1);
    expect(resp.entries[0]!.root_run_id).toBe("run-1");

    const call = fetchMock.mock.calls[0]!;
    expect(call[0]).toBe("http://test-loomcycle:8787/v1/_volumes/ephemeral");
  });

  it("raises AuthError on 401", async () => {
    const { client } = makeClient([errorResponse(401, "invalid token")]);
    await expect(client.listEphemeralVolumes()).rejects.toBeInstanceOf(AuthError);
  });
});
