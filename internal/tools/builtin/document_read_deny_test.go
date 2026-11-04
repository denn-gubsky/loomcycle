package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The remaining doors around a document read-deny (security re-audit
// 2026-11-04, B7 a-e / D14 / D16; RFC FY Part G): each test is a caller whose
// tenant ACL denies it the target, going in through a door other than the op
// the deny was written for.

// withACL is ctx with eff as the run's set, everything else kept.
func withACL(ctx context.Context, eff *authz.Effective) context.Context {
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.ACL = eff })
}

func denyRule(verb, resource string) authz.Rule {
	return authz.Rule{Effect: authz.Deny, Actions: []string{verb}, Resources: []string{resource}}
}

// TestExportMD_TransclusionRespectsTheReadersACL: a readable document that
// embeds a part of an unreadable one renders the embed as its literal text, the
// same text a missing target leaves, never the hidden body.
func TestExportMD_TransclusionRespectsTheReadersACL(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	embeds := "Before. ![[/kb/secret/plan#Plan part]] Middle. ![[" + s.secretChunk + "]] After. ![[/kb/missing/doc#Nothing]]"
	b, _ := json.Marshal(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": s.publicDoc, "title": "Embeds", "body": embeds})
	if _, r := docExec(t, d, open, string(b)); r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	export := `{"op":"export_md","scope":"tenant","document_id":"` + s.publicDoc + `","include_metadata":false}`

	// The fixture is live: with no set the embeds expand to the hidden body.
	if out := copyExec(t, d, open, export); !strings.Contains(asStr(out["markdown"]), "savepoint nesting secret") {
		t.Fatalf("fixture: with no set the embed should expand: %s", out["markdown"])
	}

	denied := tenantDocCtx(base, aclSet(t, denyRule("read", "document:tenant:/kb/secret/**")))
	md := asStr(copyExec(t, d, denied, export)["markdown"])
	if strings.Contains(md, "savepoint nesting secret") {
		t.Errorf("export_md inlined the body of a document the reader may not read:\n%s", md)
	}
	for _, literal := range []string{"![[/kb/secret/plan#Plan part]]", "![[" + s.secretChunk + "]]", "![[/kb/missing/doc#Nothing]]"} {
		if !strings.Contains(md, literal) {
			t.Errorf("the embed %s should stay as written, as a missing target does:\n%s", literal, md)
		}
	}
	if !strings.Contains(md, "savepoint nesting public") {
		t.Errorf("the readable document's own text is missing:\n%s", md)
	}
}

// TestAssetGET_AsksTheDocumentsRead: the asset GET reads through
// Document.ReadAsset, which answers an image of an unreadable document exactly
// as it answers a chunk with no asset.
func TestAssetGET_AsksTheDocumentsRead(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0xfe, 0x01}
	var img string
	for _, doc := range []string{s.secretDoc, s.publicDoc} {
		c := copyExec(t, d, open, `{"op":"create_chunk","scope":"tenant","document_id":"`+doc+`","title":"Diagram"}`)
		copyExec(t, d, open, `{"op":"set_asset","scope":"tenant","id":"`+asStr(c["id"])+`","media_type":"image/png","data":"`+base64.StdEncoding.EncodeToString(png)+`"}`)
		if doc == s.secretDoc {
			img = asStr(c["id"])
		}
	}
	if _, _, ok, err := d.ReadAsset(open, "tenant", img); err != nil || !ok {
		t.Fatalf("fixture: with no set the asset should be served: ok=%v err=%v", ok, err)
	}

	denied := tenantDocCtx(base, aclSet(t, denyRule("read", "document:tenant:/kb/secret/**")))
	mt, data, ok, err := d.ReadAsset(denied, "tenant", img)
	if err != nil || ok || len(data) != 0 || mt != "" {
		t.Errorf("the asset of an unreadable document: ok=%v type=%q bytes=%d err=%v, want the answer a missing asset gets", ok, mt, len(data), err)
	}
	_, _, okMissing, errMissing := d.ReadAsset(denied, "tenant", "no-such-chunk")
	if okMissing || errMissing != nil {
		t.Fatalf("a missing asset: ok=%v err=%v", okMissing, errMissing)
	}
}
