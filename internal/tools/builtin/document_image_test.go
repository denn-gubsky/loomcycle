package builtin

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestImageEmbedText_ComposesCaptionAndDescription covers the composition rules
// directly, including the ones whose absence would be invisible in an integration
// test: the empty result that must stay empty, and the data-URI-is-not-a-caption
// case.
func TestImageEmbedText_ComposesCaptionAndDescription(t *testing.T) {
	const dataURI = "![alt](data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==)"
	for _, tc := range []struct {
		name, caption, description, want string
	}{
		{"caption only", "the login screen", "", "image: the login screen"},
		{"description only", "", "a form with two fields", "image: a form with two fields"},
		{
			"both are kept", "login screen", "a form with two fields",
			"image: login screen a form with two fields",
		},
		{
			// Identical text twice would double-weight it in the vector.
			"identical halves collapse", "a login form", "A LOGIN FORM",
			"image: a login form",
		},
		// Nothing to index must be "" so the caller stores NO vector. A row that
		// exists ranks against every query, so a placeholder is worse than absence.
		{"neither", "", "", ""},
		{"whitespace only", "   ", "\n\t", ""},
		// A data URI is not a caption: an image chunk can transiently hold the
		// rendered markdown form as its body, and base64 indexes nothing.
		{"data uri is not a caption", dataURI, "", ""},
		{"data uri body with a description", dataURI, "a red square", "image: a red square"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageEmbedText(tc.caption, tc.description); got != tc.want {
				t.Errorf("imageEmbedText(%q, %q)\n want %q\n  got %q",
					tc.caption, tc.description, tc.want, got)
			}
		})
	}
}

// TestEmbedBody_ImageCaptionIsEmbeddedWithoutAModel is the phase-4a guarantee.
//
// Phase 1 and 3 skipped image chunks entirely, so an image was unsearchable until
// some future describe pass ran — and on a deployment with no vision model
// reachable, forever, for text the AUTHOR had already written. The caption needs no
// model, so it is embedded on write.
//
// FAIL-BEFORE: restoring `case "image": return` in embedBody makes this fail with
// no embedding stored at all.
func TestEmbedBody_ImageCaptionIsEmbeddedWithoutAModel(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "image", "the", "login", "screen")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Shots"}`))
	docID := resultField(res, "document_id")
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID, "title": "Login",
		"type": "image", "body": "the login screen",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	got := embeddedTextFor(t, vs, resultField(res, "id"))
	if got != "image: the login screen" {
		t.Errorf("caption was not embedded on write: %q", got)
	}
}

// TestEmbedBody_ImageDataURIBodyIsNeverIndexedAsBase64 — a data URI is not a
// caption. The chunk still becomes searchable, but via its TITLE (the fallback), and
// the base64 must never reach the embedder: it would match nothing while consuming
// the scope's vector quota.
func TestEmbedBody_ImageDataURIBodyIsNeverIndexedAsBase64(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "image", "raw")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Shots"}`))
	docID := resultField(res, "document_id")
	// A data URI body is the other no-caption shape, and the one that would index
	// base64 if the classifier were skipped.
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID, "title": "Raw",
		"type": "image", "body": "![](data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==)",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	got := embeddedTextFor(t, vs, resultField(res, "id"))
	if strings.Contains(got, "iVBOR") || strings.Contains(got, "base64") {
		t.Errorf("base64 reached the embedder: %q", got)
	}
	// The title carries it instead — the data URI contributed nothing.
	if got != "Raw" {
		t.Errorf("embed text = %q, want the chunk title \"Raw\"", got)
	}
}

// TestEmbedBody_ImageDescriptionJoinsTheCaption proves the phase-4b half reaches
// the index: once a description is persisted next to the asset, a body rewrite
// embeds caption AND description.
//
// This is what makes the description worth persisting — see document_image.go. A
// regenerated description would be different text on every re-embed, so the index
// would silently re-rank without the content having changed.
func TestEmbedBody_ImageDescriptionJoinsTheCaption(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "image", "login", "screen", "two", "text", "fields")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Shots"}`))
	docID := resultField(res, "document_id")
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID, "title": "Login",
		"type": "image", "body": "login screen",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	chunkID := resultField(res, "id")

	// Attach real bytes so there is a chunk_assets row to describe.
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==")
	sa, _ := json.Marshal(map[string]any{
		"op": "set_asset", "id": chunkID,
		"media_type": "image/png", "data": base64.StdEncoding.EncodeToString(png),
	})
	if res, err := d.Execute(ctx, sa); err != nil || res.IsError {
		t.Fatalf("set_asset: %v %s", err, res.Text)
	}

	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if err := d.setAssetDescription(ctx, key, chunkID, "two text fields"); err != nil {
		t.Fatalf("setAssetDescription: %v", err)
	}
	if got := d.assetDescription(ctx, key, chunkID); got != "two text fields" {
		t.Fatalf("description did not persist: %q", got)
	}

	// A body rewrite re-embeds, now with the description available.
	upd, _ := json.Marshal(map[string]any{
		"op": "update_chunk", "id": chunkID, "revision": 2, "body": "login screen",
	})
	res, err = d.Execute(ctx, upd)
	if err != nil || res.IsError {
		t.Fatalf("update_chunk: %v %s", err, res.Text)
	}
	got := embeddedTextFor(t, vs, chunkID)
	for _, w := range []string{"login screen", "two text fields"} {
		if !strings.Contains(got, w) {
			t.Errorf("embed text missing %q: %q", w, got)
		}
	}
}

// TestAssetDescription_MissingRowIsEmptyNotAnError — the reader is on the embed
// path, so a chunk with no asset (or a scope predating the column) must degrade to
// caption-only rather than fail an author's write.
func TestAssetDescription_MissingRowIsEmptyNotAnError(t *testing.T) {
	d, _, ctx := mermaidDocFixture(t, "image")
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	if got := d.assetDescription(ctx, key, "no-such-chunk"); got != "" {
		t.Errorf("want \"\" for a chunk with no asset, got %q", got)
	}
}

// TestEmbedBody_UncaptionedImageStillEmbedsItsDescription is the regression test for
// a bug found on the live deployment, after the describe pass reported success.
//
// embedBody guarded on the RAW body — `if strings.TrimSpace(body) == "" { return }`
// — BEFORE the per-type switch. An image's body is its caption, so an UNCAPTIONED
// image returned early and its generated description was never consulted. The
// describe pass then persisted a description, reported described=2, and the images
// stayed unsearchable: `get_asset` showed a description, the sweep showed success,
// and the candidate count never moved. Correct-looking from every surface.
//
// Both pre-existing image tests used a CAPTIONED chunk, which is why the case went
// uncovered — and an uncaptioned image is the common one for an uploaded asset.
//
// FAIL-BEFORE: restoring the raw-body guard ahead of the switch makes this fall
// through to the title alone, never reaching the description.
func TestEmbedBody_UncaptionedImageStillEmbedsItsDescription(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "image", "five", "round", "characters", "circuit", "board")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Logos"}`))
	docID := resultField(res, "document_id")
	// NO caption — exactly the shape of an uploaded asset.
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID, "title": "Logo",
		"type": "image", "body": "",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	chunkID := resultField(res, "id")

	png, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==")
	sa, _ := json.Marshal(map[string]any{
		"op": "set_asset", "id": chunkID,
		"media_type": "image/png", "data": base64.StdEncoding.EncodeToString(png),
	})
	if r, err := d.Execute(ctx, sa); err != nil || r.IsError {
		t.Fatalf("set_asset: %v %s", err, r.Text)
	}

	// Precondition: with no caption and no description yet, the chunk is searchable by
	// its TITLE (the fallback) — and notably NOT by anything describing the picture.
	if got := embeddedTextFor(t, vs, chunkID); got != "Logo" {
		t.Fatalf("precondition: want the title-only fallback %q, got %q", "Logo", got)
	}

	// The describe pass persists a description and re-embeds.
	if err := d.SetAssetDescription(ctx, "user", chunkID, "five round characters on a circuit board"); err != nil {
		t.Fatalf("SetAssetDescription: %v", err)
	}
	got := embeddedTextFor(t, vs, chunkID)
	if got == "" {
		t.Fatal("an uncaptioned image was NOT embedded after a description was persisted — " +
			"the description sits in the database while the image stays unsearchable, and " +
			"every surface an operator checks reports success")
	}
	if !strings.Contains(got, "five round characters") {
		t.Errorf("embed text does not carry the description: %q", got)
	}
}

// TestTitleEmbedText_KeepsHeadingsDropsNonLanguage pins the one judgement the title
// fallback makes. Both surfaces (the write path and the admin backfill) route through
// this, so they cannot disagree about what a usable title is.
//
// The threshold is "contains a letter", NOT a length. Measured on the reference
// deployment: of 20 sampled bodyless chunks, 18 had meaningful titles and the
// shortest were "Active RFCs" (11) and "Configuration" (13) — a length filter would
// discard exactly what someone searching a document would type.
func TestIndexableText_KeepsHeadingsDropsNonLanguage(t *testing.T) {
	for _, keep := range []string{
		"Active RFCs",
		"Configuration",
		"2. Backend additions (loomcycle core)",
		"RFC BE — History Tool (browse / search / rename / annotate past chats)",
		`"replica_id": "replica-a",`, // a fragment used as a heading — weak, but language
	} {
		if got := indexableText(keep); got == "" {
			t.Errorf("indexableText(%q) dropped a title that carries language", keep)
		}
	}
	for _, drop := range []string{
		"", "   ", "\n\t",
		"---",   // a rule used as a heading
		"42",    // a bare number
		"...",   // punctuation
		"1.2.3", // a version fragment
	} {
		if got := indexableText(drop); got != "" {
			t.Errorf("indexableText(%q) = %q, want \"\" — no letters means no language, "+
				"and a vector built from it can only produce false matches", drop, got)
		}
	}
	// Trimmed, not reformatted: the title is the author's text.
	if got := indexableText("  Active RFCs  "); got != "Active RFCs" {
		t.Errorf("got %q, want the trimmed title verbatim", got)
	}
}

// TestEmbedBody_BodylessChunkFallsBackToItsTitle is the feature: a heading chunk is
// the most navigable part of a document and was the one part retrieval could not see,
// because only bodies were embedded.
//
// FAIL-BEFORE: removing the fallback makes this embed nothing.
func TestEmbedBody_BodylessChunkFallsBackToItsTitle(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "phase", "name-links", "transclusion", "backend", "additions")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Plan"}`))
	docID := resultField(res, "document_id")
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID,
		"title": "Phase 2 — name-links + transclusion", "body": "",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	got := embeddedTextFor(t, vs, resultField(res, "id"))
	if got != "Phase 2 — name-links + transclusion" {
		t.Errorf("a bodyless heading embedded %q, want its title — a heading organises "+
			"the document and is exactly what a searcher types", got)
	}
}

// TestEmbedBody_BodyWinsOverTitle — the title is a FALLBACK, never an addition.
// Appending it would double-weight whatever the author happened to put in the
// heading, on every chunk in the corpus.
func TestEmbedBody_BodyWinsOverTitle(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "savepoint", "nesting", "heading", "words")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Doc"}`))
	docID := resultField(res, "document_id")
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID,
		"title": "heading words", "body": "SAVEPOINT nesting is LIFO",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	got := embeddedTextFor(t, vs, resultField(res, "id"))
	if got != "SAVEPOINT nesting is LIFO" {
		t.Errorf("embed text = %q, want the body alone; the title must not be appended", got)
	}
}

// TestEmbedBody_TitlelessBodylessChunkEmbedsNothing — the floor. A chunk with no body
// and no usable title must store NO vector, because a row that exists ranks against
// every query.
func TestEmbedBody_TitlelessBodylessChunkEmbedsNothing(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "anything")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Doc"}`))
	docID := resultField(res, "document_id")
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID, "title": "---", "body": "",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	if got := embeddedTextFor(t, vs, resultField(res, "id")); got != "" {
		t.Errorf("a chunk with no body and a letterless title embedded %q", got)
	}
}

// TestChunkIDFromBodyKey_OnlyMatchesChunkBodies guards the exported key parser the
// backfill relies on: an ordinary memory row must not be mistaken for a chunk and
// sent looking for a title.
func TestChunkIDFromBodyKey_OnlyMatchesChunkBodies(t *testing.T) {
	if got := ChunkIDFromBodyKey("doc.chunk:abc123"); got != "abc123" {
		t.Errorf("got %q, want abc123", got)
	}
	for _, notAChunk := range []string{"", "memory/fact/x", "doc.chunkabc", "chunk:abc"} {
		if got := ChunkIDFromBodyKey(notAChunk); got != "" {
			t.Errorf("ChunkIDFromBodyKey(%q) = %q, want \"\"", notAChunk, got)
		}
	}
}

// TestProseEmbedText_DropsScaffoldOnlyBodies is measured, not hypothetical. On the
// reference deployment a user asking "which medicine do I take" got these ranked
// ABOVE the fact naming their medication (0.434):
//
//	"---"      0.477
//	"```sh"    0.452
//	"#"        0.451
//
// A short syntax token embeds near the centroid of everything, so it ranks mid-high
// for every query. A heading-split import creates these by turning a fence line into
// its own chunk.
func TestIndexableText_DropsScaffoldOnlyText(t *testing.T) {
	for _, drop := range []string{
		"```sh", "```bash", "```", "```go",
		"~~~", "~~~python",
		"---", "----", "***", "___",
		"#", "##", "######",
		"  ```sh  ", "\n---\n", "",
	} {
		if got := indexableText(drop); got != "" {
			t.Errorf("indexableText(%q) = %q, want \"\" — scaffolding outranks real "+
				"answers because a short syntax token sits near every query", drop, got)
		}
	}
	// The two rejections are independent, and this is why BOTH are needed: a letter
	// test alone would accept "```sh" (it contains "sh"), and a scaffold test alone
	// would accept "42". Neither subsumes the other.
	if strings.ContainsAny("```sh", "abcdefghijklmnopqrstuvwxyz") == false {
		t.Fatal("precondition: \"```sh\" is expected to contain letters")
	}
	if mdScaffoldOnlyRe.MatchString("42") {
		t.Error("precondition: \"42\" is not scaffolding, so only the letter rule can " +
			"reject it — if the scaffold rule matches it, this test no longer shows why " +
			"both rejections exist")
	}
}

// TestProseEmbedText_KeepsRealProseIncludingFencedContent — the predicate is anchored
// and whole-string, so a body that merely CONTAINS or BEGINS with a fence is real
// content and must survive. Getting this wrong would silently delete every code
// example from the index.
func TestIndexableText_KeepsRealProseIncludingFencedContent(t *testing.T) {
	for _, keep := range []string{
		"```sh\nmake build-all\n```",
		"---\ntitle: front matter\n---",
		"# A real heading with text",
		"SAVEPOINT nesting is LIFO",
		"## Setup\n\nRun the installer.",
		"-- a SQL comment",
		"#tag",
	} {
		if got := indexableText(keep); got == "" {
			t.Errorf("indexableText(%q) dropped real content", keep)
		}
	}
	// Trimmed, not rewritten.
	if got := indexableText("  real body  "); got != "real body" {
		t.Errorf("got %q, want the trimmed body", got)
	}
}

// TestEmbedBody_ScaffoldOnlyBodyEmbedsNothing wires the predicate to the write path,
// and pins that it does NOT then fall through to the title: a chunk whose body is a
// fence line usually has a scaffold-ish title too, and embedding one to replace the
// other just moves the noise.
func TestEmbedBody_ScaffoldOnlyBodyEmbedsNothing(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "sh", "bash", "code")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Doc"}`))
	docID := resultField(res, "document_id")
	body, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "document_id": docID, "title": "```sh", "body": "```sh",
	})
	res, err := d.Execute(ctx, body)
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	if got := embeddedTextFor(t, vs, resultField(res, "id")); got != "" {
		t.Errorf("a scaffolding-only chunk embedded %q", got)
	}
}
