import { afterEach, describe, expect, it, vi } from "vitest";
import { forkTeam } from "../api";

// "Save new version" on the Teams page is a fork. The server's op=fork does not
// promote unless asked, so the saved version must be promoted explicitly or Run
// keeps starting the previous one.

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("forkTeam", () => {
  it("saves the edited graph as a new version and promotes it", async () => {
    const fetchMock = vi.fn(
      async (_url: string, _init?: RequestInit) =>
        new Response('{"def_id":"tdf_2","name":"brief","version":2,"promoted":true}', { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const overlay = { entry: "a", states: [], transitions: [] };
    await forkTeam("brief", overlay);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/v1/_teamdef");
    expect(JSON.parse(String(init?.body))).toEqual({ op: "fork", name: "brief", overlay, promote: true });
  });
});
