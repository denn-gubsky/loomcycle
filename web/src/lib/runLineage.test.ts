import { describe, expect, it } from "vitest";
import { parentRunHref } from "./runLineage";

describe("parentRunHref", () => {
  it("opens the parent run itself by run id in the run terminal", () => {
    expect(parentRunHref("r_5e8b82599098c19f")).toBe("/run?attach=r_5e8b82599098c19f");
  });

  it("escapes the run id into the query", () => {
    expect(parentRunHref("r a&b")).toBe("/run?attach=r%20a%26b");
  });
});
