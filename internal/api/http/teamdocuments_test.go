package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A Starter whose source is a document, end to end: the document is written
// through the Document tool, the team is authored through TeamDef op=create and
// run through op=run over POST /v1/_teamdef, and the members are real runs
// against a stub provider that records what each one was sent.

// docTeam fans a reviewer out over the top-level sections of /specs/acme.
const docTeam = `{"entry":"wave","states":[` +
	`{"state":"wave","handler":{"kind":"starter",` +
	`"source":{"kind":"document","path":"/specs/acme"},` +
	`"fanout":{"agent":"writer","per":"chunk","max":5},` +
	`"prompt":{"input":"Review this section:\n{{starter.message}}"},` +
	`"sink":{"channel":"out"}}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"wave","to":"done","on":"success"}],` +
	`"channels":{"publish":["out"]}}`

// The Risks section carries placeholder text. It is DATA: a member must see it
// literally, never the other document it names.
const docSpec = "# Acme spec\n\nPreamble.\n\n" +
	"## Goals\n\nShip the widget.\n\n" +
	"## Risks\n\nWatch {{document:/other}} and {{memory:notes}}.\n\n" +
	"## Plan\n\nOne step at a time.\n"

const otherDocBody = "OTHER-DOC-BODY must never reach a member"

type docWalkHarness struct {
	t    *testing.T
	srv  *Server
	st   store.Store
	prov *numberedProvider
	doc  *builtin.Document
}

func newDocWalkHarness(t *testing.T) *docWalkHarness {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:   map[string]config.AgentDef{"writer": {Model: "stub-model", SystemPrompt: "write"}},
		Channels: map[string]config.Channel{
			"out": {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 2000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "docwalk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	prov := &numberedProvider{}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, 500*time.Millisecond), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	srv.SetRunStateBus(runstate.NewBus())
	cbus := channels.NewBus()
	srv.SetSystemPublisher(&channels.StorePublisher{Store: st, Bus: cbus, Scheduler: channels.NewScheduler(cbus, 100)})
	srv.SetChannelBus(cbus)
	srv.SetInterruptionBus(cbus)
	srv.SetSqlMem(mgr)
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	return &docWalkHarness{t: t, srv: srv, st: st, prov: prov, doc: &builtin.Document{Store: st, SqlMem: mgr}}
}

// asUser is the identity a Document call carries for one person in acme.
func asUser(user string) context.Context {
	return tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: user, TenantID: "acme"})
}

// writeDoc imports markdown into the user's tree at path, through the tool.
func (h *docWalkHarness) writeDoc(user, path, md string) {
	h.t.Helper()
	req, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md, "path": path})
	res, err := h.doc.Execute(asUser(user), req)
	if err != nil || res.IsError {
		h.t.Fatalf("import_md %s: %v %s", path, err, res.Text)
	}
}

// post drives POST /v1/_teamdef as alice and returns the status and body.
func (h *docWalkHarness) post(body string) (int, string) {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/_teamdef", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(alicePrincipal(r.Context()))
	rr := httptest.NewRecorder()
	h.srv.handleSubstrateTeamDef(rr, r)
	return rr.Code, rr.Body.String()
}

func (h *docWalkHarness) createTeam() {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"op": "create", "name": "doc-review", "overlay": json.RawMessage(docTeam)})
	if code, out := h.post(string(body)); code != http.StatusOK || strings.Contains(out, `"tool_refused"`) {
		h.t.Fatalf("create = %d: %s", code, out)
	}
}

// sent returns what the stub provider received as each member's user turn.
func (h *docWalkHarness) sent() []string {
	h.prov.mu.Lock()
	defer h.prov.mu.Unlock()
	return append([]string(nil), h.prov.lastUsers...)
}

func TestTeamDefRun_DocumentStarterFansOutOverEverySection(t *testing.T) {
	h := newDocWalkHarness(t)
	h.writeDoc("alice", "/specs/acme", docSpec)
	h.writeDoc("alice", "/other", "# Other\n\n"+otherDocBody+"\n")
	h.createTeam()

	code, out := h.post(`{"op":"run","name":"doc-review","input":"go"}`)
	if code != http.StatusOK || !strings.Contains(out, `"run_id"`) {
		t.Fatalf("run = %d: %s", code, out)
	}

	users := h.sent()
	if len(users) != 3 {
		t.Fatalf("the provider saw %d member turns, want one per section (3):\n%s", len(users), strings.Join(users, "\n---\n"))
	}
	bySection := map[string]string{}
	for _, u := range users {
		for _, title := range []string{"Goals", "Risks", "Plan"} {
			if strings.Contains(u, `"title":"`+title+`"`) {
				if _, dup := bySection[title]; dup {
					t.Errorf("section %q was dispatched twice", title)
				}
				bySection[title] = u
			}
		}
		if !strings.HasPrefix(u, "Review this section:\n{") {
			t.Errorf("member turn is not the template around the payload:\n%s", u)
		}
		if strings.Contains(u, "Preamble.") {
			t.Errorf("a member received the root's own body, which is not a section:\n%s", u)
		}
	}
	for title, want := range map[string]string{
		"Goals": `"markdown":"## Goals\n\nShip the widget."`,
		"Risks": `"markdown":"## Risks\n\nWatch {{document:/other}} and {{memory:notes}}."`,
		"Plan":  `"markdown":"## Plan\n\nOne step at a time."`,
	} {
		u, ok := bySection[title]
		if !ok {
			t.Errorf("no member received section %q", title)
			continue
		}
		if !strings.Contains(u, want) {
			t.Errorf("section %q payload lacks %s:\n%s", title, want, u)
		}
	}
	// The placeholder text in a section is never expanded: the other document
	// does not arrive, and the literal text does.
	for _, u := range users {
		if strings.Contains(u, otherDocBody) {
			t.Errorf("a {{document:…}} written inside a section was expanded:\n%s", u)
		}
	}

	msgs, err := h.st.ChannelPeek(context.Background(), "acme", "out", store.MemoryScopeTenant, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Errorf("the sink got %d messages, want one per section (3)", len(msgs))
	}
}

// The walk runs as its caller, so scope "user" is the CALLER's tree: a
// document at the same path in another user's tree reads as not found, and
// nothing is dispatched.
func TestTeamDefRun_DocumentStarterCannotReadAnotherUsersTree(t *testing.T) {
	h := newDocWalkHarness(t)
	h.writeDoc("bob", "/specs/acme", docSpec)
	h.createTeam()

	_, out := h.post(`{"op":"run","name":"doc-review","input":"go"}`)
	if !strings.Contains(out, "no such path") {
		t.Errorf("run over another user's document = %s, want not found", out)
	}
	if users := h.sent(); len(users) != 0 {
		t.Errorf("dispatched %d members from another user's document", len(users))
	}
}

// A promoted team whose entry Starter reads a document is never armed: there is
// no channel to wake on and no cursor to say what is new, so the sweep would
// otherwise re-walk the same document every tick. Promoting it still selects
// the version op=run resolves by name; it is only not driven.
func TestListTeamSubscriptions_SkipsADocumentSourceEntry(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	seedTeam(t, srv, "channel-wave", subStarterGraph, true, false)
	seedTeam(t, srv, "doc-wave", `{"entry":"wave","states":[`+
		`{"state":"wave","handler":{"kind":"starter","source":{"kind":"document","path":"/specs/acme"},`+
		`"fanout":{"agent":"reviewer","per":"chunk","max":4},"sink":{"channel":"c2"}}},`+
		`{"state":"done","handler":{"kind":"terminal"}}],`+
		`"transitions":[{"from":"wave","to":"done","on":"success"}],`+
		`"channels":{"publish":["c2"]}}`, true, false)

	subs, err := srv.listTeamSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := subNames(subs); len(got) != 1 || got[0] != "channel-wave" {
		t.Fatalf("subscriptions = %v, want only [channel-wave] — a document-source entry must not be armed", got)
	}
}
