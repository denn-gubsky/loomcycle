package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A Starter whose source is the walk's input, end to end: authored through
// TeamDef op=create, run through op=run over POST /v1/_teamdef, members are
// real runs against the recording stub provider.

// inputTeam researches the part named by the form it is started with.
const inputTeam = `{"entry":"research","states":[` +
	`{"state":"research","handler":{"kind":"starter",` +
	`"source":{"kind":"input"},` +
	`"schema":{"type":"object","required":["document_id","chunk_id"]},` +
	`"fanout":{"agent":"writer","per":"message","max":2},` +
	`"binds":{"chunk_id":"$.chunk_id"},` +
	`"prompt":{"input":"Research chunk ${var.chunk_id}. Item: {{starter.message}}"},` +
	`"sink":{"channel":"out"}}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"research","to":"done","on":"success"}],` +
	`"channels":{"publish":["out"]}}`

func TestTeamDefRun_InputStarterRunsOnceForAnObjectInput(t *testing.T) {
	h := newDocWalkHarness(t)
	body, _ := json.Marshal(map[string]any{"op": "create", "name": "parts", "overlay": json.RawMessage(inputTeam)})
	if code, out := h.post(string(body)); code != http.StatusOK || strings.Contains(out, `"tool_refused"`) {
		t.Fatalf("create = %d: %s", code, out)
	}

	run, _ := json.Marshal(map[string]any{"op": "run", "name": "parts", "input": `{"document_id":"d","chunk_id":"c"}`})
	code, out := h.post(string(run))
	if code != http.StatusOK || !strings.Contains(out, `"run_id"`) {
		t.Fatalf("run = %d: %s", code, out)
	}
	users := h.sent()
	if len(users) != 1 {
		t.Fatalf("the provider saw %d member turns, want exactly one:\n%s", len(users), strings.Join(users, "\n---\n"))
	}
	if want := `Research chunk c. Item: {"document_id":"d","chunk_id":"c"}`; users[0] != want {
		t.Errorf("member turn = %q, want %q", users[0], want)
	}
	msgs, err := h.st.ChannelPeek(context.Background(), "acme", "out", store.MemoryScopeTenant, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("the sink got %d messages, want one", len(msgs))
	}
}

// A promoted team whose entry Starter reads the walk's input is never armed:
// there is no channel to sweep, and an armed one would be driven with an empty
// source name every tick.
func TestListTeamSubscriptions_SkipsAnInputSourceEntry(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	seedTeam(t, srv, "channel-wave", subStarterGraph, true, false)
	seedTeam(t, srv, "input-wave", `{"entry":"wave","states":[`+
		`{"state":"wave","handler":{"kind":"starter","source":{"kind":"input"},`+
		`"fanout":{"agent":"reviewer","per":"message","max":4},"sink":{"channel":"c2"}}},`+
		`{"state":"done","handler":{"kind":"terminal"}}],`+
		`"transitions":[{"from":"wave","to":"done","on":"success"}],`+
		`"channels":{"publish":["c2"]}}`, true, false)

	subs, err := srv.listTeamSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := subNames(subs); len(got) != 1 || got[0] != "channel-wave" {
		t.Fatalf("subscriptions = %v, want only [channel-wave] — an input-source entry must not be armed", got)
	}
}
