package config

import (
	"strings"
	"testing"
)

func TestShapeHolds_PathAbsenceAndEquality(t *testing.T) {
	body := `{"kind":"push","n":3,"ok":true,"gone":null,"a":{"b":"x"},"big":9007199254740993}`
	for _, tc := range []struct {
		rule string
		want bool
	}{
		{"$.kind", true},
		{"$.missing", false},
		{"$.gone", false},
		{"!$.missing", true},
		{"!$.gone", true},
		{"!$.kind", false},
		{"!$.a.missing", true},
		{`$.kind == "push"`, true},
		{`$.kind=="push"`, true},
		{`  $.kind  ==  "push"  `, true},
		{`$.kind == "tag_push"`, false},
		{`$.n == 3`, true},
		{`$.n == 3.0`, true},
		{`$.n == "3"`, false},
		{`$.ok == true`, true},
		{`$.ok == false`, false},
		{`$.kind == true`, false},
		{`$.missing == "push"`, false},
		{`$.gone == "push"`, false},
		{`$.a.b == "x"`, true},
	} {
		if got := ShapeHolds([]string{tc.rule}, decodeBody(t, body)); got != tc.want {
			t.Errorf("%s: %t, want %t", tc.rule, got, tc.want)
		}
	}
	if ShapeHolds(nil, decodeBody(t, body)) {
		t.Error("an empty shape held")
	}
	if ShapeHolds([]string{"$.kind", "$..kind"}, decodeBody(t, body)) {
		t.Error("a shape with a rule that does not parse held")
	}
}

func TestCheckWebhookEvents_RefusesAShapeRuleThatCannotBeRead(t *testing.T) {
	base := func(rules ...string) Webhook {
		return Webhook{Provider: "github", Events: []string{"x"}, EventShapes: map[string][]string{"x": rules}}
	}
	if err := CheckWebhookEvents(base("$.a", "!$.b", `$.c == "d"`, "$.n == 1", "$.t == false")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rules []string
		want  string
	}{
		{[]string{"!$.a"}, "absences alone"},
		{[]string{"$.a", "!$"}, "whole body"},
		{[]string{"$.a", "!a"}, "not a supported JSONPath"},
		{[]string{`$.a == push`}, "not a JSON string"},
		{[]string{`$.a == null`}, "string, number or boolean"},
		{[]string{`$.a == {"b":1}`}, "string, number or boolean"},
		{[]string{`$.a == [1]`}, "string, number or boolean"},
		{[]string{`$.a == "x" == "y"`}, "not a JSON string"},
		{[]string{`== "x"`}, "not a supported JSONPath"},
		{[]string{`$.a == "` + strings.Repeat("x", 1025) + `"`}, "at most 1024"},
	} {
		if err := CheckWebhookEvents(base(tc.rules...)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want a refusal mentioning %q", tc.rules, err, tc.want)
		}
	}
}

// With events, the receiver puts the provider, event and delivery id in the
// run's metadata.webhook: a definition may not hold its own.
func TestCheckWebhookEvents_RefusesItsOwnMetadataWebhook(t *testing.T) {
	w := Webhook{Provider: "github", Events: []string{"push"}, Metadata: map[string]any{"webhook": "mine"}}
	if err := CheckWebhookEvents(w); err == nil || !strings.Contains(err.Error(), `"webhook"`) {
		t.Errorf("%v, want a refusal naming metadata.webhook", err)
	}
	w.Events = nil
	if err := CheckWebhookEvents(w); err != nil {
		t.Errorf("without events it is the definition's own: %v", err)
	}
}
