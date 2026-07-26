package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// The driver used to ADVERTISE a context window it never REQUESTED: with no
// operator num_ctx it reported whatever /api/ps said (feeding the UI gauge and
// autocompact_at_pct, which is a percentage of that number) while omitting
// options.num_ctx entirely, so Ollama served its own default and silently
// truncated anything past it. These tests pin the two numbers together.

// chatDone is a minimal one-frame NDJSON reply carrying usage counters, so the
// done frame stamps usage.MaxContextTokens.
const chatDone = `{"model":"m","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":2}` + "\n"

// numCtxServer serves psBody on /api/ps and a canned chat stream elsewhere,
// capturing the chat request body and counting /api/ps hits.
func numCtxServer(psBody string, captured *[]byte, psHits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			atomic.AddInt32(psHits, 1)
			fmt.Fprint(w, psBody)
			return
		}
		b, _ := io.ReadAll(r.Body)
		*captured = b
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, chatDone)
	}))
}

// callOnce drives one Call to completion and returns the usage from the done
// event (nil if none arrived).
func callOnce(t *testing.T, d *Driver, model string) *providers.Usage {
	t.Helper()
	ch, err := d.Call(context.Background(), providers.Request{
		Model:    model,
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var u *providers.Usage
	for ev := range ch {
		if ev.Type == providers.EventDone {
			u = ev.Usage
		}
	}
	if u == nil {
		t.Fatal("no usage on the done event")
	}
	return u
}

// wireNumCtx extracts options.num_ctx from a captured chat body. ok=false when
// the field was omitted entirely (the "let Ollama decide" case).
func wireNumCtx(t *testing.T, body []byte) (int, bool) {
	t.Helper()
	var w struct {
		Options *struct {
			NumCtx *int `json:"num_ctx"`
		} `json:"options"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal request body: %v\n%s", err, string(body))
	}
	if w.Options == nil || w.Options.NumCtx == nil {
		return 0, false
	}
	return *w.Options.NumCtx, true
}

// TestNumCtx_LoadedWindowIsRequestedNotJustAdvertised is the headline
// regression. With no operator pin and /api/ps reporting a loaded window, that
// window must reach BOTH the wire and the usage event — asserted as one
// equality so a future change cannot re-open the gap by moving one of them.
func TestNumCtx_LoadedWindowIsRequestedNotJustAdvertised(t *testing.T) {
	const loaded = 65535 // the live-observed value; deliberately not a round number
	var captured []byte
	var psHits int32
	srv := numCtxServer(fmt.Sprintf(`{"models":[{"name":"glm-4.7-flash","context_length":%d}]}`, loaded), &captured, &psHits)
	defer srv.Close()

	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)
	usage := callOnce(t, d, "glm-4.7-flash")

	sent, ok := wireNumCtx(t, captured)
	if !ok {
		t.Fatalf("no options.num_ctx on the wire — the window is advertised but never requested; body:\n%s", string(captured))
	}
	if sent != usage.MaxContextTokens {
		t.Fatalf("requested num_ctx = %d but advertised MaxContextTokens = %d — the two must be one number", sent, usage.MaxContextTokens)
	}
	if sent != loaded {
		t.Errorf("num_ctx = %d, want %d (the window /api/ps reports loaded)", sent, loaded)
	}
}

// TestNumCtx_OperatorPinWinsAndSkipsProbe pins that an explicit num_ctx is
// authoritative on both sides and that /api/ps is never consulted for it — the
// pin is exact, and a probe could only contradict it. Also the escape hatch: an
// operator who does not want the loaded window names one here.
func TestNumCtx_OperatorPinWinsAndSkipsProbe(t *testing.T) {
	var captured []byte
	var psHits int32
	srv := numCtxServer(`{"models":[{"name":"glm-4.7-flash","context_length":65535}]}`, &captured, &psHits)
	defer srv.Close()

	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil).WithNumCtx(32768)
	usage := callOnce(t, d, "glm-4.7-flash")

	sent, ok := wireNumCtx(t, captured)
	if !ok {
		t.Fatalf("pinned num_ctx missing from the wire; body:\n%s", string(captured))
	}
	if sent != 32768 || usage.MaxContextTokens != 32768 {
		t.Errorf("pinned: sent %d / advertised %d, want 32768 for both", sent, usage.MaxContextTokens)
	}
	if n := atomic.LoadInt32(&psHits); n != 0 {
		t.Errorf("/api/ps probed %d times with an explicit num_ctx; want 0", n)
	}
	if got := d.Capabilities().MaxContextTokens; got != 32768 {
		t.Errorf("Capabilities().MaxContextTokens = %d, want 32768", got)
	}
}

// TestNumCtx_UnknownWindowAdvertisesOllamaDefault pins the third case: the
// model is not loaded, so we cannot know the window. We advertise Ollama's
// documented default — a floor we cannot under-deliver on — and deliberately
// send NOTHING. Sending 4096 here would force a small load on a deployment
// whose Modelfile (or Ollama's own sizing) would have picked something bigger,
// and the next turn would read that shrunken value back from /api/ps and pin
// it. Advertising 0 was the old behaviour and reads as "no cap" downstream.
func TestNumCtx_UnknownWindowAdvertisesOllamaDefault(t *testing.T) {
	var captured []byte
	var psHits int32
	srv := numCtxServer(`{"models":[]}`, &captured, &psHits)
	defer srv.Close()

	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)
	usage := callOnce(t, d, "glm-4.7-flash")

	if _, ok := wireNumCtx(t, captured); ok {
		t.Errorf("options.num_ctx sent for an unknown window — that shrinks a Modelfile-sized load; body:\n%s", string(captured))
	}
	if usage.MaxContextTokens != defaultNumCtx {
		t.Errorf("advertised window = %d, want %d (Ollama's documented default)", usage.MaxContextTokens, defaultNumCtx)
	}
	if got := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil).Capabilities().MaxContextTokens; got != defaultNumCtx {
		t.Errorf("Capabilities().MaxContextTokens = %d, want %d", got, defaultNumCtx)
	}
}

// TestNumCtx_DifferentModelLoadedIsTreatedAsUnknown pins that the /api/ps
// lookup is name-matched: another model resident in VRAM says nothing about
// the window OUR model will get, so it must not be borrowed as ours.
func TestNumCtx_DifferentModelLoadedIsTreatedAsUnknown(t *testing.T) {
	var captured []byte
	var psHits int32
	srv := numCtxServer(`{"models":[{"name":"some-other-model:70b","context_length":131072}]}`, &captured, &psHits)
	defer srv.Close()

	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)
	usage := callOnce(t, d, "glm-4.7-flash")

	if n, ok := wireNumCtx(t, captured); ok {
		t.Errorf("num_ctx=%d sent from ANOTHER model's loaded window; body:\n%s", n, string(captured))
	}
	if usage.MaxContextTokens != defaultNumCtx {
		t.Errorf("advertised window = %d, want %d (another model's window is not ours)", usage.MaxContextTokens, defaultNumCtx)
	}
}

// captureLog redirects the standard logger for the duration of fn and returns
// what was written. The over-window diagnostic is a log line, so this is the
// only way to assert on it.
func captureLog(fn func()) string {
	var buf bytes.Buffer
	origOut, origFlags, origPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
		log.SetPrefix(origPrefix)
	}()
	fn()
	return buf.String()
}

// bigRequest builds a request whose prompt estimates to roughly wantTokens.
func bigRequest(model string, wantTokens int) providers.Request {
	return providers.Request{
		Model:    model,
		Messages: []providers.Message{{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: strings.Repeat("x", wantTokens*approxBytesPerToken)}}}},
	}
}

// TestNumCtx_OverWindowPromptWarnsWithActionableNumbers pins the loud
// diagnostic. The whole failure class is silence — Ollama drops the overflow
// and answers anyway — so an over-window prompt must name its size, the window
// it exceeded, where that window came from, and the knob that raises it.
func TestNumCtx_OverWindowPromptWarnsWithActionableNumbers(t *testing.T) {
	var captured []byte
	var psHits int32
	srv := numCtxServer(`{"models":[]}`, &captured, &psHits)
	defer srv.Close()

	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)
	out := captureLog(func() {
		ch, err := d.Call(context.Background(), bigRequest("glm-4.7-flash", 7104))
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		for range ch {
		}
	})

	for _, want := range []string{"7104", "4096", "assumed", "glm-4.7-flash", "LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX"} {
		if !strings.Contains(out, want) {
			t.Errorf("over-window warning missing %q; got:\n%s", want, out)
		}
	}
}

// TestNumCtx_InWindowPromptDoesNotWarn keeps the diagnostic from becoming
// noise: a prompt that fits — including one that fits only because /api/ps
// reported a big loaded window — logs nothing.
func TestNumCtx_InWindowPromptDoesNotWarn(t *testing.T) {
	var captured []byte
	var psHits int32
	srv := numCtxServer(`{"models":[{"name":"glm-4.7-flash","context_length":65535}]}`, &captured, &psHits)
	defer srv.Close()

	d := New("ollama-local", "", srv.URL, streamhttp.Options{}, nil)
	out := captureLog(func() {
		ch, err := d.Call(context.Background(), bigRequest("glm-4.7-flash", 7104))
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		for range ch {
		}
	})

	if strings.Contains(out, "exceeds") {
		t.Errorf("warned on a 7104-token prompt inside a 65535-token window:\n%s", out)
	}
}

// TestNumCtx_HostedProviderNamesItsOwnEnvVar pins that the diagnostic points at
// the knob for THIS registration — the hosted and local ollama providers are
// one driver with two env vars, and naming the wrong one sends the operator to
// a setting that will not take effect.
func TestNumCtx_HostedProviderNamesItsOwnEnvVar(t *testing.T) {
	var captured []byte
	var psHits int32
	srv := numCtxServer(`{"models":[]}`, &captured, &psHits)
	defer srv.Close()

	d := New("ollama", "", srv.URL, streamhttp.Options{}, nil)
	out := captureLog(func() {
		ch, err := d.Call(context.Background(), bigRequest("gpt-oss:120b", 9000))
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		for range ch {
		}
	})

	if !strings.Contains(out, "LOOMCYCLE_OLLAMA_NUM_CTX") || strings.Contains(out, "LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX") {
		t.Errorf("hosted ollama must name LOOMCYCLE_OLLAMA_NUM_CTX, not the local one; got:\n%s", out)
	}
}
