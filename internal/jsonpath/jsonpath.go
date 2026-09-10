// Package jsonpath implements the STRICT-SUBSET JSONPath used to project a
// value out of a decoded JSON document.
//
// It is deliberately tiny. The grammar is root + dot keys + non-negative array
// indices, and everything else — wildcards, recursive descent, filters, script
// expressions, the quoted-key form — is rejected at Parse rather than evaluated.
// That is a security property, not a simplification: these paths come from
// operator-authored definitions that project ATTACKER-INFLUENCEABLE documents
// (an inbound webhook body, a channel message), so the parser's job is to make
// the unsupported half of JSONPath unreachable rather than to be capable.
//
// Extracted verbatim from internal/api/webhook, which is where it grew, so a
// second caller could share the grammar instead of copying it. A copied parser
// is a parser that drifts, and drift here means two definitions that look alike
// projecting differently.
//
// Stdlib-only by design: its callers include packages that keep a deliberately
// small dependency set.
package jsonpath

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Seg is one resolved step in a parsed JSONPath: either a map key
// (Key set, IsIndex false) or an array index (Index set, IsIndex true).
type Seg struct {
	Key     string
	Index   int
	IsIndex bool
}

// Parse validates and tokenizes a strict-subset JSONPath. Returns an
// error for any shape outside the allowlist (wildcards, filters, recursive
// descent, empty segments). The grammar is intentionally tiny so the
// rejection surface is exhaustive: a leading "$", then zero or more
// segments of the form `.key` or `[N]`.
func Parse(path string) ([]Seg, error) {
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
	var segs []Seg
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
			segs = append(segs, Seg{Key: key})
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
			segs = append(segs, Seg{Index: idx, IsIndex: true})
			rest = rest[end+1:]
		default:
			return nil, fmt.Errorf("unexpected character %q in path", string(rest[0]))
		}
	}
	return segs, nil
}

// Eval walks the parsed segments over a decoded JSON document. Returns
// (value, true) when every segment resolves; (nil, false) on any miss
// (wrong type, absent key, out-of-range index). No panics — a type
// mismatch is a miss, not a crash.
func Eval(doc interface{}, segs []Seg) (interface{}, bool) {
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

// Stringify converts a resolved JSON value to the flat string the mapping
// produces. Strings pass through verbatim; numbers/bools render via their
// natural form; objects/arrays/null render as compact JSON so a mapping
// that targets a sub-object still yields a deterministic string rather than
// Go's %v formatting.
func Stringify(v interface{}) string {
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
