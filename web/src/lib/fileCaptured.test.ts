import { describe, expect, it } from "vitest";
import { fileCapturedLine, humanBytes, inboxDownloadPath } from "./fileCaptured";
import { formatLine } from "../components/TerminalTranscript";

const info = {
  ref: "volume:inbox:documents/board-papers.pdf",
  media_type: "application/pdf",
  size: 1887437,
  sha256: "3f9a1c2b4d5e",
  origin: "mcp:ms365/get-attachment",
};

describe("file_captured", () => {
  it("renders the ref, type, size and origin on one line", () => {
    expect(fileCapturedLine(info)).toBe(
      "⇩ volume:inbox:documents/board-papers.pdf · application/pdf · 1.8 MiB · from mcp:ms365/get-attachment",
    );
  });

  it("formats sizes as the runtime's stub does", () => {
    expect(humanBytes(512)).toBe("512 B");
    expect(humanBytes(2048)).toBe("2.0 KiB");
    expect(humanBytes(64 << 20)).toBe("64.0 MiB");
  });

  it("is a line of its own in the terminal, the sha256 in the expanded view", () => {
    const l = formatLine({
      seq: 3, run_id: "run-1", ts_ns: 0, type: "file_captured",
      event: { type: "file_captured", file: info },
    });
    expect(l.payload).toBe(fileCapturedLine(info));
    expect(l.full).toContain("sha256: 3f9a1c2b4d5e");
    // The expanded row offers the file for download, from the run on the row.
    expect(l.download).toEqual({ runId: "run-1", ref: info.ref });
  });

  it("downloads from the inbox route, each path segment encoded", () => {
    expect(inboxDownloadPath("run 1", "volume:inbox:documents/board papers#1.pdf")).toBe(
      "/v1/runs/run%201/inbox/documents/board%20papers%231.pdf",
    );
  });
});
