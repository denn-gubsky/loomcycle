import { afterEach, describe, expect, it, vi } from "vitest";
import { createUser, deleteUser, updateUser } from "./api";

// The Users console lists an admin's focused tenant (?tenant=). Every action on
// a listed user must name the same tenant, or the server looks in the admin's
// own tenant and answers "no such user in this tenant".
describe("user actions carry the managed tenant", () => {
  const urls: string[] = [];
  afterEach(() => {
    urls.length = 0;
    vi.unstubAllGlobals();
  });
  const stub = (status: number, body: unknown) =>
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        urls.push(url);
        return new Response(status === 204 ? null : JSON.stringify(body), {
          status,
        });
      }),
    );

  it("adds ?tenant= when given one", async () => {
    stub(200, { user_id: "cloud:zl" });
    await createUser({ subject: "cloud:zl" }, "loomcycle-dev");
    await updateUser("cloud:zl", { status: "active" }, "loomcycle-dev");
    stub(204, null);
    await deleteUser("cloud:zl", "loomcycle-dev");
    expect(urls).toEqual([
      "/v1/_users?tenant=loomcycle-dev",
      "/v1/_users/cloud%3Azl?tenant=loomcycle-dev",
      "/v1/_users/cloud%3Azl?tenant=loomcycle-dev",
    ]);
  });

  it("sends no tenant when none is given (a tenant operator's own)", async () => {
    stub(200, { user_id: "alice" });
    await createUser({ subject: "alice" });
    await updateUser("alice", { status: "active" });
    stub(204, null);
    await deleteUser("alice");
    expect(urls).toEqual([
      "/v1/_users",
      "/v1/_users/alice",
      "/v1/_users/alice",
    ]);
  });
});
