package auditchain_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auditchain"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// exportOf renders a chain as GET /v1/_audit does (the http package's own
// test checks a real export against VerifyExport).
func exportOf(t *testing.T, s *sqlite.Store, tenant string) []map[string]any {
	t.Helper()
	ctx := context.Background()
	head, err := s.AuditHeadGet(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.AuditEventsList(ctx, tenant, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, r := range rows {
		out = append(out, map[string]any{"tenant": r.TenantID, "seq": r.Seq, "ts": r.TS, "ts_ns": r.TS.UnixNano(),
			"actor": r.Actor, "action": r.Action, "subject": r.Subject, "payload": r.Payload,
			"prev_hash": r.PrevHash, "hash": r.Hash, "mac": r.MAC})
	}
	return append(out, trailerOf(head))
}

func trailerOf(h store.AuditHead) map[string]any {
	return map[string]any{"trailer": true, "tenant": h.TenantID, "head_seq": h.Seq, "head_hash": h.Hash, "head_mac": h.MAC,
		"anchor_seq": h.AnchorSeq, "anchor_hash": h.AnchorHash, "anchor_mac": h.AnchorMAC, "anchor_state": h.AnchorState}
}

func jsonl(t *testing.T, lines []map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		j, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestVerifyExport_ChecksLinksSignaturesAndTheTail(t *testing.T) {
	s, _ := chainFixture(t, 5)
	keys := testKeys
	other := auditchain.ChainKeys{Current: []byte("not-the-deployment-key-32-bytes!")}

	check := func(t *testing.T, lines []map[string]any, k *auditchain.ChainKeys) auditchain.ExportReport {
		t.Helper()
		rep, err := auditchain.VerifyExport(strings.NewReader(jsonl(t, lines)), k, false)
		if err != nil {
			t.Fatalf("VerifyExport: %v", err)
		}
		return rep
	}
	if rep := check(t, exportOf(t, s, "t1"), &keys); !rep.OK || !rep.Signed || rep.Rows != 5 || rep.Partial {
		t.Fatalf("an untouched export with the key: %+v", rep)
	}
	if rep := check(t, exportOf(t, s, "t1"), nil); !rep.OK || rep.Signed {
		t.Fatalf("an untouched export without the key: %+v", rep)
	}
	if rep := check(t, exportOf(t, s, "t1"), &other); rep.OK || !strings.Contains(rep.Reason, "signature") {
		t.Errorf("an export checked with another key: %+v", rep)
	}

	for name, tc := range map[string]struct {
		edit    func([]map[string]any) []map[string]any
		withKey bool
		want    string
	}{
		"payload edited": {func(l []map[string]any) []map[string]any {
			l[2]["payload"] = `{"action":"nothing"}`
			return l
		}, false, "content"},
		"a row removed": {func(l []map[string]any) []map[string]any {
			return append(l[:2:2], l[3:]...)
		}, false, "missing"},
		"the tail cut off, the trailer kept": {func(l []map[string]any) []map[string]any {
			return append(l[:3:3], l[len(l)-1])
		}, false, "cut off"},
		"no trailer": {func(l []map[string]any) []map[string]any {
			return l[:len(l)-1]
		}, false, "trailer"},
		"an actor edited, checked with the key": {func(l []map[string]any) []map[string]any {
			l[4]["actor"] = "mallory"
			return l
		}, true, "row 5"},
	} {
		t.Run(name, func(t *testing.T) {
			k := (*auditchain.ChainKeys)(nil)
			if tc.withKey {
				k = &keys
			}
			if rep := check(t, tc.edit(exportOf(t, s, "t1")), k); rep.OK || !strings.Contains(rep.Reason, tc.want) {
				t.Errorf("%+v, want a break mentioning %q", rep, tc.want)
			}
		})
	}

	// Asked from a later row: the first row's link is outside the file, so
	// it is not OK unless the caller allows a partial file.
	lines := exportOf(t, s, "t1")
	if rep := check(t, lines[2:], &keys); rep.OK || !rep.Partial || !strings.Contains(rep.Reason, "starts at row 3") {
		t.Errorf("an export from row 3: %+v; want it refused as partial", rep)
	}
	rep, err := auditchain.VerifyExport(strings.NewReader(jsonl(t, lines[2:])), &keys, true)
	if err != nil || !rep.OK || !rep.Partial || !rep.Signed || rep.FirstSeq != 3 {
		t.Errorf("an export from row 3, partial allowed: %+v, %v", rep, err)
	}
}

// With the key, a file that carries no signed head verifies nothing: an
// empty chain's export, or a forged file emptied to a bare trailer, is not
// OK and never reported as signed.
func TestVerifyExport_NothingSignedIsNotOKWithTheKey(t *testing.T) {
	empty := []map[string]any{trailerOf(store.AuditHead{TenantID: "t1"})}
	rep, err := auditchain.VerifyExport(strings.NewReader(jsonl(t, empty)), &testKeys, true)
	if err != nil || rep.OK || rep.Signed {
		t.Errorf("an empty export with the key: %+v, %v; want not OK, not signed", rep, err)
	}
	rep, err = auditchain.VerifyExport(strings.NewReader(jsonl(t, empty)), nil, false)
	if err != nil || !rep.OK || rep.Signed {
		t.Errorf("an empty export without the key: %+v, %v; want OK (consistent), not signed", rep, err)
	}
	// A signed-looking trailer whose head MAC is not the key's.
	s, _ := chainFixture(t, 2)
	lines := exportOf(t, s, "t1")
	lines[len(lines)-1]["head_mac"] = "00"
	rep, err = auditchain.VerifyExport(strings.NewReader(jsonl(t, lines)), &testKeys, false)
	if err != nil || rep.OK || rep.Signed || !strings.Contains(rep.Reason, "head's signature") {
		t.Errorf("an export with an unsigned head: %+v, %v", rep, err)
	}
	// Rows cut to none, the real signed head kept: the rows are missing.
	rep, err = auditchain.VerifyExport(strings.NewReader(jsonl(t, exportOf(t, s, "t1")[2:])), &testKeys, true)
	if err != nil || rep.OK || rep.Signed {
		t.Errorf("an export emptied of rows: %+v, %v", rep, err)
	}
}
