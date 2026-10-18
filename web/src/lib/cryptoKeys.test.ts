import { describe, it, expect } from "vitest";
import type { CredentialMeta } from "../api";
import {
  CRYPTO_KEY_ENCODINGS,
  CRYPTO_KEY_TYPES,
  cryptoKeyActions,
  expiryToRFC3339,
  isCryptoKey,
  minVersionBounds,
  parseMinVersion,
  refusalText,
  retireWarning,
  versionChips,
  versionSummary,
} from "./cryptoKeys";

// key builds a crypto-key row with versions 1..current, retired below min
// unless `versions` overrides them.
function key(
  current: number,
  min: number,
  versions?: { version: number; retired: boolean }[],
): CredentialMeta {
  return {
    name: "signing",
    scope: "tenant",
    usage: "crypto",
    key_type: "ed25519",
    current_version: current,
    min_version: min,
    versions:
      versions ??
      Array.from({ length: current }, (_, i) => ({
        version: i + 1,
        retired: i + 1 < min,
      })),
  };
}

describe("isCryptoKey", () => {
  it("is true only for usage crypto", () => {
    expect(isCryptoKey(key(1, 1))).toBe(true);
    expect(isCryptoKey({ name: "X", scope: "tenant" } as CredentialMeta)).toBe(false);
  });
});

describe("key type and encoding lists", () => {
  it("mirror the server's accepted values", () => {
    expect([...CRYPTO_KEY_TYPES]).toEqual([
      "aes-256-gcm",
      "xchacha20-poly1305",
      "hmac-sha256",
      "hmac-sha512",
      "ed25519",
      "ecdsa-p256",
      "totp",
      "x25519",
      "rsa-oaep-2048",
      "rsa-oaep-3072",
      "rsa-oaep-4096",
      "rsa-pss-2048",
      "rsa-pss-3072",
      "rsa-pss-4096",
    ]);
    expect([...CRYPTO_KEY_ENCODINGS]).toEqual(["raw", "base64", "hex", "base32", "pem"]);
  });
});

describe("minVersionBounds", () => {
  it("runs from min+1 to current+1 (current+1 retires every version)", () => {
    expect(minVersionBounds(key(5, 2))).toEqual({ lo: 3, hi: 6 });
    expect(minVersionBounds(key(1, 1))).toEqual({ lo: 2, hi: 2 });
  });

  it("is null once every version is retired (min = current+1)", () => {
    expect(minVersionBounds(key(4, 5))).toBeNull();
  });

  it("treats missing version fields as the floor of 1", () => {
    expect(minVersionBounds({ name: "k", scope: "user", usage: "crypto" })).toEqual({ lo: 2, hi: 2 });
  });
});

describe("parseMinVersion", () => {
  const m = key(5, 2);
  it("accepts whole numbers inside the bounds", () => {
    expect(parseMinVersion(m, "3")).toBe(3);
    expect(parseMinVersion(m, " 5 ")).toBe(5);
  });

  it("accepts current+1, which retires every version", () => {
    expect(parseMinVersion(m, "6")).toBe(6);
  });

  it("refuses values that lower, keep, or go past current+1", () => {
    expect(parseMinVersion(m, "2")).toBeNull();
    expect(parseMinVersion(m, "1")).toBeNull();
    expect(parseMinVersion(m, "7")).toBeNull();
  });

  it("refuses non-integers and empty input", () => {
    expect(parseMinVersion(m, "")).toBeNull();
    expect(parseMinVersion(m, "3.5")).toBeNull();
    expect(parseMinVersion(m, "-3")).toBeNull();
    expect(parseMinVersion(m, "abc")).toBeNull();
  });

  it("is null for a key with nothing left to retire", () => {
    expect(parseMinVersion(key(2, 3), "3")).toBeNull();
  });
});

describe("retireWarning", () => {
  it("names a single retired version", () => {
    const w = retireWarning(key(3, 1), 2);
    expect(w).toContain("retires version 1 of");
    expect(w).toContain("stops decrypting");
    expect(w).toContain("rewrapped");
  });

  it("names a range of retired versions", () => {
    expect(retireWarning(key(6, 2), 5)).toContain("retires versions 2–4 of");
  });

  it("says when every version is retired", () => {
    expect(retireWarning(key(3, 2), 4)).toContain("every version");
    expect(retireWarning(key(3, 2), 3)).not.toContain("every version");
  });
});

describe("cryptoKeyActions", () => {
  it("offers rotate and delete always, retire until every version is retired", () => {
    const fresh = cryptoKeyActions(key(1, 1));
    expect(fresh).toEqual({ rotate: true, retire: true, deletableVersions: [], delete: true });
    expect(cryptoKeyActions(key(1, 2)).retire).toBe(false);
  });

  it("offers delete-version only for retired versions, oldest first", () => {
    const a = cryptoKeyActions(
      key(4, 3, [
        { version: 4, retired: false },
        { version: 2, retired: true },
        { version: 3, retired: false },
        { version: 1, retired: true },
      ]),
    );
    expect(a.deletableVersions).toEqual([1, 2]);
  });
});

describe("versionChips", () => {
  it("lists unretired versions individually and marks the current one", () => {
    expect(versionChips(key(3, 1))).toEqual([
      { label: "v1", retired: false, current: false },
      { label: "v2", retired: false, current: false },
      { label: "v3", retired: false, current: true },
    ]);
  });

  it("collapses consecutive retired versions into a range", () => {
    expect(versionChips(key(6, 5))).toEqual([
      { label: "v1–v4", retired: true, current: false },
      { label: "v5", retired: false, current: false },
      { label: "v6", retired: false, current: true },
    ]);
  });

  it("breaks a retired range at a deleted version", () => {
    const chips = versionChips(
      key(5, 5, [
        { version: 1, retired: true },
        { version: 3, retired: true },
        { version: 4, retired: true },
        { version: 5, retired: false },
      ]),
    );
    expect(chips.map((c) => c.label)).toEqual(["v1", "v3–v4", "v5"]);
  });

  it("is empty when the server sent no versions", () => {
    expect(versionChips({ name: "k", scope: "user", usage: "crypto" })).toEqual([]);
  });
});

describe("versionSummary", () => {
  it("reads current and min", () => {
    expect(versionSummary(key(3, 2))).toBe("current v3 · min v2");
  });
});

describe("expiryToRFC3339", () => {
  it("is undefined for an empty field (no expiry)", () => {
    expect(expiryToRFC3339("")).toBeUndefined();
    expect(expiryToRFC3339("  ")).toBeUndefined();
  });

  it("converts local wall time to whole-second UTC RFC3339", () => {
    const out = expiryToRFC3339("2026-12-31T23:30");
    expect(out).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
    expect(new Date(out as string).getTime()).toBe(new Date("2026-12-31T23:30").getTime());
  });

  it("is null for an unparseable value", () => {
    expect(expiryToRFC3339("not a date")).toBeNull();
  });
});

describe("refusalText", () => {
  it("extracts error and description from the 422 envelope", () => {
    const msg =
      '422 Unprocessable Entity: {"code":"tool_refused","error":"delete: \\"k\\" still protects data","tool":"CredentialDef","description":"Rewrap first."}';
    expect(refusalText(msg)).toBe('delete: "k" still protects data — Rewrap first.');
  });

  it("extracts the error from a body cut mid-JSON", () => {
    const msg =
      '422 Unprocessable Entity: {"code":"tool_refused","error":"set_min_version: invalid crypto key version: min_version can only be raised","tool":"Cred';
    expect(refusalText(msg)).toBe(
      "set_min_version: invalid crypto key version: min_version can only be raised",
    );
  });

  it("returns anything else unchanged", () => {
    expect(refusalText("network down")).toBe("network down");
    expect(refusalText("500 Internal Server Error: oops")).toBe(
      "500 Internal Server Error: oops",
    );
  });
});
