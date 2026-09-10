package webhook

import (
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
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
	res := projectResult{Fields: make(map[string]string, len(mapping)), RawBody: body}

	var doc interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return projectResult{}, fmt.Errorf("malformed json body: %w", err)
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
