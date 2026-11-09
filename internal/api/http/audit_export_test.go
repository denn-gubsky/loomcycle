package http

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auditchain"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// exportLines decodes a JSONL export body: its rows, and the trailer that
// must close it.
func exportLines(t *testing.T, body string) []auditExportRow {
	rows, _ := exportWithTrailer(t, body)
	return rows
}

func exportWithTrailer(t *testing.T, body string) ([]auditExportRow, auditExportTrailer) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	var tr auditExportTrailer
	if len(lines) == 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &tr) != nil || !tr.Trailer {
		t.Fatalf("the export does not end with a trailer line: %q", body)
	}
	var out []auditExportRow
	for _, line := range lines[:len(lines)-1] {
		var r auditExportRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out, tr
}

func tenantsOf(rows []auditExportRow) map[string]int {
	m := map[string]int{}
	for _, r := range rows {
		m[r.Tenant]++
	}
	return m
}

// TestAuditExport_IsTenantScopedAndCarvedOutOfMembers: GET /v1/_audit streams
// one chain as JSONL. A tenant operator gets its own tenant's chain whatever
// ?tenant= says; an admin names the chain (none = the operator chain); a
// tenant member is refused; and the export carries what an offline check
// needs to re-link the chain.
func TestAuditExport_IsTenantScopedAndCarvedOutOfMembers(t *testing.T) {
	_, ts, st := overrideFixture(t)
	sink, err := auditchain.NewStoreSink(st, auditchain.ChainKeys{Current: []byte("export-test-chain-key-32-bytes!!")})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []audit.Event{
		{Action: "auth_login", ActorTenant: "acme", ActorSubject: "alice"},
		{Action: "acl_edit", TargetTenant: "acme", ActorSubject: "ops"},
		{Action: "auth_login", ActorTenant: "globex", ActorSubject: "gina"},
		{Action: "auth_login_failed", AuthGate: "pw"},
	} {
		if err := sink.Record(ev); err != nil {
			t.Fatal(err)
		}
	}

	// A tenant operator: its own chain, and a named other tenant changes nothing.
	for _, path := range []string{"/v1/_audit", "/v1/_audit?tenant=globex", "/v1/_audit?tenant=&format=jsonl"} {
		code, body := bearerCall(t, ts, opsBearer, "GET", path, "")
		if code != 200 {
			t.Fatalf("tenant operator GET %s: %d %s", path, code, body)
		}
		rows := exportLines(t, body)
		if got := tenantsOf(rows); len(got) != 1 || got["acme"] != 2 {
			t.Errorf("tenant operator GET %s: chains %v, want acme's 2 rows only", path, got)
		}
	}

	// Admin names the chain; with none it reads the operator chain.
	code, body := bearerCall(t, ts, adminBearer, "GET", "/v1/_audit?tenant=globex", "")
	if got := tenantsOf(exportLines(t, body)); code != 200 || len(got) != 1 || got["globex"] != 1 {
		t.Errorf("admin ?tenant=globex: %d chains %v, want globex's row", code, got)
	}
	code, body = bearerCall(t, ts, adminBearer, "GET", "/v1/_audit", "")
	rows := exportLines(t, body)
	if got := tenantsOf(rows); code != 200 || len(got) != 1 || got[""] != 3 {
		t.Errorf("admin, no tenant: %d chains %v, want the operator chain's 3 rows (two openings and the failed sign-in)", code, got)
	}

	// A tenant member (runs and channels, no substrate:tenant) is refused.
	if code, body := bearerCall(t, ts, freeBearer, "GET", "/v1/_audit", ""); code != 403 {
		t.Errorf("member GET /v1/_audit: %d %s, want 403", code, body)
	}

	// since filters by time; an unknown format is refused.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code, body := bearerCall(t, ts, opsBearer, "GET", "/v1/_audit?since="+future, ""); code != 200 || len(exportLines(t, body)) != 0 {
		t.Errorf("since=future: %d %q, want 200, no rows and the trailer", code, body)
	}
	if code, _ := bearerCall(t, ts, opsBearer, "GET", "/v1/_audit?since=yesterday", ""); code != 400 {
		t.Errorf("since=yesterday: %d, want 400", code)
	}
	if code, _ := bearerCall(t, ts, opsBearer, "GET", "/v1/_audit?format=csv", ""); code != 400 {
		t.Errorf("format=csv: %d, want 400", code)
	}

	// The export re-links offline: each row's hash is H(prev ‖ row), exactly,
	// from the trailer's anchor to the trailer's signed head.
	_, body = bearerCall(t, ts, opsBearer, "GET", "/v1/_audit", "")
	rows, tr := exportWithTrailer(t, body)
	if tr.Tenant != "acme" || tr.HeadSeq != 2 || tr.HeadMAC == "" || tr.AnchorSeq != 0 {
		t.Fatalf("trailer = %+v, want acme's signed head at 2 and no anchor", tr)
	}
	prev := tr.AnchorHash
	for _, r := range rows {
		row := store.AuditEventRow{TenantID: r.Tenant, Seq: r.Seq, TS: time.Unix(0, r.TSNanos).UTC(), Actor: r.Actor,
			Action: r.Action, Subject: r.Subject, Payload: r.Payload}
		if r.PrevHash != prev || auditchain.RowHash(prev, row) != r.Hash || r.MAC == "" {
			t.Fatalf("row %d does not re-link from the export: %+v", r.Seq, r)
		}
		prev = r.Hash
	}
	if prev != tr.HeadHash {
		t.Errorf("the last row does not reach the trailer's head")
	}
}

// An export as the route streams it verifies offline with the chain key —
// the crossing between the export's format and the verifier's reader.
func TestAuditExport_VerifiesOfflineWithTheChainKey(t *testing.T) {
	_, ts, st := overrideFixture(t)
	keys := auditchain.ChainKeys{Current: []byte("export-test-chain-key-32-bytes!!")}
	sink, err := auditchain.NewStoreSink(st, keys)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := sink.Record(audit.Event{Action: "acl_edit", TargetTenant: "acme", ActorSubject: "ops"}); err != nil {
			t.Fatal(err)
		}
	}
	code, body := bearerCall(t, ts, opsBearer, "GET", "/v1/_audit", "")
	if code != 200 {
		t.Fatalf("export: %d %s", code, body)
	}
	rep, err := auditchain.VerifyExport(strings.NewReader(body), &keys, false)
	if err != nil || !rep.OK || !rep.Signed || rep.Rows != 3 || rep.Tenant != "acme" {
		t.Fatalf("VerifyExport of the route's export: %+v, %v", rep, err)
	}
}
