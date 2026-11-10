// RFC GJ: the client-tool socket presents a one-time ticket, minted per
// connect, and falls back to the bearer subprotocol only on a server without
// tickets that does not run strict sign-in.

import { describe, expect, it, vi } from "vitest";
import { LoomcycleClient } from "../src/client.js";
import type { WebSocketCtor, WebSocketLike } from "../src/client-tools.js";
import { LoomcycleError } from "../src/errors.js";
import { errorResponse, jsonResponse, makeClient } from "./helpers.js";

/** A WebSocket stand-in that records the subprotocols of each open. */
function fakeWS() {
  const opened: string[][] = [];
  class FakeWS implements WebSocketLike {
    onopen = null;
    onclose = null;
    onerror = null;
    onmessage = null;
    constructor(_url: string, protocols?: string | string[]) {
      opened.push([protocols ?? []].flat());
    }
    send(): void {}
    close(): void {}
  }
  return { opened, WS: FakeWS as unknown as WebSocketCtor };
}

const ticketBody = (t: string) => ({ ticket: t, expires_at: "2026-11-10T12:00:30Z" });
const whoami = (mode: string) => ({ tenant_id: "acme", subject: "alice", scopes: [], is_admin: false, auth: { mode } });

describe("clientToolsTicket", () => {
  it("POSTs the mint route with the bearer and maps expires_at", async () => {
    const { client, fetchMock } = makeClient([jsonResponse(ticketBody("lct1.p.m"))]);
    await expect(client.clientToolsTicket()).resolves.toEqual({ ticket: "lct1.p.m", expiresAt: "2026-11-10T12:00:30Z" });
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://test-loomcycle:8787/v1/client-tools/ticket");
    expect(init.method).toBe("POST");
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer test-bearer");
  });
});

describe("connectClientTools with tickets", () => {
  it("presents a fresh ticket, never the bearer, when the server issues tickets", async () => {
    const { opened, WS } = fakeWS();
    const { client } = makeClient([jsonResponse(ticketBody("lct1.a.b"))]);
    const host = client.connectClientTools({ tools: [], onInvoke: () => null, WebSocketImpl: WS, reconnect: false });
    await vi.waitFor(() => expect(opened).toHaveLength(1));
    expect(opened[0]).toEqual(["loomcycle.client-tools.v1", "ticket.lct1.a.b"]);
    expect(opened[0]?.join(",")).not.toContain("test-bearer");
    host.close();
  });

  it("mints a new ticket on every reconnect", async () => {
    const sockets: { onclose: ((ev: unknown) => void) | null }[] = [];
    const opened: string[][] = [];
    class WS implements WebSocketLike {
      onopen = null;
      onclose: ((ev: unknown) => void) | null = null;
      onerror = null;
      onmessage = null;
      constructor(_url: string, protocols?: string | string[]) {
        opened.push([protocols ?? []].flat());
        sockets.push(this);
      }
      send(): void {}
      close(): void {}
    }
    const { client, fetchMock } = makeClient([jsonResponse(ticketBody("t-1")), jsonResponse(ticketBody("t-2"))]);
    const host = client.connectClientTools({
      tools: [],
      onInvoke: () => null,
      WebSocketImpl: WS as unknown as WebSocketCtor,
      reconnectDelayMs: 1,
    });
    await vi.waitFor(() => expect(opened).toHaveLength(1));
    sockets[0]?.onclose?.({});
    await vi.waitFor(() => expect(opened).toHaveLength(2));
    host.close();
    expect(opened.map((p) => p[1])).toEqual(["ticket.t-1", "ticket.t-2"]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  for (const [name, refusal] of [
    ["no ticket route (older runtime, 404)", errorResponse(404, "404 page not found")],
    ["method not allowed (405)", errorResponse(405, "Method Not Allowed")],
    ["no secret key (503 ticket_unavailable)", jsonResponse({ code: "ticket_unavailable", error: "no key" }, 503)],
  ] as const) {
    it(`falls back to the bearer subprotocol outside strict mode: ${name}`, async () => {
      const { opened, WS } = fakeWS();
      const { client } = makeClient([refusal, jsonResponse(whoami("token"))]);
      const host = client.connectClientTools({ tools: [], onInvoke: () => null, WebSocketImpl: WS, reconnect: false });
      await vi.waitFor(() => expect(opened).toHaveLength(1));
      expect(opened[0]).toEqual(["loomcycle.client-tools.v1", "bearer.test-bearer"]);
      host.close();
    });

    it(`never sends the bearer under strict sign-in: ${name}`, async () => {
      const { opened, WS } = fakeWS();
      const errors: unknown[] = [];
      const { client } = makeClient([refusal, jsonResponse(whoami("strict"))]);
      const host = client.connectClientTools({
        tools: [],
        onInvoke: () => null,
        WebSocketImpl: WS,
        reconnect: false,
        onError: (e) => errors.push(e),
      });
      await vi.waitFor(() => expect(errors).toHaveLength(1));
      expect(opened).toHaveLength(0);
      expect(errors[0]).toBeInstanceOf(LoomcycleError);
      expect((errors[0] as LoomcycleError).code).toBe("ticket_unavailable");
      host.close();
    });
  }

  it("does not fall back on a refusal the socket would give too (403)", async () => {
    const { opened, WS } = fakeWS();
    const errors: unknown[] = [];
    const { client, fetchMock } = makeClient([
      jsonResponse({ code: "restricted_token", error: "limited to its agents" }, 403),
    ]);
    const host = client.connectClientTools({
      tools: [],
      onInvoke: () => null,
      WebSocketImpl: WS,
      reconnect: false,
      onError: (e) => errors.push(e),
    });
    await vi.waitFor(() => expect(errors).toHaveLength(1));
    expect(opened).toHaveLength(0);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    host.close();
  });

  it("opens bare, with no mint, when the client holds no credential", async () => {
    const { opened, WS } = fakeWS();
    const fetchMock = vi.fn();
    const client = new LoomcycleClient({ baseUrl: "http://x", fetch: fetchMock as unknown as typeof fetch });
    const host = client.connectClientTools({ tools: [], onInvoke: () => null, WebSocketImpl: WS, reconnect: false });
    await vi.waitFor(() => expect(opened).toHaveLength(1));
    expect(opened[0]).toEqual(["loomcycle.client-tools.v1"]);
    expect(fetchMock).not.toHaveBeenCalled();
    host.close();
  });
});
