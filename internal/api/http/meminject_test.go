package http

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

func labels(bs []config.CoreBlock) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Scope+"/"+b.Label)
	}
	return out
}

func has(bs []config.CoreBlock, scope, label string) bool {
	for _, b := range bs {
		if b.Scope == scope && b.Label == label {
			return true
		}
	}
	return false
}

// TestCoreBlock_NotInheritedByDefaultSubAgent pins RFC BL P1 spawn-tree rules:
// a sub-agent whose def leaves inherit_core_blocks unset (default false) gets
// ONLY its own declared blocks — the parent run's user/tenant blocks do not
// flow. With inherit_core_blocks:true it additionally receives the parent's
// user/tenant blocks, but NEVER the parent's agent-scope block.
func TestCoreBlock_NotInheritedByDefaultSubAgent(t *testing.T) {
	own := []config.CoreBlock{{Label: "notes", Scope: "agent"}}
	parent := []config.CoreBlock{
		{Label: "human", Scope: "user"},    // inheritable
		{Label: "policy", Scope: "tenant"}, // inheritable
		{Label: "secret", Scope: "agent"},  // NEVER inherited
	}

	// Default: no inheritance.
	got := effectiveCoreBlocks(own, false, parent)
	if len(got) != 1 || !has(got, "agent", "notes") {
		t.Fatalf("default sub-agent should get only its own blocks, got %v", labels(got))
	}
	if has(got, "user", "human") || has(got, "tenant", "policy") {
		t.Errorf("parent blocks leaked into a non-inheriting sub-agent: %v", labels(got))
	}

	// Opt-in: parent's user/tenant blocks flow; parent's agent block does not.
	got = effectiveCoreBlocks(own, true, parent)
	if !has(got, "agent", "notes") || !has(got, "user", "human") || !has(got, "tenant", "policy") {
		t.Errorf("inherit_core_blocks should add parent user/tenant blocks: %v", labels(got))
	}
	if has(got, "agent", "secret") {
		t.Errorf("parent AGENT-scope block must never be inherited: %v", labels(got))
	}
}

// TestEffectiveCoreBlocks_OwnWinsOnCollision pins that the agent's own block
// wins over an inherited block with the same (scope,label).
func TestEffectiveCoreBlocks_OwnWinsOnCollision(t *testing.T) {
	own := []config.CoreBlock{{Label: "human", Scope: "user", ReadOnly: true}}
	parent := []config.CoreBlock{{Label: "human", Scope: "user", ReadOnly: false}}
	got := effectiveCoreBlocks(own, true, parent)
	if len(got) != 1 {
		t.Fatalf("collision should collapse to one block, got %v", labels(got))
	}
	if !got[0].ReadOnly {
		t.Errorf("own block (read_only) should win over inherited: %+v", got[0])
	}
}

// TestEffectiveCoreBlocks_EmptyIsNil keeps a no-blocks agent byte-clean (nil,
// not an empty slice) so the fast path + policy stay zero-cost.
func TestEffectiveCoreBlocks_EmptyIsNil(t *testing.T) {
	if got := effectiveCoreBlocks(nil, true, nil); got != nil {
		t.Errorf("expected nil for no blocks, got %v", got)
	}
}

// TestFirstUserText extracts the initial user input used for search_request.
func TestFirstUserText(t *testing.T) {
	segs := []loop.PromptSegment{
		{Role: "system", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "sys"}}},
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "text", Text: "find my "}, {Type: "text", Text: "prefs"}}},
	}
	if got := firstUserText(segs); got != "find my  prefs" {
		t.Errorf("firstUserText = %q", got)
	}
	if got := firstUserText(nil); got != "" {
		t.Errorf("firstUserText(nil) = %q, want empty", got)
	}
}

// memStoreFixture wires an in-memory KV store + a temp-dir SQL Memory manager —
// the two backends the user-root Document (a chunked-graph doc) needs.
func memStoreFixture(t *testing.T) (store.Store, *sqlmem.Manager) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return st, mgr
}

// userDocCount counts documents in a (tenant, user) SQL-Memory scope. A missing
// scope DB / table (nothing provisioned yet) reads as zero.
func userDocCount(t *testing.T, mgr *sqlmem.Manager, tenant, user string) int {
	t.Helper()
	key := sqlmem.ScopeKey{Tenant: tenant, Scope: "user", ScopeID: user}
	res, err := mgr.Query(context.Background(), key, "SELECT COUNT(*) FROM documents", nil)
	if err != nil || len(res.Rows) == 0 {
		return 0
	}
	n, _ := res.Rows[0][0].(int64)
	return int(n)
}

// TestMemoryProtocol_InjectedAndCitesNoRFC pins RFC BL P1 PR4: with
// memory_protocol on, a runtime-authored protocol block is injected ABOVE the
// base prompt; with it off the prompt is byte-identical; and neither the injected
// text NOR the agentic-memory help topic it points to may cite an internal "RFC"
// (the model-visible-text house rule).
func TestMemoryProtocol_InjectedAndCitesNoRFC(t *testing.T) {
	s := &Server{} // protocol injection needs no store
	mi := memInject{Tenant: "t1", UserID: "u1", AgentName: "a"}

	on := config.AgentDef{SystemPrompt: "Base prompt.", MemoryProtocol: true}
	got, _ := s.applyMemoryInjection(context.Background(), on, mi)
	if !strings.Contains(got.SystemPrompt, "# Memory protocol") {
		t.Fatalf("memory_protocol on: protocol block missing:\n%s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "/memory/index") {
		t.Errorf("protocol should reference the memory index path:\n%s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "Context op=help topic=agentic-memory") {
		t.Errorf("protocol should point at the help topic for detail:\n%s", got.SystemPrompt)
	}
	if pi, bi := strings.Index(got.SystemPrompt, "# Memory protocol"), strings.Index(got.SystemPrompt, "Base prompt."); pi < 0 || bi < 0 || pi > bi {
		t.Errorf("protocol must sit ABOVE the base prompt:\n%s", got.SystemPrompt)
	}
	if strings.Contains(got.SystemPrompt, "RFC") {
		t.Errorf("injected protocol text must not cite RFC letters/numbers:\n%s", got.SystemPrompt)
	}

	off := config.AgentDef{SystemPrompt: "Base prompt.", MemoryProtocol: false}
	gotOff, _ := s.applyMemoryInjection(context.Background(), off, mi)
	if gotOff.SystemPrompt != "Base prompt." {
		t.Errorf("memory_protocol off must be byte-identical, got:\n%q", gotOff.SystemPrompt)
	}

	// The help topic the protocol points to must also cite no RFC.
	set, err := help.LoadSet("")
	if err != nil {
		t.Fatalf("help.LoadSet: %v", err)
	}
	topic, ok := set.Get("agentic-memory")
	if !ok {
		t.Fatal("agentic-memory help topic not found")
	}
	if strings.Contains(topic.Content, "RFC") {
		t.Errorf("agentic-memory help topic must not cite RFC letters/numbers")
	}
	if !strings.Contains(topic.Content, "/memory/index") || !strings.Contains(topic.Content, "/memory/topics/") {
		t.Errorf("agentic-memory topic should document the /memory index + topics convention")
	}
}

// userInfoBodies splits a composed user_info section into its doc + human bodies,
// dropping the (structural) truncation marker so the caller measures SOURCE bytes.
func userInfoBodies(out string) (doc, human string) {
	const dl = "## Operator-authored user profile\n"
	const hl = "## Learned about the user\n"
	if i := strings.Index(out, dl); i >= 0 {
		rest := out[i+len(dl):]
		if j := strings.Index(rest, hl); j >= 0 {
			doc = rest[:j]
			human = rest[j+len(hl):]
		} else {
			doc = rest
		}
	} else if i := strings.Index(out, hl); i >= 0 {
		human = out[i+len(hl):]
	}
	return stripTruncMarker(doc), stripTruncMarker(human)
}

func stripTruncMarker(s string) string {
	var keep []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) == userInfoTruncMarker {
			continue
		}
		keep = append(keep, ln)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// TestUserInfo_OperatorReservationBudget pins the {{memory:user_info}}
// composition budget: the operator user-root reserves its share (>= the block's),
// the block borrows unused headroom when the doc is tiny, the surviving content
// stays within budget, and truncation is line-boundary-aware (never mid-line).
func TestUserInfo_OperatorReservationBudget(t *testing.T) {
	mkLines := func(tok string, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(tok)
			b.WriteByte('\n')
		}
		return strings.TrimRight(b.String(), "\n")
	}
	// Same-width tokens so byte lengths reflect the budget split, not token width.
	doc := mkLines("DOC", 100)
	human := mkLines("USR", 100)
	const budget = 200

	out := composeUserInfo(doc, human, budget)
	if !strings.Contains(out, "## Operator-authored user profile") || !strings.Contains(out, "## Learned about the user") {
		t.Fatalf("both labeled sub-sections expected:\n%s", out)
	}
	if !strings.Contains(out, userInfoTruncMarker) {
		t.Errorf("expected a truncation marker (both bodies exceed the budget):\n%s", out)
	}
	docBody, humanBody := userInfoBodies(out)

	// Operator user-root (reserved 60%) gets at least the block's share.
	if len(docBody) < len(humanBody) {
		t.Errorf("operator user-root should get >= the block's share: doc=%d human=%d", len(docBody), len(humanBody))
	}
	// Surviving SOURCE content stays within budget.
	if len(docBody)+len(humanBody) > budget {
		t.Errorf("content %d exceeds budget %d", len(docBody)+len(humanBody), budget)
	}
	// Boundary-aware: every surviving line is a WHOLE token, never a mid-line cut.
	for _, ln := range strings.Split(docBody, "\n") {
		if ln != "" && ln != "DOC" {
			t.Errorf("doc body has a partial/foreign line %q:\n%s", ln, out)
		}
	}
	for _, ln := range strings.Split(humanBody, "\n") {
		if ln != "" && ln != "USR" {
			t.Errorf("human body has a partial/foreign line %q:\n%s", ln, out)
		}
	}

	// Borrow: a tiny operator doc lets the block claim the unused headroom.
	small := composeUserInfo("DOC", human, budget)
	_, borrow := userInfoBodies(small)
	if len(borrow) <= budget*40/100 {
		t.Errorf("block should borrow the operator's unused headroom, got %d bytes (<= 40%% of %d)", len(borrow), budget)
	}
}

// TestUserRoot_LazyProvisionOnFirstReference pins that the FIRST run to expand
// {{memory:user_info}} provisions the user-root Document exactly once, a repeat
// run does not re-create it (per-process cache), a fresh server sharing the store
// does not duplicate it (exists-check), and the provisioned profile is injected
// as framed DATA.
func TestUserRoot_LazyProvisionOnFirstReference(t *testing.T) {
	st, mgr := memStoreFixture(t)
	s := &Server{store: st}
	s.SetSqlMem(mgr)

	mi := memInject{Tenant: "t1", UserID: "alice", AgentName: "helper"}
	agent := config.AgentDef{SystemPrompt: "Base. {{memory:user_info}}"}

	s.applyMemoryInjection(context.Background(), agent, mi)
	if n := userDocCount(t, mgr, "t1", "alice"); n != 1 {
		t.Fatalf("first reference should provision exactly one user-root doc, got %d", n)
	}

	s.applyMemoryInjection(context.Background(), agent, mi)
	if n := userDocCount(t, mgr, "t1", "alice"); n != 1 {
		t.Fatalf("second run must not re-create the doc (cache), got %d", n)
	}

	s2 := &Server{store: st}
	s2.SetSqlMem(mgr)
	s2.applyMemoryInjection(context.Background(), agent, mi)
	if n := userDocCount(t, mgr, "t1", "alice"); n != 1 {
		t.Fatalf("exists-check must prevent a duplicate on a fresh server, got %d", n)
	}

	got, _ := s.applyMemoryInjection(context.Background(), agent, mi)
	if !strings.Contains(got.SystemPrompt, "Operator-authored user profile") {
		t.Errorf("user_info should render the provisioned profile:\n%s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, `<memory source="user_info">`) {
		t.Errorf("user_info must be framed as reference DATA:\n%s", got.SystemPrompt)
	}
}

// TestUserRoot_ConcurrentProvisionCreatesOne pins the RFC BL P1 duplicate-doc
// fix: N goroutines racing the FIRST reference for one (tenant,user) must
// provision exactly ONE user-root Document, not one per goroutine. createDocument
// mints a fresh doc id each call (the dirent upsert hides it), so without the
// singleflight serialization every goroutine's exists-check misses and each
// import_md leaves a duplicate, orphaned doc. Run under -race.
func TestUserRoot_ConcurrentProvisionCreatesOne(t *testing.T) {
	st, mgr := memStoreFixture(t)
	s := &Server{store: st}
	s.SetSqlMem(mgr)
	mi := memInject{Tenant: "t1", UserID: "alice", AgentName: "helper"}

	const n = 24
	start := make(chan struct{}) // release all goroutines together to widen the race window
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			s.ensureUserRootDoc(context.Background(), mi)
		}()
	}
	close(start)
	wg.Wait()

	if got := userDocCount(t, mgr, "t1", "alice"); got != 1 {
		t.Fatalf("concurrent first-references must provision exactly one user-root doc, got %d", got)
	}
}

// TestUserRoot_NotProvisionedWithoutReference pins that an agent that never
// references {{memory:user_info}} creates no user-root doc, and that
// memory_roots=suppress opts out even WITH a reference.
func TestUserRoot_NotProvisionedWithoutReference(t *testing.T) {
	st, mgr := memStoreFixture(t)
	s := &Server{store: st}
	s.SetSqlMem(mgr)
	mi := memInject{Tenant: "t1", UserID: "alice", AgentName: "helper"}

	noRef := config.AgentDef{SystemPrompt: "Base prompt, no memory references."}
	s.applyMemoryInjection(context.Background(), noRef, mi)
	if n := userDocCount(t, mgr, "t1", "alice"); n != 0 {
		t.Fatalf("no user_info reference must not provision, got %d docs", n)
	}

	// A prompt that references a DIFFERENT memory variant (so it enters the
	// injection slow path) but NOT user_info must still not provision — this
	// exercises the per-variant reference gate, not just the fast path.
	otherRef := config.AgentDef{SystemPrompt: "Base. {{memory:core_blocks}}"}
	s.applyMemoryInjection(context.Background(), otherRef, mi)
	if n := userDocCount(t, mgr, "t1", "alice"); n != 0 {
		t.Fatalf("a non-user_info memory reference must not provision the user-root, got %d docs", n)
	}

	suppress := config.AgentDef{SystemPrompt: "Base. {{memory:user_info}}", MemoryRoots: "suppress"}
	s.applyMemoryInjection(context.Background(), suppress, mi)
	if n := userDocCount(t, mgr, "t1", "alice"); n != 0 {
		t.Fatalf("memory_roots=suppress must not provision, got %d docs", n)
	}
}

// TestUserRoot_ForcePreProvisions pins that memory_roots=force pre-provisions the
// user-root doc even for a run that does NOT reference {{memory:user_info}}, so an
// operator can fill it in before first use.
func TestUserRoot_ForcePreProvisions(t *testing.T) {
	st, mgr := memStoreFixture(t)
	s := &Server{store: st}
	s.SetSqlMem(mgr)
	mi := memInject{Tenant: "t1", UserID: "alice", AgentName: "helper"}

	agent := config.AgentDef{SystemPrompt: "No memory references here.", MemoryRoots: "force"}
	s.applyMemoryInjection(context.Background(), agent, mi)
	if n := userDocCount(t, mgr, "t1", "alice"); n != 1 {
		t.Fatalf("memory_roots=force should pre-provision without a reference, got %d", n)
	}
}
