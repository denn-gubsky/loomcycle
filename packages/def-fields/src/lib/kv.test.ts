import { describe, expect, it } from "vitest";
import { kvObject, kvRows } from "./kv";

describe("the key/value rows", () => {
  // The regression: "+ add entry" appends an unnamed row, which the map cannot
  // hold. The rows must survive the round-trip through the emitted map.
  it("keep a row that has no key yet", () => {
    const rows: [string, string][] = [["Authorization", "Bearer $cred:k"], ["", ""]];
    const emitted = kvObject(rows);
    expect(emitted).toEqual({ Authorization: "Bearer $cred:k" });
    expect(kvRows(rows, emitted)).toBe(rows);
  });

  it("re-seed from a value changed outside (cleared, or another def loaded)", () => {
    const rows: [string, string][] = [["X", "1"], ["", ""]];
    expect(kvRows(rows, {})).toEqual([]);
    expect(kvRows(rows, { Y: "2" })).toEqual([["Y", "2"]]);
  });
});
