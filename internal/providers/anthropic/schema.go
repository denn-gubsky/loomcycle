package anthropic

import (
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Anthropic's Messages API rejects a tool whose input_schema carries a
// combinator at the TOP LEVEL:
//
//	tools.N.custom.input_schema: input_schema does not support
//	oneOf, allOf, or anyOf at the top level
//
// A Zod `z.discriminatedUnion(...)` / `z.union(...)` at the ROOT of an
// MCP tool's input compiles (via zod-to-json-schema) to exactly that.
// This is the Anthropic-side manifestation of the same root cause the
// v0.8.10 Gemini sanitizer (sanitizeGeminiSchema) handles — but
// Anthropic's constraint is NARROWER: it forbids the combinator ONLY at
// the top level. Nested oneOf/anyOf/allOf, $ref, $defs, and
// additionalProperties are all accepted.
//
// So this sanitizer is deliberately minimal and SURGICAL: it flattens
// ONLY a top-level combinator into a single object schema (merging the
// variants' properties + required, resolving $ref variants against the
// schema's own $defs/definitions), and returns every schema WITHOUT a
// top-level combinator byte-for-byte unchanged. Currently-working tools
// are untouched; nested unions Anthropic already accepts are preserved.
//
// The merge semantics intentionally mirror the Gemini sanitizer's
// mergeGeminiSchemaInto (union of properties + required, type-conflict
// defense) so the SAME union-rooted tool produces the same flattened
// shape on both providers — least-surprising, and already validated in
// production on the Gemini path for the union-rooted jobs-search-agent
// tools. (Kept self-contained rather than extracting a shared helper, to
// avoid refactoring the working Gemini driver on a behaviour change.)

// sanitizeAnthropicTools returns a copy of the tool list with each
// input_schema sanitized. The input slice + its ToolSpecs are not
// mutated (the raw schema may be shared with other providers in a
// fallback chain).
func sanitizeAnthropicTools(in []providers.ToolSpec) []providers.ToolSpec {
	if len(in) == 0 {
		return in
	}
	out := make([]providers.ToolSpec, len(in))
	for i, t := range in {
		t.InputSchema = sanitizeAnthropicToolSchema(t.InputSchema)
		out[i] = t
	}
	return out
}

// sanitizeAnthropicToolSchema flattens a top-level oneOf/anyOf/allOf into
// a single object schema. A schema with no top-level combinator (or one
// that doesn't parse as a JSON object) is returned unchanged.
func sanitizeAnthropicToolSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw // not an object schema we recognise — leave it
	}

	var variants []any
	combinator := ""
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if v, ok := root[key].([]any); ok && len(v) > 0 {
			variants, combinator = v, key
			break
		}
	}
	if combinator == "" {
		return raw // no top-level union → untouched (the common case)
	}

	defs := collectAnthropicDefs(root)
	for _, v := range variants {
		mergeAnthropicSchemaInto(root, resolveAnthropicVariant(v, defs))
	}
	delete(root, combinator)
	// A union-rooted schema declares no top-level type; the flattened
	// result is an object.
	if _, ok := root["type"]; !ok {
		root["type"] = "object"
	}

	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
}

// collectAnthropicDefs gathers the top-level $defs / definitions blocks
// (where zod-to-json-schema hoists the discriminated-union variants),
// keyed by canonical JSON-pointer ref string.
func collectAnthropicDefs(root map[string]any) map[string]any {
	defs := map[string]any{}
	for _, bucket := range []string{"$defs", "definitions"} {
		if d, ok := root[bucket].(map[string]any); ok {
			for k, v := range d {
				defs["#/"+bucket+"/"+k] = v
			}
		}
	}
	return defs
}

// resolveAnthropicVariant returns the variant's object schema, inlining a
// single `$ref` against defs. A dangling ref or a non-object variant
// yields nil (merged as a no-op).
func resolveAnthropicVariant(v any, defs map[string]any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if ref, ok := m["$ref"].(string); ok {
		if def, found := defs[ref].(map[string]any); found {
			return def
		}
		return nil
	}
	return m
}

// mergeAnthropicSchemaInto folds src into dst: existing dst keys win for
// scalars; `properties` maps merge key-by-key; `required` slices union.
// On a type conflict (dst object vs src array), src's structural fields
// (properties/items/required) are dropped — folding them across a type
// boundary produces a malformed schema. Mirrors mergeGeminiSchemaInto.
func mergeAnthropicSchemaInto(dst, src map[string]any) {
	if src == nil {
		return
	}
	typesConflict := false
	if dstType, dstHas := dst["type"].(string); dstHas {
		if srcType, srcHas := src["type"].(string); srcHas {
			typesConflict = dstType != srcType
		}
	}
	structural := map[string]bool{"properties": true, "items": true, "required": true}
	for k, v := range src {
		if typesConflict && structural[k] {
			continue
		}
		switch k {
		case "properties":
			vmap, ok := v.(map[string]any)
			if !ok {
				continue
			}
			dstProps, _ := dst["properties"].(map[string]any)
			if dstProps == nil {
				dstProps = map[string]any{}
				dst["properties"] = dstProps
			}
			for pk, pv := range vmap {
				if _, exists := dstProps[pk]; !exists {
					dstProps[pk] = pv
				}
			}
		case "required":
			varr, ok := v.([]any)
			if !ok {
				continue
			}
			dstReq, _ := dst["required"].([]any)
			seen := map[string]bool{}
			for _, r := range dstReq {
				if s, ok := r.(string); ok {
					seen[s] = true
				}
			}
			for _, r := range varr {
				if s, ok := r.(string); ok && !seen[s] {
					dstReq = append(dstReq, s)
					seen[s] = true
				}
			}
			dst["required"] = dstReq
		default:
			if _, exists := dst[k]; !exists {
				dst[k] = v
			}
		}
	}
}
