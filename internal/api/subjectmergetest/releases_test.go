package subjectmergetest

// Reading the ontology's releases (RFC GQ P3.5): GET /v1/_ontology/releases and
// /v1/_ontology/releases/diff, the ListOntologyReleases and DiffOntologyReleases
// RPCs, and the MCP ontology tool's ops releases and release_diff, over one real
// server.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// seedReleases is the plan fixture's ontology with two releases: 1 taken as
// confirmed, 2 after a class Clause was added under Article. Release 1 against 2 (or
// against the ontology now) differs by exactly that class.
func (e *env) seedReleases() {
	e.t.Helper()
	f := e.seedPlanOntology()
	op := tools.WithCallerTier(e.docCtx(), tools.TierOperatorPlane)
	if _, err := e.doc.TakeOntologyRelease(op, builtin.OntologyReleaseConfirm, nil); err != nil {
		e.t.Fatalf("take release 1: %v", err)
	}
	art := e.opDocCall(fmt.Sprintf(`{"op":"get_chunk","scope":"tenant","id":%q}`, f.article))
	e.opDocCall(fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"parent_id":%q,"title":"Clause","body":"A part of an article."}`,
		art["document_id"], f.article))
	if _, err := e.doc.TakeOntologyRelease(op, builtin.OntologyReleaseApply, nil); err != nil {
		e.t.Fatalf("take release 2: %v", err)
	}
}

// grpcOutcome reads a gRPC refusal in the terms the other transports share.
func grpcOutcome(err error) outcome {
	st := status.Convert(err)
	o := outcome{raw: st.Message()}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			o.code = info.GetReason()
		}
	}
	if o.code == "" && st.Code() == codes.PermissionDenied && strings.Contains(st.Message(), "insufficient scope") {
		o.code = "insufficient_scope"
	}
	return o
}

// httpGetOutcome is one GET's answer: the body on 200, else its code (a caller
// the route's scope gate turned away is insufficient_scope).
func (e *env) httpGetOutcome(path, bearer string) outcome {
	resp, raw := e.do(http.MethodGet, path, bearer, "", nil)
	if resp.StatusCode == http.StatusOK {
		return outcome{ok: true, raw: string(raw)}
	}
	if resp.StatusCode == http.StatusForbidden && strings.Contains(string(raw), "insufficient") {
		return outcome{code: "insufficient_scope", raw: string(raw)}
	}
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &b)
	return outcome{code: b.Code, raw: string(raw)}
}

func (e *env) releasesHTTP(bearer string) (connector.ListOntologyReleasesResult, outcome) {
	var res connector.ListOntologyReleasesResult
	o := e.httpGetOutcome("/v1/_ontology/releases", bearer)
	if o.ok {
		_ = json.Unmarshal([]byte(o.raw), &res)
	}
	return res, o
}

// releasesGRPC reads the response back into the shared result type, field by
// field, so a field the RPC dropped shows as a difference from HTTP.
func (e *env) releasesGRPC(bearer string) (connector.ListOntologyReleasesResult, outcome) {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	resp, err := e.grpc.ListOntologyReleases(ctx, &loomcyclepb.ListOntologyReleasesRequest{})
	if err != nil {
		return connector.ListOntologyReleasesResult{}, grpcOutcome(err)
	}
	out := connector.ListOntologyReleasesResult{Tenant: resp.GetTenant(), Releases: []connector.OntologyRelease{}}
	for _, r := range resp.GetReleases() {
		out.Releases = append(out.Releases, connector.OntologyRelease{
			Seq: r.GetSeq(), ContentVersion: r.GetContentVersion(), Fingerprint: r.GetFingerprint(), Reason: r.GetReason(),
			Actor: r.GetActor(), CreatedAt: r.GetCreatedAt(), Entries: int(r.GetEntries()),
		})
	}
	return out, outcome{ok: true, raw: resp.String()}
}

func (e *env) releasesMCP(bearer string) (connector.ListOntologyReleasesResult, outcome) {
	var res connector.ListOntologyReleasesResult
	o := e.toolMCP(bearer, "ontology", map[string]any{"op": "releases"})
	if o.ok {
		_ = json.Unmarshal([]byte(o.raw), &res)
	}
	return res, o
}

// diffQuery is HTTP's query for a diff: to 0 is to=current.
func diffQuery(from, to int64) string {
	q := fmt.Sprintf("/v1/_ontology/releases/diff?from=%d", from)
	if to != 0 {
		q += fmt.Sprintf("&to=%d", to)
	}
	return q
}

func (e *env) diffHTTP(bearer string, from, to int64) (connector.DiffOntologyReleasesResult, outcome) {
	var res connector.DiffOntologyReleasesResult
	o := e.httpGetOutcome(diffQuery(from, to), bearer)
	if o.ok {
		_ = json.Unmarshal([]byte(o.raw), &res)
	}
	return res, o
}

func (e *env) diffGRPC(bearer string, from, to int64) (connector.DiffOntologyReleasesResult, outcome) {
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
	resp, err := e.grpc.DiffOntologyReleases(ctx, &loomcyclepb.DiffOntologyReleasesRequest{From: from, To: to})
	if err != nil {
		return connector.DiffOntologyReleasesResult{}, grpcOutcome(err)
	}
	side := func(s *loomcyclepb.OntologyDiffSide) connector.OntologyDiffSide {
		return connector.OntologyDiffSide{Seq: s.GetSeq(), Current: s.GetCurrent(), ContentVersion: s.GetContentVersion()}
	}
	state := func(s *loomcyclepb.OntologyEntryState) *connector.OntologyEntryState {
		if s == nil {
			return nil
		}
		return &connector.OntologyEntryState{Name: s.GetName(), Parent: s.GetParent(), ParentName: s.GetParentName(),
			Status: s.GetStatus(), Meaning: s.GetMeaning(), Directives: s.GetDirectivesJson()}
	}
	out := connector.DiffOntologyReleasesResult{Tenant: resp.GetTenant(), From: side(resp.GetFrom()), To: side(resp.GetTo()),
		Changes: []connector.OntologyEntryDiff{}, Issues: resp.GetIssues()}
	for _, c := range resp.GetChanges() {
		out.Changes = append(out.Changes, connector.OntologyEntryDiff{ChunkID: c.GetChunkId(), Kind: c.GetKind(), Change: c.GetChange(),
			Changed: c.GetChanged(), Before: state(c.GetBefore()), After: state(c.GetAfter())})
	}
	return out, outcome{ok: true, raw: resp.String()}
}

func (e *env) diffMCP(bearer string, from, to int64) (connector.DiffOntologyReleasesResult, outcome) {
	args := map[string]any{"op": "release_diff", "from": from}
	if to != 0 {
		args["to"] = to
	}
	var res connector.DiffOntologyReleasesResult
	o := e.toolMCP(bearer, "ontology", args)
	if o.ok {
		_ = json.Unmarshal([]byte(o.raw), &res)
	}
	return res, o
}

// normalizedDiff makes an empty list and an absent one the same (proto3 cannot tell
// them apart) and re-encodes each side's directives compactly, so JSON spacing is not
// a difference.
func normalizedDiff(t *testing.T, d connector.DiffOntologyReleasesResult) connector.DiffOntologyReleasesResult {
	t.Helper()
	if len(d.Issues) == 0 {
		d.Issues = nil
	}
	for i := range d.Changes {
		c := &d.Changes[i]
		if len(c.Changed) == 0 {
			c.Changed = nil
		}
		for _, s := range []*connector.OntologyEntryState{c.Before, c.After} {
			if s == nil {
				continue
			}
			var v any
			if err := json.Unmarshal(s.Directives, &v); err != nil {
				t.Fatalf("directives of %s are not JSON: %q", c.ChunkID, s.Directives)
			}
			s.Directives, _ = json.Marshal(v)
		}
	}
	return d
}

type releaseSurface struct {
	name string
	list func(e *env, bearer string) (connector.ListOntologyReleasesResult, outcome)
	diff func(e *env, bearer string, from, to int64) (connector.DiffOntologyReleasesResult, outcome)
}

var releaseSurfaces = []releaseSurface{
	{"http", (*env).releasesHTTP, (*env).diffHTTP},
	{"grpc", (*env).releasesGRPC, (*env).diffGRPC},
	{"mcp", (*env).releasesMCP, (*env).diffMCP},
}

// TestGRPCOntologyReleases_MatchesHTTP (RFC GQ P3.5): the tenant operator's list
// of releases and its diffs — release against release, and release against the
// ontology now — are the same on GET /v1/_ontology/releases*, the
// ListOntologyReleases and DiffOntologyReleases RPCs and the MCP ontology tool,
// every field; and the diff is the fixture's one added class.
func TestGRPCOntologyReleases_MatchesHTTP(t *testing.T) {
	e := newEnv(t)
	e.seedReleases()

	var firstList connector.ListOntologyReleasesResult
	for i, s := range releaseSurfaces {
		l, o := s.list(e, operator)
		if !o.ok {
			t.Fatalf("%s: releases refused: %s %s", s.name, o.code, o.raw)
		}
		if i == 0 {
			firstList = l
			if l.Tenant != tenant || len(l.Releases) != 2 || l.Releases[0].Seq != 2 || l.Releases[0].Reason != builtin.OntologyReleaseApply ||
				l.Releases[1].Reason != builtin.OntologyReleaseConfirm || l.Releases[0].ContentVersion == l.Releases[1].ContentVersion ||
				l.Releases[0].Entries == 0 || l.Releases[0].CreatedAt == 0 {
				t.Fatalf("http releases are not the fixture's: %+v", l)
			}
			continue
		}
		if !reflect.DeepEqual(l, firstList) {
			t.Errorf("%s releases = %+v\nhttp = %+v", s.name, l, firstList)
		}
	}

	for _, sides := range [][2]int64{{1, 2}, {1, 0}, {2, 0}} {
		var first connector.DiffOntologyReleasesResult
		for i, s := range releaseSurfaces {
			d, o := s.diff(e, operator, sides[0], sides[1])
			if !o.ok {
				t.Fatalf("%s: diff %v refused: %s %s", s.name, sides, o.code, o.raw)
			}
			d = normalizedDiff(t, d)
			if i == 0 {
				first = d
				continue
			}
			if !reflect.DeepEqual(d, first) {
				a, _ := json.Marshal(d)
				b, _ := json.Marshal(first)
				t.Errorf("%s diff %v answered\n%s\nhttp answered\n%s", s.name, sides, a, b)
			}
		}
		wantChanges := 1
		if sides[0] == 2 {
			wantChanges = 0 // release 2 is the ontology as it stands
		}
		if len(first.Changes) != wantChanges || first.From.Seq != sides[0] || first.To.Current != (sides[1] == 0) {
			t.Errorf("http diff %v = %+v, want %d change(s)", sides, first, wantChanges)
		}
		if wantChanges == 1 {
			c := first.Changes[0]
			if c.Change != connector.OntologyDiffAdded || c.Kind != "class" || c.After == nil || c.After.Name != "Clause" ||
				c.After.ParentName != "Article" || c.Before != nil {
				t.Errorf("http diff %v change = %+v, want the added class Clause under Article", sides, c)
			}
		}
	}
}

// TestOntologyReleases_SameRefusalOnEveryTransport: a member and an isolated user
// of the tenant are turned away at each transport's scope gate (a release keeps text
// an edit or an erasure removed), and the operator's malformed or unknown sides carry
// the same code on all three.
func TestOntologyReleases_SameRefusalOnEveryTransport(t *testing.T) {
	e := newEnv(t)
	e.seedReleases()
	for _, s := range releaseSurfaces {
		for _, who := range []struct{ name, bearer string }{{"a member", member}, {"an isolated user", isolated}} {
			if _, o := s.list(e, who.bearer); o.ok || o.code != "insufficient_scope" {
				t.Errorf("%s: %s's releases: ok=%v code %q (%.200s), want refused at the scope gate", s.name, who.name, o.ok, o.code, o.raw)
			}
			if _, o := s.diff(e, who.bearer, 1, 0); o.ok || o.code != "insufficient_scope" {
				t.Errorf("%s: %s's diff: ok=%v code %q (%.200s), want refused at the scope gate", s.name, who.name, o.ok, o.code, o.raw)
			}
		}
		for _, c := range []struct {
			name     string
			from, to int64
			code     string
		}{
			{"a from of 0", 0, 0, "invalid_request"},
			{"a negative to", 1, -1, "invalid_request"},
			{"no such from", 9, 0, "release_not_found"},
			{"no such to", 1, 9, "release_not_found"},
		} {
			if _, o := s.diff(e, operator, c.from, c.to); o.ok || o.code != c.code {
				t.Errorf("%s %s: ok=%v code %q, want %s (%.200s)", s.name, c.name, o.ok, o.code, c.code, o.raw)
			}
		}
	}
}
