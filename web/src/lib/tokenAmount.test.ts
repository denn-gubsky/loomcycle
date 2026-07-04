import { describe, it, expect } from "vitest";
import { parseTokenAmount, fmtTokens, amountHint } from "./tokenAmount";

describe("parseTokenAmount", () => {
  it("parses plain integers", () => {
    expect(parseTokenAmount("5000000")).toBe(5000000);
    expect(parseTokenAmount("0")).toBe(0);
  });

  it("recognizes K/M/G/B/T suffixes, case-insensitive", () => {
    expect(parseTokenAmount("500k")).toBe(500000);
    expect(parseTokenAmount("500K")).toBe(500000);
    expect(parseTokenAmount("5m")).toBe(5000000);
    expect(parseTokenAmount("5M")).toBe(5000000);
    expect(parseTokenAmount("2G")).toBe(2000000000);
    expect(parseTokenAmount("2b")).toBe(2000000000); // B alias for G
    expect(parseTokenAmount("1T")).toBe(1000000000000);
  });

  it("accepts a decimal mantissa", () => {
    expect(parseTokenAmount("1.5M")).toBe(1500000);
    expect(parseTokenAmount("2.5k")).toBe(2500);
    expect(parseTokenAmount("0.5G")).toBe(500000000);
  });

  it("ignores commas / underscores / spaces as separators", () => {
    expect(parseTokenAmount("5,000,000")).toBe(5000000);
    expect(parseTokenAmount("5_000_000")).toBe(5000000);
    expect(parseTokenAmount(" 5 M ")).toBe(5000000);
  });

  it("returns null for an empty entry (tier unset)", () => {
    expect(parseTokenAmount("")).toBeNull();
    expect(parseTokenAmount("   ")).toBeNull();
  });

  it("returns undefined for an unparseable entry", () => {
    expect(parseTokenAmount("5X")).toBeUndefined();
    expect(parseTokenAmount("abc")).toBeUndefined();
    expect(parseTokenAmount("M")).toBeUndefined(); // suffix with no number
    expect(parseTokenAmount("-5")).toBeUndefined(); // no negatives
    expect(parseTokenAmount("5MM")).toBeUndefined();
  });
});

describe("fmtTokens", () => {
  it("scales through K/M/G/T", () => {
    expect(fmtTokens(500)).toBe("500");
    expect(fmtTokens(1500)).toBe("1.5K");
    expect(fmtTokens(5000000)).toBe("5.00M");
    expect(fmtTokens(2000000000)).toBe("2.00G");
    expect(fmtTokens(1000000000000)).toBe("1.00T");
  });
});

describe("amountHint", () => {
  it("shows the exact value when a shorthand was recognized", () => {
    expect(amountHint("5M")).toEqual({ text: "= 5,000,000", invalid: false });
    expect(amountHint("1.5m")).toEqual({ text: "= 1,500,000", invalid: false });
    expect(amountHint("2G")).toEqual({ text: "= 2,000,000,000", invalid: false });
  });

  it("shows no hint when the entry already reads as the number", () => {
    expect(amountHint("5000000")).toBeNull(); // plain integer
    expect(amountHint("5,000,000")).toBeNull(); // separators only, same value
    expect(amountHint("")).toBeNull(); // empty
  });

  it("flags an unparseable entry", () => {
    expect(amountHint("5X")).toEqual({ text: "not a number", invalid: true });
  });
});
