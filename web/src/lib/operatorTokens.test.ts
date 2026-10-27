import { afterEach, describe, expect, it, vi } from "vitest";
import { retireOperatorToken, rotateOperatorToken } from "../api";

// Access → Tokens acts on one token by def_id. The server refuses a per-user
// ("u-") name as a rotate or retire target, because the same name can exist in
// several tenants; sending the name made every u- row fail to rotate or retire.

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubFetch(body: string) {
  const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) => new Response(body, { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

describe("operator token actions", () => {
  it("rotates by def_id, never by name", async () => {
    const fetchMock = stubFetch('{"def_id":"def_new","name":"u-alice","token":"x"}');
    await rotateOperatorToken("def_0000000000000001");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/v1/_operatortokendef");
    expect(JSON.parse(String(init?.body))).toEqual({ op: "rotate", def_id: "def_0000000000000001" });
  });

  it("passes a grace window with the def_id", async () => {
    const fetchMock = stubFetch('{"def_id":"def_new"}');
    await rotateOperatorToken("def_0000000000000001", 60);
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({
      op: "rotate",
      def_id: "def_0000000000000001",
      grace_seconds: 60,
    });
  });

  it("retires by def_id, never by name", async () => {
    const fetchMock = stubFetch('{"def_id":"def_0000000000000001","name":"u-alice","retired":true}');
    await retireOperatorToken("def_0000000000000001");
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ op: "retire", def_id: "def_0000000000000001" });
  });
});
