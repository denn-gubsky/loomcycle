package webhook

import (
	"encoding/json"
	"fmt"

	"bytes"
	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
	"io"
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
	// RawBody is the exact signed body the mapping was applied to. Carried so
	// the run-input builder can default the agent's `goal` to the whole event
	// when the def declares no `goal` target (F28) — without re-threading the
	// bytes through every buildRunInput call site. Set only by projectPayload
	// (validated as JSON there); nil on hand-constructed results in tests.
	RawBody []byte
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
	return projectDecoded(mapping, &deliveryBody{raw: body})
}

// projectDecoded is projectPayload over a body the delivery's earlier checks
// may already have decoded.
func projectDecoded(mapping map[string]string, b *deliveryBody) (projectResult, error) {
	res := projectResult{Fields: make(map[string]string, len(mapping)), RawBody: b.raw}

	doc, err := b.decoded()
	if err != nil {
		return projectResult{}, err
	}

	for target, path := range mapping {
		segs, err := jsonpath.Parse(path)
		if err != nil {
			return projectResult{}, fmt.Errorf("payload_mapping[%q]: %w", target, err)
		}
		v, ok := jsonpath.Eval(doc, segs)
		if !ok {
			res.Fields[target] = ""
			res.MissingKeys = append(res.MissingKeys, target)
			continue
		}
		res.Fields[target] = jsonpath.Stringify(v)
	}
	return res, nil
}

// deliveryBody is a delivery's raw body, decoded at most once for the checks
// that read it: an event's shape, the filters and the payload mapping.
type deliveryBody struct {
	raw  []byte
	doc  any
	err  error
	done bool
}

func (b *deliveryBody) decoded() (any, error) {
	if !b.done {
		b.doc, b.err = decodeJSONBody(b.raw)
		b.done = true
	}
	return b.doc, b.err
}

// decodeJSONBody decodes a delivery's body, after checkJSONShape has bounded
// what decoding it costs.
func decodeJSONBody(body []byte) (interface{}, error) {
	if err := checkJSONShape(body, maxJSONDepth, maxJSONTokens); err != nil {
		return nil, fmt.Errorf("malformed json body: %w", err)
	}
	var doc interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("malformed json body: %w", err)
	}
	return doc, nil
}

// A delivery's body is scanned before it is decoded. Decoding builds the whole
// document in memory, and its cost follows the body's shape more than its
// size: deep nesting, or a few bytes per value repeated over the whole limit.
// The scan reads tokens one at a time and stops at the first limit passed.
// Far above any forge's payload (GitHub's nest about ten deep).
const (
	maxJSONDepth  = 128
	maxJSONTokens = 1_000_000
)

// checkJSONShape reports a body nested deeper than maxDepth, or holding more
// than maxTokens tokens. Malformed JSON is left to the decoder that follows.
func checkJSONShape(body []byte, maxDepth, maxTokens int) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	depth, tokens := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return nil
		}
		if tokens++; tokens > maxTokens {
			return fmt.Errorf("more than %d JSON values", maxTokens)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if depth++; depth > maxDepth {
					return fmt.Errorf("nested deeper than %d", maxDepth)
				}
			default:
				depth--
			}
		}
	}
}
