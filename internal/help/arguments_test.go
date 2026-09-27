package help

import (
	"reflect"
	"testing"
)

// Arguments reads the names from each top-level bullet of the Arguments section,
// before the description's dash: several per bullet, nothing from nested bullets,
// fenced blocks, prose, or other sections.
func TestTopicArguments_ReadsTheBulletNames(t *testing.T) {
	topic := &Topic{Content: "Intro mentioning `ignored`.\n\n" +
		"## Arguments\n\n" +
		"- `document_id` (required) — the document; not `chunk_id`.\n" +
		"- `type`, `status` — labels.\n" +
		"- `from` / `to` — a time range.\n" +
		"- `format` — how to return it:\n" +
		"  - `\"conversation\"` — turns only.\n" +
		"- `op` — the operation.\n" +
		"\n```json\n{\"fenced\": 1}\n```\n" +
		"- `after_fence` — still in the section.\n\n" +
		"## Examples\n\n- `not_an_argument` — a bullet in another section.\n"}
	got, ok := topic.Arguments()
	want := []string{"document_id", "type", "status", "from", "to", "format", "after_fence"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("Arguments() = %v, %v; want %v, true", got, ok, want)
	}
}

// "None besides `op`" is a documented op that takes nothing — different from an
// article with no Arguments section, where nothing is known.
func TestTopicArguments_NoneVersusUnknown(t *testing.T) {
	none := &Topic{Content: "## Arguments\n\nNone besides `op`.\n\n## Returns\n"}
	if got, ok := none.Arguments(); !ok || len(got) != 0 {
		t.Errorf("\"None besides op\": %v, %v; want no arguments, documented", got, ok)
	}
	missing := &Topic{Content: "## Returns\n\n- `x` — y.\n"}
	if _, ok := missing.Arguments(); ok {
		t.Error("an article with no Arguments section reported its arguments as known")
	}
}
