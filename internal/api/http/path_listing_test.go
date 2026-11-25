package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// RFC HA over POST /v1/_path and POST /v1/_document: the new ls arguments and
// the chunk list's briefs reach the tools and come back unchanged, under the
// route's own authorization (browse-by-subject is re-authorized as before —
// TestSubstrateBrowseCtxFn_MemberAndIsolatedRoutes carries the counted case).
func TestSubstratePath_LsCountsAndFiltersOverHTTP(t *testing.T) {
	f := newBrowseFixture(t)
	f.newDoc(t, f.writer, "user", "/secret/extra")
	decode := func(code int, body string) map[string]any {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("status %d: %s", code, body)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return out
	}
	entry := func(out map[string]any, name string) map[string]any {
		for _, e := range out["entries"].([]any) {
			if m := e.(map[string]any); m["name"] == name {
				return m
			}
		}
		return nil
	}

	op := auth.Principal{TenantID: "tnt", Subject: "op", Scopes: []string{auth.ScopeTenant}}
	rec := f.call(t, op, "POST", "/v1/_path?scope_id=alice", `{"op":"ls","scope":"user","path":"/","with_counts":true}`)
	out := decode(rec.Code, rec.Body.String())
	if e := entry(out, "secret"); e == nil || e["documents"] != float64(2) {
		t.Errorf("operator counting alice's /secret = %v, want 2", out["entries"])
	}
	rec = f.call(t, op, "POST", "/v1/_path?scope_id=alice", `{"op":"ls","scope":"user","path":"/secret","name_contains":"PLAN"}`)
	out = decode(rec.Code, rec.Body.String())
	if filters, _ := out["filters"].(map[string]any); filters["name_contains"] != "PLAN" {
		t.Errorf("filters echo = %v", out["filters"])
	}
	if e := entry(out, "plan"); e == nil || e["matched"] != true || entry(out, "extra") != nil {
		t.Errorf("filtered /secret = %v, want plan alone, matched", out["entries"])
	}

	// A member counts its own scope.
	bob := member("tnt", "bob", memberBaselineSet(t, "tnt", "bob"))
	f.newDoc(t, f.as("tnt", "bob"), "user", "/mine/a")
	rec = f.call(t, bob, "POST", "/v1/_path", `{"op":"ls","scope":"user","path":"/","with_counts":true}`)
	out = decode(rec.Code, rec.Body.String())
	if e := entry(out, "mine"); e == nil || e["documents"] != float64(1) {
		t.Errorf("member's own count = %v", out["entries"])
	}

	// The chunk list's with_summary rides /v1/_document (no annotation here, so
	// the rows carry none — the op takes the option rather than refusing it).
	rec = f.call(t, op, "POST", "/v1/_document?scope_id=alice",
		fmt.Sprintf(`{"op":"query_chunks","scope":"user","document_id":%q,"with_summary":true}`, f.secretDoc))
	out = decode(rec.Code, rec.Body.String())
	if chunks, _ := out["chunks"].([]any); len(chunks) == 0 {
		t.Errorf("query_chunks with_summary over HTTP = %v", out)
	}
}
