import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { verifyLoomcycleDelivery, verifyLoomcycleSignature } from "../src/index.js";

/**
 * The vector is written by the Go signer (internal/sigv1, pinned by
 * TestSign_MatchesTheCrossLanguageVector). Reading it, rather than signing a
 * fixture here, means this test never restates the algorithm it checks: a
 * verifier that disagrees with what loomcycle actually sends fails.
 */
interface Vector {
  secret: string;
  body: string;
  t: number;
  header: string;
  /** Validly signed bodies that are not a usable delivery. */
  unusable: { name: string; body: string; header: string }[];
}
const vector = JSON.parse(
  readFileSync(resolve(__dirname, "..", "..", "..", "internal", "sigv1", "testdata", "vector.json"), "utf8"),
) as Vector;
const at = vector.t * 1000;

describe("verifyLoomcycleSignature", () => {
  it("accepts the Go signer's vector, as a string and as bytes", async () => {
    expect(await verifyLoomcycleSignature(vector.body, vector.header, vector.secret, { now: at })).toBe(true);
    const bytes = new TextEncoder().encode(vector.body);
    expect(await verifyLoomcycleSignature(bytes, vector.header, vector.secret, { now: at })).toBe(true);
  });

  it("accepts a space after the comma, as loomcycle's receiver does", async () => {
    const spaced = vector.header.replace(",", ", ");
    expect(await verifyLoomcycleSignature(vector.body, spaced, vector.secret, { now: at })).toBe(true);
  });

  it("refuses a tampered body, a wrong secret and a re-serialized body", async () => {
    expect(await verifyLoomcycleSignature(vector.body + " ", vector.header, vector.secret, { now: at })).toBe(false);
    expect(await verifyLoomcycleSignature(vector.body, vector.header, "another-secret", { now: at })).toBe(false);
    const reserialized = JSON.stringify(JSON.parse(vector.body));
    expect(await verifyLoomcycleSignature(reserialized, vector.header, vector.secret, { now: at })).toBe(false);
  });

  it("refuses a stale or future t, and keeps the window inclusive", async () => {
    const v = (nowMs: number, toleranceSec?: number) =>
      verifyLoomcycleSignature(vector.body, vector.header, vector.secret, { now: nowMs, toleranceSec });
    expect(await v(at + 300_000)).toBe(true);
    expect(await v(at - 300_000)).toBe(true);
    expect(await v(at + 301_000)).toBe(false);
    expect(await v(at - 301_000)).toBe(false);
    expect(await v(at + 301_000, 600)).toBe(true);
    expect(await v(at + 1_000, 0)).toBe(false);
  });

  it("fails closed on every malformed header", async () => {
    const [tPart, macPart] = vector.header.split(",") as [string, string];
    const mac = macPart.slice(3);
    const bad = [
      "",
      tPart,
      macPart,
      `t=abc,v1=${mac}`,
      `t=${vector.t}.5,v1=${mac}`,
      `${tPart},v1=${mac.slice(1)}`,
      `${tPart},v1=zz${mac.slice(2)}`,
      `${tPart},v1=${mac.slice(0, 62)}`,
      `${tPart},v1=`,
      mac,
      `t=${vector.t + 1},v1=${mac}`,
    ];
    for (const h of bad) {
      expect(await verifyLoomcycleSignature(vector.body, h, vector.secret, { now: at }), h).toBe(false);
    }
    expect(await verifyLoomcycleSignature(vector.body, vector.header, "", { now: at })).toBe(false);
  });

  it("uses an injected Web Crypto and throws without one", async () => {
    const subtle = globalThis.crypto.subtle;
    expect(await verifyLoomcycleSignature(vector.body, vector.header, vector.secret, { now: at, subtle })).toBe(true);
    const saved = Object.getOwnPropertyDescriptor(globalThis, "crypto");
    Object.defineProperty(globalThis, "crypto", { value: undefined, configurable: true });
    try {
      await expect(verifyLoomcycleSignature(vector.body, vector.header, vector.secret, { now: at })).rejects.toThrow(
        /Web Crypto/,
      );
    } finally {
      if (saved) Object.defineProperty(globalThis, "crypto", saved);
    }
  });
});

describe("verifyLoomcycleDelivery", () => {
  it("returns the delivery id and hook from the signed body, not the headers", async () => {
    const d = await verifyLoomcycleDelivery(vector.body, vector.header, vector.secret, { now: at });
    expect(d).not.toBeNull();
    expect(d!.deliveryId).toBe("7c0e2b6a-1d2f-4a3b-8c4d-5e6f7a8b9c0d");
    expect(d!.hookName).toBe("gate");
    expect(d!.owner).toBe("agent:w");
    expect(d!.body).toEqual(JSON.parse(vector.body));
    const bytes = await verifyLoomcycleDelivery(new TextEncoder().encode(vector.body), vector.header, vector.secret, { now: at });
    expect(bytes?.deliveryId).toBe(d!.deliveryId);
  });

  it("returns null when the signature does not verify", async () => {
    expect(await verifyLoomcycleDelivery(vector.body, vector.header, "another-secret", { now: at })).toBeNull();
    const swapped = vector.body.replace("7c0e2b6a-1d2f", "00000000-0000");
    expect(await verifyLoomcycleDelivery(swapped, vector.header, vector.secret, { now: at })).toBeNull();
  });

  it("returns null for a validly signed body that is not a usable delivery", async () => {
    expect(vector.unusable.length).toBeGreaterThan(0);
    for (const u of vector.unusable) {
      // The signature itself is fine — what is refused is the body.
      expect(await verifyLoomcycleSignature(u.body, u.header, vector.secret, { now: at }), u.name).toBe(true);
      expect(await verifyLoomcycleDelivery(u.body, u.header, vector.secret, { now: at }), u.name).toBeNull();
    }
  });
});
