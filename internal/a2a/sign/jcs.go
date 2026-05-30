// Package sign implements AgentCard JWS signing + verification for the
// A2A surface (RFC G slice A2A-6). It is a leaf package — it imports only
// the standard library and the SDK's a2a type — so both the server side
// (internal/api/a2a, which signs the served card) and the client side
// (internal/tools/a2a, which verifies a fetched peer card) can depend on
// it without an import cycle.
//
// The signature is an RFC 7515 JWS in the AgentCard.Signatures slice,
// computed over the RFC 8785 JSON Canonicalization (JCS) of the card
// WITH its signatures field cleared, using ES256 (ECDSA P-256 + SHA-256)
// per RFC G.
package sign

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"
)

// Canonicalize returns the RFC 8785 (JCS) canonical JSON encoding of v.
// It marshals v with the stdlib first, then re-serialises the resulting
// generic value with: object keys sorted by their UTF-16 code units,
// no insignificant whitespace, and numbers in the JCS number form. This
// is the byte string both signer and verifier hash, so the two MUST
// produce identical bytes for the same logical card — hence canonical
// (not Go's map-order-dependent) marshalling.
func Canonicalize(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("jcs: marshal: %w", err)
	}
	// Decode into a generic value. UseNumber keeps numeric literals
	// exact so we can re-emit them in JCS form without float rounding.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("jcs: decode: %w", err)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, generic); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeCanonical emits one JCS-canonical value.
func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, t)
	case json.Number:
		return writeCanonicalNumber(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortJCSKeys(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("jcs: unsupported value type %T", v)
	}
	return nil
}

// writeCanonicalNumber emits a JSON number in JCS form. For the integers
// that appear in an AgentCard (no floats today) the canonical form is
// the literal itself; we round-trip through float64 only when the
// literal is non-integral, matching the ECMAScript Number serialisation
// JCS mandates. AgentCard numbers are small integers in practice, so the
// common path is the exact integer string.
func writeCanonicalNumber(buf *bytes.Buffer, n json.Number) error {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		buf.WriteString(strconv.FormatInt(i, 10))
		return nil
	}
	f, err := n.Float64()
	if err != nil {
		return fmt.Errorf("jcs: bad number %q: %w", s, err)
	}
	// strconv 'g' with -1 precision yields the shortest round-trippable
	// representation, which matches JCS's ECMAScript number form for the
	// value range AgentCards use.
	buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}

// writeCanonicalString emits a JSON string with JCS escaping: only the
// mandatory escapes (", \, and the C0 controls) are used, everything
// else is emitted as literal UTF-8.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u`)
				const hex = "0123456789abcdef"
				buf.WriteByte('0')
				buf.WriteByte('0')
				buf.WriteByte(hex[(r>>4)&0xF])
				buf.WriteByte(hex[r&0xF])
			} else if r == utf8.RuneError {
				// Preserve invalid bytes as the replacement char's UTF-8;
				// AgentCard fields are valid UTF-8 in practice.
				buf.WriteRune(r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// sortJCSKeys sorts object keys by their UTF-16 code-unit sequence, as
// RFC 8785 requires. For the ASCII keys an AgentCard uses this is
// identical to byte ordering; the UTF-16 conversion makes it correct for
// any future non-ASCII keys too.
func sortJCSKeys(keys []string) {
	sort.Slice(keys, func(i, j int) bool {
		return lessUTF16(keys[i], keys[j])
	})
}

// lessUTF16 reports whether a sorts before b by UTF-16 code units.
func lessUTF16(a, b string) bool {
	ar, br := []rune(a), []rune(b)
	ai, bi := 0, 0
	for ai < len(ar) && bi < len(br) {
		au := utf16Units(ar[ai])
		bu := utf16Units(br[bi])
		for k := 0; k < len(au) && k < len(bu); k++ {
			if au[k] != bu[k] {
				return au[k] < bu[k]
			}
		}
		if len(au) != len(bu) {
			return len(au) < len(bu)
		}
		ai++
		bi++
	}
	return len(ar) < len(br)
}

// utf16Units returns the UTF-16 code units for a rune (one unit for the
// BMP, a surrogate pair for astral code points).
func utf16Units(r rune) []uint16 {
	if r <= 0xFFFF {
		return []uint16{uint16(r)}
	}
	r -= 0x10000
	return []uint16{
		uint16(0xD800 + (r >> 10)),
		uint16(0xDC00 + (r & 0x3FF)),
	}
}
