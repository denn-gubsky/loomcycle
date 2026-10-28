import { describe, expect, it, vi } from "vitest";
import { b64urlToBytes, bytesToB64url, creationOptions, requestOptions, withPasskeyStepUp } from "./passkeys";

describe("passkeys", () => {
  it("round-trips base64url without padding", () => {
    for (const s of ["", "AQ", "AQI", "AQID", "-_-_", "3q2-7w"]) {
      expect(bytesToB64url(b64urlToBytes(s))).toBe(s);
    }
  });

  it("decodes the binary fields of a creation offer", () => {
    const o = creationOptions({
      ceremony: "c",
      publicKey: {
        challenge: "AQID",
        rp: { id: "lc.example", name: "loomcycle" },
        user: { id: "BAUG", name: "a", displayName: "A" },
        pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        excludeCredentials: [{ type: "public-key", id: "Bwg" }],
      },
    });
    expect(Array.from(o.challenge as Uint8Array)).toEqual([1, 2, 3]);
    expect(Array.from(o.user.id as Uint8Array)).toEqual([4, 5, 6]);
    expect(Array.from(o.excludeCredentials?.[0].id as Uint8Array)).toEqual([7, 8]);
  });

  it("decodes the challenge and allowed credentials of a request offer", () => {
    const o = requestOptions({ ceremony: "c", publicKey: { challenge: "AQID", allowCredentials: [{ type: "public-key", id: "CQ" }] } });
    expect(Array.from(o.challenge as Uint8Array)).toEqual([1, 2, 3]);
    expect(Array.from(o.allowCredentials?.[0].id as Uint8Array)).toEqual([9]);
  });

  it("answers a passkey step-up and sends again with the answer", async () => {
    const offer = { ceremony: "c1", publicKey: { challenge: "AQID" } };
    const send = vi
      .fn()
      .mockResolvedValueOnce({ ok: false, refusal: { code: "second_factor_required", webauthn: offer } })
      .mockResolvedValueOnce({ ok: true, data: {} });
    const assert = vi.fn().mockResolvedValue({ ceremony: "c1", credential: { id: "x" } });
    const r = await withPasskeyStepUp(send, assert);
    expect(r.ok).toBe(true);
    expect(assert).toHaveBeenCalledWith(offer);
    expect(send).toHaveBeenNthCalledWith(2, { ceremony: "c1", credential: { id: "x" } });
  });

  it("reports a cancelled passkey without sending again", async () => {
    const send = vi.fn().mockResolvedValue({ ok: false, refusal: { code: "second_factor_required", webauthn: { ceremony: "c", publicKey: {} } } });
    const r = await withPasskeyStepUp(send, vi.fn().mockResolvedValue(null));
    expect(r).toEqual({ ok: false, refusal: { code: "passkey_cancelled" } });
    expect(send).toHaveBeenCalledTimes(1);
  });

  it("returns a refusal that is not a passkey step-up unchanged, without a second send", async () => {
    const send = vi.fn().mockResolvedValue({ ok: false, refusal: { code: "reauth_failed" } });
    const r = await withPasskeyStepUp(send);
    expect(r).toEqual({ ok: false, refusal: { code: "reauth_failed" } });
    expect(send).toHaveBeenCalledTimes(1);
  });
});
