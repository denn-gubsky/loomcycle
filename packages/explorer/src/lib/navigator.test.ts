import { describe, expect, it } from "vitest";
import { NAVIGATOR_KEY, readNavigatorMode, writeNavigatorMode } from "./navigator";

function memory(): Storage {
  const m = new Map<string, string>();
  return {
    getItem: (k) => m.get(k) ?? null,
    setItem: (k, v) => void m.set(k, v),
    removeItem: (k) => void m.delete(k),
    clear: () => m.clear(),
    key: () => null,
    get length() {
      return m.size;
    },
  };
}

describe("navigator mode", () => {
  it("TestNavigatorMode_DefaultsToTheTree", () => {
    expect(readNavigatorMode(memory())).toBe("tree");
    expect(readNavigatorMode(undefined)).toBe("tree");
  });

  it("TestNavigatorMode_RemembersStars", () => {
    const s = memory();
    writeNavigatorMode(s, "stars");
    expect(s.getItem(NAVIGATOR_KEY)).toBe("stars");
    expect(readNavigatorMode(s)).toBe("stars");
  });

  it("TestNavigatorMode_BlockedStorageFallsBackToTheTree", () => {
    const blocked = {
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {
        throw new Error("blocked");
      },
    };
    expect(readNavigatorMode(blocked)).toBe("tree");
    expect(() => writeNavigatorMode(blocked, "stars")).not.toThrow();
  });
});
