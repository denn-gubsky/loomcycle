import { describe, expect, it } from "vitest";
import type { ChannelDescriptor } from "../api";
import { filterChannels } from "./channelFilter";

const ch = (name: string, source: string, scope = "tenant"): ChannelDescriptor =>
  ({ name, source, scope, message_count: 1 }) as ChannelDescriptor;

const rows = [
  ch("documents/ingested", "yaml", "user"),
  ch("memory/subjects", "yaml"),
  ch("documents/0101a323877fa6c3c57ab25245bac110/chunks", "document"),
  ch("documents/029dc823726933478bd8303ff7c9c0e8/chunks", "document", "user"),
  ch("stray", "orphan"),
];

const names = (f: Parameters<typeof filterChannels>[1]) => filterChannels(rows, f).map((c) => c.name);

describe("filterChannels", () => {
  it("leaves the document change feeds out of every chip but documents", () => {
    expect(names("all")).toEqual(["documents/ingested", "memory/subjects", "stray"]);
    expect(names("user")).toEqual(["documents/ingested"]);
    expect(names("orphan")).toEqual(["stray"]);
  });

  it("shows only the document change feeds under documents", () => {
    expect(names("documents")).toEqual([
      "documents/0101a323877fa6c3c57ab25245bac110/chunks",
      "documents/029dc823726933478bd8303ff7c9c0e8/chunks",
    ]);
  });
});
