package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// THE CROSSING for tool_choice: a retune writes the run's record, and the NEXT
// model call must carry the new choice. Stored-but-not-sent is the failure a
// record-only test cannot see.
//
// And its `until` counts from the adoption, not from the run's start: this run
// made no call under the new choice before, so first_call must force exactly
// the next call and then let go.
func TestRetune_ToolChoiceIsSentOnTheNextModelCallThenSpent(t *testing.T) {
	_, ts, prov, run := parkedRoutedRun(t)

	if code, b := postRetune(t, ts, run.ID, `{"tool_choice":{"mode":"none"}}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}
	if code, b := postInput(t, ts, run.ID, `{"text":"answer without tools"}`); code != 200 {
		t.Fatalf("input: %d %s", code, strings.TrimSpace(b))
	}
	first := prov.waitForRequests(t, 1)[0]
	if first.ToolChoice.Mode != config.ToolChoiceModeNone {
		t.Fatalf("the call after the retune sent tool_choice %+v, want mode none — the run "+
			"kept the choice it started with", first.ToolChoice)
	}

	if code, b := postInput(t, ts, run.ID, `{"text":"and now freely"}`); code != 200 {
		t.Fatalf("second input: %d %s", code, strings.TrimSpace(b))
	}
	second := prov.waitForRequests(t, 2)[1]
	if second.ToolChoice != (providers.ToolChoice{}) {
		t.Errorf("the second call still sent %+v; a first_call choice is spent by the call "+
			"after it was adopted", second.ToolChoice)
	}
}

// output_format reaches the next call too. The scripted provider cannot enforce
// a schema, so the schema must arrive the way it does for such a provider at
// start: in the system prompt.
func TestRetune_OutputFormatReachesTheNextModelCall(t *testing.T) {
	_, ts, prov, run := parkedRoutedRun(t)

	body := `{"output_format":{"name":"city","schema":{"type":"object","properties":{"city":{"type":"string"}}}}}`
	if code, b := postRetune(t, ts, run.ID, body); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}
	if code, b := postInput(t, ts, run.ID, `{"text":"which city?"}`); code != 200 {
		t.Fatalf("input: %d %s", code, strings.TrimSpace(b))
	}
	got := prov.waitForRequests(t, 1)[0]
	var sys strings.Builder
	for _, b := range got.System {
		sys.WriteString(b.Text)
	}
	if !strings.Contains(sys.String(), `"city"`) || !strings.Contains(sys.String(), "JSON Schema") {
		t.Errorf("the call after the retune carries no schema in its system prompt:\n%s", sys.String())
	}
}

// The start-time validators, at retune time: a block a run start would refuse
// is a 400 and changes nothing — including `{}`, which must not quietly read as
// "remove the output format".
func TestRetune_AShapeARunStartWouldRefuseIsRefused(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)

	for _, body := range []string{
		`{"tool_choice":{"mode":"tool"}}`,
		`{"tool_choice":{"mode":"required","until":"always"}}`,
		`{"output_format":{}}`,
		`{"output_format":{"schema":{"type":"array"}}}`,
	} {
		if code, b := postRetune(t, ts, run.ID, body); code != 400 {
			t.Errorf("%s → %d %s, want 400", body, code, strings.TrimSpace(b))
		}
	}
	rec, _ := decodeRunConfig(mustGetRun(t, srv.store, run.ID).RunConfig)
	if rec.ToolChoice != nil || rec.OutputFormat != nil {
		t.Errorf("a refused retune changed the record: tool_choice=%+v output_format=%+v",
			rec.ToolChoice, rec.OutputFormat)
	}
}

// mode auto REMOVES a forced choice: it is stored as absent, which is how every
// reader of the record already spells "not forced".
func TestRetune_ToolChoiceAutoRemovesTheForcing(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)

	if code, b := postRetune(t, ts, run.ID, `{"tool_choice":{"mode":"required"}}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}
	if rec, _ := decodeRunConfig(mustGetRun(t, srv.store, run.ID).RunConfig); rec.ToolChoice == nil {
		t.Fatal("the forced choice was not recorded")
	}
	if code, b := postRetune(t, ts, run.ID, `{"tool_choice":{"mode":"auto"}}`); code != 200 {
		t.Fatalf("retune to auto: %d %s", code, strings.TrimSpace(b))
	}
	if rec, _ := decodeRunConfig(mustGetRun(t, srv.store, run.ID).RunConfig); rec.ToolChoice != nil {
		t.Errorf("mode auto left %+v on the record", rec.ToolChoice)
	}
}

// The hook reports a field changed only when the RECORD moved from what the
// loop holds: an unrelated retune must not restart a tool_choice whose `until`
// is part spent, and a schema from yaml (ints) must equal the same schema read
// back from the record (float64s).
func TestReReadShape_ReportsOnlyWhatTheRecordChanged(t *testing.T) {
	srv, ts, _, run := parkedRoutedRun(t)
	ctx := context.Background()
	startTC := &config.ToolChoice{Mode: "required", Until: "until_called"}
	startOF := &config.OutputFormat{Schema: map[string]any{"type": "object", "maxProperties": 3}}
	seed := runConfigRecord{ToolChoice: startTC, OutputFormat: startOF}
	if err := srv.store.SetRunConfig(ctx, run.ID, seed.marshal()); err != nil {
		t.Fatal(err)
	}
	read := srv.reReadShapeOnOperatorTurnFn(run.ID, startTC, startOF)

	if got, err := read(ctx); err != nil || got.ToolChoiceChanged || got.OutputFormatChanged {
		t.Fatalf("an unchanged record reported %+v (err %v)", got, err)
	}
	if code, b := postRetune(t, ts, run.ID, `{"max_tokens":999}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}
	if got, _ := read(ctx); got.ToolChoiceChanged || got.OutputFormatChanged {
		t.Errorf("a budget-only retune reported %+v, which would restart the tool_choice", got)
	}
	if code, b := postRetune(t, ts, run.ID, `{"tool_choice":{"mode":"none"}}`); code != 200 {
		t.Fatalf("retune: %d %s", code, strings.TrimSpace(b))
	}
	got, _ := read(ctx)
	if !got.ToolChoiceChanged || got.ToolChoice == nil || got.ToolChoice.Mode != "none" || got.OutputFormatChanged {
		t.Errorf("after a tool_choice retune the hook reported %+v", got)
	}
	// Adopted once: the next read is quiet again.
	if again, _ := read(ctx); again.ToolChoiceChanged {
		t.Error("the same change was reported twice")
	}
}
