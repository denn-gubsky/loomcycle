package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileSink_AppendsJSONLAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	for _, action := range []string{"create", "rotate", "retire"} {
		if err := sink.Record(Event{
			Action:        action,
			ActorSubject:  "ops",
			TargetName:    "alice",
			TargetTenant:  "acme",
			TargetSubject: "alice",
			ScopesAfter:   []string{"runs:create"},
		}); err != nil {
			t.Fatalf("Record(%s): %v", action, err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var lines int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines++
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("line %d not valid JSON: %v", lines, err)
		}
		if ev.TS.IsZero() {
			t.Errorf("line %d: ts not stamped", lines)
		}
	}
	if lines != 3 {
		t.Errorf("got %d JSONL lines, want 3", lines)
	}
}

// TestFileSink_NeverContainsSecretMaterial is the security invariant:
// even when handed a struct, an audit line can never carry a token
// plaintext or hash because the Event type has no such field. This test
// guards against a future field being added with a leaky json tag.
func TestFileSink_NeverContainsSecretMaterial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, _ := NewFileSink(path)
	_ = sink.Record(Event{Action: "create", ActorTokenSuffix: "abc123", TargetName: "n"})
	raw, _ := os.ReadFile(path)
	body := string(raw)
	for _, forbidden := range []string{"token_hash", "\"token\"", "plaintext", "lct_"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("audit line contains forbidden material %q: %s", forbidden, body)
		}
	}
	// The non-secret suffix IS allowed (it's for correlation).
	if !strings.Contains(body, "abc123") {
		t.Error("expected the non-secret token suffix to be recorded")
	}
}

func TestNopSink(t *testing.T) {
	if err := (NopSink{}).Record(Event{Action: "create"}); err != nil {
		t.Errorf("NopSink.Record should never error: %v", err)
	}
}
