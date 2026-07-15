package cli

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
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
func (r *realConfig) StorageBackend() string  { return r.cfg.Storage.Backend }
func (r *realConfig) StoragePgDSN() string    { return r.cfg.Storage.PgDSN }
func (r *realConfig) StorageDataDir() string  { return r.cfg.Env.DataDir }
func (r *realConfig) ListenAddrValue() string { return r.cfg.Env.ListenAddr }

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
