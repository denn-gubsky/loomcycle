package cli

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory/embedders"
)

// doctor_adapters.go — narrow wrappers around config.Config so the
// doctor handler can take a small `configForDoctor` interface (for
// testability) without dragging the full Config dependency tree into
// every test fixture.

// realConfig wraps *config.Config in the narrow configForDoctor view.
type realConfig struct {
	cfg *config.Config
}

func (r *realConfig) ProviderPriorityList() []string { return r.cfg.ProviderPriority }

func (r *realConfig) ConfigWarnings() []string { return r.cfg.Warnings }

func (r *realConfig) AgentProviderHints() []string {
	out := []string{}
	for _, def := range r.cfg.Agents {
		if def.Provider != "" {
			out = append(out, def.Provider)
		}
		out = append(out, def.Providers...)
	}
	return out
}

func (r *realConfig) UserTierProviderHints() []string {
	out := []string{}
	for _, ut := range r.cfg.UserTiers {
		out = append(out, ut.ProviderPriority...)
	}
	return out
}

func (r *realConfig) ProviderAPIKeyEnv(id string) string {
	return r.cfg.Providers[id].APIKeyEnv
}
func (r *realConfig) AWSAuthProviders() map[string]AWSAuthProvider {
	out := map[string]AWSAuthProvider{}
	for id, pc := range r.cfg.Providers {
		if pc.Auth != nil {
			out[id] = AWSAuthProvider{BaseURL: pc.BaseURL, Auth: *pc.Auth}
		}
	}
	return out
}

// Embedder builds the embedder the server would (no network: a Bedrock
// embedder's credentials are fetched on its first call) to report its width.
func (r *realConfig) Embedder() (*DoctorEmbedder, error) {
	ec := r.cfg.Memory.Embedder
	if ec.Provider == "" && ec.Model == "" {
		return nil, nil
	}
	provider, model, err := r.cfg.ExpandServiceModel("memory.embedder", ec.Provider, ec.Model)
	if err != nil {
		return nil, err
	}
	key, err := r.cfg.EmbedderKey(provider)
	if err != nil {
		return nil, err
	}
	e, err := embedders.Build(r.cfg)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, nil
	}
	out := &DoctorEmbedder{Provider: provider, Key: key, Model: model, Dimension: e.Dimension()}
	if pc := r.cfg.Providers[provider]; key == config.EmbedderAPIBedrock && pc.Auth != nil {
		out.AWS = &AWSAuthProvider{BaseURL: pc.BaseURL, Auth: *pc.Auth}
	}
	return out, nil
}

func (r *realConfig) TokenHasher() (auth.TokenHasher, error) { return r.cfg.Env.TokenHasher() }

func (r *realConfig) HostShellEnabled() bool {
	return r.cfg.Env.BashEnabled || len(r.cfg.Env.BashboxFallbackCommands) > 0
}

func (r *realConfig) StorageBackend() string  { return r.cfg.Storage.Backend }
func (r *realConfig) StoragePgDSN() string    { return r.cfg.Storage.PgDSN }
func (r *realConfig) StorageDataDir() string  { return r.cfg.Env.DataDir }
func (r *realConfig) ListenAddrValue() string { return r.cfg.Env.ListenAddr }
func (r *realConfig) NonLoopbackListeners() []string {
	return r.cfg.Env.NonLoopbackListeners()
}
func (r *realConfig) AllowOpenMode() bool     { return r.cfg.Env.AllowOpenMode }
func (r *realConfig) DeclaredPrincipals() int { return len(r.cfg.Principals) }
func (r *realConfig) LoginPrincipals() int    { return len(r.cfg.ResolvedPrincipals) }

// loadConfigRealAdapter is the production seam for loadConfigForDoctor.
// Tests override the package-level var to plug in a stub.
func loadConfigRealAdapter(path string) (configForDoctor, error) {
	cfg, err := loadLayeredConfig(path)
	if err != nil {
		return nil, err
	}
	return &realConfig{cfg: cfg}, nil
}

// timeoutContext is a tiny wrapper over context.WithTimeout to avoid
// the context.Background() noise at the call site. Doctor's port
// check needs a short deadline; bigger checks may borrow this too.
func timeoutContext(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
