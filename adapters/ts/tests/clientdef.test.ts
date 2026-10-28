/**
 * ClientDef (RFC FL): the app-client methods post the op the server
 * dispatches, with the fields it reads, and read its answers back.
 */

import { describe, expect, it } from "vitest";
import { SubstrateToolRefusedError } from "../src/index.js";
import type { AppClientConfig, ClientDefRecord } from "../src/index.js";
import { errorResponse, jsonResponse, makeClient } from "./helpers.js";

const CLIENT: AppClientConfig = {
  kind: "public",
  label: "Loomboard",
  redirect_uris: ["https://board.test/auth/callback", "http://127.0.0.1/callback"],
  allowed_origins: ["https://board.test"],
  access_token_ttl: "1h",
  refresh: { idle: "7d", absolute: "30d" },
};

const RECORD: ClientDefRecord = {
  name: "loomboard",
  source: "def",
  def_id: "cdef_1",
  version: 1,
  tenant_id: "acme",
  all_tenants: false,
  label: "Loomboard",
  redirect_uris: CLIENT.redirect_uris,
  client: CLIENT,
  description: "the board",
  content_sha256: "ab12",
  created_at: "2026-10-27T00:00:00Z",
  created_by: "ops",
  created_by_tenant: "acme",
  read_only: false,
};

function sent(fetchMock: ReturnType<typeof makeClient>["fetchMock"], i = 0) {
  const [url, init] = fetchMock.mock.calls[i]! as [string, RequestInit];
  return { url, method: init.method, body: init.body ? JSON.parse(init.body as string) : undefined, init };
}

describe("ClientDef methods", () => {
  it("createClientDef posts op create with the client, description and tenant", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(RECORD)]);
    const row = await client.createClientDef("loomboard", CLIENT, { description: "the board", tenant: "acme" });
    expect(row.def_id).toBe("cdef_1");
    const s = sent(fetchMock);
    expect(s.url).toBe("http://test-loomcycle:8787/v1/_clientdef");
    expect(s.method).toBe("POST");
    expect(s.body).toEqual({ op: "create", name: "loomboard", client: CLIENT, description: "the board", tenant: "acme" });
    expect((s.init.headers as Record<string, string>).Authorization).toBe("Bearer test-bearer");
  });

  it("createClientDef sends an explicit every-tenant \"\" and leaves an absent tenant out", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(RECORD), jsonResponse(RECORD)]);
    await client.createClientDef("loomboard", CLIENT, { tenant: "" });
    await client.createClientDef("loomboard", CLIENT);
    expect(sent(fetchMock, 0).body.tenant).toBe("");
    expect("tenant" in sent(fetchMock, 1).body).toBe(false);
  });

  it("updateClientDef posts op update", async () => {
    const { client, fetchMock } = makeClient([jsonResponse({ ...RECORD, version: 2 })]);
    const row = await client.updateClientDef("loomboard", CLIENT);
    expect(row.version).toBe(2);
    expect(sent(fetchMock).body).toEqual({ op: "update", name: "loomboard", client: CLIENT });
  });

  it("getClientDef addresses by name or def_id", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(RECORD), jsonResponse(RECORD)]);
    await client.getClientDef({ name: "loomboard" });
    await client.getClientDef({ def_id: "cdef_1" });
    expect(sent(fetchMock, 0).body).toEqual({ op: "get", name: "loomboard" });
    expect(sent(fetchMock, 1).body).toEqual({ op: "get", def_id: "cdef_1" });
  });

  it("listClientDefs reads config and runtime clients and passes a tenant focus", async () => {
    const config = {
      name: "cli",
      source: "config",
      label: "CLI",
      all_tenants: true,
      tenant_id: "",
      redirect_uris: ["http://127.0.0.1/cb"],
      allowed_origins: [],
      access_token_ttl: "1h0m0s",
      refresh_idle: "168h0m0s",
      refresh_absolute: "720h0m0s",
      read_only: true,
    };
    const { client, fetchMock } = makeClient([jsonResponse({ clients: [config, RECORD] })]);
    const res = await client.listClientDefs({ tenant: "acme" });
    expect(res.clients.map((c) => c.source)).toEqual(["config", "def"]);
    const first = res.clients[0]!;
    if (first.source === "config") expect(first.refresh_idle).toBe("168h0m0s");
    expect(sent(fetchMock).body).toEqual({ op: "list", tenant: "acme" });
  });

  it("retireClientDef and verifyClientDef post their ops", async () => {
    const { client, fetchMock } = makeClient([
      jsonResponse({ name: "loomboard", retired: true }),
      jsonResponse({ name: "loomboard", deployed: true, current_def_id: "cdef_1", current_sha256: "ab12", version: 1, matches: true }),
      jsonResponse({ name: "gone", deployed: false, matches: false }),
    ]);
    await expect(client.retireClientDef("loomboard")).resolves.toEqual({ name: "loomboard", retired: true });
    const v = await client.verifyClientDef("loomboard", { contentSha256: "ab12" });
    expect(v.matches).toBe(true);
    const absent = await client.verifyClientDef("gone");
    expect(absent.deployed).toBe(false);
    expect(sent(fetchMock, 0).body).toEqual({ op: "retire", name: "loomboard" });
    expect(sent(fetchMock, 1).body).toEqual({ op: "verify", name: "loomboard", content_sha256: "ab12" });
    expect(sent(fetchMock, 2).body).toEqual({ op: "verify", name: "gone" });
  });

  it("a refusal raises SubstrateToolRefusedError", async () => {
    const { client } = makeClient([
      errorResponse(422, JSON.stringify({ code: "tool_refused", error: 'create: the name "x" is not available; choose another', tool: "ClientDef" })),
    ]);
    await expect(client.createClientDef("x", CLIENT)).rejects.toBeInstanceOf(SubstrateToolRefusedError);
  });

  it("listClientDefNames GETs the names route, an empty tenant being a focus", async () => {
    const names = { names: [{ name: "loomboard", source: "def", tenant_id: "acme", label: "Loomboard", read_only: false }] };
    const { client, fetchMock } = makeClient([jsonResponse(names), jsonResponse(names), jsonResponse(names)]);
    await expect(client.listClientDefNames()).resolves.toEqual(names);
    await client.listClientDefNames({ tenant: "acme" });
    await client.listClientDefNames({ tenant: "" });
    expect(fetchMock.mock.calls.map((c) => [(c[1] as RequestInit).method, c[0]])).toEqual([
      ["GET", "http://test-loomcycle:8787/v1/_clientdef/names"],
      ["GET", "http://test-loomcycle:8787/v1/_clientdef/names?tenant=acme"],
      ["GET", "http://test-loomcycle:8787/v1/_clientdef/names?tenant="],
    ]);
  });
});
