import { describe, expect, it, vi } from "vitest";
import { decideDevice, deviceInfo, userCodeFromHash } from "./deviceLogin";

describe("userCodeFromHash", () => {
  it("reads a verification link's code", () => {
    expect(userCodeFromHash("#BCDF-GHJK")).toBe("BCDF-GHJK");
    expect(userCodeFromHash("#bcdfghjk")).toBe("BCDFGHJK");
  });
  it("ignores anything else in the fragment", () => {
    for (const bad of ["", "#", "#<script>", "#BCDF-GHJK-XX", "#lci_abc"]) {
      expect(userCodeFromHash(bad)).toBe("");
    }
  });
});

describe("device approval calls", () => {
  it("post the code in the body with the CSRF header, never in the URL", async () => {
    const f = vi.fn(async () => new Response(JSON.stringify({ approved: true }), { status: 200 }));
    await deviceInfo("BCDF-GHJK", f as unknown as typeof fetch);
    await decideDevice("BCDF-GHJK", false, f as unknown as typeof fetch);
    const calls = f.mock.calls as unknown as [string, RequestInit][];
    expect(calls.map((c) => c[0])).toEqual(["/v1/_auth/device/info", "/v1/_auth/device/approve"]);
    for (const [url, init] of calls) {
      expect(url).not.toContain("BCDF");
      expect(init.method).toBe("POST");
      expect(new Headers(init.headers).get("X-Loomcycle-UI")).toBe("1");
    }
    expect(JSON.parse(String(calls[1]![1].body))).toEqual({ user_code: "BCDF-GHJK", approve: false });
  });
});
