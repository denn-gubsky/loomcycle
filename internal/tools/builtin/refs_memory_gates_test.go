package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A memory: reference (Crypto's and Calc's from) reads a row by key, so it
// must answer as Memory op=get does: a tenant document row needs the tenant
// document read grant, and a row of a document the caller's access-control
// set denies reads as missing. Both tools read through the one shared seam;
// each is tested, since each is a door.

// refTools gives a run's ctx the effective tools a memory reference needs.
func refTools(ctx context.Context, asking string) context.Context {
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) { rp.Tools = []string{asking, "Memory"} })
	if tools.RunID(ctx) == "" {
		ctx = tools.WithRunID(ctx, "run_refs")
	}
	return ctx
}

// refRead reads one memory reference through the tool and returns its result.
func refRead(t *testing.T, tool string, m *Memory, ctx context.Context, ref string) tools.Result {
	t.Helper()
	var res tools.Result
	var err error
	if tool == CryptoToolName {
		res, err = (&Crypto{Memory: m}).Execute(refTools(ctx, tool), json.RawMessage(`{"op":"hash","from":"`+ref+`"}`))
	} else {
		res, err = (&Calc{Memory: m}).Execute(refTools(ctx, tool), json.RawMessage(`{"expr":"t","from":{"t":"`+ref+`"}}`))
	}
	if err != nil {
		t.Fatalf("%s %s: hard error %v", tool, ref, err)
	}
	return res
}

// (a) A tenant document's chunk body needs sql_scopes: [tenant] as well as
// memory_scopes, as Memory op=get requires; an ordinary tenant key does not.
func TestRefs_MemoryReferenceToATenantDocumentRowNeedsTheTenantDocumentGrant(t *testing.T) {
	for _, tool := range []string{CryptoToolName, "Calc"} {
		m, _, ctx, ids := docRowsFixture(t, []string{"agent", "user"}) // no sql_scopes: [tenant]
		if r := memRun(t, m, ctx, `{"op":"set","scope":"tenant","key":"notes/n","value":"a\n1\n"}`); r.IsError {
			t.Fatalf("seed: %s", r.Text)
		}
		res := refRead(t, tool, m, ctx, "memory:tenant/"+chunkBodyKey(ids["tenant"]))
		if !res.IsError || res.Error == nil || res.Error.Category != tools.CategoryPermission || !strings.Contains(res.Text, "sql_scopes: [tenant]") {
			t.Errorf("%s: a tenant document row without the grant = %+v, want the tenant document refusal", tool, res)
		}
		if strings.Contains(res.Text, "quorum") {
			t.Errorf("%s: the refusal carries the row", tool)
		}
		if res := refRead(t, tool, m, ctx, "memory:tenant/notes/n"); res.IsError {
			t.Errorf("%s: an ordinary tenant key = %s", tool, res.Text)
		}
	}
}

// (b) A row of a document the caller's set denies reading reads as missing —
// the same answer as a key nobody wrote — while a readable one still reads.
func TestRefs_MemoryReferenceToAnACLDeniedDocumentRowReadsAsMissing(t *testing.T) {
	for _, tool := range []string{CryptoToolName, "Calc"} {
		m, _, limited, secret, unit, public := memRowsACLCorpus(t)
		missing := refRead(t, tool, m, limited, "memory:tenant/notes/never-written")
		if !missing.IsError || !strings.Contains(missing.Text, "is not set") {
			t.Fatalf("%s: a key nobody wrote = %+v", tool, missing)
		}
		for _, key := range []string{secret, unit} {
			res := refRead(t, tool, m, limited, "memory:tenant/"+key)
			if !res.IsError || res.Text != strings.Replace(missing.Text, "notes/never-written", key, 1) {
				t.Errorf("%s: denied row %s = %+v, want the missing-key answer", tool, key, res)
			}
			if strings.Contains(res.Text, "savepoint") || strings.Contains(res.Text, "secret") && !strings.Contains(res.Text, "memory:tenant/") {
				t.Errorf("%s: the answer for %s carries the row: %s", tool, key, res.Text)
			}
		}
		// The readable chunk is found (Calc may then refuse it as data that is
		// not a table, but never as missing or forbidden).
		if res := refRead(t, tool, m, limited, "memory:tenant/"+public); res.IsError && (strings.Contains(res.Text, "is not set") || res.Error.Category == tools.CategoryPermission) {
			t.Errorf("%s: the readable chunk = %+v", tool, res)
		}
	}
}
