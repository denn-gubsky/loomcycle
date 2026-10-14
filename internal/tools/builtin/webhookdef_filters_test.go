package builtin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// Filters are checked when a definition is written: paths, operators and
// value types, value_from against the definition's own metadata, and each
// filter's keys read as written.
func TestWebhookDefTool_FiltersAreCheckedWhenWritten(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	auth := `"auth":{"signing_secret_env":"LOOMCYCLE_S"}`

	ok := `{` + eventsBase + `,` + auth + `,"metadata":{"repo":"acme/app"},"filters":[` +
		`{"path":"$.ref","op":"glob","value":"refs/heads/*"},` +
		`{"path":"$.deleted","op":"eq","value":false},` +
		`{"path":"$.repository.full_name","op":"eq","value_from":"metadata.repo"}]}`
	if text, isErr := webhookCreate(t, tool, ctx, "w", ok); isErr {
		t.Fatal(text)
	}
	w := resolvedWebhook(t, tool, "w")
	want := []config.WebhookFilter{
		{Path: "$.ref", Op: "glob", Value: "refs/heads/*"},
		{Path: "$.deleted", Op: "eq", Value: false},
		{Path: "$.repository.full_name", Op: "eq", ValueFrom: "metadata.repo"},
	}
	if !reflect.DeepEqual(w.Filters, want) {
		t.Errorf("stored filters %+v, want %+v", w.Filters, want)
	}

	for _, tc := range []struct{ name, overlay, want string }{
		{"bad op", `{` + eventsBase + `,` + auth + `,"filters":[{"path":"$.ref","op":"regex","value":"x"}]}`, `filters[0]: op "regex" is not one of`},
		{"bad path", `{` + eventsBase + `,` + auth + `,"filters":[{"path":"$..ref","op":"eq","value":"x"}]}`, `not a supported JSONPath`},
		{"null", `{` + eventsBase + `,` + auth + `,"filters":[{"path":"$.ref","op":"eq","value":null}]}`, `null is not a value`},
		{"glob a number", `{` + eventsBase + `,` + auth + `,"filters":[{"path":"$.ref","op":"glob","value":1}]}`, `glob takes a string`},
		{"value_from no such key", `{` + eventsBase + `,` + auth + `,"filters":[{"path":"$.ref","op":"eq","value_from":"metadata.repo"}]}`, `has no key "repo"`},
		{"a misspelt key in a filter", `{` + eventsBase + `,` + auth + `,"filters":[{"path":"$.ref","op":"eq","valu":"x"}]}`, `unknown key "valu"`},
		{"a mapping path that does not parse", `{` + eventsBase + `,` + auth + `,"payload_mapping":{"goal":"$.commits[*].message"}}`, `payload_mapping["goal"]`},
	} {
		if text, isErr := webhookCreate(t, tool, ctx, "w2", tc.overlay); !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s: %s, want a refusal mentioning %s", tc.name, text, tc.want)
		}
	}
}

// A fork replaces the whole list, and an empty list removes it; a fork that
// leaves filters out keeps its parent's.
func TestWebhookDefTool_AForkReplacesFilters(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	base := `{` + eventsBase + `,"auth":{"signing_secret_env":"LOOMCYCLE_S"},"filters":[` +
		`{"path":"$.ref","op":"glob","value":"refs/heads/*"},{"path":"$.deleted","op":"eq","value":false}]}`
	if text, isErr := webhookCreate(t, tool, ctx, "w", base); isErr {
		t.Fatal(text)
	}
	fork := func(overlay string) {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"w","overlay":`+overlay+`}`))
		if err != nil || res.IsError {
			t.Fatalf("fork %s: %v %s", overlay, err, res.Text)
		}
	}
	fork(`{"agent":"a"}`)
	if got := len(resolvedWebhook(t, tool, "w").Filters); got != 2 {
		t.Fatalf("a fork without filters kept %d, want the parent's 2", got)
	}
	fork(`{"filters":[{"path":"$.ref","op":"eq","value":"refs/heads/main"}]}`)
	if got := resolvedWebhook(t, tool, "w").Filters; len(got) != 1 || got[0].Value != "refs/heads/main" {
		t.Fatalf("after a fork naming one filter: %+v, want only it", got)
	}
	fork(`{"filters":[]}`)
	if got := resolvedWebhook(t, tool, "w").Filters; len(got) != 0 {
		t.Fatalf("after filters: []: %+v, want none", got)
	}
}

// A snapshot restore re-validates a stored body, and must accept what it
// accepted before: a payload_mapping path that does not parse is refused
// only when a definition is created or forked.
func TestWebhookDefBody_ARestoreAcceptsAnUnparseableMappingPath(t *testing.T) {
	body := json.RawMessage(`{"enabled":true,"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"},"payload_mapping":{"goal":"$.commits[*].message"}}`)
	if err := ValidateWebhookDefBody(body); err != nil {
		t.Fatalf("a body a restore accepted before is refused: %v", err)
	}
}
