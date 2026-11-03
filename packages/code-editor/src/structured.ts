// Structured text (RFC FW): a JSON value shown as JSON or YAML, parsed back
// with the position of every problem, and a place in it named by a JSON path.
//
// Pure: no React, no CodeMirror, so a host can use these in its own logic and
// tests. One parser (`yaml`) gives positions in both formats — JSON is YAML
// 1.2 — while JSON validity is still decided by JSON.parse, so YAML-only
// syntax is never accepted as JSON.
//
// WHY YAML is parsed this strictly: pasted text is someone else's, and what a
// host saves from it is stored under the operator's authority.
//   - Aliases and anchors are refused. An alias is a reference; converting to
//     JSON expands it, and nested aliases blow up exponentially.
//   - Explicit tags are refused. The core schema still resolves `!!binary`
//     into bytes, which JSON cannot hold, and `!foo` means nothing here.
//   - Duplicate keys are refused, where JSON.parse would keep the last one
//     silently and edit the value without the author seeing which.
//   - The core schema keeps a date a string and `<<` a plain key.

import { parser as jsonParser } from "@lezer/json";
import { isAlias, isMap, isScalar, isSeq, parseDocument, stringify, visit, type Document, type Node as YamlNode } from "yaml";

export type Format = "json" | "yaml";

/** A place in a value: object keys and array indexes, outermost first. */
export type JsonPath = readonly (string | number)[];

export type JsonObject = { [key: string]: unknown };

export interface TextPosition {
  /** 0-based offset into the text. */
  offset: number;
  /** 1-based. */
  line: number;
  /** 1-based, in UTF-16 code units, as an editor counts them. */
  column: number;
}

export interface ParseError extends TextPosition {
  message: string;
  /** End offset of the offending text (>= offset). */
  to: number;
}

export type ParseResult = { ok: true; value: JsonObject } | { ok: false; error: ParseError };

/** The value as JSON text: two-space indent, the value's own key order, a final newline. */
export function toJson(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

/** The value as YAML: multi-line strings as blocks with real lines, the
 *  value's own key order, long lines never folded (the editor wraps them). */
export function toYaml(value: unknown): string {
  return stringify(value, { lineWidth: 0, minContentWidth: 0, aliasDuplicateObjects: false });
}

/** Render `value` in `format`. */
export function formatValue(value: unknown, format: Format): string {
  return format === "yaml" ? toYaml(value) : toJson(value);
}

/** Parse text in `format` into an object. */
export function parseText(text: string, format: Format): ParseResult {
  return format === "yaml" ? parseYaml(text) : parseJson(text);
}

/** JSON whose root is an object, with duplicate keys refused. */
export function parseJson(text: string): ParseResult {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (e) {
    // Engines word a syntax error differently and some give no position at
    // all, so the place comes from a JSON grammar's own error node.
    const at = jsonErrorOffset(text);
    return fail(text, cleanJsonMessage(e), at, at + 1);
  }
  if (!isPlainObject(value)) return fail(text, "The root must be an object.", 0, Math.min(text.length, 1));
  // JSON.parse keeps the last of two equal keys; the YAML parser reports them.
  const doc = parseDocument(text, { schema: "json", uniqueKeys: true, prettyErrors: false });
  const dup = doc.errors.find((e) => e.code === "DUPLICATE_KEY");
  if (dup) return fail(text, "Duplicate key.", dup.pos[0], dup.pos[1]);
  return { ok: true, value };
}

/** YAML (1.2, core schema) whose root is a mapping, refusing aliases,
 *  anchors, explicit tags, duplicate keys and numbers JSON cannot hold. */
export function parseYaml(text: string): ParseResult {
  const doc = parseDocument(text, { schema: "core", uniqueKeys: true, prettyErrors: false });
  if (doc.errors.length > 0) {
    const e = doc.errors[0];
    return fail(text, firstLine(e.message), e.pos[0], e.pos[1]);
  }
  const refused = refusedNode(doc);
  if (refused) return fail(text, refused.message, refused.from, refused.to);
  if (!isMap(doc.contents)) {
    const r = (doc.contents as YamlNode | null)?.range;
    return fail(text, "The root must be a mapping (key: value lines).", r?.[0] ?? 0, r?.[1] ?? Math.min(text.length, 1));
  }
  const value = doc.toJS({ maxAliasCount: 0 }) as unknown;
  if (!isPlainObject(value)) return fail(text, "The root must be a mapping (key: value lines).", 0, Math.min(text.length, 1));
  return { ok: true, value };
}

/** Where `path` is in `text`: the member's key for an object member, the
 *  value for an array element. Falls back to the nearest enclosing place the
 *  text has (a finding about a field not written yet is shown on its object),
 *  and undefined when the text does not parse. */
export function locate(text: string, format: Format, path: JsonPath): (TextPosition & { to: number }) | undefined {
  const doc = positionsDoc(text, format);
  if (!doc) return undefined;
  let node: unknown = doc.contents;
  let at: { from: number; to: number } | undefined = rangeOf(node);
  for (const seg of path) {
    if (isMap(node)) {
      const pair = node.items.find((p) => keyString(p.key) === String(seg));
      if (!pair) break;
      const key = rangeOf(pair.key);
      const val = rangeOf(pair.value);
      at = key ? { from: key.from, to: val?.to ?? key.to } : val ?? at;
      node = pair.value;
    } else if (isSeq(node) && typeof seg === "number") {
      const item = node.items[seg];
      const r = rangeOf(item);
      if (!r) break;
      at = r;
      node = item;
    } else {
      break;
    }
  }
  if (!at) return undefined;
  return { ...positionAt(text, at.from), to: at.to };
}

/** The JSON path of the innermost member or element holding `offset`, or
 *  undefined when the text does not parse. `[]` is the root. */
export function pathAt(text: string, format: Format, offset: number): JsonPath | undefined {
  const doc = positionsDoc(text, format);
  if (!doc) return undefined;
  const path: (string | number)[] = [];
  let node: unknown = doc.contents;
  for (;;) {
    if (isMap(node)) {
      const pair = node.items.find((p) => {
        const from = rangeOf(p.key)?.from;
        const to = rangeOf(p.value)?.to ?? rangeOf(p.key)?.to;
        return from !== undefined && to !== undefined && offset >= from && offset <= to;
      });
      if (!pair) return path;
      path.push(keyString(pair.key));
      node = pair.value;
    } else if (isSeq(node)) {
      const i = node.items.findIndex((item) => {
        const r = rangeOf(item);
        return !!r && offset >= r.from && offset <= r.to;
      });
      if (i < 0) return path;
      path.push(i);
      node = node.items[i];
    } else {
      return path;
    }
  }
}

/** 0-based offset → 1-based line and column. */
export function positionAt(text: string, offset: number): TextPosition {
  const o = Math.max(0, Math.min(offset, text.length));
  let line = 1;
  let lineStart = 0;
  for (let i = 0; i < o; i++) {
    if (text.charCodeAt(i) === 10) {
      line++;
      lineStart = i + 1;
    }
  }
  return { offset: o, line, column: o - lineStart + 1 };
}

function fail(text: string, message: string, from: number, to: number): { ok: false; error: ParseError } {
  const at = positionAt(text, from);
  return { ok: false, error: { ...at, message, to: Math.max(at.offset, Math.min(to, text.length)) } };
}

// A document to read positions from, only when the text is valid in `format`
// — positions in text that does not parse would point at the wrong place.
function positionsDoc(text: string, format: Format): Document | undefined {
  if (!parseText(text, format).ok) return undefined;
  return parseDocument(text, { schema: format === "json" ? "json" : "core", prettyErrors: false });
}

function refusedNode(doc: Document): { message: string; from: number; to: number } | undefined {
  let found: { message: string; from: number; to: number } | undefined;
  const refuse = (message: string, node: unknown) => {
    const r = rangeOf(node);
    found = { message, from: r?.from ?? 0, to: r?.to ?? 0 };
    return visit.BREAK;
  };
  visit(doc, {
    Alias: (_, node) => refuse("Aliases (*name) are not allowed; write the value out.", node),
    Node: (_, node) => {
      if (isAlias(node)) return undefined;
      if (node.anchor) return refuse("Anchors (&name) are not allowed.", node);
      if (node.tag) return refuse(`Tags (${node.tag.replace(/^tag:yaml\.org,2002:/, "!!")}) are not allowed.`, node);
      if (isScalar(node) && typeof node.value === "number" && !Number.isFinite(node.value)) {
        return refuse("Infinity and NaN are not JSON numbers.", node);
      }
      return undefined;
    },
  });
  return found;
}

function rangeOf(node: unknown): { from: number; to: number } | undefined {
  const r = (node as YamlNode | null | undefined)?.range;
  return r ? { from: r[0], to: r[1] } : undefined;
}

function keyString(key: unknown): string {
  if (isScalar(key)) return String(key.value);
  return String(key);
}

function isPlainObject(v: unknown): v is JsonObject {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function firstLine(s: string): string {
  return s.split("\n")[0];
}

function cleanJsonMessage(e: unknown): string {
  const m = e instanceof Error ? e.message : String(e);
  // Engines append the whole input to some messages; the editor marks the place.
  return firstLine(m).replace(/,\s*".*" is not valid JSON$/, "").replace(/^JSON\.parse:\s*/, "");
}

function jsonErrorOffset(text: string): number {
  let at = -1;
  jsonParser.parse(text).iterate({
    enter: (n) => {
      if (at >= 0) return false;
      if (n.type.isError) {
        at = n.from;
        return false;
      }
      return undefined;
    },
  });
  if (at >= 0) return at;
  // The grammar accepted it (e.g. a lone value JSON.parse rejects); point at the end.
  return text.trimEnd().length;
}
