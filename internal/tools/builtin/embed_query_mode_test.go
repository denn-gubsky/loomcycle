package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
	"github.com/denn-gubsky/loomcycle/internal/providers/embedtest"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EV §1.2 — which tool paths embed a SEARCH QUERY (EmbedQuery) and which
// embed stored text (Embed). An embedder with an asymmetric query mode
// (Cohere's search_query) only helps retrieval if every query site uses it, and
// would hurt it if a stored text went through it.

func TestDocumentVerbatim_EmbedsTheQuestionInQueryMode(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	rec := embedtest.New(d.Embedder)
	d.Embedder = rec
	mkFact(t, d, ctx, doc, root, "fact:gh", "The user's github username is denn.", "my github username is denn", "supported")

	if _, r := docExec(t, d, ctx, `{"op":"verbatim_answer","scope":"user","query":"github username"}`); r.IsError {
		t.Fatalf("verbatim_answer: %s", r.Text)
	}
	if got := rec.Queries(); !slices.Equal(got, []string{"github username"}) {
		t.Errorf("query-mode texts = %q, want only the question", got)
	}
	if docs := rec.Documents(); len(docs) == 0 || slices.Contains(docs, "github username") {
		t.Errorf("stored-text embeddings = %q, want the fact body and not the question", docs)
	}
}

func TestDocumentGraphRecall_SeedsFromAQueryEmbeddedInQueryMode(t *testing.T) {
	d, ctx, doc, root := verbatimFixture(t)
	rec := embedtest.New(d.Embedder)
	d.Embedder = rec
	mkFact(t, d, ctx, doc, root, "fact:gh", "The user's github username is denn.", "my github username is denn", "supported")

	if _, r := docExec(t, d, ctx, `{"op":"graph_recall","scope":"user","query":"github username"}`); r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	if got := rec.Queries(); !slices.Contains(got, "github username") {
		t.Errorf("query-mode texts = %q, want the recall query among them", got)
	}
	if docs := rec.Documents(); slices.Contains(docs, "github username") {
		t.Errorf("the recall query was embedded as stored text: %q", docs)
	}
}

func TestHistoryRelated_AFreeTextQueryIsEmbeddedInQueryModeAndAChatIsNot(t *testing.T) {
	h, s := historyFixture(t)
	inner := eval.NewDeterministicEmbedder(64)
	rec := embedtest.New(inner)
	h.Embedder = rec
	ctx := histCtx([]string{"self"}, "agentA", "alice", "t1")
	src := seedChat(t, s, "t1", "agentA", "alice")
	seedEmbedding(t, s, inner, src, "postgres migration schema upgrade")

	res, _ := h.Execute(ctx, json.RawMessage(`{"op":"related","scope":"self","query":"postgres schema migration"}`))
	_ = decodeRelated(t, res)
	if got := rec.Queries(); !slices.Equal(got, []string{"postgres schema migration"}) {
		t.Errorf("query-mode texts = %q, want the free-text query", got)
	}
	if got := rec.Documents(); len(got) != 0 {
		t.Errorf("stored-text embeddings for a query-form related = %q, want none", got)
	}

	// The by-chat form compares one stored chat with others: both sides are
	// stored text, so it stays on Embed.
	rec2 := embedtest.New(inner)
	h.Embedder = rec2
	if err := s.SetSessionMeta(context.Background(), src, store.SessionMetaPatch{Title: strptr("kubernetes deployment")}); err != nil {
		t.Fatalf("set title: %v", err)
	}
	res, _ = h.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"op":"related","scope":"self","session_id":%q}`, src)))
	_ = decodeRelated(t, res)
	if got := rec2.Queries(); len(got) != 0 {
		t.Errorf("query-mode texts for a by-chat related = %q, want none", got)
	}
	if got := rec2.Documents(); len(got) != 1 {
		t.Errorf("stored-text embeddings for a by-chat related = %q, want the source chat's text", got)
	}
}

func TestHistoryContentSearch_EmbedsTheQueryInQueryMode(t *testing.T) {
	h, s := historyVectorFixture(t)
	emb := newFakeEmbedder("fake", "v1", "postgres", "upgrade", "lunch", "pizza")
	rec := embedtest.New(emb)
	h.Embedder = rec
	ctx := histCtx([]string{"user"}, "chat", "u1", "acme")
	chat := seedChat(t, s, "acme", "chat", "u1")
	seedTurn(t, s, "acme", "u1", chat, "postgres upgrade", emb)

	if res, _ := contentSearch(t, h, ctx, "postgres upgrade"); res.IsError {
		t.Fatalf("content search: %s", res.Text)
	}
	if got := rec.Queries(); !slices.Equal(got, []string{"postgres upgrade"}) {
		t.Errorf("query-mode texts = %q, want only the search query", got)
	}
	if got := rec.Documents(); len(got) != 0 {
		t.Errorf("stored-text embeddings = %q, want none", got)
	}
}
