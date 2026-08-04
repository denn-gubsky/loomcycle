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

// TestEmbedBody_ImageWithNoCaptionEmbedsNothing — an uncaptioned image has no text
// until a describe pass runs. Storing a vector anyway would put a row that means
// nothing into the index, where it can only produce false matches.
func TestEmbedBody_ImageWithNoCaptionEmbedsNothing(t *testing.T) {
	d, vs, ctx := mermaidDocFixture(t, "image")

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
	if got := embeddedTextFor(t, vs, resultField(res, "id")); got != "" {
		t.Errorf("an uncaptioned image was embedded: %q", got)
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
