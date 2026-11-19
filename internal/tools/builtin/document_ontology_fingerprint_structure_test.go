package builtin

import (
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestOntologyEdit_SectionHeaderReReadNeedsAPlan (#2766 review): a section header is not
// an entry, but a body that changes how the section pre-pass reads it takes every entry
// of the section out of force or reads it as another section's. With a relation in use
// under it, each re-read needs a plan's fingerprint; an edit of the header's prose does
// not.
func TestOntologyEdit_SectionHeaderReReadNeedsAPlan(t *testing.T) {
	f := newFingerprintFixture(t)
	header := f.chunk("Relations")
	fp := f.planFingerprint(f.chunk("supervises"))
	for what, body := range map[string]string{
		"drop @section":      "Relation kinds.",
		"switch the section": "@section instances",
		"an unknown section": "@section bogus",
	} {
		wantPlanRefusal(t, what, f.change(f.ctx, "update_chunk", header, map[string]any{"body": body}), planRequiredCode, fp)
	}
	if r := f.change(f.ctx, "update_chunk", header, map[string]any{"body": "@section relations\n\nHow things relate."}); r.IsError {
		t.Errorf("prose edit of the header: %s", r.Text)
	}
	if r := f.change(f.ctx, "update_chunk", header, map[string]any{"body": "@section classes", "plan_fingerprint": f.planFingerprint(f.chunk("supervises"))}); r.IsError {
		t.Errorf("re-read with a fresh fingerprint: %s", r.Text)
	}
}

// TestOntologyMove_SectionHeaderUnderAClassNeedsAPlan: moving the Relations header under
// a class keeps its chunks in the read but reads every relation as a class.
func TestOntologyMove_SectionHeaderUnderAClassNeedsAPlan(t *testing.T) {
	f := newFingerprintFixture(t)
	fp := f.planFingerprint(f.chunk("supervises"))
	wantPlanRefusal(t, "move the Relations header under Provision",
		f.change(f.ctx, "move_chunk", f.chunk("Relations"), map[string]any{"new_parent_id": f.chunk("Provision")}), planRequiredCode, fp)
	if rel := relationByName(f.read(), "supervises"); rel.ChunkID == "" {
		t.Error("supervises left the relations after the refused move")
	}
}

// newWrapperFixture puts Memo (typed documents rest on it) under an untitled chunk under
// Document. The reader skips the wrapper, so Memo still reads as Document's subclass.
func newWrapperFixture(t *testing.T) (f *retiredFixture, wrapper string) {
	t.Helper()
	f = newFingerprintFixture(t)
	read := f.read()
	// create_chunk needs a title; clearing it leaves the chunk unnamed (it has no
	// heading), so the reader walks through it.
	wrapper = f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"parent_id": f.chunk("Document"), "title": "Holder", "body": "Kept together."})["id"].(string)
	if r := f.change(f.ctx, "update_chunk", wrapper, map[string]any{"title": ""}); r.IsError {
		t.Fatalf("untitle the wrapper: %s", r.Text)
	}
	if r := f.change(f.ctx, "move_chunk", f.chunk("Memo"), map[string]any{"new_parent_id": wrapper}); r.IsError {
		t.Fatalf("move Memo under the wrapper (the reader sees no move): %s", r.Text)
	}
	if termParent(t, f.read(), "Memo") != "Document" {
		t.Fatalf("Memo's parent through the wrapper = %q, want Document", termParent(t, f.read(), "Memo"))
	}
	return f, wrapper
}

// TestOntologyEdit_UnnamedWrapperOfAnEntryInUseNeedsAPlan: retiring, titling or moving
// an untitled chunk retires, re-parents or moves the entries under it.
func TestOntologyEdit_UnnamedWrapperOfAnEntryInUseNeedsAPlan(t *testing.T) {
	f, wrapper := newWrapperFixture(t)
	fp := f.planFingerprint(f.chunk("Memo"))
	wantPlanRefusal(t, "retire the wrapper", f.change(f.ctx, "update_chunk", wrapper, map[string]any{"status": memrank.OntologyStatusRetired}), planRequiredCode, fp)
	wantPlanRefusal(t, "title the wrapper", f.change(f.ctx, "update_chunk", wrapper, map[string]any{"title": "Folder"}), planRequiredCode, fp)
	wantPlanRefusal(t, "move the wrapper", f.change(f.ctx, "move_chunk", wrapper, map[string]any{"new_parent_id": f.chunk("Provision")}), planRequiredCode, fp)
	if termParent(t, f.read(), "Memo") != "Document" || f.status(f.chunk("Memo")) != "" {
		t.Error("Memo moved or was retired by a refused change")
	}
	if r := f.change(f.ctx, "update_chunk", wrapper, map[string]any{"body": "Kept together, for now."}); r.IsError {
		t.Errorf("prose edit of the wrapper: %s", r.Text)
	}
	if r := f.change(f.ctx, "move_chunk", wrapper, map[string]any{"new_parent_id": f.chunk("Provision"), "plan_fingerprint": f.planFingerprint(f.chunk("Memo"))}); r.IsError {
		t.Fatalf("move the wrapper with a fresh fingerprint: %s", r.Text)
	}
	if termParent(t, f.read(), "Memo") != "Provision" {
		t.Errorf("Memo's parent = %q, want Provision", termParent(t, f.read(), "Memo"))
	}
}

// TestOntologyEdit_RegistryKeyChangeNeedsAPlan: an adopted instance's registry_key is
// how the plan finds its subject; changing or clearing a set key in fields is planned as
// a retire. A fields write that keeps it needs no plan.
func TestOntologyEdit_RegistryKeyChangeNeedsAPlan(t *testing.T) {
	f := newFingerprintFixture(t)
	// Adoption records the key, through update_chunk: that first write needs no plan.
	if rep, err := f.d.AdoptInstances(f.ctx, false); err != nil || rep.Adopted == 0 {
		t.Fatalf("adopt: %+v %v", rep, err)
	}
	acme := f.chunk("Acme")
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": acme})
	fields, _ := g["fields"].(map[string]any)
	key, _ := fields["registry_key"].(string)
	if key == "" {
		t.Fatalf("precondition: Acme carries no registry_key: %v", g["fields"])
	}
	fp := f.planFingerprint(acme)
	wantPlanRefusal(t, "change the key", f.change(f.ctx, "update_chunk", acme, map[string]any{"fields": map[string]any{"registry_key": "organization:other"}}), planRequiredCode, fp)
	wantPlanRefusal(t, "clear the key", f.change(f.ctx, "update_chunk", acme, map[string]any{"fields": map[string]any{}}), planRequiredCode, fp)
	wantPlanRefusal(t, "change the key with a meaning edit", f.change(f.ctx, "update_chunk", acme,
		map[string]any{"body": "@a organization\n\nA supplier.", "fields": map[string]any{"registry_key": "organization:other"}}), planRequiredCode, fp)
	if r := f.change(f.ctx, "update_chunk", acme, map[string]any{"fields": map[string]any{"registry_key": key, "note": "kept"}}); r.IsError {
		t.Errorf("fields write that keeps the key: %s", r.Text)
	}
}
