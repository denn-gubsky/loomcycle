package http

import (
	"context"
	"errors"
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
	"github.com/denn-gubsky/loomcycle/internal/runner"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// jsonAnswerProvider claims structured output and answers with a JSON object.
type jsonAnswerProvider struct{ last *providers.Request }

func (p *jsonAnswerProvider) ID() string                                   { return "json" }
func (p *jsonAnswerProvider) Probe(context.Context) error                  { return nil }
func (p *jsonAnswerProvider) ListModels(context.Context) ([]string, error) { return []string{"m"}, nil }
func (p *jsonAnswerProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsStructuredOutput: true}
}
func (p *jsonAnswerProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	r := req
	p.last = &r
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: `{"verdict":"ok"}`}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

func outputFormatServer(t *testing.T) (*Server, *httptest.Server, *jsonAnswerProvider, *storesqlite.Store) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {Model: "stub-model", SystemPrompt: "hi"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &jsonAnswerProvider{}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "outputformat.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, 100*time.Millisecond), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, ts, prov, st
}

// A per-run output_format reaches the provider, is persisted with the run so a
// resume keeps it, and the parsed answer lands in runs.result as `structured`
// beside the text it came from.
func TestHandleRuns_PerRunOutputFormatReachesTheProviderAndTheResult(t *testing.T) {
	_, ts, prov, st := outputFormatServer(t)
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"agent","output_format":{"type":"json_schema","name":"verdict","schema":{"type":"object","properties":{"verdict":{"type":"string"}}}},`+
			`"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if prov.last == nil || prov.last.OutputFormat == nil || prov.last.OutputFormat.Name != "verdict" {
		t.Fatalf("provider saw output_format %+v, want the run's", prov.last)
	}
	run := onlyRun(t, st, extractSessionID(string(body)))
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.OutputFormat == nil || rec.OutputFormat.Name != "verdict" {
		t.Errorf("persisted output_format = %+v, want the run's", rec.OutputFormat)
	}
	res := readResult(t, st, run.ID)
	if res.Structured["verdict"] != "ok" || res.FinalText != `{"verdict":"ok"}` {
		t.Errorf("result = %+v, want the text and its parsed object", res)
	}
}

// A schema the providers would refuse is a 400 at intake on every entry path.
func TestRunIntake_RefusesAnInvalidOutputFormat(t *testing.T) {
	srv, ts, prov, _ := outputFormatServer(t)
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"agent","output_format":{"type":"json_schema","schema":{"type":"array"}},"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "object") {
		t.Errorf("POST /v1/runs = %d %s, want a 400 naming why", resp.StatusCode, body)
	}
	if prov.last != nil {
		t.Error("the provider was called for a refused run")
	}
	err = srv.RunOnce(context.Background(), runner.RunInput{
		Agent:        "agent",
		OutputFormat: &config.OutputFormat{Type: "json_object"},
	}, runner.RunCallbacks{})
	if !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("RunOnce = %v, want ErrInvalidArgument", err)
	}
}
