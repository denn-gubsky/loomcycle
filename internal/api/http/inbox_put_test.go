package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// liveRun is a top-level run of user in tenant whose tree is live in this
// process, as a running run's is.
func (f *fileTwinFixture) liveRun(t *testing.T, tenant, user string) string {
	t.Helper()
	runID := f.run(t, tenant, user, "")
	_, release := f.srv.treeSets.acquire(runID)
	t.Cleanup(release)
	return runID
}

func (f *fileTwinFixture) put(t *testing.T, p auth.Principal, runID, name, mediaType string, data []byte, size int64) (connector.InboxPutResult, error) {
	t.Helper()
	return f.srv.PutInboxFile(sessionCtx(p), connector.InboxPutRequest{
		RunID: runID, Name: name, MediaType: mediaType, Body: bytes.NewReader(data), Size: size,
	})
}

// inboxFiles lists the regular files under the tree's inbox on disk, hidden
// temporaries included: what a refused put must not leave.
func (f *fileTwinFixture) inboxFiles(t *testing.T, root string) []string {
	t.Helper()
	dir := dynvol.DerivedEphemeralPath(f.dynRoot, root, inbox.Name)
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func putKind(err error) connector.InboxPutErrorKind {
	var perr *connector.InboxPutError
	if errors.As(err, &perr) {
		return perr.Kind
	}
	return 0
}

var docxHead = []byte("PK\x03\x04 a passport")

// The run's owner puts a file; the File tool reads it back as the person's
// upload: origin client, who put it and when, the bytes' own hash.
func TestInboxPut_OwnerPutsAFileTheFileToolReadsAsTheirs(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.liveRun(t, "acme", "alice")
	data := []byte("%PDF-1.7\nthe passport")

	res, err := f.put(t, twinOwner, runID, "passport.pdf", "application/pdf", data, int64(len(data)))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if res.Ref != "volume:inbox:documents/passport.pdf" || res.Name != "passport.pdf" || res.Bytes != int64(len(data)) || res.MediaType != "application/pdf" || len(res.SHA256) != 64 {
		t.Fatalf("result = %+v", res)
	}
	out, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "stat", "path": res.Ref})
	if isErr || out["origin"] != "client" || out["uploaded_by"] != "alice" || out["uploaded_at"] == nil || out["sha256"] != res.SHA256 {
		t.Fatalf("stat = %v %s", out, text)
	}
}

// Another user of the tenant, and an operator of another tenant, are told
// the run is not there, and nothing is written.
func TestInboxPut_AnotherUserOrTenantIsNotFoundAndKeepsNothing(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.liveRun(t, "acme", "alice")
	for _, p := range []auth.Principal{twinReader, twinOutsider} {
		_, err := f.put(t, p, runID, "x.pdf", "application/pdf", []byte("%PDF-1.7"), 8)
		if putKind(err) != connector.InboxPutNotFound {
			t.Errorf("%s/%s: err = %v, want not found", p.TenantID, p.Subject, err)
		}
	}
	if got := f.inboxFiles(t, runID); len(got) != 0 {
		t.Fatalf("a refused put left files: %v", got)
	}
	// The run's tenant operator is an admin of its runs, as for the twin's
	// writing ops.
	if _, err := f.put(t, twinOperator, runID, "x.pdf", "application/pdf", []byte("%PDF-1.7"), 8); err != nil {
		t.Fatalf("tenant operator: %v", err)
	}
}

// An ended run's inbox went with it: 409, nothing kept.
func TestInboxPut_EndedRunIsRefusedAsNotLive(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.liveRun(t, "acme", "alice")
	if err := f.st.FinishRun(context.Background(), runID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	_, err := f.put(t, twinOwner, runID, "x.pdf", "application/pdf", []byte("%PDF-1.7"), 8)
	if putKind(err) != connector.InboxPutNotLive {
		t.Fatalf("err = %v, want not live", err)
	}
	if got := f.inboxFiles(t, runID); len(got) != 0 {
		t.Fatalf("files: %v", got)
	}
}

// A run that is not ended but has no tree in this process is another
// server's: refused, never written to a fresh tree here.
func TestInboxPut_RunServedElsewhereIsRefused(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	_, err := f.put(t, twinOwner, runID, "x.pdf", "application/pdf", []byte("%PDF-1.7"), 8)
	if putKind(err) != connector.InboxPutElsewhere {
		t.Fatalf("err = %v, want elsewhere", err)
	}
}

// Over the client put cap is refused with its reason, whether the length is
// declared or only found while streaming, and nothing partial is kept.
func TestInboxPut_OverTheClientCapIsRefusedAndNothingKept(t *testing.T) {
	f := newFileTwinFixture(t)
	f.srv.cfg().Env.FileClientPutMaxBytes = 16
	runID := f.liveRun(t, "acme", "alice")
	big := bytes.Repeat([]byte("a"), 17)
	for _, size := range []int64{17, -1} {
		_, err := f.put(t, twinOwner, runID, "big.txt", "text/plain", big, size)
		var perr *connector.InboxPutError
		if !errors.As(err, &perr) || perr.Kind != connector.InboxPutTooBig || perr.Code != "client_put_max_bytes" {
			t.Fatalf("size %d: err = %v, want client_put_max_bytes", size, err)
		}
	}
	if got := f.inboxFiles(t, runID); len(got) != 0 {
		t.Fatalf("a refused put left files: %v", got)
	}
	if _, err := f.put(t, twinOwner, runID, "ok.txt", "text/plain", big[:16], -1); err != nil {
		t.Fatalf("a file at the cap: %v", err)
	}
}

// The tree's inbox quota holds for a put too.
func TestInboxPut_OverTheInboxQuotaIsRefused(t *testing.T) {
	f := newFileTwinFixture(t)
	f.srv.cfg().Env.FileInboxMaxBytes = 20
	runID := f.liveRun(t, "acme", "alice")
	if _, err := f.put(t, twinOwner, runID, "a.txt", "text/plain", bytes.Repeat([]byte("a"), 15), -1); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := f.put(t, twinOwner, runID, "b.txt", "text/plain", bytes.Repeat([]byte("b"), 15), -1)
	var perr *connector.InboxPutError
	if !errors.As(err, &perr) || perr.Code != "inbox_full" {
		t.Fatalf("err = %v, want inbox_full", err)
	}
	if got := f.inboxFiles(t, runID); len(got) != 1 {
		t.Fatalf("files: %v, want only the first", got)
	}
}

// A person's file keeps its own name, in its own script (owner ruling
// 2026-11-24): NFC, a second file of the same name gets -2, and the File
// tool reads it by that name.
func TestInboxPut_UnicodeNameIsKeptAndAClashSuffixed(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.liveRun(t, "acme", "alice")
	breve := string(rune(0x0306))
	for _, c := range []struct{ name, want string }{
		{"Паспорт.docx", "documents/Паспорт.docx"},
		{"Паспорт.docx", "documents/Паспорт-2.docx"},
		// и + combining breve is kept as the composed й.
		{"Мии" + breve + " рішення (v2).docx", "documents/Мий рішення (v2).docx"},
	} {
		res, err := f.put(t, twinOwner, runID, c.name, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", docxHead, -1)
		if err != nil {
			t.Fatalf("%q: %v", c.name, err)
		}
		if res.Ref != "volume:inbox:"+c.want || "documents/"+res.Name != c.want {
			t.Errorf("%q: ref %s name %s, want volume:inbox:%s", c.name, res.Ref, res.Name, c.want)
		}
	}
	out, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "stat", "path": "volume:inbox:documents/Паспорт.docx"})
	if isErr || out["origin"] != "client" || out["path"] != "documents/Паспорт.docx" {
		t.Fatalf("stat = %v %s", out, text)
	}
}

// A name that names a path or could hide what it says is refused, and
// nothing is kept: a bidi override makes "fdp.exe" read as "exe.pdf".
func TestInboxPut_PathOrBidiOverrideNameIsRefused(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.liveRun(t, "acme", "alice")
	for _, name := range []string{
		"../../etc/Паспорт.docx", `..\Паспорт.docx`, "..", ".hidden.pdf",
		"invoice" + string(rune(0x202E)) + "fdp.exe", "a" + string(rune(0x2067)) + "b.pdf", "a\x07b.pdf",
	} {
		_, err := f.put(t, twinOwner, runID, name, "application/pdf", []byte("%PDF-1.7"), -1)
		var perr *connector.InboxPutError
		if !errors.As(err, &perr) || perr.Kind != connector.InboxPutBadRequest || perr.Code != "bad_name" {
			t.Errorf("%q: err = %v, want bad_name", name, err)
		}
	}
	if got := f.inboxFiles(t, runID); len(got) != 0 {
		t.Fatalf("a refused name left files: %v", got)
	}
}

// A long multibyte name is cut to 120 bytes on a rune boundary, keeping its
// extension.
func TestInboxPut_LongMultibyteNameIsCutOnARuneBoundary(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.liveRun(t, "acme", "alice")
	res, err := f.put(t, twinOwner, runID, strings.Repeat("я", 200)+".pdf", "application/pdf", []byte("%PDF-1.7"), -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Name) > 120 || !utf8.ValidString(res.Name) || !strings.HasSuffix(res.Name, ".pdf") || !strings.HasPrefix(res.Name, "яя") {
		t.Errorf("long name kept as %q (%d bytes)", res.Name, len(res.Name))
	}
}

// An image goes under images/.
func TestInboxPut_ImageGoesUnderImages(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.liveRun(t, "acme", "alice")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	res, err := f.put(t, twinOwner, runID, "scan.png", "image/png", png, -1)
	if err != nil || res.Ref != "volume:inbox:images/scan.png" {
		t.Fatalf("ref %q, err %v", res.Ref, err)
	}
}

// The put is audited as run_inbox_put with who, which run, how many bytes,
// the type and the hash, and never the file's name.
func TestInboxPut_AuditRecordCarriesNoName(t *testing.T) {
	f := newFileTwinFixture(t)
	sink := &recordingSink{}
	f.srv.tokenAudit = sink
	runID := f.liveRun(t, "acme", "alice")
	res, err := f.put(t, twinOwner, runID, "Секретний-паспорт.pdf", "application/pdf", []byte("%PDF-1.7 x"), -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("events = %+v", sink.events)
	}
	ev := sink.events[0]
	if ev.Action != "run_inbox_put" || ev.ActorSubject != "alice" || ev.ActorTenant != "acme" || ev.TargetTenant != "acme" ||
		ev.InboxRunID != runID || ev.InboxBytes != res.Bytes || ev.InboxMediaType != "application/pdf" || ev.InboxSHA256 != res.SHA256 {
		t.Fatalf("event = %+v", ev)
	}
	raw, _ := json.Marshal(ev)
	for _, part := range []string{"Sekretnii", "pasport", "Секрет", "паспорт", res.Name, res.Ref} {
		if strings.Contains(string(raw), part) {
			t.Fatalf("the audit record names the file (%q): %s", part, raw)
		}
	}
}

// A put the audit cannot record is not kept.
func TestInboxPut_UnrecordedPutIsNotKept(t *testing.T) {
	f := newFileTwinFixture(t)
	f.srv.tokenAudit = failingSink{errors.New("audit down")}
	runID := f.liveRun(t, "acme", "alice")
	_, err := f.put(t, twinOwner, runID, "x.pdf", "application/pdf", []byte("%PDF-1.7"), -1)
	var perr *connector.InboxPutError
	if !errors.As(err, &perr) || perr.Code != "audit_unavailable" {
		t.Fatalf("err = %v, want audit_unavailable", err)
	}
	if got := f.inboxFiles(t, runID); len(got) != 0 {
		t.Fatalf("files: %v", got)
	}
}

// The run's transcript gets the file_captured event, origin client.
func TestInboxPut_EmitsFileCapturedOnTheRunsTranscript(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.liveRun(t, "acme", "alice")
	res, err := f.put(t, twinOwner, runID, "x.pdf", "application/pdf", []byte("%PDF-1.7"), -1)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := f.st.GetRunEventsSince(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Type != string(providers.EventFileCaptured) {
			continue
		}
		var ev providers.Event
		if err := json.Unmarshal(e.Payload, &ev); err != nil || ev.File == nil {
			t.Fatalf("payload %s: %v", e.Payload, err)
		}
		if ev.File.Ref != res.Ref || ev.File.Origin != "client" || ev.File.SHA256 != res.SHA256 {
			t.Fatalf("event file = %+v", ev.File)
		}
		return
	}
	t.Fatalf("no file_captured event in %d events", len(evs))
}

// Over HTTP: a token limited to certain agents is refused the route unless
// it was granted inbox_put; one granted it puts into its own run (201); the
// unlimited member token of the same subject puts too.
func TestInboxPut_RestrictedTokenNeedsTheInboxPutSurface(t *testing.T) {
	srv, url, st := aclFixture(t, authz.ModeCompat)
	dynRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg().Volumes = map[string]config.Volume{"pool": {Path: dynRoot, Mode: "rw", DynamicRoot: true}}
	const putBearer = "tok-bridge-put"
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_bridge_put", Name: "u-def_bridge_put", TenantID: "acme", Subject: "dev",
		TokenHash: auth.HashToken(testTokenPepper, putBearer), AllowedScopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		RestrictionsJSON: `{"agents":["allowed"],"surfaces":["inbox_put"]}`,
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(context.Background(), "acme", "allowed", "dev")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(context.Background(), sess.ID, store.RunIdentity{TenantID: "acme", UserID: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	_, release := srv.treeSets.acquire(run.ID)
	defer release()

	put := func(bearer, name string) (int, string) {
		req, err := http.NewRequest(http.MethodPut, url+"/v1/runs/"+run.ID+"/inbox/"+name, strings.NewReader("%PDF-1.7 passport"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/pdf")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := put(restrictedBearer, "a.pdf"); code != http.StatusForbidden || !strings.Contains(body, "restricted_token") {
		t.Fatalf("no grant: %d %s", code, body)
	}
	code, body := put(putBearer, "a.pdf")
	if code != http.StatusCreated {
		t.Fatalf("granted: %d %s", code, body)
	}
	var out connector.InboxPutResult
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Ref != "volume:inbox:documents/a.pdf" || out.Name != "a.pdf" || out.MediaType != "application/pdf" {
		t.Fatalf("granted: %s (%v)", body, err)
	}
	if code, body := put(freeBearer, "a.pdf"); code != http.StatusCreated || !strings.Contains(body, "documents/a-2.pdf") {
		t.Fatalf("member owner: %d %s", code, body)
	}
	// A Unicode name goes over the wire escaped and is served back by it.
	if code, body := put(freeBearer, neturl.PathEscape("Паспорт.pdf")); code != http.StatusCreated || !strings.Contains(body, `"name":"Паспорт.pdf"`) {
		t.Fatalf("unicode name: %d %s", code, body)
	}
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "GET", "/v1/runs/"+run.ID+"/inbox/documents/"+neturl.PathEscape("Паспорт.pdf"), ""); code != http.StatusOK || body != "%PDF-1.7 passport" {
		t.Fatalf("GET the unicode name: %d %q", code, body)
	}
}
