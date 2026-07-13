package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// historyFixture opens an in-memory sqlite store and returns a wired History
// tool alongside it.
func historyFixture(t *testing.T) (*History, store.Store) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &History{Store: s}, s
}

// histCtx builds a run ctx with the given identity + granted history scopes.
func histCtx(scopes []string, agent, user, tenant string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
		AgentID:  "a_caller",
		UserID:   user,
		TenantID: tenant,
	})
	ctx = tools.WithAgentName(ctx, agent)
	return tools.WithHistoryPolicy(ctx, tools.HistoryPolicyValue{Scopes: scopes})
}

// seedChat creates a session (a "chat") owned by (tenant, agent, user).
func seedChat(t *testing.T, s store.Store, tenant, agent, user string) string {
	t.Helper()
	sess, err := s.CreateSession(context.Background(), tenant, agent, user)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sess.ID
}

func chatIDs(t *testing.T, res tools.Result) []string {
	t.Helper()
	if res.IsError {
		t.Fatalf("list: %s", res.Text)
	}
	var out struct {
		Chats []struct {
			SessionID string `json:"session_id"`
		} `json:"chats"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode list: %v (%s)", err, res.Text)
	}
	ids := make([]string, 0, len(out.Chats))
	for _, c := range out.Chats {
		ids = append(ids, c.SessionID)
	}
	return ids
}

func TestHistory_RefusesWithoutStore(t *testing.T) {
	h := &History{}
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(`{"op":"list"}`))
	if !res.IsError {
		t.Fatal("History without a Store should refuse")
	}
}

func TestHistory_DefaultDenyWithoutPolicy(t *testing.T) {
	h, _ := historyFixture(t)
	// No WithHistoryPolicy → empty scopes → default-deny.
	ctx := tools.WithAgentName(tools.WithRunIdentity(context.Background(),
		tools.RunIdentityValue{TenantID: "t1"}), "agentA")
	res, _ := h.Execute(ctx, json.RawMessage(`{"op":"list","scope":"self"}`))
	if !res.IsError {
		t.Fatal("History with no history_scope policy should refuse (default-deny)")
	}
}

func TestHistory_UnknownScopeRejected(t *testing.T) {
	h, _ := historyFixture(t)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"planet"}`))
	if !res.IsError {
		t.Fatal("unknown scope should be rejected")
	}
}

func TestHistory_UnknownOpRejected(t *testing.T) {
	h, _ := historyFixture(t)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"frobnicate","scope":"self"}`))
	if !res.IsError {
		t.Fatal("unknown op should be rejected")
	}
}

// TestHistory_ListSelfScopeFiltersByAgent: scope=self returns only the caller
// agent's chats, not a sibling agent's in the same tenant.
func TestHistory_ListSelfScopeFiltersByAgent(t *testing.T) {
	h, s := historyFixture(t)
	mine := seedChat(t, s, "t1", "agentA", "alice")
	other := seedChat(t, s, "t1", "agentB", "alice")

	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"self"}`))
	ids := chatIDs(t, res)
	if !contains(ids, mine) {
		t.Errorf("self scope should include the caller agent's chat %s; got %v", mine, ids)
	}
	if contains(ids, other) {
		t.Errorf("self scope must NOT include a sibling agent's chat %s; got %v", other, ids)
	}
}

// TestHistory_ListUserScopeFiltersByUser: scope=user returns only the caller
// user's chats within the tenant.
func TestHistory_ListUserScopeFiltersByUser(t *testing.T) {
	h, s := historyFixture(t)
	alices := seedChat(t, s, "t1", "agentA", "alice")
	bobs := seedChat(t, s, "t1", "agentA", "bob")

	res, _ := h.Execute(histCtx([]string{"user"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"user"}`))
	ids := chatIDs(t, res)
	if !contains(ids, alices) {
		t.Errorf("user scope should include alice's chat %s; got %v", alices, ids)
	}
	if contains(ids, bobs) {
		t.Errorf("user scope must NOT include bob's chat %s; got %v", bobs, ids)
	}
}

// TestHistory_ListTenantScopeSpansTenantOnly: scope=tenant sees every chat in
// the caller's tenant but none from another tenant.
func TestHistory_ListTenantScopeSpansTenantOnly(t *testing.T) {
	h, s := historyFixture(t)
	inTenant := seedChat(t, s, "t1", "agentB", "bob")
	otherTenant := seedChat(t, s, "t2", "agentA", "alice")

	res, _ := h.Execute(histCtx([]string{"tenant"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"tenant"}`))
	ids := chatIDs(t, res)
	if !contains(ids, inTenant) {
		t.Errorf("tenant scope should include same-tenant chat %s; got %v", inTenant, ids)
	}
	if contains(ids, otherTenant) {
		t.Errorf("tenant scope must NOT include another tenant's chat %s; got %v", otherTenant, ids)
	}
}

// TestHistory_GlobalRefusedForNonAdmin: even with `global` requested, a caller
// whose policy lacks it (the non-admin case, where policy resolution stripped
// it) is refused.
func TestHistory_GlobalRefusedForNonAdmin(t *testing.T) {
	h, s := historyFixture(t)
	seedChat(t, s, "t2", "agentA", "alice")
	res, _ := h.Execute(histCtx([]string{"self", "user", "tenant"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"global"}`))
	if !res.IsError {
		t.Fatal("scope:global must be refused when the policy does not grant it")
	}
}

// TestHistory_GlobalSpansTenantsWhenGranted: an admin (policy includes global)
// sees chats across tenants.
func TestHistory_GlobalSpansTenantsWhenGranted(t *testing.T) {
	h, s := historyFixture(t)
	t1chat := seedChat(t, s, "t1", "agentA", "alice")
	t2chat := seedChat(t, s, "t2", "agentB", "bob")

	res, _ := h.Execute(histCtx([]string{"self", "user", "tenant", "global"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"global"}`))
	ids := chatIDs(t, res)
	if !contains(ids, t1chat) || !contains(ids, t2chat) {
		t.Errorf("global scope should span tenants; want both %s and %s, got %v", t1chat, t2chat, ids)
	}
}

// TestHistory_GetCrossTenantOpaqueNotFound: tenant A cannot read tenant B's
// chat, and the refusal is byte-identical to a genuinely missing id (no
// existence oracle).
func TestHistory_GetCrossTenantOpaqueNotFound(t *testing.T) {
	h, s := historyFixture(t)
	victim := seedChat(t, s, "t2", "agentB", "bob")

	// A cross-tenant EXISTING id and a genuinely MISSING id must produce the
	// same opaque form: `history: chat "<the id you asked for>" not found`. The
	// message echoes only the caller's own input — it never reveals that the id
	// exists in another tenant (no existence oracle). We prove that by checking
	// each refusal equals the template for its own id.
	crossReq := fmt.Sprintf(`{"op":"get","scope":"tenant","session_id":%q}`, victim)
	crossRes, _ := h.Execute(histCtx([]string{"tenant"}, "agentA", "alice", "t1"), json.RawMessage(crossReq))
	if !crossRes.IsError {
		t.Fatal("cross-tenant get must fold to not-found")
	}
	if want := fmt.Sprintf("history: chat %q not found", victim); crossRes.Text != want {
		t.Errorf("cross-tenant refusal must be the opaque not-found form; got %q want %q", crossRes.Text, want)
	}

	missingRes, _ := h.Execute(histCtx([]string{"tenant"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"get","scope":"tenant","session_id":"does-not-exist"}`))
	if !missingRes.IsError {
		t.Fatal("missing get must error")
	}
	if want := `history: chat "does-not-exist" not found`; missingRes.Text != want {
		t.Errorf("missing refusal form = %q, want %q", missingRes.Text, want)
	}
}

// TestHistory_RenameCrossTenantRefusedAndUnmutated: a cross-tenant rename is
// refused AND the target row is never mutated.
func TestHistory_RenameCrossTenantRefusedAndUnmutated(t *testing.T) {
	h, s := historyFixture(t)
	victim := seedChat(t, s, "t2", "agentB", "bob")

	req := fmt.Sprintf(`{"op":"rename","scope":"tenant","session_id":%q,"title":"pwned"}`, victim)
	res, _ := h.Execute(histCtx([]string{"tenant"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if !res.IsError {
		t.Fatal("cross-tenant rename must be refused")
	}
	sess, err := s.GetSession(context.Background(), victim)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Title != "" {
		t.Errorf("cross-tenant rename must not mutate the row; title = %q", sess.Title)
	}
}

// TestHistory_SelfScopeGetOtherAgentOpaqueNotFound: under self scope a caller
// cannot read a sibling agent's chat in the same tenant.
func TestHistory_SelfScopeGetOtherAgentOpaqueNotFound(t *testing.T) {
	h, s := historyFixture(t)
	other := seedChat(t, s, "t1", "agentB", "alice")
	req := fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q}`, other)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if !res.IsError {
		t.Fatal("self scope must not read a sibling agent's chat")
	}
}

func TestHistory_RenameThenListReflectsTitle(t *testing.T) {
	h, s := historyFixture(t)
	id := seedChat(t, s, "t1", "agentA", "alice")

	req := fmt.Sprintf(`{"op":"rename","scope":"self","session_id":%q,"title":"Launch planning"}`, id)
	if res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req)); res.IsError {
		t.Fatalf("rename: %s", res.Text)
	}
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"self","title_contains":"launch"}`))
	ids := chatIDs(t, res)
	if !contains(ids, id) {
		t.Errorf("renamed chat should match title_contains; got %v", ids)
	}
}

func TestHistory_AnnotateSetsDescriptionAndTags(t *testing.T) {
	h, s := historyFixture(t)
	id := seedChat(t, s, "t1", "agentA", "alice")

	req := fmt.Sprintf(`{"op":"annotate","scope":"self","session_id":%q,"description":"Q3 roadmap","tags":["q3","planning"]}`, id)
	if res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req)); res.IsError {
		t.Fatalf("annotate: %s", res.Text)
	}
	sess, _ := s.GetSession(context.Background(), id)
	if sess.Description != "Q3 roadmap" {
		t.Errorf("description = %q, want Q3 roadmap", sess.Description)
	}
	if len(sess.Tags) != 2 || sess.Tags[0] != "q3" {
		t.Errorf("tags = %v, want [q3 planning]", sess.Tags)
	}
	// A tag filter should now find it.
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"self","tag":"q3"}`))
	if ids := chatIDs(t, res); !contains(ids, id) {
		t.Errorf("tag filter should find the annotated chat; got %v", ids)
	}
}

func TestHistory_PinFloatsFirst(t *testing.T) {
	h, s := historyFixture(t)
	_ = seedChat(t, s, "t1", "agentA", "alice")
	pinned := seedChat(t, s, "t1", "agentA", "alice")

	req := fmt.Sprintf(`{"op":"pin","scope":"self","session_id":%q}`, pinned)
	if res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req)); res.IsError {
		t.Fatalf("pin: %s", res.Text)
	}
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"self"}`))
	ids := chatIDs(t, res)
	if len(ids) == 0 || ids[0] != pinned {
		t.Errorf("pinned chat should sort first; got %v", ids)
	}
}

func TestHistory_ArchiveHiddenByDefault(t *testing.T) {
	h, s := historyFixture(t)
	id := seedChat(t, s, "t1", "agentA", "alice")

	req := fmt.Sprintf(`{"op":"archive","scope":"self","session_id":%q}`, id)
	if res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req)); res.IsError {
		t.Fatalf("archive: %s", res.Text)
	}
	// Excluded by default.
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"self"}`))
	if contains(chatIDs(t, res), id) {
		t.Errorf("archived chat should be hidden by default")
	}
	// Included with include_archived.
	res, _ = h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"list","scope":"self","include_archived":true}`))
	if !contains(chatIDs(t, res), id) {
		t.Errorf("include_archived should surface the archived chat")
	}
}

// TestHistory_GetReturnsTranscriptAndAggregates: get returns the transcript and
// the token/run-count aggregates rolled up from the chat's runs.
func TestHistory_GetReturnsTranscriptAndAggregates(t *testing.T) {
	h, s := historyFixture(t)
	bg := context.Background()
	id := seedChat(t, s, "t1", "agentA", "alice")
	run, err := s.CreateRun(bg, id, store.RunIdentity{AgentID: "a_run", UserID: "alice", TenantID: "t1"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	_ = s.AppendEvent(bg, run.ID, "text", []byte(`{"text":"hello world"}`))
	if err := s.FinishRun(bg, run.ID, store.RunCompleted, "end_turn",
		store.Usage{InputTokens: 10, OutputTokens: 5}, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	req := fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q}`, id)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if res.IsError {
		t.Fatalf("get: %s", res.Text)
	}
	var out struct {
		Chat struct {
			RunCount     int   `json:"run_count"`
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"chat"`
		Transcript []struct {
			Type string `json:"type"`
		} `json:"transcript"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode get: %v (%s)", err, res.Text)
	}
	if out.Chat.RunCount != 1 {
		t.Errorf("run_count = %d, want 1", out.Chat.RunCount)
	}
	if out.Chat.InputTokens != 10 || out.Chat.OutputTokens != 5 {
		t.Errorf("tokens = %d/%d, want 10/5", out.Chat.InputTokens, out.Chat.OutputTokens)
	}
	if len(out.Transcript) == 0 {
		t.Error("transcript should contain the seeded event")
	}
}

func TestHistory_GetMarkdownFormat(t *testing.T) {
	h, s := historyFixture(t)
	bg := context.Background()
	id := seedChat(t, s, "t1", "agentA", "alice")
	run, _ := s.CreateRun(bg, id, store.RunIdentity{AgentID: "a_run", UserID: "alice", TenantID: "t1"})
	_ = s.AppendEvent(bg, run.ID, "text", []byte(`{"text":"hello markdown"}`))

	req := fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q,"format":"markdown"}`, id)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if res.IsError {
		t.Fatalf("get markdown: %s", res.Text)
	}
	var out struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Markdown == "" {
		t.Fatal("markdown format should return a non-empty rendering")
	}
}

func TestHistory_SearchMatchesTitle(t *testing.T) {
	h, s := historyFixture(t)
	match := seedChat(t, s, "t1", "agentA", "alice")
	noMatch := seedChat(t, s, "t1", "agentA", "alice")
	_ = s.SetSessionMeta(context.Background(), match, store.SessionMetaPatch{Title: strptr("Deploy pipeline review")})
	_ = s.SetSessionMeta(context.Background(), noMatch, store.SessionMetaPatch{Title: strptr("Grocery list")})

	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"search","scope":"self","query":"pipeline"}`))
	ids := chatIDs(t, res)
	if !contains(ids, match) || contains(ids, noMatch) {
		t.Errorf("search should match only the title-matching chat; got %v", ids)
	}
}

func TestHistory_SearchRequiresQuery(t *testing.T) {
	h, _ := historyFixture(t)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"),
		json.RawMessage(`{"op":"search","scope":"self"}`))
	if !res.IsError {
		t.Fatal("search without a query should refuse")
	}
}

// TestHistory_RecapRefusesWhenNotConfigured: op=recap with a nil Recap closure
// refuses cleanly (the "no summarizer wired" posture) — the tool never calls a
// provider itself, mirroring TeamDef op=run on a nil Spawn.
func TestHistory_RecapRefusesWhenNotConfigured(t *testing.T) {
	h, s := historyFixture(t)
	id := seedChat(t, s, "t1", "agentA", "alice")
	req := fmt.Sprintf(`{"op":"recap","scope":"self","session_id":%q}`, id)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if !res.IsError {
		t.Fatal("recap with no Recap summarizer wired must refuse")
	}
}

// TestHistory_RecapStoresSummaryAndSurfaces: recap calls the injected summarizer,
// persists its output to the chat metadata, and get surfaces the stored summary.
func TestHistory_RecapStoresSummaryAndSurfaces(t *testing.T) {
	h, s := historyFixture(t)
	id := seedChat(t, s, "t1", "agentA", "alice")
	var gotSession string
	h.Recap = func(_ context.Context, sessionID string) (string, error) {
		gotSession = sessionID
		return "This chat is about launch planning.", nil
	}
	req := fmt.Sprintf(`{"op":"recap","scope":"self","session_id":%q}`, id)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if res.IsError {
		t.Fatalf("recap: %s", res.Text)
	}
	if gotSession != id {
		t.Errorf("Recap invoked with %q, want the folded session id %q", gotSession, id)
	}
	// The recap response echoes the summary + updated chat.
	var out struct {
		Summary string `json:"summary"`
		Chat    struct {
			Summary string `json:"summary"`
		} `json:"chat"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode recap: %v (%s)", err, res.Text)
	}
	if out.Summary != "This chat is about launch planning." || out.Chat.Summary != out.Summary {
		t.Errorf("recap response summary mismatch: %+v", out)
	}
	// Persisted to the session metadata (with summary_updated_at stamped).
	sess, _ := s.GetSession(context.Background(), id)
	if sess.Summary != "This chat is about launch planning." {
		t.Errorf("summary not persisted; got %q", sess.Summary)
	}
	if sess.SummaryUpdatedAt.IsZero() {
		t.Error("recap must stamp summary_updated_at")
	}
	// get surfaces the stored summary.
	getReq := fmt.Sprintf(`{"op":"get","scope":"self","session_id":%q}`, id)
	getRes, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(getReq))
	if getRes.IsError {
		t.Fatalf("get: %s", getRes.Text)
	}
	var got struct {
		Chat struct {
			Summary string `json:"summary"`
		} `json:"chat"`
	}
	if err := json.Unmarshal([]byte(getRes.Text), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.Chat.Summary != "This chat is about launch planning." {
		t.Errorf("get should surface the stored summary; got %q", got.Chat.Summary)
	}
}

// TestHistory_RecapIdempotentRefreshes: re-running recap replaces the cached
// summary rather than appending — the metadata always holds the latest.
func TestHistory_RecapIdempotentRefreshes(t *testing.T) {
	h, s := historyFixture(t)
	id := seedChat(t, s, "t1", "agentA", "alice")
	calls := 0
	h.Recap = func(_ context.Context, _ string) (string, error) {
		calls++
		return fmt.Sprintf("summary v%d", calls), nil
	}
	req := fmt.Sprintf(`{"op":"recap","scope":"self","session_id":%q}`, id)
	ctx := histCtx([]string{"self"}, "agentA", "alice", "t1")
	if res, _ := h.Execute(ctx, json.RawMessage(req)); res.IsError {
		t.Fatalf("first recap: %s", res.Text)
	}
	if res, _ := h.Execute(ctx, json.RawMessage(req)); res.IsError {
		t.Fatalf("second recap: %s", res.Text)
	}
	if calls != 2 {
		t.Errorf("recap should invoke the summarizer each call; got %d", calls)
	}
	sess, _ := s.GetSession(context.Background(), id)
	if sess.Summary != "summary v2" {
		t.Errorf("recap should refresh to the latest; got %q, want summary v2", sess.Summary)
	}
}

// TestHistory_RecapCrossTenantOpaqueNotFound: a cross-tenant recap folds to the
// opaque not-found BEFORE any summarization — the (costly) Recap closure is never
// invoked for an out-of-scope chat.
func TestHistory_RecapCrossTenantOpaqueNotFound(t *testing.T) {
	h, s := historyFixture(t)
	victim := seedChat(t, s, "t2", "agentB", "bob")
	called := false
	h.Recap = func(context.Context, string) (string, error) {
		called = true
		return "should not happen", nil
	}
	req := fmt.Sprintf(`{"op":"recap","scope":"tenant","session_id":%q}`, victim)
	res, _ := h.Execute(histCtx([]string{"tenant"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if !res.IsError {
		t.Fatal("cross-tenant recap must fold to not-found")
	}
	if want := fmt.Sprintf("history: chat %q not found", victim); res.Text != want {
		t.Errorf("cross-tenant recap refusal = %q, want opaque %q", res.Text, want)
	}
	if called {
		t.Error("Recap must NOT run for an out-of-scope chat (fold-first, before any LLM work)")
	}
}

// TestHistory_ResumeReturnsHandle: resume returns the continuation coordinates +
// a hint, and does not itself start a run.
func TestHistory_ResumeReturnsHandle(t *testing.T) {
	h, s := historyFixture(t)
	bg := context.Background()
	id := seedChat(t, s, "t1", "agentA", "alice")
	run, _ := s.CreateRun(bg, id, store.RunIdentity{AgentID: "a_run", UserID: "alice", TenantID: "t1"})
	_ = s.FinishRun(bg, run.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	req := fmt.Sprintf(`{"op":"resume","scope":"self","session_id":%q}`, id)
	res, _ := h.Execute(histCtx([]string{"self"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if res.IsError {
		t.Fatalf("resume: %s", res.Text)
	}
	var out struct {
		Resume struct {
			SessionID string `json:"session_id"`
			Agent     string `json:"agent"`
			TenantID  string `json:"tenant_id"`
			UserID    string `json:"user_id"`
			Status    string `json:"status"`
			Hint      string `json:"hint"`
		} `json:"resume"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode resume: %v (%s)", err, res.Text)
	}
	if out.Resume.SessionID != id || out.Resume.Agent != "agentA" ||
		out.Resume.TenantID != "t1" || out.Resume.UserID != "alice" {
		t.Errorf("resume handle coordinates mismatch: %+v", out.Resume)
	}
	if out.Resume.Status != string(store.RunCompleted) {
		t.Errorf("status = %q, want %q", out.Resume.Status, store.RunCompleted)
	}
	if out.Resume.Hint == "" {
		t.Error("resume handle must carry a continuation hint")
	}
}

// TestHistory_ResumeCrossTenantOpaqueNotFound: resume enforces the same tenant
// fold as every other by-id op.
func TestHistory_ResumeCrossTenantOpaqueNotFound(t *testing.T) {
	h, s := historyFixture(t)
	victim := seedChat(t, s, "t2", "agentB", "bob")
	req := fmt.Sprintf(`{"op":"resume","scope":"tenant","session_id":%q}`, victim)
	res, _ := h.Execute(histCtx([]string{"tenant"}, "agentA", "alice", "t1"), json.RawMessage(req))
	if !res.IsError {
		t.Fatal("cross-tenant resume must fold to not-found")
	}
	if want := fmt.Sprintf("history: chat %q not found", victim); res.Text != want {
		t.Errorf("cross-tenant resume refusal = %q, want opaque %q", res.Text, want)
	}
}

func strptr(s string) *string { return &s }
