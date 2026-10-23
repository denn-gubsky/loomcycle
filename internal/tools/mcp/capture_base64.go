package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// ResultPolicy is how one server's tool results are rendered (RFC FF).
type ResultPolicy struct {
	// CaptureBase64 is the server's binary_results: capture. A file the
	// server returns as base64 inside a text block is kept in the run's
	// inbox and the model is handed the stub instead of the base64.
	CaptureBase64 bool
	// Base64MinBytes is the shortest string considered a file; 0 takes
	// DefaultBase64MinBytes.
	Base64MinBytes int64
}

// DefaultBase64MinBytes is the base64 minimum when none is configured.
const DefaultBase64MinBytes = 4 << 10

func (p ResultPolicy) minBytes() int {
	if p.Base64MinBytes > 0 {
		return int(p.Base64MinBytes)
	}
	return DefaultBase64MinBytes
}

// base64MaxDepth bounds the JSON walk. A block nested deeper is left as it
// came: the walk keeps a frame per level, and no file-bearing reply nests
// anywhere near this.
const base64MaxDepth = 64

// captureTextBase64 keeps the files a text block carries as base64 and
// returns the block with each one replaced by its stub. A block that parses
// as JSON is walked and each long string field is a candidate, typed and
// named by its sibling fields; any other block is a candidate whole. Every
// byte that is not a kept candidate is returned as it came, so a block with
// nothing to keep is returned unchanged.
//
// A candidate is kept only when it decodes to something recognisable as a
// file: a type the bytes (or a sibling field) name, or readable text. A long
// string that decodes to bytes that are neither is an opaque token — a
// cursor, a signature, an encrypted blob the caller passes back — and is
// left alone, since replacing it would break the next call that needs it.
func captureTextBase64(ctx context.Context, origin, tool, text string, minBytes int) string {
	if len(text) < minBytes {
		return text // no candidate can be shorter than the block holding it
	}
	trimmed := strings.TrimSpace(text)
	if json.Valid([]byte(trimmed)) {
		if out, ok := captureJSONBase64(ctx, origin, tool, text, minBytes); ok {
			return out
		}
		return text
	}
	stub, ok := keepBase64(ctx, origin, tool, trimmed, fileMeta{}, minBytes)
	if !ok {
		return text
	}
	lead := len(text) - len(strings.TrimLeftFunc(text, unicode.IsSpace))
	return text[:lead] + stub + text[lead+len(trimmed):]
}

// fileMeta is what a JSON object's sibling fields say about a file in it.
type fileMeta struct {
	name      string
	mediaType string
}

// note records a sibling field that names or types the object's file. The
// names are the ones Microsoft Graph, the MCP spec and common servers use.
func (m *fileMeta) note(key, value string) {
	if len(value) > 1024 {
		return // a name or a type is short; anything longer is not one
	}
	switch strings.ToLower(key) {
	case "contenttype", "content_type", "mimetype", "mime_type", "mediatype", "media_type":
		m.mediaType = value
	case "name", "filename", "file_name":
		m.name = value
	}
}

// jsonFrame is one open object or array of the walk.
type jsonFrame struct {
	object  bool
	wantKey bool   // object: the next string is a key
	key     string // object: the key of the value being read
	meta    fileMeta
	cands   []jsonSpan
}

// jsonSpan is a string value's raw span in the block, quotes included, and
// its decoded value.
type jsonSpan struct {
	start, end int
	value      string
}

// captureJSONBase64 walks a JSON block's tokens and replaces each kept
// candidate's raw span — the quoted string exactly as the server wrote it —
// with the stub as a JSON string. The rest of the block is not re-encoded,
// so key order, spacing and escapes stay byte-identical. Candidates are
// judged when their object closes, because a sibling naming the file may
// come after the bytes. ok is false when nothing was kept.
func captureJSONBase64(ctx context.Context, origin, tool, text string, minBytes int) (string, bool) {
	type replacement struct {
		start, end int
		with       string
	}
	var repls []replacement
	judge := func(f *jsonFrame) {
		for _, c := range f.cands {
			if stub, ok := keepBase64(ctx, origin, tool, c.value, f.meta, minBytes); ok {
				repls = append(repls, replacement{c.start, c.end, jsonQuote(stub)})
			}
		}
	}

	dec := json.NewDecoder(strings.NewReader(text))
	top := &jsonFrame{} // a block that is one bare string
	var stack []*jsonFrame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false
		}
		end := int(dec.InputOffset())
		cur := top
		if len(stack) > 0 {
			cur = stack[len(stack)-1]
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				if len(stack) >= base64MaxDepth {
					return "", false
				}
				if cur.object {
					cur.wantKey = true // this container is the value of cur.key
				}
				stack = append(stack, &jsonFrame{object: v == '{', wantKey: v == '{'})
			case '}', ']':
				judge(stack[len(stack)-1])
				stack = stack[:len(stack)-1]
			}
		case string:
			if cur.object && cur.wantKey {
				cur.key, cur.wantKey = v, false
				continue
			}
			if cur.object {
				cur.meta.note(cur.key, v)
				cur.wantKey = true
			}
			if len(v) >= minBytes {
				if start := quotedStart(text, end); start >= 0 {
					cur.cands = append(cur.cands, jsonSpan{start: start, end: end, value: v})
				}
			}
		default: // a number, a bool or null
			if cur.object {
				cur.wantKey = true
			}
		}
	}
	judge(top)
	if len(repls) == 0 {
		return "", false
	}
	// An inner object is judged before the object around it, so the spans
	// arrive out of order; they never overlap (each is one string).
	slices.SortFunc(repls, func(a, b replacement) int { return a.start - b.start })
	var out strings.Builder
	at := 0
	for _, r := range repls {
		out.WriteString(text[at:r.start])
		out.WriteString(r.with)
		at = r.end
	}
	out.WriteString(text[at:])
	return out.String(), true
}

// quotedStart is the offset of the opening quote of the JSON string whose
// closing quote is at end-1, or -1. A quote inside the string is escaped,
// so the opening one is the first quote back not preceded by an odd run of
// backslashes.
func quotedStart(text string, end int) int {
	if end < 2 || end > len(text) || text[end-1] != '"' {
		return -1
	}
	for i := end - 2; i >= 0; i-- {
		if text[i] != '"' {
			continue
		}
		bs := 0
		for j := i - 1; j >= 0 && text[j] == '\\'; j-- {
			bs++
		}
		if bs%2 == 0 {
			return i
		}
	}
	return -1
}

// jsonQuote is s as a JSON string, without the HTML escaping json.Marshal
// adds: the stub is read by a model, not embedded in a page.
func jsonQuote(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return `""`
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// keepBase64 keeps s in the inbox when it is a base64 file, and returns the
// stub that stands for it. ok is false when s is not one, and then nothing
// was written.
func keepBase64(ctx context.Context, origin, tool, s string, meta fileMeta, minBytes int) (string, bool) {
	if len(s) < minBytes || !base64Alphabet(s) {
		return "", false
	}
	raw, ok := decodeBase64(s)
	if !ok || len(raw) == 0 {
		return "", false
	}
	mediaType := meta.mediaType
	if genericType(inbox.Sniff(meta.mediaType, raw[:min(len(raw), 512)], meta.name)) {
		if !readableText(raw) {
			return "", false
		}
		// Text that was sent as base64 still costs the model the base64:
		// kept as a .txt and stubbed like any other file.
		mediaType = "text/plain"
	}
	res := inbox.Capture(ctx, inbox.Item{Name: meta.name, MediaType: mediaType, Origin: origin, Fallback: tool}, raw)
	return res.Stub, true
}

// base64Alphabet reports whether s is made only of the standard and URL
// base64 alphabets, padding and line breaks. It is the cheap test: a string
// with a space or a dot in it is prose, a URL or a JWT, never one file.
func base64Alphabet(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '+', c == '/', c == '-', c == '_', c == '=', c == '\r', c == '\n':
		default:
			return false
		}
	}
	return true
}

// decodeBase64 decodes s in the standard or URL alphabet, padded or not.
// encoding/base64 skips line breaks itself, so a MIME-wrapped body decodes.
func decodeBase64(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err := enc.DecodeString(s); err == nil {
			return raw, true
		}
	}
	return nil, false
}

// genericType mirrors the inbox's own rule for a type that says nothing
// about the bytes (inbox.Sniff returns normalised types).
func genericType(mt string) bool {
	return mt == "" || mt == "application/octet-stream" || mt == "binary/octet-stream" || mt == "text/plain"
}

// readableText reports whether b is UTF-8 text a person could read: valid
// UTF-8 with no control bytes but tab, line breaks and form feed. Zero
// bytes are valid UTF-8, so validity alone would call an all-"A" string
// text.
func readableText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if (c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f') || c == 0x7f {
			return false
		}
	}
	return true
}
