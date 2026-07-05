package skills

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSign_PrefixedHex(t *testing.T) {
	h := Sign(SkillContent{Name: "summariser"})
	if !strings.HasPrefix(h, "sha256:") {
		t.Errorf("hash %q missing sha256: prefix", h)
	}
	if got := len(h); got != 71 {
		t.Errorf("hash length = %d, want 71", got)
	}
}

func TestSign_Deterministic(t *testing.T) {
	c := SkillContent{
		Name:  "summariser",
		Body:  "Summarise the input concisely.",
		Tools: []string{"Read"},
	}
	if Sign(c) != Sign(c) {
		t.Error("non-deterministic")
	}
}

func TestSign_BodyChangeMovesHash(t *testing.T) {
	a := Sign(SkillContent{Name: "x", Body: "v1"})
	b := Sign(SkillContent{Name: "x", Body: "v2"})
	if a == b {
		t.Error("body change didn't move the hash")
	}
}

func TestSign_NilEqualsEmptySlice(t *testing.T) {
	a := Sign(SkillContent{Name: "x", Tools: nil})
	b := Sign(SkillContent{Name: "x", Tools: []string{}})
	if a != b {
		t.Errorf("nil vs empty tools differ: %s vs %s", a, b)
	}
}

func TestSign_TrailingWhitespaceNormalisedInBody(t *testing.T) {
	a := Sign(SkillContent{Name: "x", Body: "hello"})
	b := Sign(SkillContent{Name: "x", Body: "hello\n\n  "})
	if a != b {
		t.Errorf("trailing whitespace caused drift: %s vs %s", a, b)
	}
}

func TestSign_CRLFNormalisedInBody(t *testing.T) {
	a := Sign(SkillContent{Name: "x", Body: "line1\nline2"})
	b := Sign(SkillContent{Name: "x", Body: "line1\r\nline2"})
	if a != b {
		t.Errorf("CRLF vs LF drift: %s vs %s", a, b)
	}
}

func TestSign_InternalWhitespacePreserved(t *testing.T) {
	// Skill bodies often use double newlines to separate paragraphs;
	// must NOT collapse.
	a := Sign(SkillContent{Name: "x", Body: "para1\n\npara2"})
	b := Sign(SkillContent{Name: "x", Body: "para1\npara2"})
	if a == b {
		t.Error("paragraph spacing lost; internal whitespace was stripped")
	}
}

func TestFromSkill_NilSafe(t *testing.T) {
	c := FromSkill(nil)
	h := Sign(c)
	if !strings.HasPrefix(h, "sha256:") {
		t.Errorf("nil skill should still hash: %s", h)
	}
}

func TestFromSkill_DropsPath(t *testing.T) {
	a := FromSkill(&Skill{Name: "x", Body: "y", Path: "/foo"})
	b := FromSkill(&Skill{Name: "x", Body: "y", Path: "/bar"})
	if Sign(a) != Sign(b) {
		t.Error("Path leaked into hash")
	}
}

func TestFromOverlay_RoundTripMatchesFromSkill(t *testing.T) {
	skill := &Skill{
		Name:        "summariser",
		Description: "shorten things",
		Body:        "be brief",
		Tools:       []string{"Read"},
	}
	hashFromSkill := Sign(FromSkill(skill))

	overlay := json.RawMessage(`{
		"name": "summariser",
		"description": "shorten things",
		"body": "be brief",
		"tools": ["Read"]
	}`)
	parsed, _ := FromOverlay(overlay)
	hashFromOverlay := Sign(parsed)

	if hashFromSkill != hashFromOverlay {
		t.Errorf("YAML vs overlay diverge: %s vs %s", hashFromSkill, hashFromOverlay)
	}
}

func TestFromOverlay_Malformed(t *testing.T) {
	_, err := FromOverlay(json.RawMessage(`{`))
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
}
