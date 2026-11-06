package providerbuild

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/deepseek" // registers the driver
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"   // registers the driver
)

// authSrv answers 400 to everything and records each request's Authorization.
func authSrv(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, `{"error":{"message":"stub"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), auths...)
	}
}

// tenantVendorKeys is a run whose tenant stored its own OpenAI and DeepSeek
// keys under the vendors' names.
func tenantVendorKeys() context.Context {
	return providers.WithCredentialResolver(context.Background(), func(_ context.Context, name string) (providers.CredentialResolution, bool, error) {
		switch name {
		case "OPENAI_API_KEY", "DEEPSEEK_API_KEY":
			return providers.CredentialResolution{Value: "tenant-" + name, Scope: "tenant", ScopeID: "t1"}, true, nil
		}
		return providers.CredentialResolution{}, false, nil
	})
}

func oneTurn() providers.Request {
	return providers.Request{Model: "m", MaxTokens: 16, Messages: []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
	}}
}

// A provider declared on the openai or deepseek driver, pointed at a server of
// the operator's own and naming no api_key_env, sends neither the operator's
// vendor key from the environment nor a tenant's stored vendor key: chat and
// transcription alike call it with no Authorization.
func TestProvider_OffVendorBaseURLGetsNoDefaultKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "operator-openai-key")
	t.Setenv("DEEPSEEK_API_KEY", "operator-deepseek-key")
	srv, auths := authSrv(t)
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"whisper-local":   {Driver: "openai", BaseURL: srv.URL},
		"deepseek-mirror": {Driver: "deepseek", BaseURL: srv.URL},
	}}
	ctx := tenantVendorKeys()

	for _, id := range []string{"whisper-local", "deepseek-mirror"} {
		p, err := Provider(cfg, id)
		if err != nil {
			t.Fatalf("Provider(%s): %v", id, err)
		}
		_, _ = p.Call(ctx, oneTurn())
	}
	p, err := Provider(cfg, "whisper-local")
	if err != nil {
		t.Fatalf("Provider(whisper-local): %v", err)
	}
	at, ok := providers.As[providers.AudioTranscriber](p)
	if !ok {
		t.Fatal("whisper-local is not an AudioTranscriber")
	}
	_, _ = at.TranscribeAudio(ctx, providers.AudioRequest{Model: "whisper", File: []byte("RIFF"), Filename: "a.wav"})

	got := auths()
	if len(got) != 3 {
		t.Fatalf("requests = %d, want 3 (two chats, one transcription)", len(got))
	}
	for i, a := range got {
		if a != "" {
			t.Errorf("request %d carried Authorization %q, want none", i, a)
		}
	}
}

// The vendor's own provider keeps its key name, and an api_key_env the operator
// names is sent wherever the provider points.
func TestProvider_VendorOrNamedKeyStillKeysTheProvider(t *testing.T) {
	t.Setenv("LOOMCYCLE_WHISPER_API_KEY", "whisper-key")
	srv, auths := authSrv(t)
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"openai":        {Driver: "openai"},
		"deepseek":      {Driver: "deepseek", BaseURL: "https://api.deepseek.com/v1"},
		"whisper-local": {Driver: "openai", BaseURL: srv.URL, APIKeyEnv: "LOOMCYCLE_WHISPER_API_KEY"},
	}}
	for id, want := range map[string]string{"openai": "OPENAI_API_KEY", "deepseek": "DEEPSEEK_API_KEY", "whisper-local": "LOOMCYCLE_WHISPER_API_KEY"} {
		p, err := Provider(cfg, id)
		if err != nil {
			t.Fatalf("Provider(%s): %v", id, err)
		}
		kp, ok := providers.As[providers.KeyedProvider](p)
		if !ok || kp.KeyEnvName() != want {
			t.Errorf("%s: KeyEnvName = %v/%q, want %q", id, ok, keyName(p), want)
		}
	}
	p, err := Provider(cfg, "whisper-local")
	if err != nil {
		t.Fatalf("Provider(whisper-local): %v", err)
	}
	_, _ = p.Call(context.Background(), oneTurn())
	if got := auths(); len(got) != 1 || got[0] != "Bearer whisper-key" {
		t.Errorf("Authorization = %q, want the named key", got)
	}
}

func keyName(p providers.Provider) string {
	if kp, ok := providers.As[providers.KeyedProvider](p); ok {
		return kp.KeyEnvName()
	}
	return ""
}
