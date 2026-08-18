import { describe, it, expect, vi, afterEach } from "vitest";
import { copyToClipboard } from "./clipboard";

// The package runs vitest in the default `node` env, where `navigator` is a
// read-only global — so stub it via vi.stubGlobal (a direct assignment throws)
// and unstub after each case.
afterEach(() => {
  vi.unstubAllGlobals();
});

describe("copyToClipboard", () => {
  it("writes the text and reports success", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    const ok = await copyToClipboard("doc-123");
    expect(ok).toBe(true);
    expect(writeText).toHaveBeenCalledWith("doc-123");
  });

  it("returns false (does not throw) when the write rejects", async () => {
    vi.stubGlobal("navigator", {
      clipboard: { writeText: vi.fn().mockRejectedValue(new Error("denied")) },
    });
    await expect(copyToClipboard("x")).resolves.toBe(false);
  });

  it("returns false when the clipboard API is absent (insecure context / no DOM)", async () => {
    vi.stubGlobal("navigator", {}); // navigator present, clipboard undefined
    await expect(copyToClipboard("x")).resolves.toBe(false);
  });

  it("returns false when navigator itself is undefined", async () => {
    vi.stubGlobal("navigator", undefined);
    await expect(copyToClipboard("x")).resolves.toBe(false);
  });
});
