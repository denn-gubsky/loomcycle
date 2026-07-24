package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// End-to-end consolidation-pipeline tests (RFC BL P2 PR4).
//
// These are TRUE end-to-end runs: a real HTTP POST /v1/runs drives the real
// agent loop, which dispatches real Memory/History tool calls against a real
// sqlite store. Only the PROVIDER is stubbed — scriptedProvider replays the
// tool_use sequence the `memory/consolidate` skill body prescribes, in the order
// it prescribes, which is exactly the part a live model would decide. So the
// pipeline's plumbing (grants, scope resolution, provenance, supersede,
// watermark) is exercised for real; the model's judgement is the only stand-in.
//
// The shipped mock DRIVER is not used: its scripts are hardcoded per-model FSMs
// for the runtime/soak suites, and adding a consolidation FSM to a production
// driver to serve a unit test would be the wrong trade. scriptedProvider is this
// package's established pattern for exactly this.

// consolidationEnv is the fixture: a consolidator agent wired with the same
// grants the shipped bundle declares, a real store, and a scripted provider.
type consolidationEnv struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	store store.Store
	prov  *scriptedProvider
}

// consolidatorAgentDef mirrors the shipped bundle's grants. Document/Path are
// omitted (the index refresh is explicitly optional in the skill, and Documents
// need the SQL-Memory subsystem) — everything the pipeline's correctness depends
// on is here.
func consolidatorAgentDef() config.AgentDef {
	return config.AgentDef{
		Model:               "stub-model",
		Tools:               []string{"Memory", "History"},
		MemoryScopes:        []string{"agent", "user"},
		MemoryConsolidation: true,
		HistoryScope:        []string{"user"},
		SystemPrompt:        "you consolidate memory",
	}
}

// newConsolidationEnv builds the server. scripts are the provider's per-call
// event sequences, in order.
func newConsolidationEnv(t *testing.T, scripts [][]providers.Event) *consolidationEnv {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"memory/consolidator": consolidatorAgentDef()},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 2000},
	}
	cfg.Env.AuthToken = ""

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "consolidation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	prov := &scriptedProvider{
		scripts:  scripts,
		defaultS: []providers.Event{{Type: providers.EventText, Text: "done"}, {Type: providers.EventDone, StopReason: "end_turn"}},
	}
	memTool := &builtin.Memory{Cfg: cfg}
	memTool.Store = st
	histTool := &builtin.History{}
	histTool.Store = st

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{memTool, histTool}, concurrency.New(4, 4, 2*time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &consolidationEnv{t: t, srv: srv, ts: ts, store: st, prov: prov}
}

// runConsolidation POSTs a run as userID — the identity the fan-out would set,
// and what the Memory tool's server-side `scope: user` resolution keys off.
func (e *consolidationEnv) runConsolidation(userID string) string {
	e.t.Helper()
	body := fmt.Sprintf(
		`{"agent":"memory/consolidator","user_id":%q,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"run one consolidation pass"}]}]}`,
		userID)
	resp, err := http.Post(e.ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		e.t.Fatalf("post run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("run status = %d: %s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// memValue reads a user-scope memory key's value, or "" when absent/superseded.
func (e *consolidationEnv) memValue(userID, key string) string {
	e.t.Helper()
	entry, err := e.store.MemoryGet(context.Background(), "", store.MemoryScopeUser, userID, key)
	if err != nil {
		return ""
	}
	var s string
	if json.Unmarshal(entry.Value, &s) == nil {
		return s
	}
	return string(entry.Value)
}

// memKeys lists the live (non-superseded) user-scope keys.
func (e *consolidationEnv) memKeys(userID string) []string {
	e.t.Helper()
	entries, _, err := e.store.MemoryList(context.Background(), "", store.MemoryScopeUser, userID, "", 500)
	if err != nil {
		e.t.Fatalf("MemoryList: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, en := range entries {
		out = append(out, en.Key)
	}
	return out
}

// toolCall builds one scripted provider turn: a single tool_use plus the
// tool_use stop reason the loop needs to dispatch it.
func toolCall(id, name, input string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 2}},
	}
}

// finalText is the terminal turn: the pass's report.
func finalText(text string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventText, Text: text},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 4, OutputTokens: 3}},
	}
}

// seedChat creates a settled chat for userID: one completed run whose transcript
// carries the planted facts. Returns (sessionID, runID) — the provenance the
// consolidator relays onto each fact it distils.
func seedChat(t *testing.T, st store.Store, userID string, turns ...string) (string, string) {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "chat", userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "chat-" + sess.ID, UserID: userID})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for _, turn := range turns {
		payload, _ := json.Marshal(map[string]string{"text": turn})
		if err := st.AppendEvent(ctx, run.ID, "text", payload); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	if err := st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	return sess.ID, run.ID
}

// consolidationScript assembles the provider turns for one full pass over a
// source chat, in the order the skill body prescribes: lease → cursor_get →
// History list → History get → pending_drain → recall → the writes → advance →
// release → report. writes are the already-JSON-encoded Memory tool inputs.
func consolidationScript(sessionID string, watermark time.Time, writes ...string) [][]providers.Event {
	scripts := [][]providers.Event{
		toolCall("tu_lease", "Memory", `{"op":"cursor_lease","scope":"user","lease_ttl_ms":600000}`),
		toolCall("tu_cursor", "Memory", `{"op":"cursor_get","scope":"user"}`),
		toolCall("tu_list", "History", `{"op":"list","scope":"user","limit":25}`),
		toolCall("tu_get", "History", fmt.Sprintf(`{"op":"get","scope":"user","session_id":%q,"format":"markdown"}`, sessionID)),
		toolCall("tu_drain", "Memory", `{"op":"pending_drain","scope":"user","limit":50}`),
		toolCall("tu_recall", "Memory", `{"op":"recall","scope":"user","query":"what do I know","top_k":8}`),
	}
	for i, w := range writes {
		scripts = append(scripts, toolCall(fmt.Sprintf("tu_write_%d", i), "Memory", w))
	}
	scripts = append(scripts,
		toolCall("tu_advance", "Memory", fmt.Sprintf(`{"op":"cursor_advance","scope":"user","completed_at":%q,"session_id":%q}`,
			watermark.UTC().Format(time.RFC3339Nano), sessionID)),
		toolCall("tu_release", "Memory", `{"op":"cursor_release","scope":"user"}`),
		finalText("consolidated 1 session, wrote facts"),
	)
	return scripts
}

// setOp renders an `add`-branch write exactly as the skill prescribes: a
// deterministic subject-derived key plus the provenance block.
func setOp(key, text, class, sessionID, runID string) string {
	in := map[string]any{
		"op":    "set",
		"scope": "user",
		"key":   key,
		"value": text,
		"provenance": map[string]string{
			"class":             class,
			"source_session_id": sessionID,
			"source_run_id":     runID,
		},
	}
	raw, _ := json.Marshal(in)
	return string(raw)
}

// TestConsolidate_PlantedFactsWritten is the pipeline's headline: facts planted
// in a finished chat come out the other side as durable memory rows, written by
// a real run through the real tool dispatch under the real grants.
func TestConsolidate_PlantedFactsWritten(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	// Build the env first so the seed lands in the SAME store the run uses.
	env := newConsolidationEnv(t, nil)
	sessID, runID := seedChat(t, env.store, "alice",
		"I always want tabs, never spaces",
		"and deploys go through staging first")

	rows, err := env.store.ConsolidatableSessions(context.Background(), "", "alice", "", "", time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("ConsolidatableSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("seeded sessions = %d, want 1", len(rows))
	}

	env.prov.scripts = consolidationScript(sessID, rows[0].MaxCompletedAt,
		setOp("memory/preference/editor-indent-style", "Alice prefers tabs over spaces in the editor.", "preference", sessID, runID),
		setOp("memory/constraint/deploy-staging-first", "Alice's deploys go through staging before production.", "constraint", sessID, runID),
	)

	env.runConsolidation("alice")

	for key, wantSubstr := range map[string]string{
		"memory/preference/editor-indent-style":  "tabs",
		"memory/constraint/deploy-staging-first": "staging",
	} {
		got := env.memValue("alice", key)
		if got == "" {
			t.Errorf("planted fact did not land at %q; live keys = %v", key, env.memKeys("alice"))
			continue
		}
		if !strings.Contains(got, wantSubstr) {
			t.Errorf("fact at %q = %q, want it to mention %q", key, got, wantSubstr)
		}
	}
}

// TestConsolidate_ProvenanceStamped: every consolidated fact must carry its
// audit trail — origin=consolidator (stamped server-side from the run's grant),
// the class the pass assigned, and the chat/run it was distilled from. Without
// this a consolidated fact is indistinguishable from one an agent typed, and
// there is no way back to the transcript that produced it.
func TestConsolidate_ProvenanceStamped(t *testing.T) {
	env := newConsolidationEnv(t, nil)
	sessID, runID := seedChat(t, env.store, "alice", "I always want tabs, never spaces")
	rows, _ := env.store.ConsolidatableSessions(context.Background(), "", "alice", "", "", time.Time{}, "", 10)
	if len(rows) != 1 {
		t.Fatalf("seeded sessions = %d, want 1", len(rows))
	}
	key := "memory/preference/editor-indent-style"
	env.prov.scripts = consolidationScript(sessID, rows[0].MaxCompletedAt,
		setOp(key, "Alice prefers tabs over spaces in the editor.", "preference", sessID, runID))

	env.runConsolidation("alice")

	prov, err := env.store.MemoryProvenanceGet(context.Background(), "", store.MemoryScopeUser, "alice", key)
	if err != nil {
		t.Fatalf("MemoryProvenanceGet(%s): %v (live keys = %v)", key, err, env.memKeys("alice"))
	}
	if prov.Origin != "consolidator" {
		t.Errorf("origin = %q, want consolidator (server-stamped from the consolidation grant)", prov.Origin)
	}
	if prov.Class != "preference" {
		t.Errorf("class = %q, want preference", prov.Class)
	}
	if prov.SourceSessionID != sessID {
		t.Errorf("source_session_id = %q, want the source chat %q", prov.SourceSessionID, sessID)
	}
	if prov.SourceRunID != runID {
		t.Errorf("source_run_id = %q, want the source run %q", prov.SourceRunID, runID)
	}
}

// TestConsolidate_UpdateSupersedesSimilar: the contradiction path. A stale fact
// the pass decides to retire must be SOFT-archived — invisible to every read,
// but the row retained for audit. A hard delete would destroy the history the
// provenance columns exist to preserve, and would also stop a later pass from
// reviving the fact if it turns out to be true again.
func TestConsolidate_UpdateSupersedesSimilar(t *testing.T) {
	env := newConsolidationEnv(t, nil)
	sessID, runID := seedChat(t, env.store, "alice", "actually I switched to spaces")
	rows, _ := env.store.ConsolidatableSessions(context.Background(), "", "alice", "", "", time.Time{}, "", 10)

	// A pre-existing fact from an earlier pass, now contradicted.
	staleKey := "memory/preference/editor-indent-style"
	if err := env.store.MemorySetProvenance(context.Background(), "", store.MemoryScopeUser, "alice", staleKey,
		json.RawMessage(`"Alice prefers tabs over spaces."`), 0,
		store.MemoryProvenance{Origin: "consolidator", Class: "preference", SourceSessionID: "older-session"}); err != nil {
		t.Fatalf("seed stale fact: %v", err)
	}

	supersede := `{"op":"supersede","scope":"user","key":"` + staleKey + `"}`
	env.prov.scripts = consolidationScript(sessID, rows[0].MaxCompletedAt,
		setOp("memory/correction/editor-indent-style-switch", "Alice switched from tabs to spaces.", "correction", sessID, runID),
		supersede,
	)

	env.runConsolidation("alice")

	// Invisible to reads...
	if got := env.memValue("alice", staleKey); got != "" {
		t.Errorf("superseded fact still readable: %q", got)
	}
	for _, k := range env.memKeys("alice") {
		if k == staleKey {
			t.Errorf("superseded key %q still listed among live keys %v", staleKey, env.memKeys("alice"))
		}
	}
	// ...but the row is RETAINED: re-writing the same key revives it rather than
	// inserting a second row, which is only possible if the row survived.
	if err := env.store.MemorySetProvenance(context.Background(), "", store.MemoryScopeUser, "alice", staleKey,
		json.RawMessage(`"revived"`), 0, store.MemoryProvenance{Origin: "consolidator", Class: "fact"}); err != nil {
		t.Fatalf("revive: %v", err)
	}
	if got := env.memValue("alice", staleKey); got != "revived" {
		t.Errorf("re-write of a superseded key = %q, want it revived to \"revived\"", got)
	}
	// The correction landed alongside.
	if got := env.memValue("alice", "memory/correction/editor-indent-style-switch"); !strings.Contains(got, "spaces") {
		t.Errorf("the correction fact did not land: %q (live keys = %v)", got, env.memKeys("alice"))
	}
}

// TestConsolidate_Idempotent is the property that makes a failed pass safe to
// retry and a re-scheduled pass harmless: consolidating the SAME sessions again
// must produce ZERO duplicate rows. It works because the skill mints
// subject-derived keys — the same fact re-derived overwrites its own row instead
// of accumulating a near-duplicate beside it.
//
// This is the assertion a "just append what you extracted" pipeline fails.
func TestConsolidate_Idempotent(t *testing.T) {
	env := newConsolidationEnv(t, nil)
	sessID, runID := seedChat(t, env.store, "alice", "I always want tabs, never spaces")
	rows, _ := env.store.ConsolidatableSessions(context.Background(), "", "alice", "", "", time.Time{}, "", 10)
	writes := []string{
		setOp("memory/preference/editor-indent-style", "Alice prefers tabs over spaces in the editor.", "preference", sessID, runID),
	}

	// Pass 1.
	env.prov.scripts = consolidationScript(sessID, rows[0].MaxCompletedAt, writes...)
	env.runConsolidation("alice")
	afterFirst := env.memKeys("alice")
	if len(afterFirst) != 1 {
		t.Fatalf("after pass 1, live keys = %v, want exactly 1", afterFirst)
	}

	// Pass 2 over the same session, same derived key, slightly reworded fact —
	// which is what a second model call would realistically produce.
	env.prov.calls.Store(0)
	env.prov.scripts = consolidationScript(sessID, rows[0].MaxCompletedAt,
		setOp("memory/preference/editor-indent-style", "Alice prefers tabs (not spaces) in the editor.", "preference", sessID, runID))
	env.runConsolidation("alice")

	afterSecond := env.memKeys("alice")
	if len(afterSecond) != 1 {
		t.Errorf("after pass 2, live keys = %v, want still exactly 1 — a re-run must not duplicate facts", afterSecond)
	}
	if got := env.memValue("alice", "memory/preference/editor-indent-style"); !strings.Contains(got, "not spaces") {
		t.Errorf("the second pass should have overwritten the row in place; got %q", got)
	}
}

// TestConsolidate_WatermarkAdvancesComposite: the pass must leave the cursor on
// the composite (completed_at, session_id) of the newest session it actually
// consolidated. Both halves matter — the timestamp alone cannot separate two
// chats that settled in the same instant, and a cursor left behind re-reads work
// already folded in while a cursor pushed too far skips sessions forever.
func TestConsolidate_WatermarkAdvancesComposite(t *testing.T) {
	env := newConsolidationEnv(t, nil)
	sessID, runID := seedChat(t, env.store, "alice", "I always want tabs, never spaces")
	rows, _ := env.store.ConsolidatableSessions(context.Background(), "", "alice", "", "", time.Time{}, "", 10)
	if len(rows) != 1 {
		t.Fatalf("seeded sessions = %d, want 1", len(rows))
	}
	want := rows[0]

	env.prov.scripts = consolidationScript(sessID, want.MaxCompletedAt,
		setOp("memory/preference/editor-indent-style", "Alice prefers tabs.", "preference", sessID, runID))
	env.runConsolidation("alice")

	cursor, err := env.store.MemoryCursorGet(context.Background(), "", store.MemoryScopeUser, "alice")
	if err != nil {
		t.Fatalf("MemoryCursorGet: %v", err)
	}
	if cursor.WatermarkSessionID != want.SessionID {
		t.Errorf("watermark session = %q, want the consolidated session %q", cursor.WatermarkSessionID, want.SessionID)
	}
	if !cursor.WatermarkCompletedAt.Equal(want.MaxCompletedAt) {
		t.Errorf("watermark completed_at = %v, want %v", cursor.WatermarkCompletedAt, want.MaxCompletedAt)
	}
	// The lease was given back, so the next pass can take it.
	if cursor.LeasedBy != "" {
		t.Errorf("lease still held by %q after the pass — the next pass would be locked out until the TTL expired", cursor.LeasedBy)
	}
	// And the target now reports no new work — which is only true with the
	// consolidator's OWN session excluded. This assertion is how the
	// perpetual-pass loop was found: the pass itself creates a settled session
	// under alice's user id, so without the self-exclusion every completed pass
	// immediately re-qualifies its own target as having new work, forever. The
	// dispatcher's has-new-work probe passes the same exclusion.
	remaining, err := env.store.ConsolidatableSessions(context.Background(), "", "alice", "", "memory/consolidator",
		cursor.WatermarkCompletedAt, cursor.WatermarkSessionID, 10)
	if err != nil {
		t.Fatalf("ConsolidatableSessions after advance: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("after advancing, %d session(s) still report as unconsolidated: %+v", len(remaining), remaining)
	}
	// Without the exclusion the pass's own session is what comes back — proving
	// the loop is real and the exclusion is what closes it.
	withSelf, err := env.store.ConsolidatableSessions(context.Background(), "", "alice", "", "",
		cursor.WatermarkCompletedAt, cursor.WatermarkSessionID, 10)
	if err != nil {
		t.Fatalf("ConsolidatableSessions (unfiltered): %v", err)
	}
	if len(withSelf) == 0 {
		t.Skip("the pass left no self-authored session; the exclusion assertion above is then vacuous")
	}
	for _, r := range withSelf {
		if r.AgentName != "memory/consolidator" {
			t.Errorf("an unconsolidated session survived that is NOT the pass's own: %+v", r)
		}
	}
}

// TestConsolidate_UngrantedAgentIsRefusedEveryControlOp is the gate check on the
// real dispatch path. The consolidation ops are gated SEPARATELY from
// memory_scopes, so an agent with memory access but without the grant must be
// refused — otherwise any memory-capable agent could move another pass's
// watermark or soft-archive facts it did not write.
func TestConsolidate_UngrantedAgentIsRefusedEveryControlOp(t *testing.T) {
	env := newConsolidationEnv(t, nil)
	// Strip the grant, keep the scopes.
	def := consolidatorAgentDef()
	def.MemoryConsolidation = false
	env.srv.cfg.Agents["memory/consolidator"] = def

	env.prov.scripts = [][]providers.Event{
		toolCall("tu_lease", "Memory", `{"op":"cursor_lease","scope":"user"}`),
		toolCall("tu_sup", "Memory", `{"op":"supersede","scope":"user","key":"anything"}`),
		finalText("refused"),
	}
	stream := env.runConsolidation("alice")

	if !strings.Contains(stream, "memory_consolidation grant") {
		t.Errorf("an ungranted agent must be refused the consolidation ops; stream:\n%s", stream)
	}
	// Nothing moved.
	cursor, err := env.store.MemoryCursorGet(context.Background(), "", store.MemoryScopeUser, "alice")
	if err != nil {
		t.Fatalf("MemoryCursorGet: %v", err)
	}
	if cursor.LeasedBy != "" || !cursor.WatermarkCompletedAt.IsZero() {
		t.Errorf("an ungranted agent changed cursor state: %+v", cursor)
	}
}
