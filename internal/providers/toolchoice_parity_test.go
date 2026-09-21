package providers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// captureServer records the last request body any driver posted to it. The
// RESPONSE is deliberately useless — every driver will fail to parse it, and
// that is fine: the body was captured before the driver ever read a byte back,
// and it is the only thing under test here.
type captureServer struct {
	mu   sync.Mutex
	body string
	srv  *httptest.Server
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	c := &captureServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.body = string(b)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *captureServer) lastBody() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

// ⚠️ DERIVED FROM THE CAPABILITY, NOT FROM A LIST OF DRIVERS.
//
// The risk this guards is a driver that CLAIMS SupportsToolChoice and never
// maps it — the claim is what the loop and the fallback gate read, so a driver
// that lies is worse than one that says false. A hand-written list of "drivers
// that should support it" would be a second copy of the capability, and the
// copy is what drifts; instead every registered driver is asked what it
// supports, and only the ones saying yes are held to it.
//
// It asserts through Call() rather than through each package's private body
// builder, because that is the path a run takes: a driver could map ToolChoice
// in a helper nothing calls and still pass a unit test of the helper.
func TestToolChoice_EveryDriverThatClaimsItSendsIt(t *testing.T) {
	cap := newCaptureServer(t)

	req := providers.Request{
		Model:    "test-model",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []providers.ToolSpec{{Name: "emit_state", Description: "d",
			InputSchema: []byte(`{"type":"object"}`)}},
		MaxTokens: 1024,
	}

	checked := 0
	for _, name := range providers.RegisteredDrivers() {
		p, err := providers.NewDriver(name, providers.DriverOptions{
			ID: name, BaseURL: cap.srv.URL, APIKey: "test-key",
		})
		if err != nil {
			t.Logf("%s: not constructible here (%v) — skipped", name, err)
			continue
		}
		if !p.Capabilities().SupportsToolChoice {
			continue
		}
		checked++

		// Unforced first, so the comparison is against this driver's OWN
		// baseline rather than against an assumed body shape.
		unforced := req
		ch, err := p.Call(context.Background(), unforced)
		if err == nil {
			for range ch { //nolint:revive // drain; the stub reply is unparseable by design
			}
		}
		before := cap.lastBody()

		forced := req
		forced.ToolChoice = providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"}
		ch, err = p.Call(context.Background(), forced)
		if err == nil {
			for range ch { //nolint:revive // drain
			}
		}
		after := cap.lastBody()

		if before == "" || after == "" {
			t.Errorf("%s: never reached the capture server, so this proves nothing", name)
			continue
		}
		if before == after {
			t.Errorf("%s reports SupportsToolChoice=true but a forced choice changed NOTHING on the wire.\n"+
				"A driver that claims the capability and drops the field is worse than one that says "+
				"false: the loop and the fallback gate both read the claim.\nbody: %s", name, after)
		}
		if !strings.Contains(after, "emit_state") {
			t.Errorf("%s: forced body does not name the tool: %s", name, after)
		}
	}

	// Non-vacuity: if nothing claims the capability the loop above is a no-op.
	if checked < 3 {
		t.Fatalf("only %d driver(s) claimed SupportsToolChoice — expected at least anthropic, openai "+
			"and gemini, so this guard is reading almost nothing", checked)
	}
}

// The other half: a driver that says false must be USABLE, not refused. Ollama
// has no tool_choice on /api/chat or on its OpenAI shim, and the decision (RFC
// DG) is that a forced request against it degrades to unforced rather than
// failing — forcing is an optimisation of a contract the prompt already states.
func TestToolChoice_ADriverWithoutItStillAcceptsTheRequest(t *testing.T) {
	cap := newCaptureServer(t)
	p, err := providers.NewDriver("ollama", providers.DriverOptions{
		ID: "ollama-local", BaseURL: cap.srv.URL,
	})
	if err != nil {
		t.Fatalf("NewDriver(ollama): %v", err)
	}
	if p.Capabilities().SupportsToolChoice {
		t.Fatal("ollama must report SupportsToolChoice=false — the parameter does not exist on /api/chat")
	}

	ch, err := p.Call(context.Background(), providers.Request{
		Model:    "ornith-1.5:35b",
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []providers.ToolSpec{{Name: "emit_state", Description: "d",
			InputSchema: []byte(`{"type":"object"}`)}},
		ToolChoice: providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "emit_state"},
	})
	if err != nil {
		t.Fatalf("a forced request was REFUSED by a driver that cannot force: %v\n"+
			"Degrading is the documented behaviour; refusing would make every local-model "+
			"agent unrunnable to buy a guarantee it never had.", err)
	}
	for range ch { //nolint:revive // drain
	}
	if body := cap.lastBody(); strings.Contains(body, "tool_choice") {
		t.Errorf("ollama put a tool_choice on the wire; /api/chat has no such field: %s", body)
	}
}
