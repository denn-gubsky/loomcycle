package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const eventsBase = `"enabled":true,"delivery":"spawn","agent":"a"`

// A definition's events are checked when it is written: a provider this
// server knows, a header to read the event from, and a shape for every
// listed event — the header is not signed, so an event with no shape would
// count on the header alone.
func TestWebhookDefTool_EventsAreCheckedWhenWritten(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	gh := `"provider":"github","auth":{"signing_secret_env":"LOOMCYCLE_S"}`

	for _, ok := range []string{
		`{` + eventsBase + `,` + gh + `,"events":["push","pull_request"]}`,
		`{` + eventsBase + `,` + gh + `,"events":["star"],"event_shapes":{"star":["$.action","$.repository.id"]}}`,
		`{` + eventsBase + `,"event_header":"X-Ci-Event","events":["done"],"event_shapes":{"done":["$.build.id"]},"auth":{"signing_secret_env":"LOOMCYCLE_S"}}`,
		`{` + eventsBase + `,"provider":"gitlab","events":["push"],"auth":{"bearer_token_env":"LOOMCYCLE_T"}}`,
	} {
		if text, isErr := webhookCreate(t, tool, ctx, "w", ok); isErr {
			t.Errorf("%s: %s", ok, text)
		}
	}
	for _, tc := range []struct{ name, overlay, want string }{
		{"unknown provider", `{` + eventsBase + `,"provider":"bitbucket","auth":{"signing_secret_env":"LOOMCYCLE_S"}}`, `provider "bitbucket" is not one`},
		{"no header", `{` + eventsBase + `,"events":["done"],"event_shapes":{"done":["$.id"]},"auth":{"signing_secret_env":"LOOMCYCLE_S"}}`, `set event_header, or a provider`},
		{"no shape", `{` + eventsBase + `,` + gh + `,"events":["star"]}`, `"star", which has no shape`},
		{"ping listed", `{` + eventsBase + `,` + gh + `,"events":["ping"]}`, `answers the provider's ping itself`},
		{"twice", `{` + eventsBase + `,` + gh + `,"events":["push","push"]}`, `lists "push" twice`},
		{"bad path", `{` + eventsBase + `,` + gh + `,"events":["star"],"event_shapes":{"star":["$..x"]}}`, `not a supported JSONPath`},
		{"whole body", `{` + eventsBase + `,` + gh + `,"events":["star"],"event_shapes":{"star":["$"]}}`, `names the whole body`},
		{"shape for an unlisted event", `{` + eventsBase + `,` + gh + `,"events":["push"],"event_shapes":{"star":["$.a"]}}`, `event_shapes["star"] is not one of events`},
		{"shapes without events", `{` + eventsBase + `,` + gh + `,"event_shapes":{"push":["$.a"]}}`, `without events`},
		{"a gitlab hmac secret", `{` + eventsBase + `,"provider":"gitlab","events":["push"],"auth":{"signing_secret_env":"LOOMCYCLE_S"}}`, `auth.kind=bearer requires`},
	} {
		if text, isErr := webhookCreate(t, tool, ctx, "w", tc.overlay); !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s: %s, want a refusal mentioning %s", tc.name, text, tc.want)
		}
	}
}

// The stored definition keeps what its author wrote: the preset is applied
// when a delivery is handled, so a corrected preset reaches it.
func TestWebhookDefTool_APresetIsNotWrittenIntoTheDefinition(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	if text, isErr := webhookCreate(t, tool, ctx, "w", `{`+eventsBase+`,"provider":"github","events":["push"],"auth":{"signing_secret_env":"LOOMCYCLE_S"}}`); isErr {
		t.Fatal(text)
	}
	w := resolvedWebhook(t, tool, "w")
	if w.Provider != "github" || w.Auth.Header != "" || w.Auth.Kind != "" || w.EventHeader != "" || len(w.EventShapes) != 0 {
		t.Errorf("stored %+v, want only what was written", w)
	}
	if eff := w.WithProviderDefaults(); eff.EventHeader != "X-GitHub-Event" || eff.Auth.DeliveryIDHeader != "X-GitHub-Delivery" {
		t.Errorf("as a delivery sees it: %+v", eff)
	}
}

// A fork merges event_shapes key by key, and an empty list removes one. A
// definition built from a static one does not change the static one.
func TestWebhookDefTool_AForkMergesEventShapes(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	static := tool.Cfg.Webhooks["gh-push"]
	static.Provider, static.Events = "github", []string{"push", "star"}
	static.EventShapes = map[string][]string{"star": {"$.action"}}
	tool.Cfg.Webhooks["gh-push"] = static

	fork := func(overlay string) (string, bool) {
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"gh-push","overlay":`+overlay+`}`))
		if err != nil {
			t.Fatal(err)
		}
		return res.Text, res.IsError
	}
	if text, isErr := fork(`{"events":["push","star","fork"],"event_shapes":{"fork":["$.forkee.id"]}}`); isErr {
		t.Fatal(text)
	}
	// The stored version (for the shared tenant the yaml entry is what
	// resolves, so the row is read directly).
	row, err := tool.Store.WebhookDefGetActive(context.Background(), "", "gh-push")
	if err != nil {
		t.Fatal(err)
	}
	var w mergedWebhookDef
	if err := json.Unmarshal(row.Definition, &w); err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"star": {"$.action"}, "fork": {"$.forkee.id"}}; !reflect.DeepEqual(w.EventShapes, want) {
		t.Errorf("after the fork: %v, want %v", w.EventShapes, want)
	}
	// A definition built straight from the static entry (no stored parent)
	// merges into a copy: the static entry itself is unchanged.
	if _, err := tool.buildDefinition("gh-push", "", json.RawMessage(`{"event_shapes":{"star":[],"fork":["$.forkee.id"]}}`)); err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"star": {"$.action"}}; !reflect.DeepEqual(tool.Cfg.Webhooks["gh-push"].EventShapes, want) {
		t.Errorf("the static definition changed: %v", tool.Cfg.Webhooks["gh-push"].EventShapes)
	}
	// Removing star's shape while star is listed leaves it with none.
	if text, isErr := fork(`{"event_shapes":{"star":[]}}`); !isErr || !strings.Contains(text, `"star", which has no shape`) {
		t.Errorf("removing a listed event's only shape: %s, want it refused", text)
	}
}

// A shape rule read as a key no body has — `!path == value`, a key holding
// whitespace, = or a quote — is refused at create and fork, while a stored
// definition holding one still passes the checks a snapshot restore re-runs.
func TestWebhookDefTool_ARuleReadAsAKeyNoBodyHasIsRefusedWhenWritten(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	gh := `"provider":"github","auth":{"signing_secret_env":"LOOMCYCLE_S"}`
	for _, tc := range []struct{ name, overlay, want string }{
		{"! with ==", `{` + eventsBase + `,` + gh + `,"events":["star"],"event_shapes":{"star":["$.action","!$.deleted == true"]}}`, `do not combine`},
		{"a spaced filter key", `{` + eventsBase + `,` + gh + `,"filters":[{"path":"$.re f","op":"eq","value":"x"}]}`, `holding whitespace, = or a quote`},
		{"a quoted dedup key", `{` + eventsBase + `,` + gh + `,"dedup_key":["$.a\"b"]}`, `holding whitespace, = or a quote`},
		{"a number past exact comparison", `{` + eventsBase + `,` + gh + `,"filters":[{"path":"$.id","op":"eq","value":9007199254740993}]}`, `past 9007199254740991`},
	} {
		if text, isErr := webhookCreate(t, tool, ctx, "w", tc.overlay); !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("create, %s: %s, want a refusal mentioning %s", tc.name, text, tc.want)
		}
	}
	if text, isErr := webhookCreate(t, tool, ctx, "w", `{`+eventsBase+`,`+gh+`,"events":["star"],"event_shapes":{"star":["$.action"]}}`); isErr {
		t.Fatal(text)
	}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"w","overlay":{"event_shapes":{"star":["$.action","!$.deleted == true"]}}}`))
	if !res.IsError || !strings.Contains(res.Text, "do not combine") {
		t.Errorf("fork: %s, want a refusal", res.Text)
	}
	stored := json.RawMessage(`{` + eventsBase + `,` + gh + `,"events":["star"],"event_shapes":{"star":["$.action","!$.deleted == true"]}}`)
	if err := ValidateWebhookDefBody(stored); err != nil {
		t.Errorf("a stored definition holding the rule is refused on restore: %v", err)
	}
}
