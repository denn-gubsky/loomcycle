package webhook

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// projectResult is the outcome of applying a WebhookDef's payload_mapping
// to a request body. Fields holds the resolved string values keyed by the
// operator-chosen dotted target (e.g. "goal", "user_id",
// "user_credentials.GITHUB_TOKEN", "run_metadata.repo"). MissingKeys lists
// the mapping targets whose source path resolved to nothing — the caller
// logs these as a tracing note but does NOT fail the request (a webhook
// payload is external and may legitimately omit optional fields).
type projectResult struct {
	Fields      map[string]string
	MissingKeys []string
}

// projectPayload applies a payload_mapping to a raw JSON body. The mapping
// is target → source-jsonpath: each key is an arbitrary dotted target the
// caller interprets (goal, user_id, user_credentials.*, run_metadata.*),
// and each value is a STRICT-subset JSONPath expression evaluated against
// the body.
//
// Supported JSONPath subset (validated up front, anything else rejected):
//   - root marker "$"
//   - dot segments: $.a.b.c
//   - array index segments: $.a[0].b, $.items[2]
//
// Explicitly UNSUPPORTED (rejected at validate, never evaluated):
//   - wildcards ($.a[*], $..b)
//   - filters ($.a[?(@.x)])
//   - recursive descent ($..)
//   - any expression / script
//
// A malformed body is a request error (returned err → server maps 400).
// A malformed mapping EXPRESSION is also a request error (the path string
// failed the allowlist) — this is a 400 because the operator's Def shape is
// validated at write time (WH-2), so an invalid path reaching here means a
// hand-edited/forged Def; failing closed is correct. An absent path inside
// a well-formed body is NOT an error: it yields an empty string and a
// MissingKeys entry.
func projectPayload(mapping map[string]string, body []byte) (projectResult, error) {
	res := projectResult{Fields: make(map[string]string, len(mapping))}

	var doc interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return projectResult{}, fmt.Errorf("malformed json body: %w", err)
	}

	for target, path := range mapping {
		segs, err := parsePath(path)
		if err != nil {
			return projectResult{}, fmt.Errorf("payload_mapping[%q]: %w", target, err)
		}
		v, ok := evalPath(doc, segs)
		if !ok {
			res.Fields[target] = ""
			res.MissingKeys = append(res.MissingKeys, target)
			continue
		}
		res.Fields[target] = stringify(v)
	}
	return res, nil
}

// pathSeg is one resolved step in a parsed JSONPath: either a map key
// (Key set, IsIndex false) or an array index (Index set, IsIndex true).
type pathSeg struct {
	Key     string
	Index   int
	IsIndex bool
}

// parsePath validates and tokenizes a strict-subset JSONPath. Returns an
// error for any shape outside the allowlist (wildcards, filters, recursive
// descent, empty segments). The grammar is intentionally tiny so the
// rejection surface is exhaustive: a leading "$", then zero or more
// segments of the form `.key` or `[N]`.
func parsePath(path string) ([]pathSeg, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("empty path")
	}
	if path[0] != '$' {
		return nil, fmt.Errorf("path must start with $")
	}
	// Reject recursive descent ("..") and the bare-wildcard forms outright,
	// before the segment walk, so the error message names the actual
	// violation rather than a downstream parse glitch.
	if strings.Contains(path, "..") {
		return nil, fmt.Errorf("recursive descent (..) not supported")
	}
	if strings.Contains(path, "*") {
		return nil, fmt.Errorf("wildcard (*) not supported")
	}
	if strings.Contains(path, "?") || strings.Contains(path, "@") {
		return nil, fmt.Errorf("filter expressions not supported")
	}

	rest := path[1:] // strip the leading $
	var segs []pathSeg
	for len(rest) > 0 {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			// Read a key up to the next '.' or '['.
			end := strings.IndexAny(rest, ".[")
			var key string
			if end == -1 {
				key = rest
				rest = ""
			} else {
				key = rest[:end]
				rest = rest[end:]
			}
			if key == "" {
				return nil, fmt.Errorf("empty key segment")
			}
			segs = append(segs, pathSeg{Key: key})
		case '[':
			end := strings.IndexByte(rest, ']')
			if end == -1 {
				return nil, fmt.Errorf("unterminated [ index")
			}
			idxStr := rest[1:end]
			idx, err := strconv.Atoi(strings.TrimSpace(idxStr))
			if err != nil || idx < 0 {
				// Only non-negative integer indices are allowed. A quoted
				// key form (['key']) is intentionally NOT supported — it
				// widens the grammar with no payload-mapping need.
				return nil, fmt.Errorf("invalid array index %q", idxStr)
			}
			segs = append(segs, pathSeg{Index: idx, IsIndex: true})
			rest = rest[end+1:]
		default:
			return nil, fmt.Errorf("unexpected character %q in path", string(rest[0]))
		}
	}
	return segs, nil
}

// evalPath walks the parsed segments over a decoded JSON document. Returns
// (value, true) when every segment resolves; (nil, false) on any miss
// (wrong type, absent key, out-of-range index). No panics — a type
// mismatch is a miss, not a crash.
func evalPath(doc interface{}, segs []pathSeg) (interface{}, bool) {
	cur := doc
	for _, s := range segs {
		if s.IsIndex {
			arr, ok := cur.([]interface{})
			if !ok || s.Index >= len(arr) {
				return nil, false
			}
			cur = arr[s.Index]
			continue
		}
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		v, present := obj[s.Key]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// stringify converts a resolved JSON value to the flat string the mapping
// produces. Strings pass through verbatim; numbers/bools render via their
// natural form; objects/arrays/null render as compact JSON so a mapping
// that targets a sub-object still yields a deterministic string rather than
// Go's %v formatting.
func stringify(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// json.Unmarshal decodes all numbers to float64. strconv with -1
		// precision avoids trailing zeros and scientific notation for the
		// common integer-id case.
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
