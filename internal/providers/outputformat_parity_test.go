package providers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// ⚠️ DERIVED FROM THE CLAIM, NOT FROM A LIST OF DRIVERS — the same reasoning
// as TestToolChoice_EveryDriverThatClaimsItSendsIt. The loop sends a schema
// only to a target that says it enforces one, so a driver that claims it and
// maps nothing silently hands back an unchecked answer the run believes was
// held to the schema. Every registered driver is asked; the ones saying yes
// must put the schema on the wire through Call().
func TestOutputFormat_EveryDriverThatClaimsItSendsTheSchema(t *testing.T) {
	cap := newCaptureServer(t)
	req := providers.Request{
		Model:     "test-model",
		Messages:  []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 1024,
	}
	const marker = "schema_marker_property"
	format := &providers.OutputFormat{Name: "answer", Schema: []byte(
		`{"type":"object","properties":{"` + marker + `":{"type":"string"}},"required":["` + marker + `"]}`)}

	checked := 0
	for _, name := range append(providers.RegisteredDrivers(), "ollama-local") {
		driver, id := name, name
		if name == "ollama-local" {
			driver = "ollama" // the self-hosted registration of the same driver
		}
		p, err := providers.NewDriver(driver, providers.DriverOptions{ID: id, BaseURL: cap.srv.URL, APIKey: "test-key"})
		if err != nil {
			t.Logf("%s: not constructible here (%v) — skipped", name, err)
			continue
		}
		if !providers.EnforcesStructuredOutput(p, req.Model, false) {
			continue
		}
		checked++
		ch, err := p.Call(context.Background(), req)
		if err == nil {
			for range ch { //nolint:revive // drain; the stub reply is unparseable by design
			}
		}
		before := cap.lastBody()

		withFormat := req
		withFormat.OutputFormat = format
		ch, err = p.Call(context.Background(), withFormat)
		if err == nil {
			for range ch { //nolint:revive // drain
			}
		}
		after := cap.lastBody()
		if before == "" || after == "" {
			t.Errorf("%s: never reached the capture server, so this proves nothing", name)
			continue
		}
		if before == after || !strings.Contains(after, marker) {
			t.Errorf("%s claims to enforce output_format but the schema is not on the wire.\nbody: %s", name, after)
		}
	}
	if checked < 4 {
		t.Fatalf("only %d driver(s) claimed structured output — expected at least anthropic, openai, "+
			"gemini and ollama-local, so this guard is reading almost nothing", checked)
	}
}

// The other half: the hosted Ollama and DeepSeek have no schema parameter, so
// they must say so — and a request that carries one anyway must go out
// without it rather than as a field the API refuses.
func TestOutputFormat_DriversWithoutItSayFalseAndSendNothing(t *testing.T) {
	cap := newCaptureServer(t)
	for _, c := range []struct{ driver, id, field string }{
		{"ollama", "ollama", `"format"`},
		{"deepseek", "deepseek", "response_format"},
	} {
		p, err := providers.NewDriver(c.driver, providers.DriverOptions{ID: c.id, BaseURL: cap.srv.URL, APIKey: "k"})
		if err != nil {
			t.Fatalf("NewDriver(%s): %v", c.driver, err)
		}
		if providers.EnforcesStructuredOutput(p, "deepseek-chat", false) {
			t.Errorf("%s claims structured output it cannot enforce", c.id)
		}
		ch, err := p.Call(context.Background(), providers.Request{
			Model:        "deepseek-chat",
			Messages:     []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
			OutputFormat: &providers.OutputFormat{Name: "a", Schema: []byte(`{"type":"object"}`)},
		})
		if err == nil {
			for range ch { //nolint:revive // drain
			}
		}
		if body := cap.lastBody(); strings.Contains(body, c.field) {
			t.Errorf("%s put %s on the wire: %s", c.id, c.field, body)
		}
	}
}
