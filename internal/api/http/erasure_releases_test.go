package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// TestErasure_RedactsTheBoundSubjectsOntologyReleaseCopies starts from a tenant as it
// stands before an erasure: an ontology declaring two people, confirmed (which adopts
// them as subjects and takes a catch_up and a confirm release), and Ada's subject bound
// to the principal u-ada. The report and the dry run count Ada's two release copies and
// change nothing; the erasure redacts them, keeps Grace's, and says in Retained what it
// left (Ada's entry still declared in the ontology in force).
func TestErasure_RedactsTheBoundSubjectsOntologyReleaseCopies(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.SetChannelBus(channels.NewBus())
	s.SetEraseAudit(&recordingSink{})
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes\n", kids: []ontologyTestChunk{{title: "person"}}},
		{title: "Instances", body: "@section instances\n", kids: []ontologyTestChunk{
			{title: "Ada", body: "@a person\n\nAda Lovelace, reached at ada@example.org.\n"},
			{title: "Grace", body: "@a person\n\nGrace Hopper, reached at grace@example.org.\n"},
		}},
	})
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)

	dctx := s.ontologyDocCtx(context.Background(), mi)
	key := sqlmem.ScopeKey{Tenant: mi.Tenant, Scope: "tenant", ScopeID: mi.Tenant}
	query := func(q string, args ...any) [][]any {
		t.Helper()
		res, err := s.sqlMem.DocPlane().Query(dctx, key, q, args)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return res.Rows
	}
	count := func(name string) int {
		t.Helper()
		rows := query(`SELECT count(*) FROM ontology_release_entries e JOIN ontology_release_bodies b ON b.sha = e.body_sha
			WHERE e.title = ? AND b.body LIKE '%reached at%'`, name)
		n, _ := rows[0][0].(int64)
		return int(n)
	}
	if count("Ada") != 2 || count("Grace") != 2 {
		t.Fatalf("before: Ada in %d releases, Grace in %d; want 2 each", count("Ada"), count("Grace"))
	}
	var adaKey string
	for _, r := range query(`SELECT b.fields FROM ontology_release_entries e JOIN ontology_release_bodies b ON b.sha = e.body_sha
		WHERE e.title = 'Ada'`) {
		var f struct {
			RegistryKey string `json:"registry_key"`
		}
		if fs, _ := r[0].(string); json.Unmarshal([]byte(fs), &f) == nil && f.RegistryKey != "" {
			adaKey = f.RegistryKey
		}
	}
	if adaKey == "" {
		t.Fatal("Ada's release copies record no registry key; the confirm did not adopt her")
	}
	if _, err := s.ontologyDocument().BindTenantSubjectPrincipal(s.ontologyDocCtx(context.Background(), memInject{Tenant: mi.Tenant}), adaKey, "u-ada"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	ctx := erasurePrincipal(mi.Tenant, nil)

	rec := httptest.NewRecorder()
	s.handleErasureReport(rec, httptest.NewRequest(http.MethodGet, "/v1/_erasure?tenant="+mi.Tenant+"&subject=u-ada", nil).WithContext(ctx))
	var rep erasure.Report
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &rep) != nil {
		t.Fatalf("report = %d: %s", rec.Code, rec.Body.String())
	}
	if rep.Tier2.Counts["ontology_release_entries"] != 2 {
		t.Errorf("report tier2 = %v, want ontology_release_entries 2", rep.Tier2.Counts)
	}

	dry := eraseAs(t, s, ctx, mi.Tenant, map[string]any{"subject": "u-ada"})
	if dry.Deleted["ontology_release_entries"] != 2 || !strings.Contains(dry.Retained["ontology_releases"], "would be redacted") {
		t.Errorf("dry run deleted %v retained %q", dry.Deleted, dry.Retained["ontology_releases"])
	}
	if count("Ada") != 2 {
		t.Fatal("the dry run redacted a release copy")
	}

	res := eraseAs(t, s, ctx, mi.Tenant, map[string]any{"subject": "u-ada", "dry_run": false, "confirm": "u-ada"})
	if len(res.Errors) != 0 {
		t.Fatalf("erasure errors: %v", res.Errors)
	}
	if res.Deleted["ontology_release_entries"] != 2 || res.Deleted["subject_bindings"] != 1 {
		t.Errorf("deleted = %v, want 2 release entries and 1 binding", res.Deleted)
	}
	if note := res.Retained["ontology_releases"]; !strings.Contains(note, "were redacted") || !strings.Contains(note, "1 instance entr(ies) still declared") {
		t.Errorf("retained ontology_releases = %q", note)
	}
	if count("Ada") != 0 || count("Grace") != 2 {
		t.Errorf("after: Ada in %d releases, Grace in %d; want 0 and 2", count("Ada"), count("Grace"))
	}
	if rows := query(`SELECT count(*) FROM ontology_releases WHERE export_md LIKE '%ada@example.org%'`); rows[0][0] != int64(0) {
		t.Errorf("%v release export(s) still hold Ada's text", rows[0][0])
	}
}
