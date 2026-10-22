import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { EntryList } from "../components/LineagePanel";
import type { LibraryEntry } from "../types";
import { contextWindowLabel } from "./contextWindow";

describe("contextWindowLabel", () => {
  it("compacts a known window", () => {
    expect(contextWindowLabel(200000)).toBe("200k");
    expect(contextWindowLabel(32768)).toBe("33k");
    expect(contextWindowLabel(8192)).toBe("8.2k");
    expect(contextWindowLabel(1_000_000)).toBe("1.0M");
    expect(contextWindowLabel(512)).toBe("512");
  });

  it("is null for an unknown window", () => {
    expect(contextWindowLabel(undefined)).toBeNull();
    expect(contextWindowLabel(0)).toBeNull();
  });
});

describe("EntryList", () => {
  const entry = (name: string, context_window?: number): LibraryEntry => ({
    name,
    source: "static-only",
    in_static: true,
    in_substrate: false,
    version_count: 0,
    context_window,
  });

  it("shows each agent's context window, and nothing for an unknown one", () => {
    const html = renderToStaticMarkup(
      createElement(EntryList, {
        entries: [entry("big", 200000), entry("unknown")],
        selectedName: "",
        onSelect: () => {},
      }),
    );
    expect(html).toContain("ctx 200k");
    expect(html.match(/def-chip-ctx/g)?.length).toBe(1);
  });
});
