import { describe, expect, it } from "vitest";
import type { TranscriptionEventInfo } from "../api";
import { transcriptionDetail, transcriptionLine } from "./transcription";
import { formatLine } from "../components/TerminalTranscript";

const ocr: TranscriptionEventInfo = {
  ref: "volume:inbox:documents/scan.pdf",
  out: "volume:inbox:documents/scan.ocr.md",
  purpose: "ocr",
  engine: "vision",
  provider: "anthropic",
  model: "claude-haiku-5-5",
  pages: 12,
  input_tokens: 9000,
  output_tokens: 4000,
  cost: 0.04,
  currency: "USD",
  status: "ok",
  run_id: "run-7",
};

const row = (t: TranscriptionEventInfo) => ({
  seq: 4, run_id: "run-7", ts_ns: 0, type: "transcription",
  event: { type: "transcription", transcription: t },
});

describe("transcription", () => {
  it("renders an OCR pass as one line: file, pages, model, cost, status", () => {
    expect(transcriptionLine(ocr)).toBe("⟲ OCR scan.pdf · 12 pages · claude-haiku-5-5 · $0.04 · ok");
  });

  it("renders a speech-to-text pass in minutes, and the engine when no model was called", () => {
    const stt = { ref: "volume:inbox:call.mp4", purpose: "stt", engine: "command", seconds: 2550, status: "ok" };
    expect(transcriptionLine(stt)).toBe("⟲ speech-to-text call.mp4 · 42.5 min · command · ok");
    expect(transcriptionLine({ ...stt, seconds: 9.25 })).toBe("⟲ speech-to-text call.mp4 · 9.3 s · command · ok");
  });

  it("says partial and failed, and colours a failure as an error in the terminal", () => {
    expect(transcriptionLine({ ...ocr, status: "partial", pages: 5 })).toBe(
      "⟲ OCR scan.pdf · 5 pages · claude-haiku-5-5 · $0.04 · partial",
    );
    const failed = { ...ocr, status: "failed", pages: 0, cost: undefined, input_tokens: 0, output_tokens: 0 };
    expect(transcriptionLine(failed)).toBe("⟲ OCR scan.pdf · claude-haiku-5-5 · failed");
    expect(formatLine(row(failed)).cls).toBe("tl-error");
    expect(formatLine(row({ ...ocr, status: "partial" })).cls).toBe("tl-tool");
  });

  it("marks a dry run as an estimate, never as spent", () => {
    const dry = { ...ocr, dry_run: true };
    expect(transcriptionLine(dry)).toBe("⟲ OCR scan.pdf · 12 pages · claude-haiku-5-5 · ~$0.04 · estimate");
    expect(transcriptionLine({ ...dry, status: "failed" })).toBe(
      "⟲ OCR scan.pdf · 12 pages · claude-haiku-5-5 · ~$0.04 · estimate · failed",
    );
    expect(formatLine(row(dry)).cls).toBe("tl-meta");
  });

  it("prices small costs readably and names a currency other than dollars", () => {
    expect(transcriptionLine({ ...ocr, cost: 0.0031 })).toContain(" · $0.0031 · ");
    expect(transcriptionLine({ ...ocr, cost: 0 })).toContain(" · $0 · ");
    expect(transcriptionLine({ ...ocr, cost: 1.5, currency: "EUR" })).toContain(" · 1.50 EUR · ");
  });

  it("is a line of its own in the terminal; expanding it shows the run, the output and the tokens", () => {
    const l = formatLine(row(ocr));
    expect(l.payload).toBe(transcriptionLine(ocr));
    expect(l.collapsible).toBe(true);
    expect(l.full).toBe(transcriptionDetail(ocr));
    expect(l.full).toContain("run_id: run-7");
    expect(l.full).toContain("out: volume:inbox:documents/scan.ocr.md");
    expect(l.full).toContain("tokens: 9000 in / 4000 out");
    expect(l.full).toContain("ref: volume:inbox:documents/scan.pdf");
  });

  it("says an unpriced pass is unpriced in the expanded view", () => {
    expect(transcriptionDetail({ ...ocr, cost: undefined })).toContain("cost: unpriced");
  });

  it("falls back to a bare label without a payload", () => {
    expect(transcriptionLine(undefined)).toBe("transcription");
  });
});
