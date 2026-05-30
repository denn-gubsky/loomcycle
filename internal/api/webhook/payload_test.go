package webhook

import "testing"

func TestProjectPayload_NestedArrayAndAbsent_ProjectsCorrectly(t *testing.T) {
	body := []byte(`{
		"goal_text": "research the company",
		"user": {"id": "u-42"},
		"items": [{"name": "first"}, {"name": "second"}],
		"meta": {"count": 7, "flag": true}
	}`)

	mapping := map[string]string{
		"goal":        "$.goal_text",
		"user_id":     "$.user.id",
		"first_item":  "$.items[0].name",
		"second_item": "$.items[1].name",
		"count":       "$.meta.count",
		"flag":        "$.meta.flag",
		"missing":     "$.does.not.exist",
	}

	res, err := projectPayload(mapping, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"goal":        "research the company",
		"user_id":     "u-42",
		"first_item":  "first",
		"second_item": "second",
		"count":       "7",
		"flag":        "true",
		"missing":     "",
	}
	for k, v := range want {
		if res.Fields[k] != v {
			t.Errorf("field %q = %q, want %q", k, res.Fields[k], v)
		}
	}
	// The absent path must surface as a missing-key note (not a failure).
	foundMissing := false
	for _, mk := range res.MissingKeys {
		if mk == "missing" {
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Errorf("expected %q in MissingKeys, got %v", "missing", res.MissingKeys)
	}
}

func TestProjectPayload_MalformedBody_Errors(t *testing.T) {
	_, err := projectPayload(map[string]string{"goal": "$.x"}, []byte(`{not json`))
	if err == nil {
		t.Fatal("want error on malformed body, got nil")
	}
}

func TestParsePath_RejectsDisallowedShapes(t *testing.T) {
	cases := []string{
		"$.a[*]",      // wildcard index
		"$..a",        // recursive descent
		"$.a[?(@.x)]", // filter
		"a.b",         // missing $ root
		"$.",          // empty key
		"$.a[xyz]",    // non-integer index
		"$.a[-1]",     // negative index
	}
	for _, c := range cases {
		if _, err := parsePath(c); err == nil {
			t.Errorf("path %q: want reject, got accept", c)
		}
	}
}

func TestParsePath_AcceptsAllowedShapes(t *testing.T) {
	cases := []string{
		"$",
		"$.a",
		"$.a.b.c",
		"$.a[0]",
		"$.a[10].b",
	}
	for _, c := range cases {
		if _, err := parsePath(c); err != nil {
			t.Errorf("path %q: want accept, got %v", c, err)
		}
	}
}

func TestProjectPayload_ObjectValue_RendersCompactJSON(t *testing.T) {
	body := []byte(`{"obj":{"k":"v"}}`)
	res, err := projectPayload(map[string]string{"o": "$.obj"}, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Fields["o"] != `{"k":"v"}` {
		t.Errorf("object render = %q, want compact JSON", res.Fields["o"])
	}
}
