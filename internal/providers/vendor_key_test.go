package providers_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// keyHeaderSrv answers every request with a 400 and records the key headers
// each driver authenticates with, so a test sees what credential reached the
// endpoint whatever the driver's wire shape.
func keyHeaderSrv(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		for _, h := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
			if v := r.Header.Get(h); v != "" {
				seen = append(seen, h+": "+v)
			}
		}
		mu.Unlock()
		http.Error(w, `{"error":{"message":"stub"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// tenantKeyCtx stamps a credential resolver that holds a tenant key under
// EVERY name and records the names it was asked for.
func tenantKeyCtx() (context.Context, func() []string) {
	var mu sync.Mutex
	var asked []string
	ctx := providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool, error) {
		mu.Lock()
		asked = append(asked, name)
		mu.Unlock()
		return providers.CredentialResolution{Value: "tenant-vendor-key", Scope: "tenant", ScopeID: "t1"}, true, nil
	})
	return ctx, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

func chatReq() providers.Request {
	return providers.Request{Model: "m", MaxTokens: 16, Messages: []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
	}}
}

// vendorKeyed are the drivers whose built-in key name is a cloud vendor's key,
// with that name and the vendor's own API endpoint. The hosted ollama key is
// the "ollama" id's; the driver's other ids are keyless.
var vendorKeyed = []struct {
	driver, id, keyName, vendorURL string
}{
	{"anthropic", "anthropic", "ANTHROPIC_API_KEY", "https://api.anthropic.com"},
	{"openai", "openai", "OPENAI_API_KEY", "https://api.openai.com/v1"},
	{"deepseek", "deepseek", "DEEPSEEK_API_KEY", "https://api.deepseek.com/v1"},
	{"gemini", "gemini", "GEMINI_API_KEY", "https://generativelanguage.googleapis.com/v1beta"},
	{"ollama", "ollama", "OLLAMA_API_KEY", "https://ollama.com"},
}

// A provider on a vendor's driver that points at some other host and names no
// api_key_env must not key itself by the vendor's key name: a tenant that
// stored its own OpenAI key would otherwise have it sent to every self-hosted
// OpenAI-compatible endpoint the operator declared without a key.
func TestNewDriver_OffVendorBaseURLSendsNoTenantVendorKey(t *testing.T) {
	for _, v := range vendorKeyed {
		t.Run(v.driver, func(t *testing.T) {
			srv, seen := keyHeaderSrv(t)
			p, err := providers.NewDriver(v.driver, providers.DriverOptions{ID: v.id + "-selfhosted", BaseURL: srv.URL})
			if v.driver == "ollama" {
				// The hosted key belongs to the "ollama" id; keep that id so
				// the vendor key name is in play.
				p, err = providers.NewDriver(v.driver, providers.DriverOptions{ID: v.id, BaseURL: srv.URL})
			}
			if err != nil {
				t.Fatalf("NewDriver: %v", err)
			}
			if got := keyEnv(p); got != "" {
				t.Errorf("KeyEnvName() = %q, want \"\" for an endpoint that is not the vendor's", got)
			}
			ctx, asked := tenantKeyCtx()
			_, _ = p.Call(ctx, chatReq())
			if got := seen(); len(got) != 0 {
				t.Errorf("the endpoint received key headers %q, want none", got)
			}
			if got := asked(); len(got) != 0 {
				t.Errorf("the credential resolver was asked for %q, want no lookup", got)
			}
		})
	}
}

// The speech paths of the openai driver resolve their key as its chat does, so
// a self-hosted whisper or TTS server declared without a key gets none either.
func TestNewDriver_OffVendorAudioSendsNoTenantVendorKey(t *testing.T) {
	srv, seen := keyHeaderSrv(t)
	p, err := providers.NewDriver("openai", providers.DriverOptions{ID: "whisper-local", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	at, ok := providers.As[providers.AudioTranscriber](p)
	if !ok {
		t.Fatal("openai driver is not an AudioTranscriber")
	}
	ss, ok := providers.As[providers.SpeechSynthesizer](p)
	if !ok {
		t.Fatal("openai driver is not a SpeechSynthesizer")
	}
	ctx, asked := tenantKeyCtx()
	_, _ = at.TranscribeAudio(ctx, providers.AudioRequest{Model: "whisper", File: []byte("RIFF"), Filename: "a.wav"})
	_, _ = ss.SynthesizeSpeech(ctx, providers.SpeechRequest{Model: "tts", Voice: "v", Input: "hi", Format: "wav"})
	if got := seen(); len(got) != 0 {
		t.Errorf("the audio endpoint received key headers %q, want none", got)
	}
	if got := asked(); len(got) != 0 {
		t.Errorf("the credential resolver was asked for %q, want no lookup", got)
	}
}

// The vendor's own endpoint, or no base_url at all (the driver's default is the
// vendor), keeps the vendor key name. Host matching ignores case and the path.
func TestNewDriver_VendorOrUnsetBaseURLKeepsVendorKeyName(t *testing.T) {
	for _, v := range vendorKeyed {
		for _, base := range []string{"", v.vendorURL, "HTTPS://" + upperHost(v.vendorURL) + "/other/path"} {
			p, err := providers.NewDriver(v.driver, providers.DriverOptions{ID: v.id, BaseURL: base})
			if err != nil {
				t.Fatalf("NewDriver(%s, %q): %v", v.driver, base, err)
			}
			if got := keyEnv(p); got != v.keyName {
				t.Errorf("%s at %q: KeyEnvName() = %q, want %q", v.driver, base, got, v.keyName)
			}
		}
	}
}

// An api_key_env the operator names is used wherever the provider points.
func TestNewDriver_ExplicitKeyNameOffVendorIsResolvedAndSent(t *testing.T) {
	for _, v := range vendorKeyed {
		t.Run(v.driver, func(t *testing.T) {
			srv, seen := keyHeaderSrv(t)
			p, err := providers.NewDriver(v.driver, providers.DriverOptions{ID: v.id, BaseURL: srv.URL, KeyEnvName: "LOOMCYCLE_WHISPER_API_KEY"})
			if err != nil {
				t.Fatalf("NewDriver: %v", err)
			}
			if got := keyEnv(p); got != "LOOMCYCLE_WHISPER_API_KEY" {
				t.Errorf("KeyEnvName() = %q, want the configured name", got)
			}
			ctx, asked := tenantKeyCtx()
			_, _ = p.Call(ctx, chatReq())
			if got := asked(); len(got) == 0 || got[0] != "LOOMCYCLE_WHISPER_API_KEY" {
				t.Errorf("resolver asked for %q, want LOOMCYCLE_WHISPER_API_KEY", got)
			}
			if got := seen(); len(got) == 0 {
				t.Error("the endpoint received no key, want the credential stored under the configured name")
			}
		})
	}
}

// Every registered driver is classified: a driver added later must decide its
// default key name by endpoint too, or be listed here with the reason it may
// keep one off its default host.
func TestNewDriver_EveryDriverDropsItsDefaultKeyNameOffItsVendor(t *testing.T) {
	// vllm and llamacpp have no vendor: their default endpoint is localhost and
	// their key names are the servers' own (a secured vLLM's --api-key), so a
	// credential under that name is meant for whichever such server the
	// operator runs.
	selfHosted := map[string]string{"vllm": "VLLM_API_KEY", "llamacpp": "LLAMACPP_API_KEY"}
	for _, driver := range providers.RegisteredDrivers() {
		p, err := providers.NewDriver(driver, providers.DriverOptions{
			ID: driver, BaseURL: "http://127.0.0.1:8000/v1",
			Options: map[string]any{"code_root": t.TempDir()},
		})
		if err != nil {
			t.Fatalf("NewDriver(%s): %v", driver, err)
		}
		if got, want := keyEnv(p), selfHosted[driver]; got != want {
			t.Errorf("%s off its vendor: KeyEnvName() = %q, want %q", driver, got, want)
		}
	}
}

// An empty key name never reaches the credential store, and a provider with no
// key at all serves a run that may not use the operator's key: there is no
// operator key to spend. Routing already counts such a provider as keyable.
func TestResolveKeyOrOperator_EmptyNameIsKeyless(t *testing.T) {
	ctx, asked := tenantKeyCtx()
	restricted := providers.WithOperatorKeyAllowed(ctx, false)

	key, source, _, err := providers.ResolveKeyOrOperator(restricted, "", "")
	if err != nil || key != "" || source != "operator" {
		t.Errorf("keyless, restricted = (%q, %q, %v), want (\"\", operator, nil)", key, source, err)
	}
	if got := asked(); len(got) != 0 {
		t.Errorf("resolver asked for %q, want no lookup for an empty name", got)
	}
	// With an operator key in hand and no name, a restricted run is still
	// refused: the empty name only skips the tenant lookup.
	if _, _, _, err := providers.ResolveKeyOrOperator(restricted, "", "host-key"); !errors.Is(err, providers.ErrOperatorKeyForbidden) {
		t.Errorf("no name, operator key, restricted: err = %v, want ErrOperatorKeyForbidden", err)
	}
	if key, _, _, err := providers.ResolveKeyOrOperator(ctx, "", "host-key"); err != nil || key != "host-key" {
		t.Errorf("no name, operator key, allowed = (%q, %v), want the operator key", key, err)
	}
}

func TestIsVendorEndpoint_MatchesTheVendorHostOnly(t *testing.T) {
	const vendor = "https://api.openai.com/v1"
	for _, c := range []struct {
		base string
		want bool
	}{
		{"", true},
		{"https://api.openai.com/v1", true},
		{"https://API.OpenAI.com", true},
		{"https://api.openai.com:443/v2", true},
		{"https://api.openai.com./v1", true},
		{"http://api.openai.com/v1", false},
		{"https://api.openai.com:8443/v1", false},
		{"https://api.openai.com.evil.example/v1", false},
		{"https://api.openai.com@evil.example/v1", false},
		{"https://eu.api.openai.com/v1", false},
		{"http://127.0.0.1:8000/v1", false},
		{"api.openai.com", false},
		{"://bad", false},
	} {
		if got := providers.IsVendorEndpoint(c.base, vendor); got != c.want {
			t.Errorf("IsVendorEndpoint(%q) = %v, want %v", c.base, got, c.want)
		}
	}
}

// upperHost returns the host of u in upper case, for the case-insensitivity
// check.
func upperHost(u string) string {
	h := u[len("https://"):]
	for i, r := range h {
		if r == '/' {
			h = h[:i]
			break
		}
	}
	out := []byte(h)
	for i, b := range out {
		if b >= 'a' && b <= 'z' {
			out[i] = b - 'a' + 'A'
		}
	}
	return string(out)
}
