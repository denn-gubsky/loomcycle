package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A filter-free op=search withholds the chunks of documents the caller may not
// read BEFORE the backend cuts its answer at limit, so a confined reader gets a
// full page and the same bytes it would get if those documents did not exist.

// searchACLCorpus is eight chunks ranked 1 … 8 for the query "norm clause" on
// both legs (fewer repeats of the query, more noise words, as the rank grows).
// The odd ranks sit in /kb/h1 (1, 5) and /kb/h2 (3, 7), the even ranks in
// /kb/v1 (2, 6) and /kb/v2 (4, 8), so hidden and visible hits interleave.
type searchACLCorpus struct {
	d      *Document
	spy    *legSpy
	emb    *countingEmbedder
	open   context.Context
	rank   []string // chunk ids, rank 1 at index 0
	hidden []string // the /kb/h* document ids
	docs   map[string]string
}

func newSearchACLCorpus(t *testing.T, d *Document, base context.Context) *searchACLCorpus {
	t.Helper()
	spy := &legSpy{ftsStore: &ftsStore{newVectorStore(d.Store)}}
	d.Store = spy
	emb := &countingEmbedder{Embedder: newFakeEmbedder("fake", "m1", "norm", "clause", "x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8")}
	d.Embedder = emb
	c := &searchACLCorpus{d: d, spy: spy, emb: emb, open: tenantDocCtx(base, nil), docs: map[string]string{}}
	for _, p := range []string{"/kb/h1", "/kb/h2", "/kb/v1", "/kb/v2"} {
		out, r := docExec(t, d, c.open, `{"op":"create_document","scope":"tenant","title":"Doc","path":"`+p+`"}`)
		if r.IsError {
			t.Fatalf("create_document %s: %s", p, r.Text)
		}
		c.docs[p] = out["document_id"].(string)
	}
	c.hidden = []string{c.docs["/kb/h1"], c.docs["/kb/h2"]}
	home := map[int]string{1: "/kb/h1", 5: "/kb/h1", 3: "/kb/h2", 7: "/kb/h2", 2: "/kb/v1", 6: "/kb/v1", 4: "/kb/v2", 8: "/kb/v2"}
	for i := 1; i <= 8; i++ {
		body := strings.Repeat("norm clause ", 9-i)
		for n := 1; n <= i; n++ {
			body += fmt.Sprintf("x%d ", n)
		}
		out, r := docExec(t, d, c.open, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Rank %d","body":%q}`, c.docs[home[i]], i, body))
		if r.IsError {
			t.Fatalf("create_chunk rank %d: %s", i, r.Text)
		}
		c.rank = append(c.rank, out["id"].(string))
	}
	// Staging: an unconfined search ranks the chunks 1 … 8.
	if ids, _ := c.search(t, c.open, `{"op":"search","scope":"tenant","query":"norm clause","limit":8}`); strings.Join(ids, ",") != strings.Join(c.rank, ",") {
		t.Fatalf("staging: the open search ranks %v, want %v", ids, c.rank)
	}
	return c
}

// visible is the chunks of the /kb/v* documents, in rank order.
func (c *searchACLCorpus) visible() []string {
	return []string{c.rank[1], c.rank[3], c.rank[5], c.rank[7]}
}

// search runs body and returns the ranked chunk ids (expanded rows left out) and
// the raw answer.
func (c *searchACLCorpus) search(t *testing.T, ctx context.Context, body string) ([]string, string) {
	t.Helper()
	out, r := docExec(t, c.d, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	var ids []string
	for _, e := range out["chunks"].([]any) {
		m := e.(map[string]any)
		if _, expanded := m["expanded_via"]; !expanded {
			ids = append(ids, m["chunk_id"].(string))
		}
	}
	return ids, r.Text
}

// hiddenByDeny is a reader denied the /kb/h* documents by name; hiddenByAllow is
// one allowed only the /kb/v* documents, so everything else of the scope is
// hidden — the two shapes a confined reader comes in.
func hiddenByDeny(t *testing.T, base context.Context) context.Context {
	return tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/h1", "document:tenant:/kb/h2"}}))
}

func hiddenByAllow(t *testing.T, base context.Context) context.Context {
	return authz.WithMode(tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/v1", "document:tenant:/kb/v2"}})), authz.ModeEnforce)
}

// A confined reader whose best hits interleave with hidden ones gets limit
// visible hits in rank order, and the same bytes as if the hidden documents did
// not exist.
func TestDocumentSearch_HiddenHitsNeverShortenAFilterFreeAnswer(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := newSearchACLCorpus(t, d, base)
		readers := map[string]context.Context{"deny": hiddenByDeny(t, base), "allow": hiddenByAllow(t, base)}
		withHidden := map[string]string{}
		for name, reader := range readers {
			for limit := 1; limit <= 5; limit++ {
				body := fmt.Sprintf(`{"op":"search","scope":"tenant","query":"norm clause","limit":%d}`, limit)
				ids, text := c.search(t, reader, body)
				withHidden[name+body] = text
				want := c.visible()[:min(limit, 4)]
				if strings.Join(ids, ",") != strings.Join(want, ",") {
					t.Errorf("%s reader, limit %d: hits %v, want the first visible %v", name, limit, ids, want)
				}
			}
		}
		deleteDocs(t, d, c.open, c.hidden)
		for name, reader := range readers {
			for limit := 1; limit <= 5; limit++ {
				body := fmt.Sprintf(`{"op":"search","scope":"tenant","query":"norm clause","limit":%d}`, limit)
				if _, text := c.search(t, reader, body); text != withHidden[name+body] {
					t.Errorf("%s reader, limit %d: hidden documents change the answer:\nwith    %s\nwithout %s", name, limit, withHidden[name+body], text)
				}
			}
		}
	})
}

// expand starts from the visible hits the page holds: hidden hits neither take
// a frontier place nor make expanded / expand_truncated count anything, so the
// expansion is the one the reader would get if the hidden documents did not
// exist.
func TestDocumentSearch_ExpandCountsIgnoreHiddenHits(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := newSearchACLCorpus(t, d, base)
		mk := func(title string) string {
			out, r := docExec(t, d, c.open, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":%q,"body":"unrelated words"}`, c.docs["/kb/v1"], title))
			if r.IsError {
				t.Fatal(r.Text)
			}
			return out["id"].(string)
		}
		// Rank 4 (visible) cites one target, rank 1 (hidden) another.
		fromVisible, fromHidden := mk("Cited by a visible hit"), mk("Cited by a hidden hit")
		for _, l := range [][2]string{{c.rank[3], fromVisible}, {c.rank[0], fromHidden}} {
			if _, r := docExec(t, d, c.open, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites"}`, l[0], l[1])); r.IsError {
				t.Fatal(r.Text)
			}
		}
		reader := hiddenByDeny(t, base)
		bodies := []string{
			`{"op":"search","scope":"tenant","query":"norm clause","limit":2,"expand":{"kinds":["cites"]}}`,
			`{"op":"search","scope":"tenant","query":"norm clause","limit":2,"expand":{"kinds":["cites"],"budget_chars":1}}`,
		}
		withHidden := map[string]string{}
		for _, body := range bodies {
			_, text := c.search(t, reader, body)
			withHidden[body] = text
			if strings.Contains(text, fromHidden) {
				t.Errorf("%s: a hidden hit's relation reached the answer: %s", body, text)
			}
		}
		if text := withHidden[bodies[0]]; !strings.Contains(text, fromVisible) || !strings.Contains(text, `"expanded":1`) {
			t.Errorf("the second visible hit was not expanded from: %s", text)
		}
		if text := withHidden[bodies[1]]; !strings.Contains(text, `"expanded":0`) || !strings.Contains(text, `"expand_truncated":true`) {
			t.Errorf("budget_chars 1: want the visible neighbour to truncate the expansion: %s", text)
		}
		deleteDocs(t, d, c.open, c.hidden)
		for _, body := range bodies {
			if _, text := c.search(t, reader, body); text != withHidden[body] {
				t.Errorf("%s: hidden documents change the answer:\nwith    %s\nwithout %s", body, withHidden[body], text)
			}
		}
	})
}

// A caller with no set, or one whose set hides nothing in the scope, gets the
// answer it always got, from one search: the legs are sent no id list.
func TestDocumentSearch_UnrestrictedCallerAnswerUnchanged(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		c := newSearchACLCorpus(t, d, base)
		callers := map[string]context.Context{
			"allow-all": tenantDocCtx(base, aclSet(t,
				authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant"}})),
			// Names part of the scope, so its answer depends on paths, yet hides no
			// document there is.
			"deny-elsewhere": tenantDocCtx(base, aclSet(t,
				authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant"}},
				authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/none/**"}})),
		}
		for _, limit := range []int{3, 8} {
			body := fmt.Sprintf(`{"op":"search","scope":"tenant","query":"norm clause","limit":%d,"expand":{"kinds":["cites"]}}`, limit)
			c.spy.reset()
			before := c.emb.n.Load()
			_, want := c.search(t, c.open, body)
			if n := c.emb.n.Load() - before; n != 1 {
				t.Fatalf("staging, limit %d: %d searches with no set, want 1", limit, n)
			}
			legs := len(c.spy.filters)
			for name, ctx := range callers {
				c.spy.reset()
				before := c.emb.n.Load()
				if _, got := c.search(t, ctx, body); got != want {
					t.Errorf("%s, limit %d: the answer changed:\nno set %s\ngot    %s", name, limit, want, got)
				}
				if n := c.emb.n.Load() - before; n != 1 {
					t.Errorf("%s, limit %d: %d searches, want 1", name, limit, n)
				}
				if len(c.spy.filters) != legs {
					t.Errorf("%s, limit %d: %d leg queries, want the %d of a caller with no set", name, limit, len(c.spy.filters), legs)
				}
				for _, f := range c.spy.filters {
					if len(f.IDs) > 0 || len(f.ExcludeIDs) > 0 {
						t.Errorf("%s, limit %d: a leg was sent an id list: %+v", name, limit, f)
					}
				}
			}
		}
	})
}

// Over the operator's cap on the id list the search does not refuse: it answers
// as it did before the list existed, without the hidden hits. The page may then
// be short — the bound.
func TestDocumentSearch_HiddenSetOverTheCapStillAnswers(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := newSearchACLCorpus(t, d, base)
	d.Cfg = &config.Config{}
	// The hidden documents hold six chunks: their roots and four ranked ones.
	d.Cfg.Env.DocumentSearchFilterMaxIDs = 5
	ids, text := c.search(t, hiddenByDeny(t, base), `{"op":"search","scope":"tenant","query":"norm clause","limit":3}`)
	for _, id := range []string{c.rank[0], c.rank[2], c.rank[4], c.rank[6]} {
		if strings.Contains(text, id) {
			t.Errorf("a hidden chunk reached the answer: %s", text)
		}
	}
	if strings.Join(ids, ",") != c.rank[1] {
		t.Errorf("hits %v, want the visible hit among the top three, %s", ids, c.rank[1])
	}
	d.Cfg.Env.DocumentSearchFilterMaxIDs = 6
	if ids, _ := c.search(t, hiddenByDeny(t, base), `{"op":"search","scope":"tenant","query":"norm clause","limit":3}`); strings.Join(ids, ",") != strings.Join(c.visible()[:3], ",") {
		t.Errorf("at the cap: hits %v, want %v", ids, c.visible()[:3])
	}
}

// A reader that may read nothing is answered with nothing, without a search; one
// that may read only documents with no ranked chunk searches those alone — never
// with an empty id list, which would mean "every chunk".
func TestDocumentSearch_NothingReadableSearchesNothing(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := newSearchACLCorpus(t, d, base)
	if _, r := docExec(t, d, c.open, `{"op":"create_document","scope":"tenant","title":"Empty","path":"/kb/empty"}`); r.IsError {
		t.Fatal(r.Text)
	}
	const body = `{"op":"search","scope":"tenant","query":"norm clause"}`
	c.spy.reset()
	before := c.emb.n.Load()
	denied := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant"}}))
	if ids, text := c.search(t, denied, body); len(ids) != 0 || text != `{"chunks":[]}` {
		t.Errorf("the whole scope denied: %s", text)
	}
	if c.emb.n.Load() != before || len(c.spy.filters) != 0 {
		t.Errorf("the whole scope denied: searched anyway (%d embeds, %d leg queries)", c.emb.n.Load()-before, len(c.spy.filters))
	}

	c.spy.reset()
	onlyEmpty := authz.WithMode(tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/empty"}})), authz.ModeEnforce)
	if ids, text := c.search(t, onlyEmpty, body); len(ids) != 0 {
		t.Errorf("only an empty document: hits %v: %s", ids, text)
	}
	if len(c.spy.filters) == 0 {
		t.Fatal("only an empty document: no leg ran")
	}
	for _, f := range c.spy.filters {
		if len(f.IDs) == 0 {
			t.Errorf("only an empty document: a leg ran without the allowlist: %+v", f)
		}
	}
}

// op=related reads the whole scope's embeddings at once, so it has no page for
// hidden documents to shorten: a reader denied any document is refused.
func TestDocumentRelated_RefusesAReaderDeniedPartOfTheScope(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := newSearchACLCorpus(t, d, base)
	for _, body := range []string{
		`{"op":"related","scope":"tenant","id":"` + c.rank[1] + `"}`,
		`{"op":"related","scope":"tenant","related_to":"` + c.rank[1] + `"}`,
	} {
		_, r := docExec(t, d, hiddenByDeny(t, base), body)
		refusedByACL(t, body, r)
	}
}
