package http

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// historyProvider answers every run turn with "ok" and every recap with
// "RECAP TEXT", keeping what each run turn was sent. recapGate, when set,
// holds a recap until it is closed.
type historyProvider struct {
	mu        sync.Mutex
	turns     []string
	recaps    int
	recapGate chan struct{}
}

func (p *historyProvider) ID() string                                   { return "scripted" }
func (p *historyProvider) Probe(context.Context) error                  { return nil }
func (p *historyProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *historyProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *historyProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	recap := len(req.System) > 0 && strings.Contains(req.System[0].Text, "two sentences")
	reply := "ok"
	if recap {
		p.mu.Lock()
		p.recaps++
		gate := p.recapGate
		p.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		reply = "RECAP TEXT"
	} else {
		var b strings.Builder
		for _, m := range req.Messages {
			for _, c := range m.Content {
				b.WriteString(c.Text)
				b.WriteString("\n")
			}
		}
		p.mu.Lock()
		p.turns = append(p.turns, b.String())
		p.mu.Unlock()
	}
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: reply}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

func (p *historyProvider) lastTurn(t *testing.T) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turns) == 0 {
		t.Fatal("the provider was never called for a run turn")
	}
	return p.turns[len(p.turns)-1]
}

func (p *historyProvider) recapCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.recaps
}

// historyFixture is a server with three agents: "chat" and "other" preload
// history, "plain" does not. hp overrides chat's block.
func historyFixture(t *testing.T, hp *config.HistoryPreload) (*Server, *historyProvider, string) {
	t.Helper()
	if hp == nil {
		hp = &config.HistoryPreload{}
	}
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"chat":  {Provider: "scripted", Model: "stub-model", Tools: []string{}, HistoryPreload: hp},
			"other": {Provider: "scripted", Model: "stub-model", Tools: []string{}, HistoryPreload: &config.HistoryPreload{}},
			"plain": {Provider: "scripted", Model: "stub-model", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8},
	}
	cfg.Env.AuthToken = ""
	prov := &historyProvider{}
	srv, dbPath := makeServer(t, prov, cfg)
	return srv, prov, dbPath
}

var seedSeq int

// seedChat stores a finished chat of agent for (tenant, user) with one
// exchange per turn pair, and its title and summary when given.
func seedPreloadChat(t *testing.T, srv *Server, tenant, user, agent, title, summary string, turns ...string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, tenant, agent, user)
	if err != nil {
		t.Fatal(err)
	}
	seedSeq++
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: fmt.Sprintf("seed_%d", seedSeq), UserID: user, TenantID: tenant, Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(turns); i += 2 {
		appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{{Role: "user",
			Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: turns[i]}}}})
		appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: turns[i+1]})
		appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
	}
	if err := srv.store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	patch := store.SessionMetaPatch{}
	if title != "" {
		patch.Title = &title
	}
	if summary != "" {
		patch.Summary = &summary
	}
	if patch.Title != nil || patch.Summary != nil {
		if err := srv.store.SetSessionMeta(ctx, sess.ID, patch); err != nil {
			t.Fatal(err)
		}
	}
	return sess.ID
}

func runChat(t *testing.T, srv *Server, in runner.RunInput) {
	t.Helper()
	if in.Segments == nil {
		in.Segments = []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hello again"}}}}
	}
	if err := srv.RunOnce(context.Background(), in, runner.RunCallbacks{}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

// The newest session of (tenant, user, agent) — the one a run just made.
func newestSession(t *testing.T, srv *Server, tenant, user, agent string) string {
	t.Helper()
	rows, _, err := srv.store.ListSessions(context.Background(), store.SessionFilter{TenantID: tenant, UserID: user, AgentName: agent}, 50, 0)
	if err != nil || len(rows) == 0 {
		t.Fatalf("ListSessions: %v (%d rows)", err, len(rows))
	}
	newest := rows[0]
	for _, r := range rows {
		if r.CreatedAt.After(newest.CreatedAt) {
			newest = r
		}
	}
	return newest.SessionID
}

func TestHistoryPreload_NewSessionSeesOnlyTheSameUsersChatsWithTheSameAgent(t *testing.T) {
	srv, prov, _ := historyFixture(t, nil)
	seedPreloadChat(t, srv, "", "alice", "chat", "Passport review", "Checked the passport draft for gaps.", "q", "a")
	seedPreloadChat(t, srv, "", "bob", "chat", "Bob's private chat", "Bob secret summary.", "q", "a")
	seedPreloadChat(t, srv, "", "alice", "other", "Other agent chat", "Another agent's summary.", "q", "a")
	seedPreloadChat(t, srv, "acme", "alice", "chat", "Another tenant chat", "Tenant acme summary.", "q", "a")

	runChat(t, srv, runner.RunInput{Agent: "chat", UserID: "alice"})

	got := prov.lastTurn(t)
	for _, want := range []string{recentHistoryKind, "Passport review", "Checked the passport draft for gaps."} {
		if !strings.Contains(got, want) {
			t.Errorf("the new chat's first turn lacks %q:\n%s", want, got)
		}
	}
	for _, leak := range []string{"Bob", "Other agent chat", "Another agent's summary", "Another tenant chat", "Tenant acme"} {
		if strings.Contains(got, leak) {
			t.Errorf("the preload carries %q, which is not this user's chat with this agent:\n%s", leak, got)
		}
	}
}

func TestHistoryPreload_ContinuationIsNotPreloaded(t *testing.T) {
	srv, prov, _ := historyFixture(t, nil)
	seedPreloadChat(t, srv, "", "alice", "chat", "Passport review", "Checked the passport draft.", "q", "a")
	runChat(t, srv, runner.RunInput{Agent: "chat", UserID: "alice"})
	if !strings.Contains(prov.lastTurn(t), "Passport review") {
		t.Fatal("precondition: the new chat was not preloaded")
	}
	sessID := newestSession(t, srv, "", "alice", "chat")
	runChat(t, srv, runner.RunInput{Agent: "chat", UserID: "alice", SessionID: sessID})

	if got := prov.lastTurn(t); strings.Contains(got, "Passport review") || strings.Contains(got, recentHistoryKind) {
		t.Errorf("a continuation was preloaded (or the preload was stored into the chat):\n%s", got)
	}
}

func TestHistoryPreload_ChatWithoutSummaryIsListedByTitle(t *testing.T) {
	srv, prov, _ := historyFixture(t, nil)
	id := seedPreloadChat(t, srv, "", "alice", "chat", "Untouched chat", "", "q", "a")
	runChat(t, srv, runner.RunInput{Agent: "chat", UserID: "alice"})
	got := prov.lastTurn(t)
	if !strings.Contains(got, "Untouched chat") || !strings.Contains(got, id) {
		t.Fatalf("the summary-less chat is not listed by title and id:\n%s", got)
	}
	if strings.Contains(got, "summary:") {
		t.Errorf("a chat with no stored summary was given one:\n%s", got)
	}
}

func TestHistoryPreload_ChatOutsideTheWindowIsLeftOut(t *testing.T) {
	srv, prov, dbPath := historyFixture(t, &config.HistoryPreload{Within: "7d"})
	old := seedPreloadChat(t, srv, "", "alice", "chat", "Ancient chat", "Long ago.", "q", "a")
	seedPreloadChat(t, srv, "", "alice", "chat", "Recent chat", "Yesterday.", "q", "a")

	// Age the old chat to ten days ago: its session, run start and run end.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tenDays := time.Now().Add(-10 * 24 * time.Hour).UnixNano()
	if _, err := db.Exec(`UPDATE runs SET started_at = ?, completed_at = ? WHERE session_id = ?`, tenDays, tenDays, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, tenDays, old); err != nil {
		t.Fatal(err)
	}

	runChat(t, srv, runner.RunInput{Agent: "chat", UserID: "alice"})
	got := prov.lastTurn(t)
	if !strings.Contains(got, "Recent chat") {
		t.Fatalf("the chat inside the window is missing:\n%s", got)
	}
	if strings.Contains(got, "Ancient chat") {
		t.Errorf("a chat last active ten days ago was listed under within: 7d:\n%s", got)
	}
}

func TestHistoryPreload_PerRunOffAndAnAgentWithoutTheBlockGetNone(t *testing.T) {
	srv, prov, _ := historyFixture(t, nil)
	seedPreloadChat(t, srv, "", "alice", "chat", "Passport review", "Checked it.", "q", "a")
	seedPreloadChat(t, srv, "", "alice", "plain", "Plain chat", "Plain summary.", "q", "a")

	runChat(t, srv, runner.RunInput{Agent: "chat", UserID: "alice", HistoryPreload: runner.HistoryPreloadOff})
	if got := prov.lastTurn(t); strings.Contains(got, "Passport review") || strings.Contains(got, recentHistoryKind) {
		t.Errorf("history_preload: off still preloaded:\n%s", got)
	}
	runChat(t, srv, runner.RunInput{Agent: "plain", UserID: "alice"})
	if got := prov.lastTurn(t); strings.Contains(got, "Plain chat") || strings.Contains(got, recentHistoryKind) {
		t.Errorf("an agent without history_preload was preloaded:\n%s", got)
	}
}

func TestHistoryPreload_RunWithoutAUserIsNotPreloaded(t *testing.T) {
	srv, prov, _ := historyFixture(t, nil)
	seedPreloadChat(t, srv, "", "", "chat", "Nobody's chat", "No user.", "q", "a")
	runChat(t, srv, runner.RunInput{Agent: "chat"})
	if got := prov.lastTurn(t); strings.Contains(got, recentHistoryKind) {
		t.Errorf("a run with no user was preloaded:\n%s", got)
	}
}

func TestHistoryPreload_SummaryTailAddsTheNewestChatsLastTurns(t *testing.T) {
	srv, prov, _ := historyFixture(t, &config.HistoryPreload{Include: config.HistoryPreloadIncludeSummaryTail, TailTurns: 2})
	seedPreloadChat(t, srv, "", "alice", "chat", "Newest", "N.", "first question", "first answer", "last question", "last answer")
	runChat(t, srv, runner.RunInput{Agent: "chat", UserID: "alice"})
	got := prov.lastTurn(t)
	if !strings.Contains(got, "last question") || !strings.Contains(got, "last answer") {
		t.Fatalf("the newest chat's last two turns are missing:\n%s", got)
	}
	if strings.Contains(got, "first question") {
		t.Errorf("tail_turns: 2 carried an older turn:\n%s", got)
	}
}

func TestRenderRecentHistory_StaysWithinMaxTokensKeepingTheNewest(t *testing.T) {
	now := time.Now()
	var chats []store.SessionSummary
	for i := 0; i < 5; i++ {
		chats = append(chats, store.SessionSummary{SessionID: fmt.Sprintf("s%d", i), Title: fmt.Sprintf("chat number %d", i),
			Summary: strings.Repeat("summary words ", 10), LastActivity: now.Add(-time.Duration(i) * time.Hour)})
	}
	tail := []builtin.ConversationTurn{{Speaker: "user", Text: strings.Repeat("я", 400)}}
	const budget = 100 * 4 // max_tokens: 100
	got := renderRecentHistory(chats, tail, budget)
	if len(got) > budget {
		t.Errorf("rendered %d bytes, over the %d-byte budget", len(got), budget)
	}
	if !strings.Contains(got, "chat number 0") {
		t.Errorf("the newest chat was cut:\n%s", got)
	}
	if strings.Contains(got, "chat number 4") {
		t.Errorf("the oldest chat fit a budget too small for all five:\n%s", got)
	}

	// A newest turn longer than the room left is cut, on a character boundary.
	got = renderRecentHistory(chats[:1], tail, budget)
	if len(got) > budget || !utf8.ValidString(got) {
		t.Errorf("tail cut: %d bytes (budget %d), valid UTF-8 %v", len(got), budget, utf8.ValidString(got))
	}
	if !strings.Contains(got, "user: я") {
		t.Errorf("the newest turn was dropped instead of cut:\n%s", got)
	}
}

func TestHistoryRecap_RunEndRecapsTheChatInTheBackgroundOnlyForAPreloadingAgent(t *testing.T) {
	srv, prov, _ := historyFixture(t, nil)
	prov.recapGate = make(chan struct{})

	// RunOnce returns while the recap is still held: it is not on the run's path.
	runChat(t, srv, runner.RunInput{Agent: "chat", UserID: "alice"})
	sessID := newestSession(t, srv, "", "alice", "chat")
	deadline := time.Now().Add(10 * time.Second)
	for prov.recapCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if prov.recapCount() != 1 {
		t.Fatalf("recaps started = %d, want 1 after the run ended", prov.recapCount())
	}
	sess, err := srv.store.GetSession(context.Background(), sessID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Summary != "" {
		t.Fatalf("the summary was written before the held recap finished: %q", sess.Summary)
	}
	close(prov.recapGate)
	waitDetached(t, srv)
	if sess, _ = srv.store.GetSession(context.Background(), sessID); sess.Summary != "RECAP TEXT" {
		t.Errorf("summary after the run = %q, want the recap", sess.Summary)
	}

	runChat(t, srv, runner.RunInput{Agent: "plain", UserID: "alice"})
	waitDetached(t, srv)
	if n := prov.recapCount(); n != 1 {
		t.Errorf("recaps = %d after a run of an agent without history_preload, want still 1", n)
	}
	plain := newestSession(t, srv, "", "alice", "plain")
	if sess, _ := srv.store.GetSession(context.Background(), plain); sess.Summary != "" {
		t.Errorf("an agent without history_preload had its chat recapped: %q", sess.Summary)
	}
}

func TestHistoryRecap_SkipsAChatWhoseSummaryIsNewerThanItsLastRun(t *testing.T) {
	srv, prov, _ := historyFixture(t, nil)
	id := seedPreloadChat(t, srv, "", "alice", "chat", "Done", "Already recapped.", "q", "a")
	if err := srv.recapIfStale(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if n := prov.recapCount(); n != 0 {
		t.Errorf("a chat recapped after its last run was summarised again (%d calls)", n)
	}
}

func waitDetached(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.WaitDetached(ctx); err != nil {
		t.Fatalf("background work did not finish: %v", err)
	}
}
