import { describe, expect, it } from "vitest";
import { approveUser, linkErrorMessage, listIdentities, startLink, unlink } from "./identityLinks";

function recorder(status: number, body: unknown) {
  const calls: { url: string; init?: RequestInit }[] = [];
  const impl = (async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return new Response(status === 204 ? null : JSON.stringify(body), { status });
  }) as unknown as typeof fetch;
  return { calls, impl };
}

describe("identity links", () => {
  it("lists, links, unlinks and approves on the user's routes", async () => {
    const list = recorder(200, { subject: "google:1", identities: [{ gate: "google", external_subject: "1" }] });
    const r = await listIdentities("google:1", list.impl);
    expect(r.ok && r.data.identities[0].gate).toBe("google");
    expect(list.calls[0].url).toBe("/v1/_users/google%3A1/identities");

    const link = recorder(200, { redirect: "https://idp.example/authorize?x" });
    const l = await startLink("alice", "entra", link.impl);
    expect(l.ok && l.data.redirect).toBe("https://idp.example/authorize?x");
    expect(link.calls[0].init?.method).toBe("POST");
    expect(link.calls[0].init?.body).toBe(JSON.stringify({ gate: "entra" }));

    const del = recorder(204, null);
    expect((await unlink("alice", "entra", del.impl)).ok).toBe(true);
    expect(del.calls[0].url).toBe("/v1/_users/alice/identities/entra");

    const ap = recorder(200, {});
    expect((await approveUser("p1", ap.impl)).ok).toBe(true);
    expect(ap.calls[0].url).toBe("/v1/_users/p1/approve");
  });

  it("reports the server's refusal code", async () => {
    const refused = recorder(403, { code: "reauth_required", error: "sign in again first" });
    const r = await startLink("alice", "entra", refused.impl);
    expect(r.ok).toBe(false);
    if (!r.ok) {
      expect(r.code).toBe("reauth_required");
      expect(linkErrorMessage(r.code)).toMatch(/Sign in again/);
    }
    expect(linkErrorMessage("nope")).toMatch(/could not be linked/);
  });
});
