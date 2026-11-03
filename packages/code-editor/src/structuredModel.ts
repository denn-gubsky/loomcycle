// The state behind StructuredEditor (RFC FW): which format is shown, the text
// as the operator sees it, and the JSON value that text stands for. Pure, so
// the rules below are tested without a DOM.
//
//   - JSON is the canonical form. The host holds a JSON text; in JSON mode the
//     operator edits exactly that text, invalid or not, so a host's own
//     parse-on-save stays as it was.
//   - In YAML mode an edit that parses becomes new JSON for the host; one that
//     does not leaves the host's value alone and reports the error.
//   - The host's echo of our own edit is recognised and does not rewrite the
//     text, so the operator's YAML formatting and cursor survive typing.
//   - Switching format from text that does not parse is refused: converting it
//     would lose what the operator typed.

import { formatValue, parseJson, parseText, toJson, type Format, type ParseError } from "./structured";

export interface StructuredState {
  /** The format shown. */
  format: Format;
  /** The format the operator chose. It differs from `format` only while the
   *  host's value is not valid JSON, which can only be shown as JSON. */
  preferred: Format;
  /** What the editor shows. */
  text: string;
  /** The host's JSON text that `text` stands for (the last one we emitted or accepted). */
  value: string;
  /** Why `text` does not parse, or null. */
  error: ParseError | null;
}

/** The state for a host value. YAML is only possible when the value is valid
 *  JSON; otherwise it is shown as JSON so nothing is hidden. An empty value (a
 *  definition still loading, or not written yet) keeps the chosen format. */
export function initState(value: string, preferred: Format): StructuredState {
  if (value.trim() === "") return { format: preferred, preferred, text: "", value, error: null };
  if (preferred === "yaml") {
    const parsed = parseJson(value);
    if (parsed.ok) return { format: "yaml", preferred, text: formatValue(parsed.value, "yaml"), value, error: null };
  }
  return { format: "json", preferred, text: value, value, error: errorOf(value, "json") };
}

/** The operator edited the text. `emit` is the JSON to hand the host, if any. */
export function applyEdit(state: StructuredState, text: string): { state: StructuredState; emit?: string } {
  if (state.format === "json") {
    return { state: { ...state, text, value: text, error: text.trim() === "" ? null : errorOf(text, "json") }, emit: text };
  }
  if (text.trim() === "") return { state: { ...state, text, value: "", error: null }, emit: "" };
  const parsed = parseText(text, "yaml");
  if (!parsed.ok) return { state: { ...state, text, error: parsed.error } };
  const json = toJson(parsed.value);
  return { state: { ...state, text, value: json, error: null }, emit: json };
}

/** The host's value changed. Our own echo keeps the text as typed. */
export function applyExternal(state: StructuredState, value: string): StructuredState {
  if (value === state.value) return state;
  return initState(value, state.preferred);
}

/** Show the value in another format. Refused (with the error) while the
 *  current text does not parse. */
export function switchFormat(state: StructuredState, to: Format): { state: StructuredState; refused?: ParseError } {
  if (to === state.format) return { state: { ...state, preferred: to } };
  if (state.text.trim() === "") return { state: { ...state, format: to, preferred: to } };
  const parsed = parseText(state.text, state.format);
  if (!parsed.ok) return { state, refused: parsed.error };
  if (to === "yaml") return { state: { format: "yaml", preferred: "yaml", text: formatValue(parsed.value, "yaml"), value: state.value, error: null } };
  // Back to JSON: the host's own text, as it holds it.
  return { state: { format: "json", preferred: "json", text: state.value, value: state.value, error: null } };
}

function errorOf(text: string, format: Format): ParseError | null {
  const r = parseText(text, format);
  return r.ok ? null : r.error;
}
