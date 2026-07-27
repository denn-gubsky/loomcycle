package builtin

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// TestMemory_CursorScanSkipsInternalAgentSessions is the regression for the
// self-consuming consolidator.
//
// The pass excluded exactly one agent — itself. Its extractor CHILDREN were not
// excluded, and each child is a session under the same user whose transcript
// CONTAINS the chat it was extracting. Those sessions settle immediately, sit
// past the watermark forever (a pass never consolidates them, so it never
// advances over them), and came back on the next scan as work: on the live store
// 7 of the last 8 chats were extractor sessions, out of 95 total, growing ~15 a
// pass with no bound — every pass re-extracting nested copies of its own input.
func TestMemory_CursorScanSkipsInternalAgentSessions(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)
	tool.Cfg = &config.Config{Agents: map[string]config.AgentDef{
		"memory/consolidator": {Internal: true},
		"memory/extractor":    {Internal: true},
		"chat":                {},
	}}

	human, _ := seedSettledChat(t, tool, "", "chat", "alice")
	// One child per chat read — the shape a real pass produces.
	extractorA, _ := seedSettledChat(t, tool, "", "memory/extractor", "alice")
	extractorB, _ := seedSettledChat(t, tool, "", "memory/extractor", "alice")
	// A DIFFERENTLY-NAMED consolidator's session. The scanning agent here is
	// "qa-agent" (the fixture's name), so this is not covered by self-exclusion
	// and only the internal marker can reach it.
	peer, _ := seedSettledChat(t, tool, "", "memory/consolidator", "alice")

	got := scanIDs(runScan(t, tool, gctx, `{"op":"cursor_scan","scope":"user"}`))

	if !contains(got, human) {
		t.Errorf("cursor_scan dropped the real chat %s: %v", human, got)
	}
	for _, id := range []string{extractorA, extractorB, peer} {
		if contains(got, id) {
			t.Errorf("cursor_scan returned an internal agent's session %s: %v — the pass would consolidate its own output", id, got)
		}
	}
}

// TestMemory_CursorScanStillExcludesItselfWithoutConfig pins the floor: the
// self-exclusion is not something the new internal set replaced. A tool with no
// Cfg (every other unit test, and any pre-wiring caller) must behave exactly as
// it did — the scanning agent's own sessions out, everything else in.
func TestMemory_CursorScanStillExcludesItselfWithoutConfig(t *testing.T) {
	tool, ctx, cleanup := memoryFixture(t)
	defer cleanup()
	gctx := grantedConsolidationCtx(ctx)

	human, _ := seedSettledChat(t, tool, "", "chat", "alice")
	own, _ := seedSettledChat(t, tool, "", "qa-agent", "alice") // the fixture ctx's agent name
	extractor, _ := seedSettledChat(t, tool, "", "memory/extractor", "alice")

	got := scanIDs(runScan(t, tool, gctx, `{"op":"cursor_scan","scope":"user"}`))

	if contains(got, own) {
		t.Errorf("cursor_scan returned the scanning agent's own session %s: %v", own, got)
	}
	if !contains(got, human) {
		t.Errorf("cursor_scan dropped the real chat %s: %v", human, got)
	}
	// Without a config nothing is declared internal, so the extractor session is
	// visible — the documented pre-wiring behaviour, asserted so a future change
	// that hardcodes agent names somewhere shows up here.
	if !contains(got, extractor) {
		t.Errorf("with no Cfg the extractor session %s should still be visible: %v", extractor, got)
	}
}
