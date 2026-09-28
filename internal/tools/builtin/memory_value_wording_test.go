package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/help"
)

// A model read "`value` must be valid JSON — to store text, pass a JSON string"
// as "JSON-encode the text yourself" and sent "value": "\"Standup moves…\"",
// which stores the quote marks. Measured on deepseek-v4-flash: 3 such writes in
// 3 runs, one of which then told the user the quoted value read back correctly.
// What a model reads about `value` — the set article and the schema — must say
// text goes in as a plain string with no quotes inside, and must not ask for
// "valid JSON".
func TestMemoryValueWording_SaysTextIsAPlainStringWithoutInnerQuotes(t *testing.T) {
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	article, ok := set.Get("Memory/set")
	if !ok {
		t.Fatal("no Memory/set article")
	}

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&Memory{}).InputSchema(), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	for name, text := range map[string]string{
		"Memory/set article": article.Content,
		"schema value field": schema.Properties["value"].Description,
	} {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "must be valid json") {
			t.Errorf("%s still asks for valid JSON, which reads as \"encode the text yourself\"", name)
		}
		if !strings.Contains(lower, "plain string") || !strings.Contains(lower, "quotes inside") {
			t.Errorf("%s does not say text is a plain string with no quotes inside it:\n%s", name, text)
		}
	}
}
