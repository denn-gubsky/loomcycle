package mcp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// The minimal pairs below hand the same text block to a server under
// passthrough and under capture, or vary one property of the block under
// capture, so each test isolates one rule of the base64 heuristic.

var (
	capturePolicy     = mcp.ResultPolicy{CaptureBase64: true}
	passthroughPolicy = mcp.ResultPolicy{}
)

// runTextUnder returns what the model sees for one text block from a server
// rendered under pol.
func runTextUnder(t *testing.T, ctx context.Context, pol mcp.ResultPolicy, text string) string {
	t.Helper()
	pool := mcp.NewPool(func(_, _ string) (mcp.Caller, error) {
		return contentCaller{content: []map[string]any{{"type": "text", "text": text}}}, nil
	}, nil, nil)
	pool.SetResultPolicy(func(_, name string) mcp.ResultPolicy {
		if name != "ms365" {
			t.Errorf("policy asked for server %q", name)
		}
		return pol
	})
	t.Cleanup(pool.Close)
	res, err := firstTool(t, ctx, pool, "ms365").Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res.Text
}

// pdfOf is a PDF-signed body of n bytes.
func pdfOf(n int) []byte {
	b := bytes.Repeat([]byte("board papers, page text. "), n/25+1)
	return append([]byte("%PDF-1.7\n"), b...)[:n]
}

// opaqueBytes are n bytes with no file signature that are not text: what a
// cursor, a signature or an encrypted token decodes to.
func opaqueBytes(n int) []byte {
	var out []byte
	seed := sha256.Sum256([]byte("opaque"))
	for len(out) < n {
		seed = sha256.Sum256(seed[:])
		out = append(out, seed[:]...)
	}
	out = out[:n]
	out[0] = 0xff // no signature can start here, and it is not UTF-8
	return out
}

// graphAttachment is a Microsoft Graph fileAttachment as ms-365-mcp-server
// returns it by default: the bytes as base64 in a JSON string field.
func graphAttachment() (prefix, suffix string) {
	return `{"@odata.type":"#microsoft.graph.fileAttachment","name":"Board papers.pdf","contentType":"application/pdf","size":6000,"contentBytes":"`,
		`","isInline":false}`
}

// short bounds a block quoted in a failure message: the blocks here are
// kilobytes of base64.
func short(s string) string {
	if len(s) <= 240 {
		return s
	}
	return s[:120] + " … " + s[len(s)-120:]
}

func keptFile(t *testing.T, r *inboxtest.Run, rel string) []byte {
	t.Helper()
	ref, ok := r.Set.Get(inbox.Name)
	if !ok {
		t.Fatalf("no inbox was provisioned")
	}
	b, err := os.ReadFile(filepath.Join(ref.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read kept %s: %v", rel, err)
	}
	return b
}

// Under passthrough (the default) a file sent as base64 in a text block
// reaches the model exactly as the server wrote it, and nothing is kept.
func TestMCPCapture_Base64PassthroughByDefault(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	b64 := base64.StdEncoding.EncodeToString(pdfOf(6000))
	pre, suf := graphAttachment()
	block := pre + b64 + suf
	if got := runTextUnder(t, r.Ctx, passthroughPolicy, block); got != block {
		t.Errorf("passthrough changed the block (%d bytes → %d)", len(block), len(got))
	}
	// A pool with no policy installed renders the same way.
	if got := runContent(t, r.Ctx, map[string]any{"type": "text", "text": block}); got != block {
		t.Errorf("a pool without a policy changed the block")
	}
	if evs := r.Events(); len(evs) != 0 {
		t.Errorf("passthrough kept a file: %+v", evs)
	}
}

// The same block under capture: the base64 field becomes the stub, the file
// is in the inbox under the name and type its sibling fields declare, and
// every other byte of the block is as the server wrote it.
func TestMCPCapture_Base64DecodedWhenOptedIn(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	body := pdfOf(6000)
	b64 := base64.StdEncoding.EncodeToString(body)
	pre, suf := graphAttachment()
	got := runTextUnder(t, r.Ctx, capturePolicy, pre+b64+suf)

	if strings.Contains(got, b64[:64]) {
		t.Fatal("the base64 reached the model")
	}
	if !strings.HasPrefix(got, pre) || !strings.HasSuffix(got, suf) {
		t.Fatalf("the rest of the block changed: %q", short(got))
	}
	var stub string
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(got, pre[:len(pre)-1]), suf[1:])), &stub); err != nil {
		t.Fatalf("the field is not a JSON string: %v (%q)", err, short(got))
	}
	if !strings.HasPrefix(stub, "[file: volume:inbox:documents/Board-papers.pdf · application/pdf · 5.9 KiB") || !strings.HasSuffix(stub, "from mcp:ms365/get]") {
		t.Errorf("stub = %q", stub)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("the block is no longer JSON: %q", short(got))
	}
	if kept := keptFile(t, r, "documents/Board-papers.pdf"); !bytes.Equal(kept, body) {
		t.Errorf("kept %d bytes, want the decoded %d", len(kept), len(body))
	}
	if evs := r.Events(); len(evs) != 1 || evs[0].Type != providers.EventFileCaptured || evs[0].File.MediaType != "application/pdf" {
		t.Errorf("events = %+v", evs)
	}
}

// A string as long as a file's base64 that decodes to bytes with no file
// signature and no text is an opaque token: under capture it is left alone,
// because the caller may need to send it back.
func TestMCPCapture_OpaqueTokenNotCaptured(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	token := base64.RawURLEncoding.EncodeToString(opaqueBytes(4096))
	for _, block := range []string{
		`{"@odata.nextLink":"https://graph.example/next","skipToken":"` + token + `"}`,
		token,
	} {
		if got := runTextUnder(t, r.Ctx, capturePolicy, block); got != block {
			t.Errorf("an opaque token was replaced: %q", short(got))
		}
	}
	if evs := r.Events(); len(evs) != 0 {
		t.Errorf("an opaque token was kept: %+v", evs)
	}
}

// A PDF's base64 shorter than the minimum is left in the text: under it a
// candidate is too short to be worth a file, and too short to be safe to guess.
func TestMCPCapture_ShortBase64Untouched(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	b64 := base64.StdEncoding.EncodeToString(pdfOf(2000)) // 2668 chars < 4096
	pre, suf := graphAttachment()
	block := pre + b64 + suf
	if got := runTextUnder(t, r.Ctx, capturePolicy, block); got != block {
		t.Errorf("a base64 under the minimum was replaced: %q", short(got))
	}
	// The same string over a lowered minimum is kept: the knob is what decides.
	lowered := mcp.ResultPolicy{CaptureBase64: true, Base64MinBytes: 1024}
	if got := runTextUnder(t, r.Ctx, lowered, block); strings.Contains(got, b64) {
		t.Errorf("a base64 over a lowered minimum was not replaced")
	}
}

// Text that is not base64 — prose, a long JSON reply of prose — is not
// touched under capture, however long it is.
func TestMCPCapture_PlainTextBlockUntouched(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	prose := strings.Repeat("The board approved the budget for the next quarter. ", 200)
	reply, _ := json.Marshal(map[string]any{"subject": "Minutes", "body": map[string]string{"contentType": "text", "content": prose}})
	for _, block := range []string{prose, string(reply)} {
		if got := runTextUnder(t, r.Ctx, capturePolicy, block); got != block {
			t.Errorf("a text block was changed: %q", short(got))
		}
	}
	if evs := r.Events(); len(evs) != 0 {
		t.Errorf("plain text was kept as a file: %+v", evs)
	}
}

// A block that is nothing but base64 (not JSON) is a candidate whole: it is
// replaced by the stub, the whitespace around it kept, and named after the
// tool since nothing else names it.
func TestMCPCapture_WholeBlockBase64(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	png := append([]byte("\x89PNG\r\n\x1a\n"), opaqueBytes(5000)...)
	b64 := base64.StdEncoding.EncodeToString(png)
	got := runTextUnder(t, r.Ctx, capturePolicy, "\n"+b64+"\n")
	if !strings.HasPrefix(got, "\n[file: volume:inbox:images/get.png · image/png") || !strings.HasSuffix(got, "from mcp:ms365/get]\n") {
		t.Fatalf("model text = %q", short(got))
	}
	if kept := keptFile(t, r, "images/get.png"); !bytes.Equal(kept, png) {
		t.Errorf("kept %d bytes, want %d", len(kept), len(png))
	}
}

// Base64 of readable text costs the model the base64 all the same: it is kept
// as a .txt and stubbed.
func TestMCPCapture_Base64OfTextKeptAsTxt(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	text := []byte(strings.Repeat("Протокол засідання правління. Line of minutes.\n", 120))
	b64 := base64.StdEncoding.EncodeToString(text)
	got := runTextUnder(t, r.Ctx, capturePolicy, `{"name":"minutes","contentBytes":"`+b64+`"}`)
	if !strings.Contains(got, `"[file: volume:inbox:documents/minutes.txt · text/plain`) {
		t.Fatalf("model text = %q", short(got))
	}
	if kept := keptFile(t, r, "documents/minutes.txt"); !bytes.Equal(kept, text) {
		t.Errorf("kept %d bytes, want %d", len(kept), len(text))
	}
}

// MIME-wrapped base64 (line breaks, written as JSON escapes) in a nested
// object, in the URL alphabet without padding, is still found, and only its
// own quoted span is replaced: the escapes elsewhere in the block survive.
func TestMCPCapture_WrappedBase64InNestedJSONIsKept(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	// High bytes so the encoding uses the URL alphabet's '-' and '_'.
	body := append(pdfOf(6000), bytes.Repeat([]byte{0xfb, 0xff, 0xbf}, 100)...)
	raw := base64.RawURLEncoding.EncodeToString(body)
	if !strings.ContainsAny(raw, "-_") {
		t.Fatal("fixture does not exercise the URL alphabet")
	}
	var wrapped strings.Builder
	for i := 0; i < len(raw); i += 76 {
		wrapped.WriteString(raw[i:min(i+76, len(raw))])
		wrapped.WriteString(`\r\n`)
	}
	pre := `{"value":[{"note":"a \"quoted\" \/ path","item":{"fileName":"Звіт.pdf","mimeType":"application/pdf","data":"`
	suf := `"}}],"count":1}`
	got := runTextUnder(t, r.Ctx, capturePolicy, pre+wrapped.String()+suf)
	if !strings.HasPrefix(got, pre[:len(pre)-1]+`"[file: volume:inbox:documents/Zvit.pdf · application/pdf`) || !strings.HasSuffix(got, suf) {
		t.Fatalf("model text = %q", short(got))
	}
	if kept := keptFile(t, r, "documents/Zvit.pdf"); !bytes.Equal(kept, body) {
		t.Errorf("kept %d bytes, want %d", len(kept), len(body))
	}
}
