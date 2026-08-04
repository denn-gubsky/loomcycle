package builtin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// decodeUseNumber re-parses a tool result with json.Number so a unix-nanos
// timestamp survives the check: default map[string]any decoding lands large
// int64s in a float64, which loses precision above 2^53 (~9e15) — and unix-nanos
// are ~1.7e18, so an exact == on the float form would fail even on correct code.
func decodeUseNumber(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func numInt64(t *testing.T, v any) int64 {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("value %v (%T) is not a json.Number", v, v)
	}
	i, err := n.Int64()
	if err != nil {
		t.Fatalf("json.Number %v to int64: %v", n, err)
	}
	return i
}

// TestGetChunk_ReturnsEntityBlockForFact: a chunk with a sidecar reads back with
// a typed entity block; a plain chunk carries none — the read-side distinction
// the memory view is built on.
func TestGetChunk_ReturnsEntityBlockForFact(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)

	validAt := time.Now().Add(-24 * time.Hour).UnixNano()
	up, r := docExec(t, d, ctx, fmt.Sprintf(
		`{"op":"upsert_chunk","scope":"user","document_id":"%s","parent_id":"%s",`+
			`"title":"Ada Lovelace","natural_key":"person:ada-lovelace","type":"person",`+
			`"class":"evidential","valid_at":%d}`, docID, root, validAt))
	if r.IsError {
		t.Fatalf("upsert: %s", r.Text)
	}
	factID := asStr(up["id"])

	_, r = docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+factID+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	got := decodeUseNumber(t, r.Text)
	ent, ok := got["entity"].(map[string]any)
	if !ok {
		t.Fatalf("get_chunk on a fact has no entity block: %v", got)
	}
	if ent["class"] != "evidential" {
		t.Errorf("entity.class = %v, want evidential", ent["class"])
	}
	if ent["natural_key"] != "person:ada-lovelace" {
		t.Errorf("entity.natural_key = %v, want person:ada-lovelace", ent["natural_key"])
	}
	if ent["retired"] != false {
		t.Errorf("entity.retired = %v, want false (a live fact)", ent["retired"])
	}
	if va := numInt64(t, ent["valid_at"]); va != validAt {
		t.Errorf("entity.valid_at = %d, want %d (raw unix-nanos must round-trip losslessly)", va, validAt)
	}

	// A plain chunk (no sidecar) must NOT carry an entity block, or a reader
	// cannot tell a fact from an ordinary section.
	plain, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"plain section"}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	pg, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+asStr(plain["id"])+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk plain: %s", r.Text)
	}
	if _, has := pg["entity"]; has {
		t.Errorf("a plain chunk must not carry an entity block: %v", pg["entity"])
	}
}

// factIDByKey maps each returned fact's natural_key → id, and asserts every fact
// carries an entity block (the whole point of list_facts).
func factIDByKey(t *testing.T, out map[string]any) map[string]string {
	t.Helper()
	byKey := map[string]string{}
	arr, _ := out["facts"].([]any)
	for _, f := range arr {
		m, _ := f.(map[string]any)
		ent, ok := m["entity"].(map[string]any)
		if !ok {
			t.Errorf("a fact in list_facts is missing its entity block: %v", m)
			continue
		}
		byKey[asStr(ent["natural_key"])] = asStr(m["id"])
	}
	return byKey
}

// TestListFacts_FiltersAndShape: list_facts returns only chunks that have a
// sidecar (facts), narrows by type/class, and honors the retirement filter.
func TestListFacts_FiltersAndShape(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)

	mkFact := func(key, title, typ, class string) string {
		t.Helper()
		out, r := docExec(t, d, ctx, fmt.Sprintf(
			`{"op":"upsert_chunk","scope":"user","document_id":"%s","parent_id":"%s",`+
				`"title":"%s","natural_key":"%s","type":"%s","class":"%s"}`,
			docID, root, title, key, typ, class))
		if r.IsError {
			t.Fatalf("upsert %s: %s", key, r.Text)
		}
		return asStr(out["id"])
	}

	aID := mkFact("person:ada", "Ada", "person", "derived")
	mkFact("place:london", "London", "place", "evidential")
	// A plain chunk (no sidecar) must never appear in list_facts.
	plain, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"just a section"}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	plainID := asStr(plain["id"])

	// No filter → both facts, never the plain chunk.
	all, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if r.IsError {
		t.Fatalf("list_facts: %s", r.Text)
	}
	byKey := factIDByKey(t, all)
	if len(byKey) != 2 {
		t.Errorf("list_facts returned %d facts, want 2 (the plain chunk must be excluded): %v", len(byKey), all["facts"])
	}
	for _, id := range byKey {
		if id == plainID {
			t.Errorf("list_facts returned the plain chunk %s — it has no sidecar and is not a fact", plainID)
		}
	}
	if _, ok := byKey["person:ada"]; !ok {
		t.Errorf("list_facts is missing the person fact: %v", all["facts"])
	}

	// type filter narrows to one.
	byType, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","type":"person"}`)
	if r.IsError {
		t.Fatalf("list_facts type: %s", r.Text)
	}
	if k := factIDByKey(t, byType); len(k) != 1 || k["person:ada"] == "" {
		t.Errorf("type=person did not narrow to the one person fact: %v", byType["facts"])
	}

	// class filter narrows to the other.
	byClass, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","class":"evidential"}`)
	if r.IsError {
		t.Fatalf("list_facts class: %s", r.Text)
	}
	if k := factIDByKey(t, byClass); len(k) != 1 || k["place:london"] == "" {
		t.Errorf("class=evidential did not narrow to the one evidential fact: %v", byClass["facts"])
	}

	// An unknown class is refused, naming the valid set.
	if _, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","class":"permanent"}`); !r.IsError ||
		!strings.Contains(r.Text, "derived") {
		t.Errorf("an unknown class must be refused naming the valid set: %q", r.Text)
	}

	// Retire the person fact: a replacement supersedes it.
	cID := mkFact("person:ada-corrected", "Ada Lovelace", "person", "derived")
	if _, r := docExec(t, d, ctx, `{"op":"supersede_chunk","scope":"user","id":"`+cID+
		`","supersedes_id":"`+aID+`"}`); r.IsError {
		t.Fatalf("supersede: %s", r.Text)
	}

	// Default list excludes the retired fact.
	def, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if r.IsError {
		t.Fatalf("list_facts default: %s", r.Text)
	}
	if k := factIDByKey(t, def); k["person:ada"] != "" {
		t.Errorf("a superseded fact must be excluded by default: %v", def["facts"])
	} else if k["person:ada-corrected"] == "" {
		t.Errorf("the replacement fact should be current: %v", def["facts"])
	}

	// include_retired brings it back.
	withRetired, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user","include_retired":true}`)
	if r.IsError {
		t.Fatalf("list_facts include_retired: %s", r.Text)
	}
	k := factIDByKey(t, withRetired)
	if k["person:ada"] == "" {
		t.Errorf("include_retired must return the superseded fact: %v", withRetired["facts"])
	}
	// The retired fact's entity block must SAY it is retired.
	dec := decodeUseNumber(t, r.Text)
	for _, f := range dec["facts"].([]any) {
		m := f.(map[string]any)
		ent := m["entity"].(map[string]any)
		if asStr(ent["natural_key"]) == "person:ada" && ent["retired"] != true {
			t.Errorf("the superseded fact's entity.retired = %v, want true", ent["retired"])
		}
	}
}
